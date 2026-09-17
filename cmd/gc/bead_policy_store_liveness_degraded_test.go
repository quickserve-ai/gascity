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
