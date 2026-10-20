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

// TestHookClaimUnwindRestoreFailureFallsBackToRelease: a restore that errors
// outright still gets the claim back (released, unassigned) and surfaces the
// lost owner with its recovery, rather than leaving the claim parked.
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
	for _, want := range []string{"binding refused the restore", "bd update " + seeded.ID + " --assignee \"" + seat + "\""} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q; stderr=%s", want, stderr.String())
		}
	}
}

// TestHookClaimUnwindRestoresLegacySpellingAfterRestamp: a ready claim of a
// bead held under a legacy spelling is re-stamped to the claim identity, so the
// unwind's holder and the pre-claim owner are DIFFERENT strings. The restore
// must fence on the re-stamped holder and write back the legacy spelling.
func TestHookClaimUnwindRestoresLegacySpellingAfterRestamp(t *testing.T) {
	const (
		current = "gcg-session-1"
		legacy  = "claude-gcg-session-1"
	)
	store := beads.NewMemStore()
	seeded, err := store.Create(beads.Bead{Title: "legacy-assigned", Status: "open", Assignee: legacy})
	if err != nil {
		t.Fatal(err)
	}
	ops := readyUnwindOps(t, store, seeded.ID)
	var restoredHolder string
	baseRestore := ops.Restore
	ops.Restore = func(ctx context.Context, dir string, env []string, beadID, holder, prior string) (bool, error) {
		restoredHolder = holder
		return baseRestore(ctx, dir, env, beadID, holder, prior)
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
	if restoredHolder != current {
		t.Fatalf("restore fenced on holder %q, want the re-stamped %q (the restamp did not run, so this test proves nothing)", restoredHolder, current)
	}
	got, _ := store.Get(seeded.ID)
	if got.Status != "open" || got.Assignee != legacy {
		t.Fatalf("after unwind: status=%q assignee=%q, want open under the pre-claim spelling %q; stderr=%s", got.Status, got.Assignee, legacy, stderr.String())
	}
}

// TestHookClaimRestoreStatusOnlyChangeIsLeftAlone: the fence covers status as
// well as owner. A bead the holder still owns but that is no longer in_progress
// is not the claim being given back, and is not written.
func TestHookClaimRestoreStatusOnlyChangeIsLeftAlone(t *testing.T) {
	const seat = "gc__role-mc-sess1"
	store := beads.NewMemStore()
	seeded, err := store.Create(beads.Bead{Title: "blocked"})
	if err != nil {
		t.Fatal(err)
	}
	blocked, holder := "blocked", seat
	if err := store.Update(seeded.ID, beads.UpdateOpts{Status: &blocked, Assignee: &holder}); err != nil {
		t.Fatal(err)
	}
	restored, err := hookClaimRestoreOnStore(store, seeded.ID, seat, "someone-prior")
	if err != nil || restored {
		t.Fatalf("restore of a status-changed bead = (%v, %v), want (false, nil)", restored, err)
	}
	got, _ := store.Get(seeded.ID)
	if got.Status != "blocked" || got.Assignee != seat {
		t.Fatalf("status-changed bead was written: status=%q assignee=%q", got.Status, got.Assignee)
	}
}

// racingOwnerStore hands the restore's fence read a snapshot, then moves the
// bead to another owner before the write: the change lands BETWEEN the
// restore's read and its write.
type racingOwnerStore struct {
	*beads.MemStore
	raced bool
	to    string
}

func (s *racingOwnerStore) Get(id string) (beads.Bead, error) {
	snap, err := s.MemStore.Get(id)
	if err == nil && !s.raced {
		s.raced = true
		if uerr := s.MemStore.Update(id, beads.UpdateOpts{Assignee: &s.to}); uerr != nil {
			return beads.Bead{}, uerr
		}
	}
	return snap, err
}

// TestHookClaimRestoreLosesToAnOwnerChangeMidWrite pins that the restore's write
// is fenced on the revision it read: an owner change between the read and the
// write is not overwritten, and is reported as a lost fence (so the unwind
// falls back to the holder-fenced release, which also leaves it alone).
func TestHookClaimRestoreLosesToAnOwnerChangeMidWrite(t *testing.T) {
	const seat = "gc__role-mc-sess1"
	mem := beads.NewMemStore()
	seeded, err := mem.Create(beads.Bead{Title: "racing"})
	if err != nil {
		t.Fatal(err)
	}
	inProgress, holder := "in_progress", seat
	if err := mem.Update(seeded.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &holder}); err != nil {
		t.Fatal(err)
	}
	store := &racingOwnerStore{MemStore: mem, to: "thief"}
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

// TestHookClaimRestoreOnGraphBinding drives the graph-binding restore over a
// real class store: it restores a claim it holds, and leaves alone one that
// changed hands or status.
func TestHookClaimRestoreOnGraphBinding(t *testing.T) {
	const seat = "gc__role-mc-sess1"
	class := newClaimRouteClassStore(t)
	graph, err := storebinding.NewBeadsGraphStore(class)
	if err != nil {
		t.Fatal(err)
	}
	mint := func(id, status, assignee string) {
		t.Helper()
		mintClaimRouteBead(t, class, id, nil)
		if err := class.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			t.Fatal(err)
		}
	}
	mint("gcg-held", "in_progress", seat)
	mint("gcg-moved", "in_progress", "someone-else")
	mint("gcg-blocked", "blocked", seat)

	if ok, err := restoreGraphHookClaim(graph, "gcg-held", seat, seat); !ok || err != nil {
		t.Fatalf("restore of a held graph claim = (%v, %v), want (true, nil)", ok, err)
	}
	if got, _ := class.Get("gcg-held"); got.Status != "open" || got.Assignee != seat {
		t.Fatalf("gcg-held after restore: status=%q assignee=%q, want open under %q", got.Status, got.Assignee, seat)
	}
	for id, want := range map[string][2]string{
		"gcg-moved":   {"in_progress", "someone-else"},
		"gcg-blocked": {"blocked", seat},
	} {
		if ok, err := restoreGraphHookClaim(graph, id, seat, seat); ok || err != nil {
			t.Fatalf("restore of %s = (%v, %v), want (false, nil)", id, ok, err)
		}
		if got, _ := class.Get(id); got.Status != want[0] || got.Assignee != want[1] {
			t.Fatalf("%s was written: status=%q assignee=%q", id, got.Status, got.Assignee)
		}
	}
	if ok, err := restoreGraphHookClaim(graph, "gcg-absent", seat, seat); ok || err != nil {
		t.Fatalf("restore of an absent bead = (%v, %v), want (false, nil)", ok, err)
	}
}
