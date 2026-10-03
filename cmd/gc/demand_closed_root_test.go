package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	for _, err := range errs {
		if !errors.Is(err, errDemandRootsUnresolved) {
			t.Fatalf("defaultScaleCheckCountsAndDemand err = %v", err)
		}
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

// resetDemandRootMemos keeps root verdicts and knobs from crossing tests.
func resetDemandRootMemos(t *testing.T) {
	t.Helper()
	ttl, openTTL, budget, refresh, sweep, logEvery, clock := demandClosedRootTTL, demandOpenRootTTL,
		demandRootResolveBudget, demandRootRefreshBudget, demandRootMemoSweepAt, demandRootUnresolvedLogEvery, demandClosedRootClock
	reset := func() {
		demandRootMemos.m, demandRootMemos.logged = map[string]demandRootMemo{}, time.Time{}
	}
	reset()
	t.Cleanup(func() {
		reset()
		demandClosedRootTTL, demandOpenRootTTL, demandRootResolveBudget, demandRootRefreshBudget = ttl, openTTL, budget, refresh
		demandRootMemoSweepAt, demandRootUnresolvedLogEvery, demandClosedRootClock = sweep, logEvery, clock
	})
}
