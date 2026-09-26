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
// stops occupying a slot. The release reuses the stranded-repair primitive
// (unclaimWorkAssignedToSessionInfo), and the session bead closes only when
// every release landed — a failed release keeps the seat open rather than
// leaving work assigned to a closed seat.
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
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// drainAckTeardownReleasePath names the drain-ack teardown in ReleaseWorkBead's
// audit line, so a release it performed is attributed to it.
const drainAckTeardownReleasePath = "drain-ack-teardown"

// drainAckTeardownCloseReason is the state code the torn-down seat closes with:
// the same terminal "drained" state a no-work drain ack closes a pool seat with.
const drainAckTeardownCloseReason = "drained"

// drainAckTeardownOutcome is what tearDownDrainAckedPoolSeat did. Exactly one of
// closed or retainedFor is set: closed when the work was released and the seat's
// bead closed, retainedFor naming why the seat was kept otherwise.
type drainAckTeardownOutcome struct {
	closed      bool
	retainedFor string
}

// tearDownDrainAckedPoolSeat releases the work a drain-acked pool seat still
// holds and closes the seat's bead. The caller has already observed that the
// seat drain-acked with assigned work and that its runtime is gone.
func tearDownDrainAckedPoolSeat(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
	now time.Time,
	stderr io.Writer,
) drainAckTeardownOutcome {
	if stderr == nil {
		stderr = io.Discard
	}
	if store == nil || info.ID == "" {
		return drainAckTeardownOutcome{retainedFor: "no_store"}
	}
	if !isPoolManagedSessionInfo(info) || isNamedSessionInfo(info) {
		return drainAckTeardownOutcome{retainedFor: "not_pool_seat"}
	}
	if standing := sessionpkg.StandingSleepIntent(info.SleepIntent); standing != "" {
		return drainAckTeardownOutcome{retainedFor: "standing_hold:" + string(standing)}
	}
	parkedID, scanErr := drainAckedSeatCertParkedBead(cityPath, cfg, store, rigStores, info, stderr)
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
	res := unclaimWorkAssignedToSessionInfo(cityPath, cfg, store, rigStores, info, retiredSessionFallbackRouteInfo(info), drainAckTeardownReleasePath, stderr)
	if res.Failed > 0 {
		fmt.Fprintf(stderr, "session reconciler: drain-ack teardown of %s deferred: %d of %d release(s) failed; keeping the seat open\n", info.ID, res.Failed, res.Failed+res.Released) //nolint:errcheck
		return drainAckTeardownOutcome{retainedFor: "release_failed"}
	}
	if !closeBead(store, cfg, info.ID, drainAckTeardownCloseReason, now, stderr) {
		return drainAckTeardownOutcome{retainedFor: "close_failed"}
	}
	return drainAckTeardownOutcome{closed: true}
}

// drainAckedSeatCertParkedBead returns the ID of an in_progress bead the seat
// holds that is parked on a certification wait, or "" when it holds none. Labels
// are read live: cached work rows can carry empty labels (see cert_park_wake.go).
// A non-nil error means the scan did not see every row.
func drainAckedSeatCertParkedBead(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
	stderr io.Writer,
) (string, error) {
	identifiers := sessionAssignmentIdentifiersInfo(info)
	parkedID := ""
	var scanErr error
	complete := sweepAssignedWorkLegs(cityPath, cfg, store, rigStores, identifiers, stderr, func(_ int, owner beads.Store) {
		if parkedID != "" {
			return
		}
		wa := workAssignmentForStore(beads.WorkStore{Store: owner})
		for _, assignee := range identifiers {
			rows, err := wa.OpenAssignedTo(assignee, "in_progress", beads.TierBoth, true)
			if err != nil {
				scanErr = errors.Join(scanErr, fmt.Errorf("listing in_progress work assigned to %q: %w", assignee, err))
				continue
			}
			for _, row := range rows {
				if sessionpkg.IsSessionBeadOrRepairable(row) {
					continue
				}
				live, err := owner.Get(row.ID)
				if err != nil {
					scanErr = errors.Join(scanErr, fmt.Errorf("reading labels of %s: %w", row.ID, err))
					continue
				}
				if beadmeta.CertParkSuppressesAssignedWake(live.Labels) {
					parkedID = row.ID
					return
				}
			}
		}
	})
	if parkedID != "" {
		return parkedID, nil
	}
	if !complete {
		scanErr = errors.Join(scanErr, errors.New("assigned-work leg set could not be resolved"))
	}
	return "", scanErr
}
