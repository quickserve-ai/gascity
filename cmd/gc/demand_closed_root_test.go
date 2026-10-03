package main

import (
	"bytes"
	"context"
	"fmt"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// Pins ga-b2oxyf: the controller's demand count skips exactly the steps the
// router's closed-root gate skips (qc-z0fmn0n). Before it, an open step of a
// molecule whose root was closed was still counted as pool demand: the pool
// spawned a seat, gc hook --claim skipped every row, and the seat drained —
// every tick. The 2026-09-23 census found ~500 such steps (20 of 27 open
// molecules wrapping closed work).
//
// Both sides read ONE fixture through real MemStores: the demand count via
// defaultScaleCheckCountsAndDemand, the router via claimHookWorkWithRunner
// over the same rows rendered as its work query.

const (
	demandClosedRootID       = "qc-4al798r"
	demandClosedRootTemplate = "worker"
	demandClosedRootTeardown = "qc-4al798r.27"
)

// demandClosedRootFixture is the replay shape: a molecule root with the given
// status, 27 routed open steps, and the closed teardown-scoped step whose
// gc.step_id the 27th (a retry attempt with gc.scope_role stripped) shares —
// so the 27th is in the root's teardown tail and the other 26 are not.
// withStepIDs=false drops gc.step_id from the 26 ordinary steps.
func demandClosedRootFixture(rootStatus string, withStepIDs bool) (all, open []beads.Bead) {
	all = append(all, closedRootWorkflowRoot(demandClosedRootID, rootStatus))
	all = append(all, beads.Bead{
		ID: demandClosedRootID + ".cleanup", Title: "cleanup", Status: "closed", Type: "task",
		Metadata: map[string]string{
			"gc.root_bead_id": demandClosedRootID,
			"gc.step_id":      "cleanup-worktree",
			"gc.scope_role":   "teardown",
		},
	})
	for i := 1; i <= 27; i++ {
		meta := map[string]string{
			"gc.routed_to":    demandClosedRootTemplate,
			"gc.root_bead_id": demandClosedRootID,
		}
		switch {
		case i == 27:
			meta["gc.step_id"] = "cleanup-worktree"
		case withStepIDs:
			meta["gc.step_id"] = fmt.Sprintf("step-%d", i)
		}
		open = append(open, beads.Bead{
			ID: fmt.Sprintf("%s.%d", demandClosedRootID, i), Title: "step", Status: "open", Type: "task",
			Metadata: meta,
		})
	}
	return append(all, open...), open
}

func countDemandClosedRoot(t *testing.T, store beads.Store) (int, []string) {
	t.Helper()
	counts, demand, _, errs := defaultScaleCheckCountsAndDemand(nil, []defaultScaleCheckTarget{{
		template: demandClosedRootTemplate,
		storeKey: "rig:q",
		store:    store,
	}})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCountsAndDemand errs = %v", errs)
	}
	return counts[demandClosedRootTemplate], demand[demandClosedRootTemplate].WorkBeadIDs
}

func workQueryFor(rows []beads.Bead) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, stepJSON(row))
	}
	return `[` + strings.Join(parts, ",") + `]`
}

// The replay: 27 routed open steps under a closed root, one in its teardown
// tail. Demand counts the teardown step alone, the router claims that one
// alone, and a seat that drains on any of the other 26 is classified
// closed_root — never an unexplained divergence.
func TestDemandReplayClosedRootCountsOnlyTheTeardownTailTheRouterServes(t *testing.T) {
	resetDemandRootMemos(t)
	all, open := demandClosedRootFixture("closed", true)

	count, ids := countDemandClosedRoot(t, beads.NewMemStoreFrom(0, all, nil))
	if count != 1 || len(ids) != 1 || ids[0] != demandClosedRootTeardown {
		t.Fatalf("demand = %d %v, want 1 [%s]: only the teardown-tail step is servable under a closed root", count, ids, demandClosedRootTeardown)
	}

	spy := &closedRootClaimSpy{}
	reader := newClosedRootStoreReader(all...)
	ops, opts := closedRootOpsOpts("", spy, reader)
	ops.Runner = nil
	legs := []hookStore{{dir: "/rig-q", env: []string{"BEADS_DIR=/rig-q"}}}
	run := func(string, string, []string) (string, error) { return workQueryFor(open), nil }
	var stdout, stderr bytes.Buffer
	claimHookWorkWithRunner("query", "/rig-q", nil, legs, opts, ops, run, func(string, error) {}, &stdout, &stderr)
	if len(spy.ids) != 1 || spy.ids[0] != ids[0] {
		t.Fatalf("router claimed %v, demand counted %v: spawn and claim disagree; stderr=%s", spy.ids, ids, stderr.String())
	}

	// The teardown step is gone; a seat spawned on any of the other 26 drains.
	for _, trigger := range open[:26] {
		spy := &closedRootClaimSpy{}
		reader := newClosedRootStoreReader(all...)
		ops, opts := closedRootOpsOpts(workQueryFor(open[:26]), spy, reader)
		ops.ReadWorkMeta = reader.fn
		ops.ConfirmBlocked = func(context.Context, string, []string, string, string) (bool, error) { return false, nil }
		_, classification, calls := classifyAfterDrain(t, trigger.ID, opts, ops)
		if len(spy.ids) != 0 || calls != 1 {
			t.Fatalf("%s: claims = %v, divergence calls = %d; want a drain that reaches the classifier once", trigger.ID, spy.ids, calls)
		}
		if classification != events.DemandClaimClosedRoot {
			t.Fatalf("%s: classification = %q, want %q", trigger.ID, classification, events.DemandClaimClosedRoot)
		}
	}
}

// Done-means 1: a route whose only open steps sit under a closed root, outside
// its teardown tail, is no demand at all — with or without gc.step_id.
func TestDemandIsZeroWhenEveryRoutedStepSitsUnderAClosedRoot(t *testing.T) {
	resetDemandRootMemos(t)
	for _, withStepIDs := range []bool{true, false} {
		resetDemandRootMemos(t)
		all, _ := demandClosedRootFixture("closed", withStepIDs)
		store := beads.NewMemStoreFrom(0, all[:len(all)-1], nil) // drop the teardown-tail step
		if count, ids := countDemandClosedRoot(t, store); count != 0 {
			t.Fatalf("withStepIDs=%v: demand = %d %v, want 0", withStepIDs, count, ids)
		}
	}
}

// Control: the same 27 steps under an OPEN root are all demand, so the zero
// above came from the root's status, not the fixture.
func TestDemandCountsEveryStepUnderAnOpenRoot(t *testing.T) {
	resetDemandRootMemos(t)
	all, _ := demandClosedRootFixture("open", true)
	if count, ids := countDemandClosedRoot(t, beads.NewMemStoreFrom(0, all, nil)); count != 27 {
		t.Fatalf("demand = %d %v, want 27 under an open root", count, ids)
	}
}

// The router skips a closed root's step that carries no gc.step_id without
// building any teardown tail (it cannot be a retry attempt). The drain's
// classifier must agree and call it closed_root, not a divergence.
func TestDemandDivergenceClassifiesAClosedRootStepWithoutStepIDAsClosedRoot(t *testing.T) {
	all, open := demandClosedRootFixture("closed", false)
	trigger := open[0]
	spy := &closedRootClaimSpy{}
	reader := newClosedRootStoreReader(all...)
	ops, opts := closedRootOpsOpts(workQueryFor([]beads.Bead{trigger}), spy, reader)
	ops.ReadWorkMeta = reader.fn
	ops.ConfirmBlocked = func(context.Context, string, []string, string, string) (bool, error) { return false, nil }

	_, classification, calls := classifyAfterDrain(t, trigger.ID, opts, ops)
	if len(spy.ids) != 0 || calls != 1 {
		t.Fatalf("claims = %v, divergence calls = %d; want a drain that reaches the classifier once", spy.ids, calls)
	}
	if reader.tails != 0 {
		t.Fatalf("teardown-tail builds = %d, want 0 — the router needs no tail for a step without gc.step_id", reader.tails)
	}
	if classification != events.DemandClaimClosedRoot {
		t.Fatalf("classification = %q, want %q", classification, events.DemandClaimClosedRoot)
	}
}

// resetDemandRootMemos empties the process-level root cache and restores its
// knobs when the test ends, so no verdict crosses tests.
func resetDemandRootMemos(t *testing.T) {
	t.Helper()
	ttl, budget, clock := demandClosedRootTTL, demandRootResolveBudget, demandClosedRootClock
	clear := func() {
		demandRootMemos.Lock()
		demandRootMemos.m = map[string]demandRootMemo{}
		demandRootMemos.Unlock()
	}
	clear()
	t.Cleanup(func() {
		clear()
		demandClosedRootTTL, demandRootResolveBudget, demandClosedRootClock = ttl, budget, clock
	})
}

// demandCountingStore counts the root reads and teardown queries the demand
// gate sends to its store.
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

// A closed root (and its teardown tail) is read once, then remembered: the
// next pass sends no read for it to the store, until the TTL lapses.
func TestDemandRemembersAClosedRootAcrossPassesUntilTheTTL(t *testing.T) {
	resetDemandRootMemos(t)
	now := time.Now()
	demandClosedRootClock = func() time.Time { return now }
	all, _ := demandClosedRootFixture("closed", true)
	store := &demandCountingStore{MemStore: beads.NewMemStoreFrom(0, all, nil)}

	for pass, want := range [][2]int{{1, 1}, {1, 1}} {
		if count, ids := countDemandClosedRoot(t, store); count != 1 {
			t.Fatalf("pass %d: demand = %d %v, want 1", pass+1, count, ids)
		}
		if store.gets != want[0] || store.lists != want[1] {
			t.Fatalf("pass %d: root reads = %d, teardown queries = %d; want %d and %d in total", pass+1, store.gets, store.lists, want[0], want[1])
		}
	}

	now = now.Add(demandClosedRootTTL)
	if count, _ := countDemandClosedRoot(t, store); count != 1 || store.gets != 2 || store.lists != 2 {
		t.Fatalf("after the TTL: demand = %d, root reads = %d, teardown queries = %d; want 1, 2, 2", count, store.gets, store.lists)
	}
}

// One pass resolves at most demandRootResolveBudget new roots; the steps of
// the rest are counted, and the next pass resolves the next ones.
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

// A closed root whose teardown tail cannot be read is unresolved: its steps
// are counted (a retry attempt the router serves is never dropped), and the
// failure is not remembered.
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
