package main

import (
	"context"
	"errors"
	"testing"
	"time"

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

// brokenLivenessStore fails every write with a connection-class error, standing
// in for a dead liveness pool. Reads still work, so a test can write through the
// broken store and then read through a healthy one over the same rows.
type brokenLivenessStore struct {
	*liveness.MemStore
	writes int
}

func (s *brokenLivenessStore) SetBatch(context.Context, string, map[string]string) error {
	s.writes++
	return errors.New("invalid connection")
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
	if len(backing.batches) != 1 {
		t.Fatalf("versioned batches = %v, want exactly one", backing.batches)
	}
	got := backing.batches[0]
	if got["state"] != "asleep" || got["alias"] != "katya" {
		t.Fatalf("versioned batch = %v, want the FULL patch (rollback behavior)", got)
	}
	// The fence is what actually makes committed metadata authoritative: the
	// overlay is unconditional and cannot know which mode wrote a row.
	if got[liveness.FallbackAtKey] == "" {
		t.Fatalf("metadata mode wrote no %s fence; a table row would still shadow the committed value", liveness.FallbackAtKey)
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

// TestLivenessTxWritesEverythingVersionedAndFenced pins the Tx contract after
// the review: inside a transaction NOTHING is diverted to the liveness table, so
// a bead's status and its terminal state can never be observed split across two
// stores. The fence keeps any pre-existing row from shadowing what committed.
//
// This replaces an earlier test that asserted the opposite (that a Tx split like
// every other write). That behavior was the review's blocker 2.
func TestLivenessTxWritesEverythingVersionedAndFenced(t *testing.T) {
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
	committed, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	for _, k := range []string{"state", "pending_create_claim", "pending_create_started_at", "close_reason"} {
		if _, present := committed.Metadata[k]; !present {
			t.Errorf("committed metadata is missing %q; a Tx must land every key in the same write as the status", k)
		}
	}
	if committed.Metadata["state"] != "failed_create" {
		t.Errorf("committed state = %q, want failed_create", committed.Metadata["state"])
	}
	if committed.Metadata[liveness.FallbackAtKey] == "" {
		t.Errorf("the transactional write committed no fence")
	}
	// Nothing was routed to the table.
	snap, err := lv.Get(context.Background(), bead.ID)
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	if len(snap.Values) != 0 {
		t.Errorf("liveness rows = %v, want none: a Tx must not write the table", snap.Values)
	}
}

// --- review blocker 1: stale-shadow after a degraded write --------------------

// TestDegradedWriteIsFencedAgainstStaleRows is the mandatory end-to-end case.
// Sequence: healthy liveness write, then the pool dies and a transition falls
// back to versioned metadata, then the pool recovers. The PRE-outage rows are
// still sitting in the table; the post-outage committed values must win anyway.
//
// Before the fence this test failed on every key: the overlay let any surviving
// row win, so wake fencing would have compared against a resurrected
// instance_token and torn down a live session.
func TestDegradedWriteIsFencedAgainstStaleRows(t *testing.T) {
	ctx := context.Background()
	backing := &recordingMetaStore{Store: beads.NewMemStore()}
	lv := liveness.NewMemStore()

	// Pin the liveness clock behind the fence so the pre-outage rows are
	// unambiguously older than the stamp the degraded write mints.
	preOutage := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	lv.Clock = func() time.Time { return preOutage }

	healthy := wrapStoreWithBeadPolicies(backing, &config.City{}, newLivenessBindingForTest(lv, liveness.ModeTable))
	bead := mustCreateSessionBead(t, healthy, nil)

	if err := healthy.SetMetadataBatch(bead.ID, map[string]string{
		"instance_token": "pre-outage",
		"generation":     "4",
		"state":          "active",
	}); err != nil {
		t.Fatalf("healthy SetMetadataBatch: %v", err)
	}
	if snap, _ := lv.Get(ctx, bead.ID); snap.Values["instance_token"] != "pre-outage" {
		t.Fatalf("precondition: liveness rows = %v, want the pre-outage values", snap.Values)
	}

	// The pool dies. The transition falls back to a versioned write.
	broken := &brokenLivenessStore{MemStore: lv}
	degraded := wrapStoreWithBeadPolicies(backing, &config.City{}, newLivenessBindingForTest(broken, liveness.ModeTable))
	if err := degraded.SetMetadataBatch(bead.ID, map[string]string{
		"instance_token": "post-outage",
		"generation":     "5",
	}); err != nil {
		t.Fatalf("degraded SetMetadataBatch: %v", err)
	}
	if broken.writes == 0 {
		t.Fatalf("the degraded store was never asked to write; the test proves nothing")
	}

	committed, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if committed.Metadata["instance_token"] != "post-outage" {
		t.Fatalf("the degraded write did not reach versioned metadata: %v", committed.Metadata)
	}
	if committed.Metadata[liveness.FallbackAtKey] == "" {
		t.Fatalf("the degraded write committed no %s fence; stale rows can shadow it", liveness.FallbackAtKey)
	}

	// The pool recovers. Reads go through a healthy store over the SAME rows.
	got, err := healthy.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after recovery: %v", err)
	}
	if got.Metadata["instance_token"] != "post-outage" {
		t.Errorf("instance_token = %q, want post-outage; the pre-outage row shadowed the committed value", got.Metadata["instance_token"])
	}
	if got.Metadata["generation"] != "5" {
		t.Errorf("generation = %q, want 5", got.Metadata["generation"])
	}
	// A key the degraded write never touched is still fenced: its row predates
	// the fallback, and the committed value is what the fallback preserved.
	if got.Metadata["state"] != "active" {
		t.Errorf("state = %q, want the committed active", got.Metadata["state"])
	}

	// And a NEW successful liveness write takes over again with no reset step.
	lv.Clock = nil // back to the real clock, which is after the fence
	if err := healthy.SetMetadataBatch(bead.ID, map[string]string{"state": "asleep"}); err != nil {
		t.Fatalf("post-recovery SetMetadataBatch: %v", err)
	}
	got, err = healthy.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after post-recovery write: %v", err)
	}
	if got.Metadata["state"] != "asleep" {
		t.Errorf("state = %q, want the post-fence liveness write to win again", got.Metadata["state"])
	}
}

// --- review blocker 2: transactional atomicity -------------------------------

// TestTxFailureLeavesLivenessUnwritten is the mandatory case. A Tx that fails
// must leave the liveness table untouched — the old code wrote the liveness half
// first, outside the transaction, so a rollback left the telemetry applied.
func TestTxFailureLeavesLivenessUnwritten(t *testing.T) {
	ctx := context.Background()
	store, _, lv := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, nil)

	boom := errors.New("transaction failed")
	err := store.Tx("gc: close session", func(tx beads.Tx) error {
		if err := tx.SetMetadataBatch(bead.ID, map[string]string{
			"state":    "closed",
			"slept_at": "2026-09-03T00:00:00Z",
		}); err != nil {
			t.Fatalf("tx.SetMetadataBatch: %v", err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx err = %v, want the callback's error", err)
	}
	snap, err := lv.Get(ctx, bead.ID)
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	if len(snap.Values) != 0 {
		t.Fatalf("liveness rows = %v, want none: a failed Tx must leave the table untouched", snap.Values)
	}
}

// TestTxKeepsLivenessVersionedAndFenced pins the other half of the invariant:
// a session bead that reports closed always carries its terminal state, because
// the state travelled in the same store write as the Close — not to a different
// store that a crash could leave behind.
func TestTxKeepsLivenessVersionedAndFenced(t *testing.T) {
	store, backing, lv := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, nil)

	// A stale row that a split-then-flush design would have left free to shadow
	// the terminal state.
	if err := lv.SetBatch(context.Background(), bead.ID, map[string]string{"state": "active"}); err != nil {
		t.Fatalf("seed liveness: %v", err)
	}

	err := store.Tx("gc: close session", func(tx beads.Tx) error {
		return tx.SetMetadataBatch(bead.ID, map[string]string{
			"state":        "closed",
			"close_reason": "done",
		})
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	committed, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if committed.Metadata["state"] != "closed" {
		t.Errorf("committed state = %q, want closed: the terminal state must ride with the Close", committed.Metadata["state"])
	}
	if committed.Metadata[liveness.FallbackAtKey] == "" {
		t.Errorf("the transactional write committed no fence; the stale row can shadow the terminal state")
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["state"] != "closed" {
		t.Errorf("overlaid state = %q, want closed; the pre-close row won", got.Metadata["state"])
	}
}

// --- review blocker 3: the pool re-dials -------------------------------------

// TestBindingRetiresThePoolOnAConnectionError proves the binding drops a dead
// handle so the next attempt re-RESOLVES the endpoint. Without this a managed
// Dolt rebind (which moves the port) left the pool dialing a dead address for
// the life of the process, silently reverting liveness to committing.
func TestBindingRetiresThePoolOnAConnectionError(t *testing.T) {
	lv := liveness.NewMemStore()
	binding := newLivenessBindingForTest(lv, liveness.ModeTable)
	if binding.Store() == nil {
		t.Fatalf("precondition: binding has no store")
	}

	// A statement-level error must NOT retire the pool: one bad query should not
	// flap the endpoint.
	binding.noteOpError(errors.New("Error 1054: Unknown column 'nope'"))
	if binding.Store() == nil {
		t.Fatalf("a statement error retired the pool")
	}

	binding.noteOpError(errors.New("invalid connection"))
	if got := binding.Store(); got != nil {
		t.Fatalf("Store() = %v after a connection error, want nil so the next attempt re-resolves the endpoint", got)
	}
}

// TestDegradedWriteFencesWhenTheStoreIsGoneEntirely covers the no-store branch:
// after the pool is retired, writes still fence, so the rows left behind cannot
// shadow them when it comes back.
func TestDegradedWriteFencesWhenTheStoreIsGoneEntirely(t *testing.T) {
	backing := &recordingMetaStore{Store: beads.NewMemStore()}
	binding := newLivenessBindingForTest(nil, liveness.ModeTable)
	store := wrapStoreWithBeadPolicies(backing, &config.City{}, binding)
	bead := mustCreateSessionBead(t, store, nil)

	if err := store.SetMetadataBatch(bead.ID, map[string]string{"state": "asleep"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	committed, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if committed.Metadata["state"] != "asleep" {
		t.Errorf("state = %q, want the versioned fallback", committed.Metadata["state"])
	}
	if committed.Metadata[liveness.FallbackAtKey] == "" {
		t.Errorf("no fence stamped with the store gone entirely")
	}
}

// --- review item C: the list-path overlay filter -----------------------------

func TestBeadMayCarryLiveness(t *testing.T) {
	for _, tc := range []struct {
		name string
		bead beads.Bead
		want bool
	}{
		{name: "session type", bead: beads.Bead{Type: sessionBeadType}, want: true},
		{name: "session label", bead: beads.Bead{Type: "task", Labels: []string{sessionBeadLabel}}, want: true},
		{
			name: "work bead that has been heartbeated",
			bead: beads.Bead{Type: "task", Metadata: beads.StringMap{heartbeatMetadataKey: "2026-09-03T00:00:00Z"}},
			want: true,
		},
		{
			name: "work bead that took a fenced write",
			bead: beads.Bead{Type: "task", Metadata: beads.StringMap{liveness.FallbackAtKey: "2026-09-03T00:00:00Z"}},
			want: true,
		},
		{name: "ordinary work bead", bead: beads.Bead{Type: "task", Metadata: beads.StringMap{"alias": "x"}}, want: false},
		{name: "bare bead", bead: beads.Bead{Type: "task"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := beadMayCarryLiveness(tc.bead); got != tc.want {
				t.Errorf("beadMayCarryLiveness = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListOverlaySkipsBeadsThatCannotCarryLiveness(t *testing.T) {
	store, _, lv := newLivenessTestStore(t, liveness.ModeTable)
	session := mustCreateSessionBead(t, store, nil)
	work, err := store.Create(beads.Bead{Title: "ordinary work", Type: "task"})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	ctx := context.Background()
	if err := lv.SetBatch(ctx, session.ID, map[string]string{"state": "asleep"}); err != nil {
		t.Fatalf("liveness SetBatch: %v", err)
	}
	// A row on a bead the filter excludes: it must simply not be looked up.
	if err := lv.SetBatch(ctx, work.ID, map[string]string{"state": "bogus"}); err != nil {
		t.Fatalf("liveness SetBatch: %v", err)
	}

	listed, err := store.List(beads.ListQuery{Status: "open", AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, b := range listed {
		switch b.ID {
		case session.ID:
			if b.Metadata["state"] != "asleep" {
				t.Errorf("session bead state = %q, want the overlaid asleep", b.Metadata["state"])
			}
		case work.ID:
			if b.Metadata["state"] != "" {
				t.Errorf("ordinary work bead was overlaid (state=%q); the filter should have skipped it", b.Metadata["state"])
			}
		}
	}
}
