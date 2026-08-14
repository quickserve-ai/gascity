package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// ---------------------------------------------------------------------------
// ga-2otk73 ADVERSARIAL REVIEW suite (reviewer-authored, not the implementer's).
//
// PROVENANCE. This file arrived on woodhouse/ga-2otk73-review @ 334dba7a6 as
// four tests that all PASSED on iteration 1 (woodhouse/ga-2otk73-squat @
// febcfd406) — i.e. each one DOCUMENTED A DEFECT that iteration 1 introduced,
// and each was written to FAIL if the behavior were tightened. Iteration 1 was
// rejected, so every one of them is now inverted: the assertion is flipped to
// demand the FIXED behavior, and the original defect claim is preserved above
// it so the inversion is auditable rather than a silent goalpost move.
//
// The reviewer's fixtures, comments, and structure are otherwise unchanged.
// Each test carries an INVERTED block stating (a) what it used to assert,
// (b) what it asserts now, and (c) why the new expectation is the correct one
// and not merely the one the implementation happens to produce.
//
// Three of the four findings (R1, R2a, R3) are satisfied by REMOVAL: iteration
// 2 drops the GAP-1 sweep carve-out entirely, so the sweep once again refuses
// every named session unconditionally and none of those three defects has a
// code path to occur in. Those tests are kept — un-deleted and inverted — as
// standing regression guards that the carve-out is not reintroduced.
// ---------------------------------------------------------------------------

// FINDING R1 (reviewer, on iteration 1). The sweep comment says "Everything
// else about a named session — asleep, suspended, quarantined, idle, mid-start
// — still bails here." It does not. pendingCreateRollbackState
// (session_reconciler.go:1185) accepts StateAsleep, so an ASLEEP named session
// that still carries a pending-create claim is admitted and CLOSED. That shape
// is not hypothetical: the reconciler itself documents it ("keep never-started
// pending-create leases alive after heal has rewritten state=creating to
// asleep", session_reconcile.go:1033-1036).
//
// An asleep named session is the one named shape that carries real continuity
// (session_key, continuation_epoch, resume metadata) with NO runtime to probe,
// so the runtime gate cannot protect it — "asleep" means "authoritatively
// stopped" by construction.
//
// INVERTED. Was: closed == 1 (asleep+claim IS admitted). Now: closed == 0 and
// the bead is untouched. The inversion is correct because the finding is not
// "the guard was mis-tuned", it is "no runtime gate can protect this shape" —
// an asleep session is stopped by definition, so any sweep that admits it is
// deciding to destroy continuity on evidence that is always available. There
// is no tightening that rescues the carve-out for this class, so iteration 2
// removes the carve-out rather than narrowing it, and this test now pins that
// removal.
func TestReview_SweepClosesAsleepNamedSessionWithStalePendingCreateClaim(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	store := beads.NewMemStore()
	// Healed-to-asleep shape: state=asleep, claim still set, no last_woke_at.
	bead := squattingNamedSessionBead(t, store, sessionName, 15*time.Minute, map[string]string{
		"state":       string(session.StateAsleep),
		"session_key": "sk-live-conversation",
	})

	closed := sweepUndesiredPoolSessionBeads(
		beads.SessionStore{Store: store},
		nil,
		newSessionBeadSnapshot([]beads.Bead{bead}),
		nil, // undesired: agent/rig suspended, spec removed, or bead not canonical
		cfg,
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — an asleep named session carrying a claim is 'authoritatively stopped' by construction; the sweep must never admit it", closed)
	}
	assertBeadOpen(t, store, bead.ID, "asleep named session with stale claim")
}

// FINDING R2 (reviewer, on iteration 1). The implementer's report said: "Close
// reason pending_create_lease_expired is reopen-eligible, so the identity's
// session_key/continuity survives." That was true only of the collision path.
// The GAP-1 sweep path routes through GCSweepSessionBeads
// (pool_session_name.go:87), which hardcodes reason "gc_swept" — and
// session.ClosePatch writes state="gc_swept", which
// closedNamedSessionReopenEligible (named_config.go:680) explicitly REJECTS.
// So a named session recovered by the sweep lost its conversation continuity,
// while the same bead recovered by the collision path kept it.
//
// INVERTED (part a only). Was: the sweep closes the bead, its state is
// "gc_swept", and FindClosedNamedSessionBead therefore does NOT find it. Now:
// the sweep does not close it at all. The inversion is correct because the
// requirement the finding states — "any close of a NAMED bead must use a
// reopen-eligible reason" — is satisfied by there being no sweep close of a
// named bead to make. Part (b) is unchanged: it already asserted the correct
// behavior, and it still does (with the multi-tick confirmation window the
// collision path now requires).
func TestReview_SweptNamedSessionBeadIsNotReopenEligible(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)

	// (a) sweep path.
	sweepStore := beads.NewMemStore()
	sweptBead := squattingNamedSessionBead(t, sweepStore, sessionName, 15*time.Minute, map[string]string{
		"session_key": "sk-live-conversation",
	})
	if closed := sweepUndesiredPoolSessionBeads(
		beads.SessionStore{Store: sweepStore}, nil,
		newSessionBeadSnapshot([]beads.Bead{sweptBead}), nil, cfg, runtime.NewFake(), false,
	); closed != 0 {
		t.Fatalf("sweep closed = %d, want 0 — GCSweepSessionBeads hardcodes the non-reopen-eligible reason %q, so it must never reach a named bead", closed, "gc_swept")
	}
	assertBeadOpen(t, sweepStore, sweptBead.ID, "named bead offered to the pool sweep")

	// (b) collision path, same bead shape: this is the ONE path that may close
	// a named bead, and it must leave it reopen-eligible.
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, map[string]string{
		"session_key": "sk-live-conversation",
	})
	split := &cacheSplitStore{Store: mem, hiddenID: holder.ID}
	runSquatSyncTicks(t, namedNameReleaseConfirmTicks, split, cfg, sessionName, runtime.NewFake())
	assertBeadClosed(t, mem, holder.ID, "collision-path holder")
	if _, found, err := session.FindClosedNamedSessionBead(mem, squatTestIdentity); err != nil || !found {
		t.Fatalf("collision-path bead reopen-eligible = %v (err=%v), want true", found, err)
	}
}

// FINDING R3 (reviewer, on iteration 1). The report called the sweep's work
// guard a "live cross-store assigned-work guard". It was blind to work assigned
// under the session's CONFIGURED NAMED IDENTITY whenever the bead's
// configured_named_identity metadata was missing — precisely the half-created
// shape the carve-out existed to admit. GCSweepSessionBeads passes cfg=nil
// (pool_session_name.go:87), and sessionAssignmentIdentifiersForConfigInfo
// (session_beads.go:915-921) can only recover the identity from config when cfg
// is non-nil.
//
// INVERTED (second subtest only). Was: with the identity metadata missing the
// guard is blind and closed == 1. Now: closed == 0 in BOTH subtests, because
// the sweep no longer admits named beads under any metadata shape. The
// inversion is correct because the finding describes a fail-OPEN — a guard that
// silently stops guarding on a specific input — and the fix for a fail-open is
// never "accept the open case", it is to remove the path that reaches it. The
// first subtest is untouched and still passes; keeping both makes this a
// two-sided pin that the carve-out has not returned.
func TestReview_SweepWorkGuardMissesNamedIdentityAssignment(t *testing.T) {
	newCase := func(t *testing.T, identityMeta string) (beads.Store, string, int) {
		t.Helper()
		cfg := squatTestCity()
		sessionName := squatTestSessionName(t, cfg)
		store := beads.NewMemStore()
		bead := squattingNamedSessionBead(t, store, sessionName, 15*time.Minute, map[string]string{
			namedSessionIdentityMetadata: identityMeta,
		})
		// Work assigned the way the fleet actually assigns it to a named
		// session: by identity string, not by session-bead ID.
		work, err := store.Create(beads.Bead{Title: "in-flight work", Type: "task", Assignee: squatTestIdentity})
		if err != nil {
			t.Fatalf("Create(work): %v", err)
		}
		inProgress := "in_progress"
		if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("Update(work): %v", err)
		}
		closed := sweepUndesiredPoolSessionBeads(
			beads.SessionStore{Store: store}, nil,
			newSessionBeadSnapshot([]beads.Bead{bead}), nil, cfg, runtime.NewFake(), false,
		)
		return store, bead.ID, closed
	}

	t.Run("identity metadata present: guard holds", func(t *testing.T) {
		store, id, closed := newCase(t, squatTestIdentity)
		if closed != 0 {
			t.Fatalf("closed = %d, want 0 — identity-assigned work must block the close", closed)
		}
		assertBeadOpen(t, store, id, "identity-assigned work, identity metadata present")
	})

	t.Run("identity metadata missing: guard is blind", func(t *testing.T) {
		store, id, closed := newCase(t, "")
		if closed != 0 {
			t.Fatalf("closed = %d, want 0 — the sweep must not reach a named bead at all, so the guard's cfg=nil blind spot is unreachable", closed)
		}
		assertBeadOpen(t, store, id, "identity-assigned work, identity metadata missing")
	})
}

// FINDING R4 (reviewer, on iteration 1) — THE DECISIVE ONE. The collision
// recovery was gated on closeSessionInfoIfUnassigned, so it refused to release
// the name whenever open/in-progress work was assigned to the identity. For an
// ON_DEMAND named session — the incident's own mode — that is the normal state
// at the moment the collision happens:
//
//   - the holder is invisible to the controller's list tier (that is the whole
//     premise of the collision repair), so findCanonicalNamedSessionInfo finds
//     nothing and hasCanonical is false;
//   - with hasCanonical false, build_desired_state.go:969 materializes an
//     on_demand named session ONLY when namedWorkReady[identity] is true;
//   - namedWorkReady is set exclusively from work whose Assignee equals the
//     identity (build_desired_state.go:895-940; defaultNamedSessionDemand
//     documents "A work item targets a named session by Assignee=...");
//   - sessionAssignmentIdentifierRawInfo (session_beads.go:949) includes
//     configured_named_identity, so that same bead is exactly what the work
//     guard finds.
//
// So the only path that reaches the collision site for an on_demand named
// session implied the guard would block the release. The outage persisted
// across ticks, which this test asserted directly.
//
// INVERTED. Was: after 3 ticks the squatter is still open and still owns the
// name ("the outage is NOT repaired"). Now: the squatter is released and the
// name is free. The inversion is correct — and is NOT the implementer moving
// the goalposts — because the finding proves the guard is not evidence of life
// in this shape: an on_demand named session materializes BECAUSE its identity
// has assigned work, so "it owns work" is the trigger for the wake, not a
// reason to believe the dead half-create is running. Applying a work guard to a
// bead already proven dead (lease expired by 10m+, no runtime, confirmed across
// namedNameReleaseConfirmTicks consecutive ticks spanning
// namedNameReleaseConfirmWindow) makes the repair self-cancelling for exactly
// the case it exists to repair.
//
// The reviewer's 3-tick loop is preserved and is now load-bearing in a second
// way: three consecutive confirming ticks are the minimum the release requires,
// so the collision diagnostic must still appear on every one of them (the name
// stays held for the whole window, including the tick that releases it).
//
// Added beyond the inversion: the demand work must SURVIVE. Releasing the name
// by clearing the identity's assignments would trade a name squat for a silent
// work orphan, and the successor would never materialize.
func TestReview_OnDemandNamedSquatterIsNeverReleasedWhenIdentityHasDemandWork(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	// The demand bead: assigned to the named identity, which is the ONLY way
	// an on_demand named session enters the desired set without a canonical
	// bead — i.e. the reason this create is being attempted at all.
	demand, err := mem.Create(beads.Bead{Title: "demand", Type: "task", Assignee: squatTestIdentity})
	if err != nil {
		t.Fatalf("Create(demand work): %v", err)
	}

	for tick := 1; tick <= namedNameReleaseConfirmTicks; tick++ {
		out := runSquatSync(t, store, cfg, sessionName, runtime.NewFake())
		if !strings.Contains(out, "already belongs to "+holder.ID) {
			t.Fatalf("tick %d: stderr = %q, want the unchanged collision diagnostic", tick, out)
		}
	}
	assertBeadClosed(t, mem, holder.ID, "on-demand squatter with identity-assigned demand work")
	if owners := openSessionBeadsOwningName(t, mem, sessionName); len(owners) != 0 {
		t.Fatalf("owners = %v, want the name released — the outage must be repaired", owners)
	}
	// The demand that justified the wake must still be addressed to the
	// identity, or the successor session never materializes.
	got, err := mem.Get(demand.ID)
	if err != nil {
		t.Fatalf("Get(demand): %v", err)
	}
	if strings.TrimSpace(got.Assignee) != squatTestIdentity {
		t.Fatalf("demand assignee = %q after the release, want %q — the on_demand wake signal was destroyed", got.Assignee, squatTestIdentity)
	}
}
