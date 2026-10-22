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

// demandOpenMolecules stores n open roots, each with one routed open step.
func demandOpenMolecules(n int) []beads.Bead {
	var rows []beads.Bead
	for i := 0; i < n; i++ {
		rootID := fmt.Sprintf("qc-live%02d", i)
		rows = append(rows, closedRootWorkflowRoot(rootID, "open"), beads.Bead{
			ID: rootID + ".1", Title: "step", Status: "open", Type: "task",
			Metadata: map[string]string{"gc.routed_to": demandClosedRootTemplate, "gc.root_bead_id": rootID},
		})
	}
	return rows
}

// Refreshes of more than twice the budget of open roots, expiring every pass,
// never starve a closed root that sorts behind them: it is resolved within
// two passes and stays resolved.
func TestDemandOpenRootRefreshesDoNotStarveAClosedRoot(t *testing.T) {
	resetDemandRootMemos(t)
	now := time.Now()
	demandClosedRootClock = func() time.Time { return now }
	store := &demandCountingStore{MemStore: beads.NewMemStoreFrom(0, demandOpenMolecules(20), nil)}
	for pass := 0; pass < 3; pass++ { // warm up: every open root seen once
		countDemandClosedRoot(t, store)
	}
	root, err := store.Create(beads.Bead{Title: "dead root", Type: "task"})
	if err != nil {
		t.Fatalf("creating root: %v", err)
	}
	if err := store.Close(root.ID); err != nil {
		t.Fatalf("closing root: %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "orphan", Type: "task", Metadata: map[string]string{
		"gc.routed_to": demandClosedRootTemplate, "gc.root_bead_id": root.ID,
	}}); err != nil {
		t.Fatalf("creating orphan step: %v", err)
	}
	for pass := 1; pass <= 4; pass++ {
		now = now.Add(demandOpenRootTTL) // every open verdict has expired
		count, _ := countDemandClosedRoot(t, store)
		if pass >= 2 && count != 20 {
			t.Fatalf("pass %d: demand = %d, want 20 — the closed root's step is still counted", pass, count)
		}
	}
}

// A pass that leaves roots unresolved says so once per log interval, not
// every pass.
func TestDemandUnresolvedRootsAreLoggedOncePerInterval(t *testing.T) {
	resetDemandRootMemos(t)
	now := time.Now()
	demandClosedRootClock = func() time.Time { return now }
	demandRootResolveBudget = 1
	var rows []beads.Bead
	for i := 0; i < 10; i++ {
		rootID := fmt.Sprintf("qc-dead%02d", i)
		rows = append(rows, closedRootWorkflowRoot(rootID, "closed"), beads.Bead{
			ID: rootID + ".1", Title: "step", Status: "open", Type: "task",
			Metadata: map[string]string{"gc.routed_to": demandClosedRootTemplate, "gc.root_bead_id": rootID},
		})
	}
	store := beads.NewMemStoreFrom(0, rows, nil)
	lines := func() int {
		_, _, _, errs := defaultScaleCheckCountsAndDemand(nil, []defaultScaleCheckTarget{{
			template: demandClosedRootTemplate, storeKey: "rig:q", store: store,
		}})
		n := 0
		for _, err := range errs {
			if errors.Is(err, errDemandRootsUnresolved) {
				n++
			}
		}
		return n
	}
	for pass, want := range []int{1, 0, 0} {
		if got := lines(); got != want {
			t.Fatalf("pass %d: unresolved lines = %d, want %d", pass+1, got, want)
		}
	}
	now = now.Add(demandRootUnresolvedLogEvery)
	if got := lines(); got != 1 {
		t.Fatalf("after the log interval: unresolved lines = %d, want 1", got)
	}
}

// Past the sweep threshold, remembering a verdict drops the expired ones.
func TestDemandRootMemoSweepRemovesExpiredEntries(t *testing.T) {
	resetDemandRootMemos(t)
	now := time.Now()
	demandClosedRootClock = func() time.Time { return now }
	demandRootMemoSweepAt = 2
	demandRootRemember("a", "open", nil, time.Minute)
	demandRootRemember("b", "closed", nil, time.Hour)
	now = now.Add(2 * time.Minute)
	demandRootRemember("c", "open", nil, time.Minute)
	if _, kept := demandRootMemos.m["a"]; kept || len(demandRootMemos.m) != 2 {
		t.Fatalf("memo keys after sweep = %v, want b and c only", demandRootMemos.m)
	}
}
