package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/liveness"
)

// TestAtomicCloseThroughThePolicyStoreFencesAndSweepsLiveness pins the fold of
// upstream #6784 (CloseWithTerminalPatch, the fenced single-write close) onto
// the carry's liveness overlay (ga-lys454). The controller resolves the atomic
// closer through beads.SessionStore over the policy store; that resolution must
// stop at the policy layer, which fences the terminal patch's liveness keys and
// sweeps their table rows after the commit, exactly as beadPolicyStore.Tx does.
// Resolved past it, the close committed state=closed while the state=awake row
// a concurrent wake left in the liveness table kept shadowing it on every read.
func TestAtomicCloseThroughThePolicyStoreFencesAndSweepsLiveness(t *testing.T) {
	ctx := context.Background()
	leaf, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = leaf.(*beads.SQLiteStore).CloseStore() })
	lv := liveness.NewMemStore()
	store := wrapStoreWithBeadPolicies(leaf, &config.City{}, newLivenessBindingForTest(lv, liveness.ModeTable))
	bead := mustCreateSessionBead(t, store, map[string]string{"session_name": "agent-a"})

	// A wake in another process left its telemetry in the liveness table.
	if err := lv.SetBatch(ctx, bead.ID, map[string]string{"state": "awake"}); err != nil {
		t.Fatalf("liveness SetBatch: %v", err)
	}

	closer, ok := beads.AtomicConditionalCloserFor(beads.SessionStore{Store: store})
	if !ok {
		t.Fatal("no atomic closer resolved over the policy store; the SQLite backing provides one")
	}
	current, err := leaf.Get(bead.ID)
	if err != nil {
		t.Fatalf("leaf Get: %v", err)
	}
	closed, err := closer.CloseWithMetadataIfMatch(bead.ID, current.Revision, map[string]string{
		"state":        "closed",
		"close_reason": "done",
	})
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("closed.Status = %q, want closed", closed.Status)
	}

	snap, err := lv.Get(ctx, bead.ID)
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	if _, present := snap.Values["state"]; present {
		t.Fatalf("the stale liveness row survived the atomic close: %v", snap.Values)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" || got.Metadata["state"] != "closed" {
		t.Fatalf("overlaid read = status %q state %q, want closed/closed; the table row shadowed the committed terminal state", got.Status, got.Metadata["state"])
	}
}
