package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/liveness"
)

// recordingMetaStore counts every metadata write that actually reaches the
// backing bead store. A count of zero is the assertion that matters: a write the
// backing store never sees is a Dolt commit that never happens.
type recordingMetaStore struct {
	beads.Store
	batches []map[string]string
	singles [][2]string
	updates []beads.UpdateOpts
}

func (s *recordingMetaStore) SetMetadataBatch(id string, kvs map[string]string) error {
	cloned := make(map[string]string, len(kvs))
	for k, v := range kvs {
		cloned[k] = v
	}
	s.batches = append(s.batches, cloned)
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *recordingMetaStore) SetMetadata(id, key, value string) error {
	s.singles = append(s.singles, [2]string{key, value})
	return s.Store.SetMetadata(id, key, value)
}

func (s *recordingMetaStore) Update(id string, opts beads.UpdateOpts) error {
	s.updates = append(s.updates, opts)
	return s.Store.Update(id, opts)
}

// newLivenessTestStore builds a policy-wrapped store over a recording MemStore
// with an in-process liveness store attached, and returns all three handles.
func newLivenessTestStore(t *testing.T, mode liveness.Mode) (beads.Store, *recordingMetaStore, *liveness.MemStore) {
	t.Helper()
	backing := &recordingMetaStore{Store: beads.NewMemStore()}
	lv := liveness.NewMemStore()
	store := wrapStoreWithBeadPolicies(backing, &config.City{}, newLivenessBindingForTest(lv, mode))
	return store, backing, lv
}

func mustCreateSessionBead(t *testing.T, store beads.Store, meta map[string]string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    "session bead",
		Type:     "session",
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return b
}

func TestLivenessSplitterDivertsLivenessKeysAndKeepsTheRest(t *testing.T) {
	store, backing, lv := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, map[string]string{"alias": "katya"})
	backing.batches = nil

	err := store.SetMetadataBatch(bead.ID, map[string]string{
		"state":        "asleep",               // liveness
		"slept_at":     "2026-09-03T00:00:00Z", // liveness
		"state_reason": "idle timeout",         // versioned
	})
	if err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	if len(backing.batches) != 1 {
		t.Fatalf("backing saw %d batches, want 1", len(backing.batches))
	}
	got := backing.batches[0]
	if len(got) != 1 || got["state_reason"] != "idle timeout" {
		t.Fatalf("versioned batch = %v, want only the non-liveness key", got)
	}

	snap, err := lv.Get(context.Background(), bead.ID)
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	if snap.Values["state"] != "asleep" || snap.Values["slept_at"] != "2026-09-03T00:00:00Z" {
		t.Fatalf("liveness values = %v, want both liveness keys diverted", snap.Values)
	}
}

// TestLivenessSplitterSkipsTheBeadWriteEntirelyForAnAllLivenessPatch is the
// acceptance test for the whole change: the transition patches the reconciler
// fires several times a minute per session are all-liveness, and they must
// produce NO bead write at all.
func TestLivenessSplitterSkipsTheBeadWriteEntirelyForAnAllLivenessPatch(t *testing.T) {
	store, backing, lv := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, nil)
	backing.batches = nil
	backing.singles = nil

	patch := map[string]string{
		"state":            "active",
		"awake_started_at": "2026-09-03T01:00:00Z",
		"generation":       "12",
		"last_woke_at":     "2026-09-03T01:00:00Z",
	}
	if err := store.SetMetadataBatch(bead.ID, patch); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if len(backing.batches) != 0 {
		t.Fatalf("backing saw %d batches, want 0 — an all-liveness patch must not touch the versioned table", len(backing.batches))
	}

	// The single-key form takes the same path.
	if err := store.SetMetadata(bead.ID, "synced_at", "2026-09-03T01:00:01Z"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if len(backing.singles) != 0 {
		t.Fatalf("backing saw %d single writes, want 0", len(backing.singles))
	}

	snap, err := lv.Get(context.Background(), bead.ID)
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	if len(snap.Values) != len(patch)+1 {
		t.Fatalf("liveness values = %v, want all %d keys", snap.Values, len(patch)+1)
	}
}

func TestLivenessSplitterEmptyStringClears(t *testing.T) {
	store, _, _ := newLivenessTestStore(t, liveness.ModeTable)
	// The committed metadata carries a STALE value; the clear must win over it,
	// which is why a clear is written as a tombstone row and never a delete.
	bead := mustCreateSessionBead(t, store, map[string]string{"pending_create_claim": "true"})

	if err := store.SetMetadataBatch(bead.ID, map[string]string{"pending_create_claim": ""}); err != nil {
		t.Fatalf("SetMetadataBatch clear: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v := got.Metadata["pending_create_claim"]; v != "" {
		t.Fatalf("pending_create_claim = %q, want the clear to win over the stale committed %q", v, "true")
	}
}

func TestLivenessOverlayOnGetAndList(t *testing.T) {
	store, _, lv := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, map[string]string{
		"state": "active", // committed, and about to be shadowed
		"alias": "katya",  // committed, no liveness row
	})
	if err := lv.SetBatch(context.Background(), bead.ID, map[string]string{"state": "asleep"}); err != nil {
		t.Fatalf("liveness SetBatch: %v", err)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["state"] != "asleep" {
		t.Errorf("Get state = %q, want the liveness value", got.Metadata["state"])
	}
	if got.Metadata["alias"] != "katya" {
		t.Errorf("Get alias = %q, want the committed value carried through", got.Metadata["alias"])
	}
	if got.Metadata[liveness.WrittenAtKey] == "" {
		t.Errorf("Get did not stamp %s", liveness.WrittenAtKey)
	}

	listed, err := store.List(beads.ListQuery{Status: "open", AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, b := range listed {
		if b.ID != bead.ID {
			continue
		}
		found = true
		if b.Metadata["state"] != "asleep" {
			t.Errorf("List state = %q, want the liveness value", b.Metadata["state"])
		}
		if b.Metadata["alias"] != "katya" {
			t.Errorf("List alias = %q, want the committed value", b.Metadata["alias"])
		}
	}
	if !found {
		t.Fatalf("List did not return the session bead")
	}
}

func TestLivenessOverlayLeavesBeadsWithNoRowsUntouched(t *testing.T) {
	store, _, _ := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, map[string]string{"state": "active"})
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["state"] != "active" {
		t.Errorf("state = %q, want the committed fallback for a bead with no liveness rows", got.Metadata["state"])
	}
	if _, stamped := got.Metadata[liveness.WrittenAtKey]; stamped {
		t.Errorf("stamped %s on a bead with no liveness rows", liveness.WrittenAtKey)
	}
}

func TestLivenessMetadataModeWritesVersionedAndMirrors(t *testing.T) {
	store, backing, lv := newLivenessTestStore(t, liveness.ModeMetadata)
	bead := mustCreateSessionBead(t, store, nil)
	backing.batches = nil

	if err := store.SetMetadataBatch(bead.ID, map[string]string{"state": "asleep", "alias": "katya"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if len(backing.batches) != 1 || len(backing.batches[0]) != 2 {
		t.Fatalf("versioned batches = %v, want the FULL patch (rollback behavior)", backing.batches)
	}
	// The mirror is what keeps the always-on read overlay from shadowing fresh
	// committed metadata with frozen rows after a rollback.
	snap, err := lv.Get(context.Background(), bead.ID)
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	if snap.Values["state"] != "asleep" {
		t.Fatalf("liveness mirror = %v, want state mirrored in metadata mode", snap.Values)
	}
}

func TestLivenessDisabledBindingIsAPassThrough(t *testing.T) {
	backing := &recordingMetaStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, &config.City{}) // no binding at all
	bead := mustCreateSessionBead(t, store, nil)
	backing.batches = nil

	if err := store.SetMetadataBatch(bead.ID, map[string]string{"state": "asleep"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if len(backing.batches) != 1 || backing.batches[0]["state"] != "asleep" {
		t.Fatalf("versioned batches = %v, want the legacy write with liveness disabled", backing.batches)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want the committed value", got.Metadata["state"])
	}
}

func TestLivenessUpdateSplitsMetadataButKeepsOtherFields(t *testing.T) {
	store, backing, lv := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, nil)
	backing.updates = nil

	// Metadata-only, all liveness: the Update must be skipped entirely.
	if err := store.Update(bead.ID, beads.UpdateOpts{Metadata: map[string]string{"state": "asleep"}}); err != nil {
		t.Fatalf("Update metadata-only: %v", err)
	}
	if len(backing.updates) != 0 {
		t.Fatalf("backing saw %d updates, want 0 for a metadata-only all-liveness update", len(backing.updates))
	}

	// A status change is genuine lifecycle history and must still reach the store.
	status := "in_progress"
	if err := store.Update(bead.ID, beads.UpdateOpts{
		Status:   &status,
		Metadata: map[string]string{"generation": "3"},
	}); err != nil {
		t.Fatalf("Update with status: %v", err)
	}
	if len(backing.updates) != 1 {
		t.Fatalf("backing saw %d updates, want 1", len(backing.updates))
	}
	if len(backing.updates[0].Metadata) != 0 {
		t.Errorf("update metadata = %v, want the liveness key stripped", backing.updates[0].Metadata)
	}
	if backing.updates[0].Status == nil || *backing.updates[0].Status != "in_progress" {
		t.Errorf("update status was dropped")
	}
	snap, _ := lv.Get(context.Background(), bead.ID)
	if snap.Values["generation"] != "3" {
		t.Errorf("liveness generation = %q, want 3", snap.Values["generation"])
	}
}

func TestLivenessTxSplitsMetadataWrites(t *testing.T) {
	store, backing, lv := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, nil)
	backing.batches = nil

	// This is the shape closeFailedCreateBeadInTx writes: terminal close metadata
	// plus liveness clears, inside a Tx.
	err := store.Tx("close failed create", func(tx beads.Tx) error {
		return tx.SetMetadataBatch(bead.ID, map[string]string{
			"state":                     "failed_create",
			"pending_create_claim":      "",
			"pending_create_started_at": "",
			"close_reason":              "create rollback",
		})
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	// The Tx handle comes from the backing store itself, so its writes bypass the
	// recorder; assert on the committed row instead, read straight from the
	// backing store so no overlay can mask what actually landed.
	committed, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if committed.Metadata["close_reason"] != "create rollback" {
		t.Errorf("committed close_reason = %q, want it versioned", committed.Metadata["close_reason"])
	}
	for _, k := range []string{"state", "pending_create_claim", "pending_create_started_at"} {
		if _, present := committed.Metadata[k]; present {
			t.Errorf("committed metadata still carries liveness key %q; the Tx split did not divert it", k)
		}
	}
	snap, _ := lv.Get(context.Background(), bead.ID)
	if snap.Values["state"] != "failed_create" {
		t.Errorf("liveness state = %q, want failed_create", snap.Values["state"])
	}
	if v, ok := snap.Values["pending_create_claim"]; !ok || v != "" {
		t.Errorf("pending_create_claim = (%q,%v), want a present empty tombstone", v, ok)
	}
}
