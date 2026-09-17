package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/liveness"
)

// slowLivenessStore fails every read with the op deadline, the shape a merely
// slow liveness query takes.
type slowLivenessStore struct {
	*liveness.MemStore
}

func (s *slowLivenessStore) Get(context.Context, string) (liveness.Snapshot, error) {
	return liveness.Snapshot{}, fmt.Errorf("liveness: read: %w", context.DeadlineExceeded)
}

func (s *slowLivenessStore) GetMany(context.Context, []string) (map[string]liveness.Snapshot, error) {
	return nil, fmt.Errorf("liveness: read: %w", context.DeadlineExceeded)
}

// TestLivenessOverlayMarksDegradedReads pins the read-side marker the
// lifecycle guards key on (fork PR #59 review, item 1): a read the overlay
// could not apply is still served fail-open, but carries
// liveness.ReadDegradedKey — for a failed point read, a failed list read, a
// slow read (the op deadline, which does not retire the pool), and every read
// through a retired pool — while a healthy read and a scope with no liveness at
// all carry nothing. The marker never reaches storage.
func TestLivenessOverlayMarksDegradedReads(t *testing.T) {
	backing := beads.NewMemStore()
	lv := liveness.NewMemStore()
	wrap := func(binding *livenessBinding) beads.Store {
		return wrapStoreWithBeadPolicies(backing, &config.City{}, binding)
	}
	healthy := wrap(newLivenessBindingForTest(lv, liveness.ModeTable))
	session := mustCreateSessionBead(t, healthy, map[string]string{"alias": "katya"})
	if err := healthy.SetMetadataBatch(session.ID, map[string]string{"sleep_intent": "user-hold"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	work, err := healthy.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	marked := func(b beads.Bead) bool { return b.Metadata[liveness.ReadDegradedKey] == "true" }

	if got := mustGetBead(t, healthy, session.ID); marked(got) || got.Metadata["sleep_intent"] != "user-hold" {
		t.Fatalf("healthy read = %v, want the overlaid intent and no marker", got.Metadata)
	}
	if got := mustGetBead(t, wrap(nil), session.ID); marked(got) {
		t.Errorf("a scope with no liveness binding marked its read degraded: %v", got.Metadata)
	}

	// A slow read: not a transport failure, so the pool stays, but the read
	// is still the committed fallback and must say so.
	if liveness.IsConnectionError(fmt.Errorf("liveness: read: %w", context.DeadlineExceeded)) {
		t.Fatalf("precondition changed: the op deadline now classifies as a connection error")
	}
	slowBinding := newLivenessBindingForTest(&slowLivenessStore{MemStore: lv}, liveness.ModeTable)
	slow := wrap(slowBinding)
	if got := mustGetBead(t, slow, session.ID); !marked(got) || got.Metadata["sleep_intent"] != "" {
		t.Errorf("slow point read = %v, want committed metadata marked degraded", got.Metadata)
	}
	if slowBinding.Store() == nil {
		t.Errorf("a deadline-exceeded read retired the pool; only transport errors should")
	}
	list, err := slow.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("slow List: %v", err)
	}
	for _, b := range list {
		switch b.ID {
		case session.ID:
			if !marked(b) {
				t.Errorf("slow list read left the session bead unmarked: %v", b.Metadata)
			}
		case work.ID:
			if marked(b) {
				t.Errorf("slow list read marked a bead that carries no liveness: %v", b.Metadata)
			}
		}
	}

	// A transport failure retires the pool; every later read takes the
	// no-store path and is marked too.
	downBinding := newLivenessBindingForTest(&unreachableLivenessStore{MemStore: lv}, liveness.ModeTable)
	down := wrap(downBinding)
	if got := mustGetBead(t, down, session.ID); !marked(got) {
		t.Errorf("failed point read unmarked: %v", got.Metadata)
	}
	if downBinding.Store() != nil {
		t.Fatalf("precondition: a connection error did not retire the pool")
	}
	if got := mustGetBead(t, down, session.ID); !marked(got) {
		t.Errorf("read through a retired pool unmarked: %v", got.Metadata)
	}

	// The marker is read-side only: a patch carrying it never lands anywhere.
	if err := healthy.SetMetadataBatch(session.ID, map[string]string{
		liveness.ReadDegradedKey: "true",
		"alias":                  "katya-2",
	}); err != nil {
		t.Fatalf("SetMetadataBatch with the marker: %v", err)
	}
	committed, err := backing.Get(session.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if _, ok := committed.Metadata[liveness.ReadDegradedKey]; ok {
		t.Errorf("the degraded marker was committed: %v", committed.Metadata)
	}
	if snap, err := lv.Get(context.Background(), session.ID); err != nil {
		t.Fatalf("liveness Get: %v", err)
	} else if _, ok := snap.Values[liveness.ReadDegradedKey]; ok {
		t.Errorf("the degraded marker reached the liveness table: %v", snap.Values)
	}
}

// TestLivenessReadMarkersNeverPersistOnCreate pins counsel-astra's second
// blocking finding (fork PR #59, round 2). The overlay's read-side keys are
// synthetic: the degraded marker says "this read did not overlay", and the
// written-at key is the overlay's freshness clock. Split drops both from every
// inbound patch, but the create paths never went through Split. A bead created
// from metadata copied off a degraded read therefore COMMITTED the marker, and
// a healthy overlay then served it forever: every lifecycle decision on that
// bead would defer for good, and session.EffectiveUpdatedAt would believe a
// forged clock. Creates strip them on all three paths.
func TestLivenessReadMarkersNeverPersistOnCreate(t *testing.T) {
	backing := beads.NewMemStore()
	lv := liveness.NewMemStore()
	store := wrapStoreWithBeadPolicies(backing, &config.City{}, newLivenessBindingForTest(lv, liveness.ModeTable))
	carried := func() map[string]string {
		return map[string]string{
			liveness.ReadDegradedKey: "true",
			liveness.WrittenAtKey:    "2026-09-17T00:00:00Z",
			"alias":                  "katya",
			"sleep_intent":           "user-hold",
		}
	}
	assertClean := func(t *testing.T, path, id string) {
		t.Helper()
		committed, err := backing.Get(id)
		if err != nil {
			t.Fatalf("%s: backing Get: %v", path, err)
		}
		for _, key := range []string{liveness.ReadDegradedKey, liveness.WrittenAtKey} {
			if _, ok := committed.Metadata[key]; ok {
				t.Errorf("%s committed %q: %v", path, key, committed.Metadata)
			}
		}
		if committed.Metadata["alias"] != "katya" {
			t.Errorf("%s dropped ordinary metadata: %v", path, committed.Metadata)
		}
		read := mustGetBead(t, store, id)
		if read.Metadata[liveness.ReadDegradedKey] == "true" {
			t.Errorf("%s: a healthy read still reports the bead degraded: %v", path, read.Metadata)
		}
		info, err := sessionFrontDoor(store).Get(id)
		if err != nil {
			t.Fatalf("%s: front-door Get: %v", path, err)
		}
		if info.LivenessReadDegraded {
			t.Errorf("%s: the session projection reports a committed degraded marker", path)
		}
	}

	meta := carried()
	created, err := store.Create(beads.Bead{Title: "session bead", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	assertClean(t, "Create", created.ID)
	if meta[liveness.ReadDegradedKey] != "true" {
		t.Errorf("Create mutated the caller's metadata map: %v", meta)
	}

	var txID string
	if err := store.Tx("create in tx", func(tx beads.Tx) error {
		b, err := tx.Create(beads.Bead{Title: "session bead", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: carried()})
		txID = b.ID
		return err
	}); err != nil {
		t.Fatalf("Tx create: %v", err)
	}
	assertClean(t, "Tx.Create", txID)

	// The graph-apply path creates whole node sets; both its metadata maps
	// reach the store.
	capture := &captureGraphStore{Store: beads.NewMemStore()}
	graph := wrapStoreWithBeadPolicies(capture, &config.City{}, newLivenessBindingForTest(lv, liveness.ModeTable))
	applier, ok := beads.GraphApplyFor(graph)
	if !ok {
		t.Fatalf("the policy wrapper exposed no graph-apply capability")
	}
	plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{{
		Key:          "n1",
		Title:        "step",
		Metadata:     carried(),
		MetadataRefs: map[string]string{liveness.ReadDegradedKey: "n1", "gc.parent_bead_id": "n1"},
	}}}
	if _, err := applier.ApplyGraphPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if capture.plan == nil || len(capture.plan.Nodes) != 1 {
		t.Fatalf("the graph applier saw %+v, want one node", capture.plan)
	}
	node := capture.plan.Nodes[0]
	for _, key := range []string{liveness.ReadDegradedKey, liveness.WrittenAtKey} {
		if _, ok := node.Metadata[key]; ok {
			t.Errorf("ApplyGraphPlan forwarded %q in node metadata: %v", key, node.Metadata)
		}
		if _, ok := node.MetadataRefs[key]; ok {
			t.Errorf("ApplyGraphPlan forwarded %q in node metadata refs: %v", key, node.MetadataRefs)
		}
	}
	if node.Metadata["alias"] != "katya" || node.MetadataRefs["gc.parent_bead_id"] != "n1" {
		t.Errorf("ApplyGraphPlan dropped ordinary node metadata: %v / %v", node.Metadata, node.MetadataRefs)
	}
	if plan.Nodes[0].Metadata[liveness.ReadDegradedKey] != "true" {
		t.Errorf("ApplyGraphPlan mutated the caller's plan: %v", plan.Nodes[0].Metadata)
	}
}
