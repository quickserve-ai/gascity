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

// seedWorkBead seeds a work bead in the requested status/assignee and returns the
// bead as the store now holds it. mustCreateDrainAckBead returns the CREATE-time
// snapshot (open, unassigned), and a writer handed that snapshot releases or
// proposes against the wrong assignee, so every direct writer call here needs the
// re-read row.
func seedWorkBead(t *testing.T, store beads.Store, bead beads.Bead, status, assignee string) beads.Bead {
	t.Helper()
	created := mustCreateDrainAckBead(t, store, bead, status, assignee)
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("re-reading seeded %q: %v", bead.Title, err)
	}
	return got
}

// namedSeatPortfolio seeds one bead per status the 2026-09-11 wave touched
// (in_progress, open, blocked) on assignee, and returns their IDs by status.
func namedSeatPortfolio(t *testing.T, store beads.Store, assignee string) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, status := range []string{"in_progress", "open", "blocked"} {
		b := seedWorkBead(t, store, beads.Bead{Title: status + " work", Type: "task"}, status, assignee)
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
	held := seedWorkBead(t, store, beads.Bead{Title: "held by handle", Type: "task"}, "in_progress", sb.ID)
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
	held := seedWorkBead(t, store, beads.Bead{Title: "named claim", Type: "task"}, "in_progress", crewRuntimeIdentity)
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
			held := seedWorkBead(t, store, beads.Bead{Title: "pool claim", Type: "task"}, "in_progress", "worker-1")
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
	held := seedWorkBead(t, store, beads.Bead{Title: "pool claim", Type: "task"}, "in_progress", "worker-1")
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
	held := seedWorkBead(t, store, beads.Bead{Title: "named claim", Type: "task"}, "in_progress", crewRuntimeIdentity)
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
	held := seedWorkBead(t, store, beads.Bead{
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
	byIdentity := seedWorkBead(t, store, beads.Bead{Title: "identity-held", Type: "task"}, "in_progress", crewRuntimeIdentity)
	byHandle := seedWorkBead(t, store, beads.Bead{Title: "handle-held", Type: "task"}, "in_progress", loser.ID)
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

// TestBrokenConfigCloseProposesNamedSessionHandleWork: when the city config does
// not load, `gc session close` releases only work bound to the session bead ID,
// through an ID-only copy of the bead. That copy used to drop the named marker,
// so a named seat's bead-ID work was released. It keeps the marker now (and only
// the marker: no identifier is added to the sweep).
func TestBrokenConfigCloseProposesNamedSessionHandleWork(t *testing.T) {
	sb := crewSessionBead()
	idOnly := sessionBeadIDOnlyIdentity(sb)
	if ids := sessionAssignmentIdentifiers(idOnly); len(ids) != 1 || ids[0] != sb.ID {
		t.Fatalf("ID-only identifiers = %v, want only %s: the marker must not widen the sweep", ids, sb.ID)
	}
	store := beads.NewMemStore()
	held := seedWorkBead(t, store, beads.Bead{Title: "held by handle", Type: "task"}, "in_progress", sb.ID)
	var stderr bytes.Buffer
	unclaimWorkAssignedToRetiredSessionBead("", nil, store, nil, idOnly, "", &stderr)
	got, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertProposedNotReleased(t, got, sb.ID, "in_progress", "retired-session-unclaim", "broken-config handle work")
	// The close command's stderr says "proposing" rather than "releasing" on
	// exactly this predicate, so it must hold for the ID-only form.
	if !isNamedSessionBead(idOnly) {
		t.Fatalf("ID-only form of a named session bead is not named; the close message would claim a release")
	}

	pool := sessionBeadIDOnlyIdentity(drainAckSessionBead())
	if len(pool.Metadata) != 0 {
		t.Fatalf("pool ID-only bead metadata = %v, want none", pool.Metadata)
	}
}

// TestNamedReleaseGuardRecognizesAssigneeForms pins the assignee half of the
// guard directly, per spelling and per agent state, so a lookup that misses one
// form fails here by name rather than only through a sweep.
func TestNamedReleaseGuardRecognizesAssigneeForms(t *testing.T) {
	for _, suspended := range []bool{false, true} {
		cfg := crewRollConfig(suspended)
		spec, ok := findNamedSessionSpec(cfg, cfg.EffectiveCityName(), crewRuntimeIdentity)
		t.Logf("suspended=%v findNamedSessionSpec(%q) ok=%v identity=%q session_name=%q", suspended, crewRuntimeIdentity, ok, spec.Identity, spec.SessionName)
		g := namedReleaseGuardForAssignee(cfg)
		for _, assignee := range []string{crewRuntimeIdentity, spec.SessionName} {
			if assignee == "" {
				continue
			}
			if g.withholdReason(assignee) == "" {
				t.Errorf("suspended=%v: withholdReason(%q) = \"\", want the named agent recognized", suspended, assignee)
			}
		}
		for _, pool := range []string{"worker-1", "qcore/polecat-3", "gc-123"} {
			if r := g.withholdReason(pool); r != "" {
				t.Errorf("suspended=%v: withholdReason(%q) = %q, want \"\" for pool work", suspended, pool, r)
			}
		}
	}
}

// TestOrphanSweepHonorsPendingProposal: a named session's handle-held work,
// proposed at teardown, is assigned to a dead session ID the assignee lookup
// cannot resolve as named. The next orphan sweep must still leave it to the judge.
func TestOrphanSweepHonorsPendingProposal(t *testing.T) {
	store := beads.NewMemStore()
	held := seedWorkBead(t, store, beads.Bead{
		Title: "proposed at teardown", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	}, "in_progress", "ga-oldsession")
	if _, err := proposeNamedRelease(store, held, "session ga-oldsession serves a named agent", "closing-session-release", nil); err != nil {
		t.Fatalf("propose: %v", err)
	}
	proposed, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if released := releaseOrphanedPoolAssignment(store, proposed, false, namedReleaseGuardForAssignee(nil)); released {
		t.Fatalf("orphan sweep released a bead with a pending release proposal")
	}
	got, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Assignee != "ga-oldsession" || got.Status != "in_progress" {
		t.Fatalf("proposed bead is status=%q assignee=%q, want in_progress/ga-oldsession kept", got.Status, got.Assignee)
	}
	if got.Metadata[beadmeta.ReleaseProposedPathMetadataKey] != "closing-session-release" {
		t.Fatalf("proposal path rewritten to %q, want the first proposal kept", got.Metadata[beadmeta.ReleaseProposedPathMetadataKey])
	}
}

// TestPoolAliasEqualToNamedShorthandIsNotANamedSession: a V2 named session
// "team.ray" is reachable by the bare shorthand "ray". A POOL session whose alias
// happens to be "ray" must not be classified as a named session, or every release
// it makes would be withheld and the pool stranded.
func TestPoolAliasEqualToNamedShorthandIsNotANamedSession(t *testing.T) {
	cfg := &config.City{
		Agents:        []config.Agent{{Name: "reviewer", BindingName: "team"}, {Name: "worker", MaxActiveSessions: intPtr(4)}},
		NamedSessions: []config.NamedSession{{BindingName: "team", Name: "ray", Template: "reviewer"}},
	}
	if _, ok := findNamedSessionSpecForAssignee(cfg, cfg.EffectiveCityName(), "ray"); !ok {
		t.Fatalf("fixture: shorthand %q does not resolve, so this test would pass vacuously", "ray")
	}
	pool := beads.Bead{ID: "ga-pool1", Metadata: map[string]string{
		"pool_managed": "true", "template": "worker", "alias": "ray", "session_name": "worker-ga-pool1",
	}}
	if g := namedReleaseGuardForSessionBead(cfg, pool); g.sessionNamed {
		t.Fatalf("pool session with alias %q was classified as a named session", "ray")
	}
	named := beads.Bead{ID: "ga-named1", Metadata: map[string]string{"session_name": "team.ray"}}
	if g := namedReleaseGuardForSessionBead(cfg, named); !g.sessionNamed {
		t.Fatalf("session named by the exact identity %q was not classified as named", "team.ray")
	}
}

// TestJudgeClearedProposalReArms: the judge discharges a proposal by removing the
// label and clearing the stamp; a later teardown must be able to propose again.
func TestJudgeClearedProposalReArms(t *testing.T) {
	store := beads.NewMemStore()
	held := seedWorkBead(t, store, beads.Bead{Title: "named claim", Type: "task"}, "in_progress", crewRuntimeIdentity)
	if wrote, err := proposeNamedRelease(store, held, "r", "p1", nil); err != nil || !wrote {
		t.Fatalf("first proposal wrote=%v err=%v", wrote, err)
	}
	if err := store.Update(held.ID, beads.UpdateOpts{
		RemoveLabels: []string{beadmeta.ReleaseProposedLabel},
		Metadata:     map[string]string{beadmeta.ReleaseProposedAtMetadataKey: ""},
	}); err != nil {
		t.Fatalf("judge clear: %v", err)
	}
	cleared, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if wrote, err := proposeNamedRelease(store, cleared, "r", "p2", nil); err != nil || !wrote {
		t.Fatalf("proposal after the judge cleared it wrote=%v err=%v, want a fresh proposal", wrote, err)
	}
	got, err := store.Get(held.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Metadata[beadmeta.ReleaseProposedPathMetadataKey] != "p2" || !hasLabel(got.Labels, beadmeta.ReleaseProposedLabel) {
		t.Fatalf("re-armed proposal: path=%q labels=%v, want p2 and the label", got.Metadata[beadmeta.ReleaseProposedPathMetadataKey], got.Labels)
	}
}

// TestProposedClaimIsNotWakeDemand: a named seat that drain-acks while holding a
// claim keeps it (ga-9n8hjv); if the claim still counted as wake demand the seat
// would be re-woken by it and drain-ack again, in a loop. A pending proposal is
// the judge's, so it is not demand; the same claim without one still is.
func TestProposedClaimIsNotWakeDemand(t *testing.T) {
	claim := AwakeWorkBead{ID: "gc-1", Assignee: crewRuntimeIdentity, Status: "in_progress"}
	if !workBeadHasAwakeDemand(claim) {
		t.Fatalf("control: an in_progress claim with no proposal must be wake demand")
	}
	claim.ReleaseProposed = true
	if workBeadHasAwakeDemand(claim) {
		t.Fatalf("an in_progress claim with a pending release proposal is wake demand; want none (drain-ack loop)")
	}
	if !releaseProposalPending(beads.Bead{Metadata: map[string]string{beadmeta.ReleaseProposedAtMetadataKey: "2026-09-28T00:00:00Z"}}) {
		t.Fatalf("the bridge's input: a proposal stamp must read as pending with no labels hydrated")
	}
}

// TestProposeNamedReleaseSkipsAReClaimedBead: the writers act on a snapshot. When
// a fresh worker re-claims the bead between the List and the proposal write, the
// proposal must not land on the new owner's bead: it would name the old assignee,
// keep every writer off the bead and drop it from wake demand until a judge
// cleared it.
func TestProposeNamedReleaseSkipsAReClaimedBead(t *testing.T) {
	store := beads.NewMemStore()
	snapshot := seedWorkBead(t, store, beads.Bead{Title: "re-claimed after the snapshot", Type: "task"}, "in_progress", crewRuntimeIdentity)
	newOwner := "qcore/polecat-7"
	if err := store.Update(snapshot.ID, beads.UpdateOpts{Assignee: &newOwner}); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	var audit bytes.Buffer
	wrote, err := proposeNamedRelease(store, snapshot, "reason", "test", &audit)
	if err != nil || wrote {
		t.Fatalf("proposal over a re-claimed bead wrote=%v err=%v, want no write", wrote, err)
	}
	got, err := store.Get(snapshot.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if releaseProposalPending(got) {
		t.Fatalf("re-claimed bead carries a release proposal: labels=%v metadata=%v", got.Labels, got.Metadata)
	}
	if got.Assignee != newOwner {
		t.Fatalf("assignee = %q, want the new owner %q untouched", got.Assignee, newOwner)
	}
	if !strings.Contains(audit.String(), "SKIPPED release proposal for "+snapshot.ID) {
		t.Fatalf("no SKIPPED audit line; audit=%s", audit.String())
	}
}

// TestOrphanSweepProposesWorkHeldUnderADeadNamedSessionHandle: a named session
// closed by a path that proposes nothing (POST /v0/session/{id}/close closes the
// session and leaves its work alone) hands its handle-held work to the orphan
// sweep, where the assignee is only a bead ID. The sweep looks the handle up, so
// the work is proposed, not released. CONTROLS: a pool session's handle and an
// assignee that is no session at all still release.
func TestOrphanSweepProposesWorkHeldUnderADeadNamedSessionHandle(t *testing.T) {
	sessions := beads.NewMemStore()
	sessions.HonorExplicitIDs = true
	named := crewSessionBead()
	pool := drainAckSessionBead()
	for _, sb := range []beads.Bead{named, pool} {
		if _, err := sessions.Create(sb); err != nil {
			t.Fatalf("seeding session bead %s: %v", sb.ID, err)
		}
	}
	work := beads.NewMemStore()
	fallback := namedReleaseGuardForAssignee(nil)
	memo := map[string]orphanSweepGuardResult{}
	routed := map[string]string{beadmeta.RoutedToMetadataKey: "worker"}

	held := seedWorkBead(t, work, beads.Bead{Title: "held by a dead named handle", Type: "task", Metadata: routed}, "in_progress", named.ID)
	guard, decided := orphanSweepGuard(nil, sessions, held, fallback, memo)
	if !decided {
		t.Fatalf("lookup of a present session bead was undecided")
	}
	if released := releaseOrphanedPoolAssignment(work, held, false, guard); released {
		t.Fatalf("orphan sweep released work held under a dead named session's handle")
	}
	got, err := work.Get(held.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertProposedNotReleased(t, got, named.ID, "in_progress", "orphaned-pool-assignment", "dead named handle work")

	for _, assignee := range []string{pool.ID, "ga-never-a-session"} {
		b := seedWorkBead(t, work, beads.Bead{Title: "held by " + assignee, Type: "task", Metadata: routed}, "in_progress", assignee)
		g, ok := orphanSweepGuard(nil, sessions, b, fallback, memo)
		if !ok {
			t.Fatalf("%s: lookup undecided, want the fallback guard", assignee)
		}
		if reason := g.withholdReasonForBead(b); reason != "" {
			t.Fatalf("%s: work withheld (%s), want it releasable", assignee, reason)
		}
	}
}
