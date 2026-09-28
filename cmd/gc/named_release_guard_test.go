package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Fence #1 enforcement 2 of 2 (ga-9n8hjv; Cherub's typed Q12 re-ruling recorded
// on ga-kashye 2026-09-27T03:33Z): session teardown never unassigns or resets a
// NAMED agent's beads; it proposes. The spec's test, from katya's 2026-09-26T21:53Z
// comment: (1) a named seat with in_progress, open AND blocked beads keeps every
// assignee and status when its session bead closes, including the rebalance case;
// (2) CONTROL: a dead POOL session's in_progress bead is still released. Part (3),
// the dolt_diff_issues read-back, runs in town after the install.

// poolReleaseGuardForTest is the guard a pool-work release test passes: built,
// with no config and no session, so nothing is named and the release proceeds.
func poolReleaseGuardForTest() namedReleaseGuard {
	return namedReleaseGuardForAssignee(nil)
}

// namedSeatPortfolio seeds one bead per status the 2026-09-11 wave touched
// (in_progress, open, blocked) on assignee, and returns their IDs by status.
func namedSeatPortfolio(t *testing.T, store beads.Store, assignee string) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, status := range []string{"in_progress", "open", "blocked"} {
		b := mustCreateDrainAckBead(t, store, beads.Bead{Title: status + " work", Type: "task"}, status, assignee)
		ids[status] = b.ID
	}
	return ids
}

func assertPortfolioKept(t *testing.T, store beads.Store, ids map[string]string, assignee string, stderr *bytes.Buffer) {
	t.Helper()
	for status, id := range ids {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Assignee != assignee || got.Status != status {
			t.Fatalf("%s bead %s is status=%q assignee=%q, want status=%q assignee=%q kept; stderr=%s",
				status, id, got.Status, got.Assignee, status, assignee, stderr.String())
		}
	}
}

// TestNamedSeatCloseKeepsPortfolioWhateverTheAgentState is spec part (1) on the
// close path, for every state the seat can be in when its session bead closes:
// live, SUSPENDED (released before ga-9n8hjv), REMOVED from config (released
// before), and with no config at all.
func TestNamedSeatCloseKeepsPortfolioWhateverTheAgentState(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.City
	}{
		{"live", crewRollConfig(false)},
		{"suspended", crewRollConfig(true)},
		{"removed from config", &config.City{}},
		{"no config", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			ids := namedSeatPortfolio(t, store, crewRuntimeIdentity)
			var stderr bytes.Buffer
			releaseWorkFromClosedSessionBeadExcept(store, tc.cfg, crewSessionBead(), nil, &stderr)
			assertPortfolioKept(t, store, ids, crewRuntimeIdentity, &stderr)
			if strings.Contains(stderr.String(), "RELEASED work") {
				t.Fatalf("close released a named seat's work: %s", stderr.String())
			}
		})
	}
}

// TestNamedSeatCloseAfterRebalanceKeepsPortfolio is spec part (1)'s rebalance
// case: the seat's template changed (a provider or account flip) before the old
// session bead closed, so the session bead's template metadata no longer matches
// the config. The work is still the named agent's.
func TestNamedSeatCloseAfterRebalanceKeepsPortfolio(t *testing.T) {
	store := beads.NewMemStore()
	ids := namedSeatPortfolio(t, store, crewRuntimeIdentity)
	sb := crewSessionBead()
	sb.Metadata["template"] = "qcore/cherub-law.ray-previous-provider"
	var stderr bytes.Buffer
	releaseWorkFromClosedSessionBeadExcept(store, crewRollConfig(false), sb, nil, &stderr)
	assertPortfolioKept(t, store, ids, crewRuntimeIdentity, &stderr)
}

// TestNamedSeatCloseProposesWorkHeldUnderTheSessionHandle: work the named session
// held under its bead ID is released by nothing else once the handle dies, which
// is why it used to be released here. Now it keeps its assignee and carries the
// proposal instead.
func TestNamedSeatCloseProposesWorkHeldUnderTheSessionHandle(t *testing.T) {
	store := beads.NewMemStore()
	sb := crewSessionBead()
	held := mustCreateDrainAckBead(t, store, beads.Bead{Title: "held by handle", Type: "task"}, "in_progress", sb.ID)
	var stderr bytes.Buffer
	releaseWorkFromClosedSessionBeadExcept(store, crewRollConfig(false), sb, nil, &stderr)
	got, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertProposedNotReleased(t, got, sb.ID, "in_progress", "closing-session-release", "handle-held work")
	if !strings.Contains(stderr.String(), "WITHHELD work "+held.ID) {
		t.Fatalf("no WITHHELD audit line for %s; stderr=%s", held.ID, stderr.String())
	}
}

// TestNamedSeatDrainAckKeepsHeldClaim: the drain-ack release swept every
// identifier, the configured identity included, with no named check at all.
func TestNamedSeatDrainAckKeepsHeldClaim(t *testing.T) {
	store := beads.NewMemStore()
	held := mustCreateDrainAckBead(t, store, beads.Bead{Title: "named claim", Type: "task"}, "in_progress", crewRuntimeIdentity)
	var stderr bytes.Buffer
	releaseUnexecutedClaimsOnDrainAck("", crewRollConfig(false), store, nil, crewSessionBead(), drainAckReleaseBudget, &stderr)
	got, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertProposedNotReleased(t, got, crewRuntimeIdentity, "in_progress", "draining-session-unexecuted-claim", "drain-ack held claim")
}

// TestPoolSessionCloseStillReleasesInProgressWork is spec part (2), the CONTROL:
// the guard must not disarm the legitimate path. A dead pool worker's in_progress
// claim is released for re-dispatch, on the close path and on drain-ack.
func TestPoolSessionCloseStillReleasesInProgressWork(t *testing.T) {
	poolCfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(4)}}}
	for _, tc := range []struct {
		name string
		run  func(store beads.Store, stderr *bytes.Buffer)
	}{
		{"close", func(store beads.Store, stderr *bytes.Buffer) {
			releaseWorkFromClosedSessionBeadExcept(store, poolCfg, drainAckSessionBead(), nil, stderr)
		}},
		{"drain-ack", func(store beads.Store, stderr *bytes.Buffer) {
			releaseUnexecutedClaimsOnDrainAck("", poolCfg, store, nil, drainAckSessionBead(), drainAckReleaseBudget, stderr)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			held := mustCreateDrainAckBead(t, store, beads.Bead{Title: "pool claim", Type: "task"}, "in_progress", "worker-1")
			var stderr bytes.Buffer
			tc.run(store, &stderr)
			got, err := store.Get(held.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Assignee != "" || got.Status != "open" {
				t.Fatalf("pool claim is status=%q assignee=%q, want released (open, unassigned); stderr=%s", got.Status, got.Assignee, stderr.String())
			}
			if got.Metadata[beadmeta.ReleaseProposedAtMetadataKey] != "" {
				t.Fatalf("pool claim carries a release proposal; pool work must be released, not proposed")
			}
		})
	}
}

// TestZeroNamedReleaseGuardFailsClosed: a release path that forgets to build a
// guard withholds rather than releases.
func TestZeroNamedReleaseGuardFailsClosed(t *testing.T) {
	store := beads.NewMemStore()
	held := mustCreateDrainAckBead(t, store, beads.Bead{Title: "pool claim", Type: "task"}, "in_progress", "worker-1")
	wa := workAssignmentForStore(beads.WorkStore{Store: store})
	var audit bytes.Buffer
	if err := wa.ReleaseWorkBead(held, "", namedReleaseGuard{}, &audit, "test"); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	got, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertProposedNotReleased(t, got, "worker-1", "in_progress", "test", "zero-guard release")
}

// TestProposeNamedReleaseWritesOnce: the orphan sweep revisits the same bead every
// tick, so a proposal is written on first sight only.
func TestProposeNamedReleaseWritesOnce(t *testing.T) {
	store := beads.NewMemStore()
	held := mustCreateDrainAckBead(t, store, beads.Bead{Title: "named claim", Type: "task"}, "in_progress", crewRuntimeIdentity)
	wrote, err := proposeNamedRelease(store, held, "reason", "test", nil)
	if err != nil || !wrote {
		t.Fatalf("first proposal wrote=%v err=%v, want a write", wrote, err)
	}
	again, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	wrote, err = proposeNamedRelease(store, again, "reason", "test", nil)
	if err != nil || wrote {
		t.Fatalf("second proposal wrote=%v err=%v, want no write", wrote, err)
	}
}

// TestOrphanSweepProposesNamedAssigneeOnAnyTemplate: the pool writer consults the
// guard itself, so a named agent's claim is withheld even when the bead is routed
// to a pool template (the case assigneePreservesNamedSessionRoute skips) and when
// the claim is held under the runtime-name spelling.
func TestOrphanSweepProposesNamedAssigneeOnAnyTemplate(t *testing.T) {
	store := beads.NewMemStore()
	held := mustCreateDrainAckBead(t, store, beads.Bead{
		Title: "slung to the pool, held by the named seat", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "qcore/polecat"},
	}, "in_progress", crewRuntimeIdentity)
	if released := releaseOrphanedPoolAssignment(store, held, false, namedReleaseGuardForAssignee(crewRollConfig(true))); released {
		t.Fatalf("orphan sweep released a (suspended) named agent's claim")
	}
	got, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertProposedNotReleased(t, got, crewRuntimeIdentity, "in_progress", "orphaned-pool-assignment", "orphan-sweep claim")
}

// TestDuplicateRepairLeavesNamedIdentityWorkInPlace: duplicate named-session
// repair used to re-home work held under the named identity onto the winner's
// session bead ID, where the next close could release it. Identity-held work now
// stays put; handle-held work still moves to the winner.
func TestDuplicateRepairLeavesNamedIdentityWorkInPlace(t *testing.T) {
	store := beads.NewMemStore()
	loser := crewSessionBead()
	byIdentity := mustCreateDrainAckBead(t, store, beads.Bead{Title: "identity-held", Type: "task"}, "in_progress", crewRuntimeIdentity)
	byHandle := mustCreateDrainAckBead(t, store, beads.Bead{Title: "handle-held", Type: "task"}, "in_progress", loser.ID)
	var stderr bytes.Buffer
	reassignWorkAssignedToRetiredSessionBead("", crewRollConfig(false), store, nil, loser, "ga-winner", &stderr)

	got, err := store.Get(byIdentity.ID)
	if err != nil {
		t.Fatalf("get identity-held: %v", err)
	}
	if got.Assignee != crewRuntimeIdentity {
		t.Fatalf("identity-held work re-homed to %q, want it left on %q", got.Assignee, crewRuntimeIdentity)
	}
	got, err = store.Get(byHandle.ID)
	if err != nil {
		t.Fatalf("get handle-held: %v", err)
	}
	if got.Assignee != "ga-winner" {
		t.Fatalf("handle-held work assignee = %q, want moved to the winner ga-winner; stderr=%s", got.Assignee, stderr.String())
	}
}
