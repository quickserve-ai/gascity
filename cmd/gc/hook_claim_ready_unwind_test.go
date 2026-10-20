package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
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
		<-ctx.Done()
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

// unwindRaceGraph is unwindRaceStore for the graph binding: its first Get returns the
// snapshot, then (after an optional stall) applies mutate before the caller can
// write. done closes when that Get returns, so a test can wait out a Get its
// caller abandoned before the store underneath is closed.
type unwindRaceGraph struct {
	storebinding.GraphStore
	mutate func(id string) error
	stall  time.Duration
	raced  bool
	done   chan struct{}
}

func (g *unwindRaceGraph) Get(id string) (beads.Bead, error) {
	if g.raced {
		return g.GraphStore.Get(id)
	}
	g.raced = true
	defer close(g.done)
	snap, err := g.GraphStore.Get(id)
	if g.stall > 0 {
		time.Sleep(g.stall)
	}
	if err == nil && g.mutate != nil {
		if merr := g.mutate(id); merr != nil {
			return beads.Bead{}, merr
		}
	}
	return snap, err
}

// graphRestoreRig is a class store, a graph binding over it that races, and
// the route's ops with id known resident in the binding.
func graphRestoreRig(t *testing.T, id, status, assignee string, race *unwindRaceGraph) (beads.Store, hookClaimOps) {
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
		if race.raced {
			<-race.done
		}
	})
	route := newClaimClassRouteOver(class, race)
	route.resident[id] = true
	ops := classRoutedHookClaimOps(hookClaimOps{EmitClaimReleased: func(hookClaimReleaseRecord) {}}, route)
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
			if !race.raced {
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
// and the fallback release still gets the claim back.
func TestHookClaimUnwindGraphRestoreHonorsDeadline(t *testing.T) {
	old := hookClaimMutationTimeout
	hookClaimMutationTimeout = 200 * time.Millisecond
	t.Cleanup(func() { hookClaimMutationTimeout = old })

	const seat = "gc__role-mc-sess1"
	race := &unwindRaceGraph{stall: 2 * time.Second}
	class, ops := graphRestoreRig(t, "gcg-stall", "in_progress", seat, race)

	var stderr bytes.Buffer
	start := time.Now()
	code := unwindUndeliveredHookClaim(hookClaimReleaseReasonUndelivered, "test unwind", beads.Bead{ID: "gcg-stall", Assignee: seat}, seat,
		hookClaimOptions{Assignee: seat}, ops, "/rig", &stderr)
	elapsed := time.Since(start)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if elapsed > time.Second {
		t.Fatalf("unwind took %s behind a stalled graph read, want it bounded by the %s restore deadline", elapsed, hookClaimMutationTimeout)
	}
	if got, _ := class.Get("gcg-stall"); got.Status != "open" || got.Assignee != "" {
		t.Fatalf("after a stalled restore: status=%q assignee=%q, want the fallback release (open, unassigned); stderr=%s", got.Status, got.Assignee, stderr.String())
	}
}
