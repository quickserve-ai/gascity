//go:build integration

package beads

import (
	"context"
	"testing"
)

// TestNativeDoltStoreSetMetadataBatchKeepsAConcurrentUpdateAgainstRealDolt is
// the real-store proof of the compare-and-swap: a second writer changes other
// keys of the same bead between the merge's read and its checked write, on
// the pinned backend with its own row version and audit events. The refused
// swap is retried from a fresh read, every key of both writers survives, and
// exactly one event is recorded for the stamp — the refused attempt leaves
// none. The in-memory test owns the interleaving detail; this pins that the
// backend's version check is the one the store compares against.
func TestNativeDoltStoreSetMetadataBatchKeepsAConcurrentUpdateAgainstRealDolt(t *testing.T) {
	ctx := context.Background()
	store := openRealNativeDoltStoreForCAS(t, "merge-race")

	created, err := store.Create(Bead{
		Title:    "fenced entry step",
		Metadata: map[string]string{"gc.run_target": "pool", "gc.instantiating": "true", "gc.deferred_routed_to": "rig/pool"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := created.ID

	countEvents := func() int {
		t.Helper()
		storage, release, err := store.acquireStorage()
		if err != nil {
			t.Fatalf("acquire storage: %v", err)
		}
		defer release()
		events, err := storage.GetEvents(ctx, id, 100)
		if err != nil {
			t.Fatalf("GetEvents: %v", err)
		}
		return len(events)
	}

	reads := 0
	afterCompetingWrite := -1
	store.afterMetadataMergeRead = func(readID string) {
		reads++
		if reads != 1 || readID != id {
			return
		}
		// The competing writer: the fence activation, on other keys of the same
		// row, committing after the merge's read and before its checked write.
		if err := store.Update(id, UpdateOpts{Metadata: map[string]string{
			"gc.instantiating":      "",
			"gc.deferred_routed_to": "",
			"gc.routed_to":          "rig/pool",
		}}); err != nil {
			t.Errorf("competing Update: %v", err)
		}
		afterCompetingWrite = countEvents()
	}

	if err := store.SetMetadataBatch(id, map[string]string{"gc.heartbeat": "now"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if reads != 2 {
		t.Fatalf("merge reads = %d, want 2 (the swap refused after the competing write, then a fresh read)", reads)
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for key, want := range map[string]string{
		"gc.heartbeat":          "now",
		"gc.instantiating":      "",
		"gc.deferred_routed_to": "",
		"gc.routed_to":          "rig/pool",
		"gc.run_target":         "pool",
	} {
		if got.Metadata[key] != want {
			t.Errorf("%s = %q, want %q (the competing write was lost or the merge was)", key, got.Metadata[key], want)
		}
	}

	if afterCompetingWrite < 0 {
		t.Fatal("the competing write never ran")
	}
	if delta := countEvents() - afterCompetingWrite; delta != 1 {
		t.Fatalf("events recorded by the merge = %d, want 1: the committed stamp records one event and the refused swap none", delta)
	}
}
