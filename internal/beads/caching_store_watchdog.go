package beads

import (
	"context"
	"errors"
	"time"
)

// The reconcile watchdog: absence-of-heartbeat as a signal (ga-yc0chj).
//
// The reconcile loop already logs a success line per completed pass, and
// caching_store_reconcile.go documents a long-stale cache as a known hazard.
// But a missing log line alarms nobody: on 2026-08-13 the city rig's reconciler
// stopped for 3h31m while the qc rig ticked normally, and nothing noticed —
// because a partial cache stays SERVABLE (cacheServableLocked), so every
// non-Live read kept being answered, confidently, from a frozen snapshot. That
// is how a session bead the controller could not see took the refinery's
// session name (ga-2otk73) and how an order-tracking watchdog read zero rows
// for 43 hours (ga-v5vnyp).
//
// So something has to watch for the absence. This is that something.
//
// WHY A SEPARATE GOROUTINE, and not a check inside the reconcile loop. The loop
// cannot detect its own death. The measured failure mode is the loop wedged
// inside backing.List — a call that takes no context and therefore cannot be
// timed out or cancelled — so any check that lives on the loop's own goroutine
// is wedged with it. The watchdog keeps ticking because it shares nothing with
// the scan but the mutex, and it only takes that briefly.
//
// WHICH CLOCK. Not lastFreshAt: that is bumped by EVERY write (ten call sites
// in caching_store_writes.go, plus the events, conditional, reads and
// graph-apply paths), so event-bus traffic kept it current right through the
// 3h31m stall. stats.LastReconcileAt is the only field a completed reconcile
// writes, which makes it the only honest reconciler-liveness signal — and
// before this file nothing read it except the cadence calculation.

const (
	// cacheReconcileStaleFactor is how many adaptive intervals may pass with no
	// COMPLETED reconcile before the cache is overdue. Five rather than two
	// because a reconcile can legitimately run long under load and the cadence
	// itself already stretches to MEDIUM/LARGE under pressure; this alarm exists
	// for hours-long stalls, not for a slow pass.
	cacheReconcileStaleFactor = 5

	// cacheReconcileStaleFloor keeps the bound sane when the adaptive interval
	// is small: a 30s cadence would otherwise alarm after 2m30s, which is inside
	// the noise of a loaded box.
	cacheReconcileStaleFloor = 5 * time.Minute
)

// cacheReconcileWatchdogTick is how often the watchdog looks. A var so tests
// can drive it without sleeping through a real interval.
var cacheReconcileWatchdogTick = time.Minute

// cacheReconcileOverdueLogWindow rate-limits the overdue line. It must be
// comfortably LONGER than cacheReconcileWatchdogTick: the two being equal would
// mean a line every tick for the whole stall, and the cure for that noise is
// always to stop believing the alarm.
const cacheReconcileOverdueLogWindow = 15 * time.Minute

// errCacheReconcileOverdue is the message the watchdog logs. Constant on
// purpose so the elapsed time cannot turn one condition into a new line every
// tick; the elapsed time is recoverable from Stats() — LastReconcileAt, or
// ReconcilerArmedAt when no reconcile has ever completed.
var errCacheReconcileOverdue = errors.New(
	"no reconcile has completed within the staleness bound; the reconciler is stopped or wedged, " +
		"and every non-Live read is being served from a frozen snapshot")

// startReconcileWatchdog launches the absence-of-heartbeat watchdog. Cancelling
// ctx or calling StopReconciler stops it. Caller must have registered a
// lifecycleWG delta for it.
func (c *CachingStore) startReconcileWatchdog(ctx context.Context) {
	ticker := time.NewTicker(cacheReconcileWatchdogTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case now := <-ticker.C:
			c.checkReconcileOverdue(now)
		}
	}
}

// checkReconcileOverdue records the overdue condition on its own counters and
// its own log window.
//
// IT MUST NOT ROUTE THROUGH recordProblemLocked, tempting as that is. That
// helper stamps stats.LastProblemAt, and nextReconcileDelay uses LastProblemAt
// as the RETRY-BACKOFF ANCHOR: dueAt = LastProblemAt + backoff. A watchdog
// ticking every minute would therefore push the retry deadline out by a minute
// every minute, and a store already in backoff — which is exactly the store
// this watchdog fires on — would never retry again. The alarm would convert a
// recoverable stall into a permanent one. So the watchdog keeps its own
// counters and never touches the reconciler's clocks.
func (c *CachingStore) checkReconcileOverdue(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.reconcileOverdueLocked(now) {
		return
	}
	c.stats.ReconcileOverdueCount++
	c.stats.LastReconcileOverdueAt = now
	if c.problemf == nil {
		return
	}
	if !c.lastOverdueLogAt.IsZero() && now.Sub(c.lastOverdueLogAt) < cacheReconcileOverdueLogWindow {
		return
	}
	c.lastOverdueLogAt = now
	// No "beads cache:" prefix here — the production problemf already adds one
	// (NewCachingStore), and a doubled prefix in the supervisor log reads as a bug
	// in the alarm, which is the last thing an alarm can afford to look like.
	c.problemf("reconcile overdue: " + errCacheReconcileOverdue.Error())
}

// reconcileOverdueLocked reports whether a completed reconcile is overdue.
//
// The arming case matters as much as the running case: LastReconcileAt is zero
// until the FIRST reconcile lands, and treating zero as "fresh" would blind the
// watchdog to precisely the failure that motivated it — a reconciler that was
// armed and never ran a pass. So an armed-but-never-reconciled cache is measured
// from ReconcilerArmedAt instead. A cache with no reconciler armed at all is not
// overdue: it was never promised one (the suspended-rig opt-out builds exactly
// that), and alarming there would fire on every deliberately static store.
//
// Caller must hold c.mu.
func (c *CachingStore) reconcileOverdueLocked(now time.Time) bool {
	// The LATER of the two clocks, not "LastReconcileAt unless it is zero". A
	// store whose reconciler was stopped and started again has a fresh
	// ReconcilerArmedAt and a stale-but-nonzero LastReconcileAt; preferring the
	// reconcile clock there would report a healthy just-restarted store as
	// instantly overdue, which is a false alarm on the one event most likely to
	// follow an operator touching the thing.
	last := c.stats.LastReconcileAt
	if armed := c.stats.ReconcilerArmedAt; armed.After(last) {
		last = armed
	}
	if last.IsZero() {
		return false
	}
	return now.Sub(last) > c.reconcileStaleBoundLocked()
}

// reconcileStaleBoundLocked is how long a cache may go without a completed
// reconcile before the watchdog calls it overdue: a multiple of the CURRENT
// adaptive cadence, so a store that has legitimately slowed to MEDIUM or LARGE
// under load is judged against its own cadence rather than a fixed number, but
// never below the floor. Caller must hold c.mu.
func (c *CachingStore) reconcileStaleBoundLocked() time.Duration {
	bound := c.adaptiveIntervalLocked() * cacheReconcileStaleFactor
	if bound < cacheReconcileStaleFloor {
		bound = cacheReconcileStaleFloor
	}
	return bound
}
