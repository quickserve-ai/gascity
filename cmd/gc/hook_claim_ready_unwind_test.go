package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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

func unwindSetAssignee(store *beads.MemStore, assignee string) func(string) error {
	return func(id string) error { return store.Update(id, beads.UpdateOpts{Assignee: &assignee}) }
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
