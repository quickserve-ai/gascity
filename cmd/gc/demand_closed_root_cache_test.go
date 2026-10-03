package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Pins the demand closed-root gate's cost bounds (ga-b2oxyf): each pass reads
// at most demandRootResolveBudget roots, and every verdict is remembered across
// passes per store — closed and not-found for demandClosedRootTTL, open for
// demandOpenRootTTL.

// demandCountingStore counts the gate's root reads and teardown queries.
type demandCountingStore struct {
	*beads.MemStore
	gets, lists int
	listErr     error
}

func (s *demandCountingStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.MemStore.Get(id)
}

func (s *demandCountingStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.lists++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.MemStore.ListByMetadata(filters, limit, opts...)
}

// A closed root and its tail are read once, then remembered until the TTL.
func TestDemandRemembersAClosedRootAcrossPassesUntilTheTTL(t *testing.T) {
	resetDemandRootMemos(t)
	now := time.Now()
	demandClosedRootClock = func() time.Time { return now }
	all, _ := demandClosedRootFixture("closed", true)
	store := &demandCountingStore{MemStore: beads.NewMemStoreFrom(0, all, nil)}

	for pass := 1; pass <= 2; pass++ {
		if count, _ := countDemandClosedRoot(t, store); count != 1 || store.gets != 1 || store.lists != 1 {
			t.Fatalf("pass %d: demand = %d, root reads = %d, teardown queries = %d; want 1, and 1 and 1 in total", pass, count, store.gets, store.lists)
		}
	}

	now = now.Add(demandClosedRootTTL)
	if count, _ := countDemandClosedRoot(t, store); count != 1 || store.gets != 2 || store.lists != 2 {
		t.Fatalf("after the TTL: demand = %d, root reads = %d, teardown queries = %d; want 1, 2, 2", count, store.gets, store.lists)
	}
}

// Each pass resolves at most the budget of new roots; the rest are counted.
func TestDemandResolvesABoundedNumberOfNewRootsPerPass(t *testing.T) {
	resetDemandRootMemos(t)
	var rows []beads.Bead
	for i := 0; i < 20; i++ {
		rootID := fmt.Sprintf("qc-dead%02d", i)
		rows = append(rows, closedRootWorkflowRoot(rootID, "closed"), beads.Bead{
			ID: rootID + ".1", Title: "step", Status: "open", Type: "task",
			Metadata: map[string]string{"gc.routed_to": demandClosedRootTemplate, "gc.root_bead_id": rootID},
		})
	}
	store := &demandCountingStore{MemStore: beads.NewMemStoreFrom(0, rows, nil)}

	for pass, want := range []int{12, 4, 0} {
		if count, _ := countDemandClosedRoot(t, store); count != want {
			t.Fatalf("pass %d: demand = %d, want %d (budget %d new roots per pass)", pass+1, count, want, demandRootResolveBudget)
		}
	}
	if store.gets != 20 {
		t.Fatalf("root reads = %d, want 20 — each root read once", store.gets)
	}
}

// An unreadable teardown tail leaves the root unresolved, counted, and unremembered.
func TestDemandCountsStepsOfAClosedRootWhoseTailIsUnreadable(t *testing.T) {
	resetDemandRootMemos(t)
	all, _ := demandClosedRootFixture("closed", true)
	store := &demandCountingStore{MemStore: beads.NewMemStoreFrom(0, all, nil), listErr: errors.New("partial list")}
	for pass := 1; pass <= 2; pass++ {
		if count, _ := countDemandClosedRoot(t, store); count != 27 || store.gets != pass {
			t.Fatalf("pass %d: demand = %d, root reads = %d; want 27 and %d", pass, count, store.gets, pass)
		}
	}
}

// An OPEN root is charged to the budget too, and remembered for the open TTL:
// a root that closes inside it stays counted (over-counted, the safe
// direction) until the TTL lapses.
func TestDemandRemembersAnOpenRootForTheOpenTTL(t *testing.T) {
	resetDemandRootMemos(t)
	now := time.Now()
	demandClosedRootClock = func() time.Time { return now }
	all, _ := demandClosedRootFixture("open", true)
	store := &demandCountingStore{MemStore: beads.NewMemStoreFrom(0, all, nil)}

	if count, _ := countDemandClosedRoot(t, store); count != 27 || store.gets != 1 {
		t.Fatalf("pass 1: demand = %d, root reads = %d; want 27 and 1", count, store.gets)
	}
	if err := store.Close(demandClosedRootID); err != nil {
		t.Fatalf("closing the root: %v", err)
	}
	if count, _ := countDemandClosedRoot(t, store); count != 27 || store.gets != 1 {
		t.Fatalf("pass 2: demand = %d, root reads = %d; want 27 (remembered open) and 1", count, store.gets)
	}
	now = now.Add(demandOpenRootTTL)
	if count, _ := countDemandClosedRoot(t, store); count != 1 || store.gets != 2 {
		t.Fatalf("after the open TTL: demand = %d, root reads = %d; want 1 and 2", count, store.gets)
	}
}

// Open roots get no refund: 20 open roots cost 8 reads a pass, then none.
func TestDemandChargesOpenRootReadsToTheBudget(t *testing.T) {
	resetDemandRootMemos(t)
	var rows []beads.Bead
	for i := 0; i < 20; i++ {
		rootID := fmt.Sprintf("qc-live%02d", i)
		rows = append(rows, closedRootWorkflowRoot(rootID, "open"), beads.Bead{
			ID: rootID + ".1", Title: "step", Status: "open", Type: "task",
			Metadata: map[string]string{"gc.routed_to": demandClosedRootTemplate, "gc.root_bead_id": rootID},
		})
	}
	store := &demandCountingStore{MemStore: beads.NewMemStoreFrom(0, rows, nil)}
	for pass, wantGets := range []int{8, 16, 20, 20} {
		if count, _ := countDemandClosedRoot(t, store); count != 20 || store.gets != wantGets {
			t.Fatalf("pass %d: demand = %d, root reads = %d; want 20 and %d", pass+1, count, store.gets, wantGets)
		}
	}
}

// Two stores under one group key never share a verdict: the same root id
// closed in one and open in the other is judged per store.
func TestDemandRootVerdictsAreKeyedByStore(t *testing.T) {
	resetDemandRootMemos(t)
	closedRows, _ := demandClosedRootFixture("closed", true)
	openRows, _ := demandClosedRootFixture("open", true)
	if count, _ := countDemandClosedRoot(t, beads.NewMemStoreFrom(0, closedRows, nil)); count != 1 {
		t.Fatalf("closed-root store: demand = %d, want 1", count)
	}
	if count, _ := countDemandClosedRoot(t, beads.NewMemStoreFrom(0, openRows, nil)); count != 27 {
		t.Fatalf("open-root store: demand = %d, want 27 — it read the other store's closed verdict", count)
	}
}
