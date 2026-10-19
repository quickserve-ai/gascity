package main

// A pool seat that acknowledges a drain while it still holds work is torn down,
// and its work released, in the same act (ga-x99xh0).
//
// Before this, finalizeDrainAckStoppedSession put such a seat to sleep as
// asleep/idle and left the work assigned to it. The assigned work was then wake
// demand for that same seat, so the next tick resumed it — on the provider it
// was just drained OFF (an account move) — and the seat sat awake, holding its
// bead, while counting as one of the pool's open slots. Two seats in that state
// filled a max-2 pool that served nothing for 10.5 awake hours on 2026-09-25/26.
//
// A pool seat is disposable (only its work is durable), so the drain ack is the
// point where the seat is retired and the work handed back to the pool: the
// assignee is cleared and in_progress work reopened, gc.routed_to is kept (the
// pool re-draws the bead with a fresh seat), and the seat's bead is closed so it
// stops occupying a slot. The release is ReleaseClaims' body (releaseListed)
// under its own policy row (releaseDrainAckTeardown: open and in_progress, the
// carried lane, the seat's route), and the session bead closes only when every
// release landed — a failed release keeps the seat open rather than leaving
// work assigned to a closed seat.
//
// Three shapes keep the old sleep-with-work behavior, and the controller log
// names which one (the seat is retained FOR that reason, never by default):
//
//   - not a pool seat: named and singleton identities keep their bead so a later
//     wake happens in place; the #2293 contract (the SDK only reports) stands
//     for them.
//   - a standing hold (sleep_intent): the hold is why the seat slept, and a
//     parked seat must not lose its work (gastownhall/gascity#5561).
//   - work parked on a certification wait (ga-mzovhi): the cert-landing-patrol
//     wakes the owner through the assignment, so the owner must keep it.

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// drainAckTeardownReleasePath names the drain-ack teardown in ReleaseWorkBead's
// audit line (releaseAuditPaths[releaseDrainAckTeardown]), so a release it
// performed is attributed to it.
const drainAckTeardownReleasePath = "drain-ack-teardown"

// drainAckTeardownCloseReason is the state code the torn-down seat closes with:
// the same terminal "drained" state a no-work drain ack closes a pool seat with.
const drainAckTeardownCloseReason = "drained"

// drainAckTeardownOutcome is what tearDownDrainAckedPoolSeat did. Exactly one of
// closed or retainedFor is set: closed when the work was released and the seat's
// bead closed, retainedFor naming why the seat was kept otherwise. released and
// attempted count the release sweep's work (attempted = released + failed), so a
// seat kept AFTER the sweep ran (release_failed, close_failed) reports how much
// of its work it no longer holds instead of implying it still holds all of it.
type drainAckTeardownOutcome struct {
	closed      bool
	retainedFor string
	released    int
	attempted   int
}

// The retained_for reasons for a drain-ack with assigned work that the teardown
// is not allowed to act on: the seat is not a disposable pool seat, it is a
// canonical singleton pool's stable identity, or the call site did not grant
// permission to close it; and the ones for a seat the teardown acted on but
// kept because it may still own work: drainAckObservedNotReleased's, and
// withheld_work_kept:<id> (a named agent's claim, proposed not released).
const (
	drainAckRetainedNotPoolSeat           = "not_pool_seat"
	drainAckRetainedSingletonIdentity     = "singleton_identity"
	drainAckRetainedCloseNotPermitted     = "close_not_permitted"
	drainAckRetainedReleaseFoundNothing   = "release_found_nothing"
	drainAckRetainedReleaseMissedObserved = "release_missed_observed"
	drainAckRetainedObservedWorkKept      = "observed_work_kept"
	drainAckRetainedObservedScanFailed    = "observed_scan_failed"
	drainAckRetainedWithheldWorkKept      = "withheld_work_kept"
)

// drainAckTeardownRefusal returns "" when a drain-acked seat is one the teardown
// may retire — a pool-managed, non-named seat whose agent does not use a
// canonical singleton identity — and otherwise the retained_for reason. It is
// checked BEFORE the work is classified, so a seat the teardown will never act
// on costs no extra store read.
//
// A canonical singleton pool (config.Agent.UsesCanonicalSingletonPoolIdentity:
// max_active_sessions = 1, no namepool) is pool-managed but deliberately keeps
// one stable configured identity, its bead and its conversation; it is not a
// disposable seat. A seat whose agent cannot be resolved from config is judged
// by its bead markers alone.
func drainAckTeardownRefusal(cfg *config.City, info sessionpkg.Info) string {
	if !isPoolManagedSessionInfo(info) || isNamedSessionInfo(info) {
		return drainAckRetainedNotPoolSeat
	}
	if agent := sessionAgentConfigInfo(cfg, info); agent != nil && agent.UsesCanonicalSingletonPoolIdentity() {
		return drainAckRetainedSingletonIdentity
	}
	return ""
}

// tearDownDrainAckedPoolSeat releases the work a drain-acked pool seat still
// holds and closes the seat's bead. The caller has already observed that the
// seat drain-acked with assigned work and that its runtime is gone. sw is the
// tick's read of every work leg; the release reads the seat live (fresh).
// observed is the ID of the bead the caller saw the seat holding, in sw's read;
// when it is set, the seat closes only if the sweep released every row of that
// bead sw holds for the seat, and is otherwise kept (drainAckObservedNotReleased
// names why) rather than closed over work it may still hold. A claim the sweep
// withheld keeps the seat too (drainAckWithheldClaim).
func tearDownDrainAckedPoolSeat(
	store beads.Store,
	sw *SeatWork,
	info sessionpkg.Info,
	observed string,
	now time.Time,
	stderr io.Writer,
) drainAckTeardownOutcome {
	if stderr == nil {
		stderr = io.Discard
	}
	if store == nil || info.ID == "" {
		return drainAckTeardownOutcome{retainedFor: "no_store"}
	}
	cfg := sw.Legs().cfg
	if refusal := drainAckTeardownRefusal(cfg, info); refusal != "" {
		return drainAckTeardownOutcome{retainedFor: refusal}
	}
	if standing := sessionpkg.StandingIntent(sessionpkg.DispositionOfInfo(info)); standing != "" {
		return drainAckTeardownOutcome{retainedFor: "standing_hold:" + string(standing)}
	}
	parkedID, scanErr := drainAckedSeatCertParkedBead(sw, info)
	if parkedID != "" {
		return drainAckTeardownOutcome{retainedFor: "cert_parked_work:" + parkedID}
	}
	if scanErr != nil {
		// The park scan could not see every row, so a parked bead may be among
		// the ones it missed. Releasing it would orphan the park; keep the seat
		// and let the next finalize pass (or the stranded repair) retry.
		fmt.Fprintf(stderr, "session reconciler: drain-ack teardown of %s: cert-park scan incomplete: %v\n", info.ID, scanErr) //nolint:errcheck
		return drainAckTeardownOutcome{retainedFor: "cert_park_scan_failed"}
	}
	if err := clearSessionCurrentClaim(store, info.ID); err != nil {
		fmt.Fprintf(stderr, "session beads: clearing current claim on drain-acked session %s: %v\n", info.ID, err) //nolint:errcheck
	}
	// The seat's own mol-do-work drain step is skipped (notOwnDrainStep): the
	// close gate and the classifier already treat it as not-work, and this
	// sweep must not do more to it than a no-work drain ack does. That path
	// leaves the step to closeBead's post-close release (closing-session-
	// release), which the closeBead below applies here too — so the step is
	// disposed of exactly as a no-work ack disposes of it, not reopened a
	// second time by this sweep.
	scope := releaseScope(info, cfg)
	// The observed bead's rows as sw holds them, read before the sweep: a sw
	// not yet loaded would otherwise load after the release, without them.
	observedRows, observedErr := drainAckObservedRows(sw, scope, observed)
	claims, readErr := sw.fresh(scope).snapshot(seatWorkQuery{scope: scope, statuses: releasePolicies[releaseDrainAckTeardown].statuses, keep: notOwnDrainStep}, false)
	out := ReleaseOutcome{}
	if cfg == nil && len(claims) > 0 {
		readErr = errors.Join(readErr, errReleaseNoConfig)
	} else {
		out = releaseListed(claims, releaseDrainAckTeardown, releaseSeatOfInfo(info), cfg, ReleaseOpts{Audit: stderr})
	}
	logRelease(stderr, "drain-acked", info.ID, out, readErr)
	// A kept claim (a named agent's, proposed not released: ga-9n8hjv; or one
	// no lane would serve) counts in the denominator, so "released N of M"
	// never reads as complete when the sweep kept work.
	released := len(out.Released)
	attempted := released + len(out.Kept) + len(out.Failed)
	if readErr != nil || len(out.Failed) > 0 {
		return drainAckTeardownOutcome{retainedFor: "release_failed", released: released, attempted: attempted}
	}
	// The caller observed a bead on this seat, but the sweep did not release
	// it: the two finders disagree, or the release kept it (a named agent's
	// claim, proposed not released). Closing would leave the observed bead
	// assigned to a closed seat while the log reads as a finished sweep
	// (pl-xx64, pl-1dkn). Keep the seat: the caller completes the ack as a
	// sleep, the shape this teardown replaced, so a bead still under the seat's
	// names stays owned by an open seat that its assignment can wake and the
	// stranded repair can see, never by a closed one.
	if retained := drainAckObservedNotReleased(observed, observedRows, observedErr, out, attempted); retained != "" {
		return drainAckTeardownOutcome{retainedFor: retained, released: released, attempted: attempted}
	}
	// A withheld claim stays assigned (the close's release honours the proposal
	// too), so closing would strand it: keep the seat until a judge settles it.
	if id := drainAckWithheldClaim(out); id != "" {
		return drainAckTeardownOutcome{retainedFor: drainAckRetainedWithheldWorkKept + ":" + id, released: released, attempted: attempted}
	}
	if !closeBead(store, sw, info, drainAckTeardownCloseReason, now, stderr) {
		return drainAckTeardownOutcome{retainedFor: "close_failed", released: released, attempted: attempted}
	}
	return drainAckTeardownOutcome{closed: true, released: released, attempted: attempted}
}

// drainAckObservedNotReleased returns "" when the sweep released every one of
// the observed bead's rows (or the caller observed none), else the
// retained_for reason: release_found_nothing (no release attempted),
// observed_scan_failed, observed_work_kept:<why>, or release_missed_observed.
// Rows match by store AND ID: independent city and rig stores can hold the
// same ID (storeScopedBeadKey), so one store's release says nothing of another.
func drainAckObservedNotReleased(observed string, rows []seatWorkHit, err error, out ReleaseOutcome, attempted int) string {
	if observed == "" {
		return ""
	}
	if attempted == 0 {
		return drainAckRetainedReleaseFoundNothing
	}
	if err != nil {
		return drainAckRetainedObservedScanFailed
	}
	if len(rows) == 0 {
		return drainAckRetainedReleaseMissedObserved
	}
	same := func(a, b seatWorkHit) bool { return a.store == b.store && a.bead.ID == b.bead.ID }
	for _, row := range rows {
		if slices.ContainsFunc(out.Released, func(c seatWorkHit) bool { return same(c, row) }) {
			continue
		}
		if i := slices.IndexFunc(out.Kept, func(c seatWorkHit) bool { return same(c, row) }); i >= 0 {
			return drainAckRetainedObservedWorkKept + ":" + out.keptReason(i)
		}
		return drainAckRetainedReleaseMissedObserved
	}
	return ""
}

// drainAckObservedRows returns the seat's rows of bead observed in sw, the read
// the caller observed it in: one per store holding the ID. None for "".
func drainAckObservedRows(sw *SeatWork, scope ReleaseScope, observed string) ([]seatWorkHit, error) {
	if observed == "" {
		return nil, nil
	}
	return sw.snapshot(seatWorkQuery{scope: scope, statuses: releasePolicies[releaseDrainAckTeardown].statuses, keep: func(s beads.Store, b beads.Bead) (bool, error) {
		if b.ID != observed {
			return false, nil
		}
		return notOwnDrainStep(s, b)
	}}, false)
}

// drainAckWithheldClaim returns the ID of a claim the sweep withheld (proposed
// for a judge, not released: ga-9n8hjv fence #1), or "".
func drainAckWithheldClaim(out ReleaseOutcome) string {
	for i, c := range out.Kept {
		if out.keptReason(i) == keptNamedProposed {
			return c.bead.ID
		}
	}
	return ""
}

// drainAckedSeatCertParkedBead returns the ID of an in_progress bead the seat
// holds that is parked on a certification wait, or "" when it holds none. The
// seat is read live on every leg (fresh), and each hit's labels are read
// through its store's LIVE handle, as the wake-side probe reads them
// (cert_park_wake.go): a CachingStore's Get can serve a cached row that
// predates the hold label, and releasing on that row would orphan a real park.
// A live row that is no longer in_progress under the same assignee has changed
// hands and is not this seat's park (the release is conditional on the
// assignee too). A non-nil error means the scan did not see every row.
func drainAckedSeatCertParkedBead(sw *SeatWork, info sessionpkg.Info) (string, error) {
	scope := releaseScope(info, sw.Legs().cfg)
	hits, scanErr := sw.fresh(scope).snapshot(seatWorkQuery{scope: scope, statuses: []string{"in_progress"}}, false)
	for _, hit := range hits {
		live := beads.HandlesFor(hit.store).Live
		if live == nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("reading labels of %s: no live handle", hit.bead.ID))
			continue
		}
		fresh, err := live.Get(hit.bead.ID)
		if err != nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("reading labels of %s: %w", hit.bead.ID, err))
			continue
		}
		if fresh.Status != "in_progress" || strings.TrimSpace(fresh.Assignee) != strings.TrimSpace(hit.bead.Assignee) {
			continue
		}
		if beadmeta.CertParkSuppressesAssignedWake(fresh.Labels) {
			return hit.bead.ID, nil
		}
	}
	return "", scanErr
}
