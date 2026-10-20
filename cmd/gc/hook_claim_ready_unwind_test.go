package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// readyUnwindOps wires a hook claim over one real MemStore: the work query
// reads the store's row, the claim promotes it to in_progress in place, and the
// release and restore run the store's own conditional writes. Every assertion
// is then about the row the store holds, not about which seam was called.
func readyUnwindOps(t *testing.T, store *beads.MemStore, id string) hookClaimOps {
	t.Helper()
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, "")
	ops.Runner = func(string, string) (string, error) {
		b, err := store.Get(id)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`[{"id":%q,"status":%q,"assignee":%q,"metadata":{"gc.routed_to":"worker"}}]`, b.ID, b.Status, b.Assignee), nil
	}
	ops.Claim = func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
		cur, err := store.Get(beadID)
		if err != nil {
			return beads.Bead{}, false, err
		}
		if cur.Assignee != "" && cur.Assignee != assignee {
			return cur, false, nil
		}
		inProgress := "in_progress"
		if err := store.Update(beadID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
			return beads.Bead{}, false, err
		}
		got, err := store.Get(beadID)
		return got, err == nil, err
	}
	ops.Release = func(_ context.Context, _ string, _ []string, beadID, assignee string) (bool, error) {
		return store.ReleaseIfCurrent(beadID, assignee)
	}
	ops.Restore = func(_ context.Context, _ string, _ []string, beadID, holder, prior string) (bool, error) {
		return hookClaimRestoreOnStore(store, beadID, holder, prior)
	}
	// The canonical readback (and the unwind's re-read after a failed restore)
	// reads the same store.
	ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
		return store.Get(beadID)
	}
	return ops
}

// TestHookClaimUnwindRestoresReadyAssignment pins ga-812fpb: the
// ready-assignment tier claims an OPEN bead that was ALREADY assigned to this
// seat. When the claim then has to be unwound (its result could not be
// delivered), the bead must go back to exactly what it was — open AND assigned
// to the seat — not open and unassigned, which silently strips a named seat's
// pre-assigned work of its owner.
func TestHookClaimUnwindRestoresReadyAssignment(t *testing.T) {
	const seat = "gc__role-mc-sess1"
	store := beads.NewMemStore()
	seeded, err := store.Create(beads.Bead{Title: "pre-assigned", Status: "open", Assignee: seat})
	if err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := doHookClaim("query", "/rig", poolClaimOpts(), readyUnwindOps(t, store, seeded.ID), brokenPipeWriter{}, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (the claim was not delivered); stderr=%s", code, stderr.String())
	}
	got, err := store.Get(seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "open" || got.Assignee != seat {
		t.Fatalf("after unwind: status=%q assignee=%q, want status=open assignee=%q (the pre-claim owner); stderr=%s", got.Status, got.Assignee, seat, stderr.String())
	}
}

// TestHookClaimUnwindStillClearsFreshClaim is the control: a FRESH claim of an
// unassigned bead had no prior owner, so the unwind leaves it open and
// unassigned. A restore that wrote the claimant back would park it instead.
func TestHookClaimUnwindStillClearsFreshClaim(t *testing.T) {
	store := beads.NewMemStore()
	seeded, err := store.Create(beads.Bead{Title: "fresh", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := doHookClaim("query", "/rig", poolClaimOpts(), readyUnwindOps(t, store, seeded.ID), brokenPipeWriter{}, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
	}
	got, err := store.Get(seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("after unwind: status=%q assignee=%q, want open and unassigned; stderr=%s", got.Status, got.Assignee, stderr.String())
	}
}

// TestHookClaimUnwindRestoreLeavesMovedBeadAlone pins the fence on the restore:
// a bead that changed hands after the claim is not written back to the seat.
func TestHookClaimUnwindRestoreLeavesMovedBeadAlone(t *testing.T) {
	store := beads.NewMemStore()
	seeded, err := store.Create(beads.Bead{Title: "moved"})
	if err != nil {
		t.Fatal(err)
	}
	inProgress, other := "in_progress", "someone-else"
	if err := store.Update(seeded.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &other}); err != nil {
		t.Fatal(err)
	}
	restored, err := hookClaimRestoreOnStore(store, seeded.ID, "gc__role-mc-sess1", "gc__role-mc-sess1")
	if err != nil || restored {
		t.Fatalf("restore of a bead held by someone else = (%v, %v), want (false, nil)", restored, err)
	}
	got, _ := store.Get(seeded.ID)
	if got.Status != "in_progress" || got.Assignee != "someone-else" {
		t.Fatalf("moved bead was written: status=%q assignee=%q", got.Status, got.Assignee)
	}
}

// TestHookClaimUnwindFallbackReleaseGetsItsOwnDeadline pins the PR #305 review
// P1: a restore that spends its whole deadline without committing must not hand
// the fallback release an already-expired context, or the bead stays
// in_progress under a claim nobody received.
func TestHookClaimUnwindFallbackReleaseGetsItsOwnDeadline(t *testing.T) {
	old := hookClaimMutationTimeout
	hookClaimMutationTimeout = 200 * time.Millisecond
	t.Cleanup(func() { hookClaimMutationTimeout = old })

	const seat = "gc__role-mc-sess1"
	store := beads.NewMemStore()
	seeded, err := store.Create(beads.Bead{Title: "pre-assigned", Status: "open", Assignee: seat})
	if err != nil {
		t.Fatal(err)
	}
	ops := readyUnwindOps(t, store, seeded.ID)
	ops.Restore = func(ctx context.Context, _ string, _ []string, _, _, _ string) (bool, error) {
		if err := awaitUnwindBarrier(ctx.Done(), "the restore's deadline never expired"); err != nil {
			t.Error(err)
		}
		return false, ctx.Err()
	}
	ops.Release = func(ctx context.Context, _ string, _ []string, beadID, assignee string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return store.ReleaseIfCurrent(beadID, assignee)
	}

	var stderr bytes.Buffer
	if code := doHookClaim("query", "/rig", poolClaimOpts(), ops, brokenPipeWriter{}, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
	}
	got, _ := store.Get(seeded.ID)
	if got.Status != "open" {
		t.Fatalf("after a timed-out restore: status=%q assignee=%q, want the fallback release to land (open); stderr=%s", got.Status, got.Assignee, stderr.String())
	}
	if !strings.Contains(stderr.String(), "bd update "+seeded.ID+" --assignee \""+seat+"\"") {
		t.Fatalf("stderr does not name the lost owner's recovery; stderr=%s", stderr.String())
	}
}

// unwindRaceStore hands the restore's fence read a snapshot, then applies mutate to
// the live bead before returning it: the change lands AFTER the restore's read
// and BEFORE its write. Only the first Get races, and raced records that it
// did, so a test can refuse to pass on a race that never fired.
type unwindRaceStore struct {
	*beads.MemStore
	mutate func(id string) error
	raced  bool
}

func (s *unwindRaceStore) Get(id string) (beads.Bead, error) {
	snap, err := s.MemStore.Get(id)
	if err == nil && !s.raced {
		s.raced = true
		if merr := s.mutate(id); merr != nil {
			return beads.Bead{}, merr
		}
	}
	return snap, err
}

// unwindSeedInProgress creates a bead in_progress under holder.
func unwindSeedInProgress(t *testing.T, store *beads.MemStore, holder string) beads.Bead {
	t.Helper()
	seeded, err := store.Create(beads.Bead{Title: "claimed"})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := store.Update(seeded.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &holder}); err != nil {
		t.Fatal(err)
	}
	return seeded
}

func unwindSetStatus(store *beads.MemStore, status string) func(string) error {
	return func(id string) error { return store.Update(id, beads.UpdateOpts{Status: &status}) }
}

func unwindSetAssignee(store *beads.MemStore, assignee string) func(string) error {
	return func(id string) error { return store.Update(id, beads.UpdateOpts{Assignee: &assignee}) }
}

// TestHookClaimUnwindRestoreLostFallsBackToRelease: a write lands between the
// restore's read and its write without changing owner or status (a metadata
// stamp). The restore's fence is lost, so it must not write; the unwind falls
// back to the holder-fenced release, which still gets the claim back, and the
// lost owner is surfaced with its recovery.
func TestHookClaimUnwindRestoreLostFallsBackToRelease(t *testing.T) {
	const seat = "gc__role-mc-sess1"
	store := beads.NewMemStore()
	seeded, err := store.Create(beads.Bead{Title: "pre-assigned", Status: "open", Assignee: seat})
	if err != nil {
		t.Fatal(err)
	}
	ops := readyUnwindOps(t, store, seeded.ID)
	race := &unwindRaceStore{MemStore: store, mutate: func(id string) error {
		return store.Update(id, beads.UpdateOpts{Metadata: map[string]string{"gc.unrelated": "stamped"}})
	}}
	ops.Restore = func(_ context.Context, _ string, _ []string, beadID, holder, prior string) (bool, error) {
		return hookClaimRestoreOnStore(race, beadID, holder, prior)
	}

	var stderr bytes.Buffer
	if code := doHookClaim("query", "/rig", poolClaimOpts(), ops, brokenPipeWriter{}, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !race.raced {
		t.Fatal("the race never fired, so this test proves nothing")
	}
	got, _ := store.Get(seeded.ID)
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("after a lost restore: status=%q assignee=%q, want the fallback release (open, unassigned)", got.Status, got.Assignee)
	}
	for _, want := range []string{errHookClaimRestoreLost.Error(), "bd update " + seeded.ID + " --assignee \"" + seat + "\""} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q; stderr=%s", want, stderr.String())
		}
	}
}

// TestHookClaimUnwindRestoreFailureFallsBackToRelease: a restore that errors
// outright still gets the claim back through the release.
func TestHookClaimUnwindRestoreFailureFallsBackToRelease(t *testing.T) {
	const seat = "gc__role-mc-sess1"
	store := beads.NewMemStore()
	seeded, err := store.Create(beads.Bead{Title: "pre-assigned", Status: "open", Assignee: seat})
	if err != nil {
		t.Fatal(err)
	}
	ops := readyUnwindOps(t, store, seeded.ID)
	ops.Restore = func(context.Context, string, []string, string, string, string) (bool, error) {
		return false, errors.New("binding refused the restore")
	}

	var stderr bytes.Buffer
	if code := doHookClaim("query", "/rig", poolClaimOpts(), ops, brokenPipeWriter{}, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
	}
	got, _ := store.Get(seeded.ID)
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("after a failed restore: status=%q assignee=%q, want released (open, unassigned)", got.Status, got.Assignee)
	}
	if !strings.Contains(stderr.String(), "binding refused the restore") {
		t.Fatalf("stderr does not surface the restore failure; stderr=%s", stderr.String())
	}
}

// TestHookClaimUnwindRestoresLegacySpellingAfterRestamp: a ready claim of a
// bead held under a legacy spelling is re-stamped to the claim identity, so the
// unwind's holder and the pre-claim owner are DIFFERENT strings. The restore
// must fence on the re-stamped holder and write back the legacy spelling, and
// must not overwrite an owner change that lands between its read and write.
func TestHookClaimUnwindRestoresLegacySpellingAfterRestamp(t *testing.T) {
	const (
		current = "gcg-session-1"
		legacy  = "claude-gcg-session-1"
	)
	run := func(t *testing.T, race bool) (beads.Bead, *unwindRaceStore, string) {
		store := beads.NewMemStore()
		seeded, err := store.Create(beads.Bead{Title: "legacy-assigned", Status: "open", Assignee: legacy})
		if err != nil {
			t.Fatal(err)
		}
		ops := readyUnwindOps(t, store, seeded.ID)
		rs := &unwindRaceStore{MemStore: store, mutate: unwindSetAssignee(store, "thief"), raced: !race}
		var holderSeen string
		ops.Restore = func(_ context.Context, _ string, _ []string, beadID, holder, prior string) (bool, error) {
			holderSeen = holder
			return hookClaimRestoreOnStore(rs, beadID, holder, prior)
		}
		ops.RestampAdopted = func(_ context.Context, _ string, _ []string, beadID, from, to string) (bool, error) {
			cur, err := store.Get(beadID)
			if err != nil || cur.Status != "in_progress" || cur.Assignee != from {
				return false, err
			}
			return true, store.Update(beadID, beads.UpdateOpts{Assignee: &to})
		}
		opts := poolClaimOpts()
		opts.Assignee = current
		opts.RuntimeActor = current
		opts.IdentityCandidates = []string{current, legacy}
		var stderr bytes.Buffer
		if code := doHookClaim("query", "/rig", opts, ops, brokenPipeWriter{}, &stderr); code != 1 {
			t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
		}
		if holderSeen != current {
			t.Fatalf("restore fenced on holder %q, want the re-stamped %q (the restamp did not run, so this test proves nothing)", holderSeen, current)
		}
		got, _ := store.Get(seeded.ID)
		return got, rs, stderr.String()
	}
	t.Run("restores", func(t *testing.T) {
		got, _, stderr := run(t, false)
		if got.Status != "open" || got.Assignee != legacy {
			t.Fatalf("after unwind: status=%q assignee=%q, want open under the pre-claim spelling %q; stderr=%s", got.Status, got.Assignee, legacy, stderr)
		}
	})
	t.Run("owner change mid-write", func(t *testing.T) {
		got, rs, stderr := run(t, true)
		if !rs.raced {
			t.Fatal("the race never fired, so this test proves nothing")
		}
		if got.Status != "in_progress" || got.Assignee != "thief" {
			t.Fatalf("owner change was overwritten: status=%q assignee=%q; stderr=%s", got.Status, got.Assignee, stderr)
		}
	})
}

// TestHookClaimRestoreStatusOnlyChangeIsLeftAlone: the fence covers status as
// well as owner. A status change that lands between the restore's read and its
// write (owner unchanged) must not be overwritten.
func TestHookClaimRestoreStatusOnlyChangeIsLeftAlone(t *testing.T) {
	const seat = "gc__role-mc-sess1"
	mem := beads.NewMemStore()
	seeded := unwindSeedInProgress(t, mem, seat)
	store := &unwindRaceStore{MemStore: mem, mutate: unwindSetStatus(mem, "blocked")}
	restored, err := hookClaimRestoreOnStore(store, seeded.ID, seat, "someone-prior")
	if restored || !errors.Is(err, errHookClaimRestoreLost) {
		t.Fatalf("restore across a status change = (%v, %v), want (false, errHookClaimRestoreLost)", restored, err)
	}
	if !store.raced {
		t.Fatal("the race never fired, so this test proves nothing")
	}
	got, _ := mem.Get(seeded.ID)
	if got.Status != "blocked" || got.Assignee != seat {
		t.Fatalf("status change was overwritten: status=%q assignee=%q", got.Status, got.Assignee)
	}
}

// TestHookClaimRestoreLosesToAnOwnerChangeMidWrite pins that the restore's write
// is fenced on the revision it read: an owner change between the read and the
// write is not overwritten, and is reported as a lost fence (so the unwind
// falls back to the holder-fenced release, which also leaves it alone).
func TestHookClaimRestoreLosesToAnOwnerChangeMidWrite(t *testing.T) {
	const seat = "gc__role-mc-sess1"
	mem := beads.NewMemStore()
	seeded := unwindSeedInProgress(t, mem, seat)
	store := &unwindRaceStore{MemStore: mem, mutate: unwindSetAssignee(mem, "thief")}
	restored, err := hookClaimRestoreOnStore(store, seeded.ID, seat, seat)
	if restored || !errors.Is(err, errHookClaimRestoreLost) {
		t.Fatalf("restore across an owner change = (%v, %v), want (false, errHookClaimRestoreLost)", restored, err)
	}
	if !store.raced {
		t.Fatal("the race never fired, so this test proves nothing")
	}
	if released, rerr := mem.ReleaseIfCurrent(seeded.ID, seat); released || rerr != nil {
		t.Fatalf("fallback release across the owner change = (%v, %v), want (false, nil)", released, rerr)
	}
	got, _ := mem.Get(seeded.ID)
	if got.Status != "in_progress" || got.Assignee != "thief" {
		t.Fatalf("owner change was overwritten: status=%q assignee=%q", got.Status, got.Assignee)
	}
}

// unwindBarrierTimeout bounds every barrier wait in this file. A barrier that
// never opens means the code under test stopped doing what the test relies
// on (a restore that no longer honors its deadline, a write that never
// commits); that must fail the test, not hang it to the suite timeout.
const unwindBarrierTimeout = 5 * time.Second

// awaitUnwindBarrier waits for ch to close, and reports what never happened
// if it does not within unwindBarrierTimeout.
func awaitUnwindBarrier(ch <-chan struct{}, what string) error {
	select {
	case <-ch:
		return nil
	case <-time.After(unwindBarrierTimeout):
		return fmt.Errorf("test barrier: %s within %s", what, unwindBarrierTimeout)
	}
}

// unwindRaceGraph is unwindRaceStore for the graph binding: its first Get returns the
// snapshot, then (after an optional hold) applies mutate before the caller can
// write. done closes when that Get returns, so a test can wait out a Get its
// caller abandoned before the store underneath is closed.
//
// The restore's calls run on a goroutine runWithDeadline may abandon, so the
// double's own state is guarded by mu.
type unwindRaceGraph struct {
	storebinding.GraphStore
	mutate func(id string) error
	// hold, when set, parks the first Get after its read until the test
	// closes it: a stalled read, as a channel barrier rather than a sleep.
	hold chan struct{}
	// stallRelease parks ReleaseIfCurrent on hold, then fails it unwritten.
	stallRelease bool
	// holdWrite, when set, parks the FIRST UpdateIfMatch (the restore's) until
	// the fallback release arrives — its first call into this double after
	// the restore's read — then lets that write COMMIT before the release
	// proceeds: a restore write started inside the deadline that lands after
	// it. writeDone closes once that late write has committed.
	holdWrite chan struct{}
	writeDone chan struct{}
	done      chan struct{}

	mu      sync.Mutex
	raced   bool
	wrote   bool
	arrived bool
}

// hasRaced reports whether the first Get has run.
func (g *unwindRaceGraph) hasRaced() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.raced
}

// arrive is the fallback release reaching the store: it lets a parked
// restore write commit, and waits until it has.
func (g *unwindRaceGraph) arrive() error {
	if g.holdWrite == nil {
		return nil
	}
	g.mu.Lock()
	if !g.arrived {
		g.arrived = true
		close(g.holdWrite)
	}
	g.mu.Unlock()
	return awaitUnwindBarrier(g.writeDone, "the late restore write never committed")
}

func (g *unwindRaceGraph) Get(id string) (beads.Bead, error) {
	g.mu.Lock()
	first := !g.raced
	g.raced = true
	g.mu.Unlock()
	if !first {
		if err := g.arrive(); err != nil {
			return beads.Bead{}, err
		}
		return g.GraphStore.Get(id)
	}
	defer close(g.done)
	snap, err := g.GraphStore.Get(id)
	if g.hold != nil {
		if herr := awaitUnwindBarrier(g.hold, "the stalled read was never released"); herr != nil {
			return beads.Bead{}, herr
		}
	}
	if err == nil && g.mutate != nil {
		if merr := g.mutate(id); merr != nil {
			return beads.Bead{}, merr
		}
	}
	return snap, err
}

func (g *unwindRaceGraph) UpdateIfMatch(id string, expected int64, opts beads.UpdateOpts) error {
	g.mu.Lock()
	first := !g.wrote
	g.wrote = true
	g.mu.Unlock()
	if g.holdWrite == nil || !first {
		return g.GraphStore.UpdateIfMatch(id, expected, opts)
	}
	if err := awaitUnwindBarrier(g.holdWrite, "the fallback release never arrived"); err != nil {
		return err
	}
	defer close(g.writeDone)
	return g.GraphStore.UpdateIfMatch(id, expected, opts)
}

func (g *unwindRaceGraph) ReleaseIfCurrent(id, assignee string) (bool, error) {
	if err := g.arrive(); err != nil {
		return false, err
	}
	if !g.stallRelease {
		return g.GraphStore.ReleaseIfCurrent(id, assignee)
	}
	return false, awaitUnwindBarrier(g.hold, "the stalled release was never released")
}

// graphRestoreRig is a class store, a graph binding over it that races, and
// the route's ops with id known resident in the binding.
func graphRestoreRig(t *testing.T, id, status, assignee string, race *unwindRaceGraph, released ...*[]hookClaimReleaseRecord) (beads.Store, hookClaimOps) {
	t.Helper()
	class := newClaimRouteClassStore(t)
	graph, err := storebinding.NewBeadsGraphStore(class)
	if err != nil {
		t.Fatal(err)
	}
	mintClaimRouteBead(t, class, id, nil)
	if err := class.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatal(err)
	}
	race.GraphStore = graph
	race.done = make(chan struct{})
	// Registered after the store's own cleanup, so it runs first: never close
	// the store under a Get the restore abandoned at its deadline.
	t.Cleanup(func() {
		if race.hasRaced() {
			if err := awaitUnwindBarrier(race.done, "the abandoned graph read never returned"); err != nil {
				t.Error(err)
			}
		}
	})
	route := newClaimClassRouteOver(class, race)
	route.resident[id] = true
	emit := func(rec hookClaimReleaseRecord) {
		for _, sink := range released {
			*sink = append(*sink, rec)
		}
	}
	ops := classRoutedHookClaimOps(hookClaimOps{EmitClaimReleased: emit}, route)
	return class, ops
}

func unwindClassUpdate(class *beads.Store, status, assignee string) func(string) error {
	return func(id string) error {
		opts := beads.UpdateOpts{}
		if status != "" {
			opts.Status = &status
		}
		if assignee != "" {
			opts.Assignee = &assignee
		}
		return (*class).Update(id, opts)
	}
}

// TestHookClaimRestoreOnGraphBinding drives the routed restore of a bead
// resident in the graph binding over a real class store: it restores a claim
// it still holds, and leaves alone one whose owner or status changes between
// the restore's read and its write.
func TestHookClaimRestoreOnGraphBinding(t *testing.T) {
	const seat = "gc__role-mc-sess1"
	t.Run("restores", func(t *testing.T) {
		race := &unwindRaceGraph{}
		class, ops := graphRestoreRig(t, "gcg-held", "in_progress", seat, race)
		if ok, err := ops.Restore(context.Background(), "/rig", nil, "gcg-held", seat, seat); !ok || err != nil {
			t.Fatalf("restore of a held graph claim = (%v, %v), want (true, nil)", ok, err)
		}
		if got, _ := class.Get("gcg-held"); got.Status != "open" || got.Assignee != seat {
			t.Fatalf("after restore: status=%q assignee=%q, want open under %q", got.Status, got.Assignee, seat)
		}
	})
	for name, change := range map[string][2]string{
		"owner change mid-write":  {"", "someone-else"},
		"status change mid-write": {"blocked", ""},
	} {
		t.Run(name, func(t *testing.T) {
			var class beads.Store
			race := &unwindRaceGraph{}
			race.mutate = func(id string) error { return unwindClassUpdate(&class, change[0], change[1])(id) }
			class, ops := graphRestoreRig(t, "gcg-race", "in_progress", seat, race)
			_ = class
			ok, err := ops.Restore(context.Background(), "/rig", nil, "gcg-race", seat, seat)
			if ok || !errors.Is(err, errHookClaimRestoreLost) {
				t.Fatalf("restore across the change = (%v, %v), want (false, errHookClaimRestoreLost)", ok, err)
			}
			if !race.hasRaced() {
				t.Fatal("the race never fired, so this test proves nothing")
			}
			wantStatus, wantAssignee := "in_progress", seat
			if change[0] != "" {
				wantStatus = change[0]
			}
			if change[1] != "" {
				wantAssignee = change[1]
			}
			if got, _ := class.Get("gcg-race"); got.Status != wantStatus || got.Assignee != wantAssignee {
				t.Fatalf("change was overwritten: status=%q assignee=%q, want %q/%q", got.Status, got.Assignee, wantStatus, wantAssignee)
			}
		})
	}
}

// TestHookClaimUnwindGraphRestoreHonorsDeadline pins the PR #305 re-review P2:
// the graph-binding restore runs under the unwind's deadline. A graph read that
// stalls past it must not hold the unwind: the restore gives up at the deadline
// and the fallback release still gets the claim back, or gives up at its own.
func TestHookClaimUnwindGraphRestoreHonorsDeadline(t *testing.T) {
	old := hookClaimMutationTimeout
	hookClaimMutationTimeout = 200 * time.Millisecond
	t.Cleanup(func() { hookClaimMutationTimeout = old })

	const seat = "gc__role-mc-sess1"
	for _, stallRelease := range []bool{false, true} {
		t.Run(fmt.Sprintf("stallRelease=%v", stallRelease), func(t *testing.T) {
			race := &unwindRaceGraph{hold: make(chan struct{}), stallRelease: stallRelease}
			class, ops := graphRestoreRig(t, "gcg-stall", "in_progress", seat, race)
			// Runs before graphRestoreRig's cleanup, which waits on what it unparks.
			t.Cleanup(func() { close(race.hold) })

			var stderr bytes.Buffer
			start := time.Now()
			code := unwindUndeliveredHookClaim(hookClaimReleaseReasonUndelivered, "test unwind", beads.Bead{ID: "gcg-stall", Assignee: seat}, seat,
				hookClaimOptions{Assignee: seat}, ops, "/rig", &stderr)
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("unwind took %s behind a stalled graph store, want it bounded by the %s restore and release deadlines", elapsed, hookClaimMutationTimeout)
			}
			if code != 1 {
				t.Fatalf("code = %d, want 1", code)
			}
			if stallRelease {
				if !strings.Contains(stderr.String(), "undelivered claim gcg-stall is still in_progress under this claim") {
					t.Fatalf("a timed-out release did not name the still-held claim; stderr=%s", stderr.String())
				}
			} else if got, _ := class.Get("gcg-stall"); got.Status != "open" || got.Assignee != "" {
				t.Fatalf("after a stalled restore: status=%q assignee=%q, want the fallback release (open, unassigned); stderr=%s", got.Status, got.Assignee, stderr.String())
			}
		})
	}
}

// TestHookClaimUnwindLateRestoreIsRecognized pins the PR #305 third review:
// runWithDeadline stops WAITING at the deadline but cannot stop a write the
// binding already started, so a restore can commit after its deadline. The
// fallback must then classify from a fresh read instead of assuming the
// restore failed: open under the pre-claim owner is a restore that landed
// late OR another actor's identical write; the unwind cannot tell which, so it
// says so, emits no bead.claim_released (that event records a release THIS
// process made), and never reports "no longer ours".
func TestHookClaimUnwindLateRestoreIsRecognized(t *testing.T) {
	old := hookClaimMutationTimeout
	hookClaimMutationTimeout = 200 * time.Millisecond
	t.Cleanup(func() { hookClaimMutationTimeout = old })

	const seat = "gc__role-mc-sess1"
	race := &unwindRaceGraph{holdWrite: make(chan struct{}), writeDone: make(chan struct{})}
	var released []hookClaimReleaseRecord
	class, ops := graphRestoreRig(t, "gcg-late", "in_progress", seat, race, &released)

	var stderr bytes.Buffer
	code := unwindUndeliveredHookClaim(hookClaimReleaseReasonUndelivered, "test unwind", beads.Bead{ID: "gcg-late", Assignee: seat}, seat,
		hookClaimOptions{Assignee: seat}, ops, "/rig", &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	select {
	case <-race.writeDone:
	default:
		t.Fatal("the late restore write never committed, so this test proves nothing")
	}
	if got, _ := class.Get("gcg-late"); got.Status != "open" || got.Assignee != seat {
		t.Fatalf("after a late restore: status=%q assignee=%q, want open under %q", got.Status, got.Assignee, seat)
	}
	if len(released) != 0 {
		t.Fatalf("bead.claim_released = %+v, want none: the unwind cannot prove it made this write", released)
	}
	if !strings.Contains(stderr.String(), "open under its pre-claim owner") || !strings.Contains(stderr.String(), "our late restore or an identical write") {
		t.Fatalf("stderr does not report the ambiguous late-restore verdict; stderr=%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "no longer ours") || strings.Contains(stderr.String(), "released it unassigned") {
		t.Fatalf("a late restore was misreported; stderr=%s", stderr.String())
	}
}

// TestHookClaimUnwindStillHeldIsNamed pins the PR #305 fifth review P2: a
// restore that timed out, a fallback release that errored, and a re-read that
// finds the bead UNCHANGED (still in_progress under our claim) is the claim
// still parked on us. It is named as such, with the recovery that reopens it,
// and never reported as "no longer ours".
func TestHookClaimUnwindStillHeldIsNamed(t *testing.T) {
	const seat = "gc__role-mc-sess1"
	store := beads.NewMemStore()
	seeded, err := store.Create(beads.Bead{Title: "pre-assigned", Status: "open", Assignee: seat})
	if err != nil {
		t.Fatal(err)
	}
	ops := readyUnwindOps(t, store, seeded.ID)
	ops.Restore = func(context.Context, string, []string, string, string, string) (bool, error) {
		return false, context.DeadlineExceeded
	}
	releaseRan := false
	ops.Release = func(context.Context, string, []string, string, string) (bool, error) {
		releaseRan = true
		return false, errors.New("release timed out")
	}
	var stderr bytes.Buffer
	if code := doHookClaim("query", "/rig", poolClaimOpts(), ops, brokenPipeWriter{}, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !releaseRan {
		t.Fatal("the fallback release never ran, so this test proves nothing")
	}
	if got, _ := store.Get(seeded.ID); got.Status != "in_progress" || got.Assignee != seat {
		t.Fatalf("seed: status=%q assignee=%q, want still in_progress under %q", got.Status, got.Assignee, seat)
	}
	out := stderr.String()
	for _, want := range []string{
		"undelivered claim " + seeded.ID + " is still in_progress under this claim",
		"release timed out",
		"bd update " + seeded.ID + " --status open --assignee \"" + seat + "\"",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q; stderr=%s", want, out)
		}
	}
	if strings.Contains(out, "no longer ours") {
		t.Fatalf("a bead still held by this claim was reported as no longer ours; stderr=%s", out)
	}
}

// TestHookClaimUnwindUnreadableAfterFailedRestoreIsUnresolved: a restore that
// failed, a fallback release that missed, and a re-read that FAILED leave the
// bead's state unknown. That is reported as unresolved with the recovery,
// never as "no longer ours".
func TestHookClaimUnwindUnreadableAfterFailedRestoreIsUnresolved(t *testing.T) {
	const seat = "gc__role-mc-sess1"
	store := beads.NewMemStore()
	seeded, err := store.Create(beads.Bead{Title: "pre-assigned", Status: "open", Assignee: seat})
	if err != nil {
		t.Fatal(err)
	}
	ops := readyUnwindOps(t, store, seeded.ID)
	ops.Restore = func(context.Context, string, []string, string, string, string) (bool, error) {
		return false, context.DeadlineExceeded
	}
	// The fallback release misses; only reads AFTER it (the unwind's re-read)
	// fail, so the claim path's own canonical readbacks still answer.
	releaseRan := false
	ops.Release = func(context.Context, string, []string, string, string) (bool, error) {
		releaseRan = true
		return false, nil
	}
	base := ops.ReadWorkMeta
	ops.ReadWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, error) {
		if releaseRan {
			return beads.Bead{}, errors.New("store unreachable")
		}
		return base(ctx, dir, env, beadID, assignee)
	}
	var stderr bytes.Buffer
	if code := doHookClaim("query", "/rig", poolClaimOpts(), ops, brokenPipeWriter{}, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !releaseRan {
		t.Fatal("the fallback release never ran, so this test proves nothing")
	}
	out := stderr.String()
	for _, want := range []string{"unresolved: could not re-read " + seeded.ID + ": store unreachable; check it", "bd update " + seeded.ID + " --status open --assignee \"" + seat + "\""} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr missing %q; stderr=%s", want, out)
		}
	}
	if strings.Contains(out, "no longer ours") {
		t.Fatalf("an unreadable bead was reported as no longer ours; stderr=%s", out)
	}
}

// TestHookClaimUnwindErroringFallbackIsReRead: a fallback release that ERRORS
// (here: after the restore's write committed late) is re-read too, so a bead
// now open under its pre-claim owner is reported as such, not as a release
// failure on a bead that is in fact back where it belongs.
func TestHookClaimUnwindErroringFallbackIsReRead(t *testing.T) {
	const seat = "gc__role-mc-sess1"
	store := beads.NewMemStore()
	seeded, err := store.Create(beads.Bead{Title: "pre-assigned", Status: "open", Assignee: seat})
	if err != nil {
		t.Fatal(err)
	}
	ops := readyUnwindOps(t, store, seeded.ID)
	baseRestore := ops.Restore
	ops.Restore = func(ctx context.Context, dir string, env []string, beadID, holder, prior string) (bool, error) {
		// The write commits, but the caller stopped waiting for it.
		if _, err := baseRestore(ctx, dir, env, beadID, holder, prior); err != nil {
			t.Fatalf("seeding the late restore: %v", err)
		}
		return false, context.DeadlineExceeded
	}
	ops.Release = func(context.Context, string, []string, string, string) (bool, error) {
		return false, errors.New("release timed out")
	}
	var stderr bytes.Buffer
	if code := doHookClaim("query", "/rig", poolClaimOpts(), ops, brokenPipeWriter{}, &stderr); code != 1 {
		t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if got, _ := store.Get(seeded.ID); got.Status != "open" || got.Assignee != seat {
		t.Fatalf("seed: status=%q assignee=%q, want open under %q", got.Status, got.Assignee, seat)
	}
	if out := stderr.String(); !strings.Contains(out, "open under its pre-claim owner") {
		t.Fatalf("an erroring fallback was not re-read; stderr=%s", out)
	}
}
