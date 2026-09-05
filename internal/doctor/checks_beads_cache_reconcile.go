package doctor

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// beadsCacheStaleFactor is how many adaptive reconcile intervals a cache
	// may go without COMPLETING a reconcile before this check alarms. The
	// reconciler re-arms its timer every cycle, so missing five consecutive
	// cycles is not jitter — it means the loop is wedged, starved, or gone.
	beadsCacheStaleFactor = 5
	// beadsCacheStaleFloor is the minimum staleness window regardless of
	// cadence. At SMALL cadence (30 s) the factor alone would fire after
	// 2.5 minutes, which a single slow bd full scan under data-plane
	// pressure can produce legitimately. Five minutes is still two orders
	// of magnitude tighter than the 3h31m outage that motivated the check.
	beadsCacheStaleFloor = 5 * time.Minute
)

// BeadsCacheReconcileCheck alarms when a beads cache that is SUPPOSED to be
// reconciling has stopped completing reconciles.
//
// The gap it closes: the reconciler's heartbeat exists (the rate-limited
// "beads cache: reconciled rig=..." log line) but nothing watches for its
// absence, and a missing log line alarms nobody. That matters more than a
// stale cache normally would, because CachingStore.List answers
// IncludeClosed=false purely from cache without consulting the backing store,
// and the reconciler is the ONLY path by which a bead created through a
// different store instance or a different gc process enters the cache. A
// stalled reconciler therefore makes the controller structurally blind to
// externally created beads in that scope — the shape of ga-2otk73, where an
// agent was down 15 minutes with 62 silent failed re-creates because its
// session bead was invisible to the reconciler yet visible to the
// name-uniqueness check.
//
// Every other freshness signal reads healthy through that failure. In
// particular the X-GC-Cache-Age-S header is computed from CacheStats.LastFreshAt,
// which every local write bumps, so it stays near zero while the reconciler is
// dead. Only CacheStats.LastReconcileAt — published durably per scope by
// beads.ReconcileHeartbeat — distinguishes the two.
//
// Cost: one os.ReadFile of a sub-kilobyte JSON record per expected scope
// (city + non-suspended rigs, typically 2-3 files), plus one signal-0 liveness
// probe per record. No log tailing, no directory walk, no store open, no
// network, no subprocess. Worst case is bounded by the number of configured
// rigs, not by the size of any store, log, or worktree.
//
// It fails QUIET in every ambiguous case — no record, unreadable record,
// malformed record, record written by a process that is no longer alive,
// controller not running — because a watchdog that cries wolf gets ignored,
// and being ignored is the exact failure mode this check exists to fix.
type BeadsCacheReconcileCheck struct {
	cityPath string
	// scopes are the scope labels expected to be reconciling: "city" plus
	// each non-suspended rig. Suspended rigs are deliberately absent — the
	// controller does not arm their reconcilers, so their (possibly stale)
	// leftover record must never be read as a fault.
	scopes []string
	// controllerRunning gates the whole check. With no controller there is
	// no reconciler to be stalled, and any record on disk is a leftover.
	controllerRunning bool
	staleFactor       int
	staleFloor        time.Duration
	now               func() time.Time
	// readHeartbeat and processAlive are injectable seams for tests.
	readHeartbeat func(cityPath, scope string) (beads.ReconcileHeartbeat, bool, error)
	processAlive  func(pid int) bool
}

// NewBeadsCacheReconcileCheck builds the check for a city's expected
// reconciling scopes.
func NewBeadsCacheReconcileCheck(cityPath string, scopes []string, controllerRunning bool) *BeadsCacheReconcileCheck {
	return &BeadsCacheReconcileCheck{
		cityPath:          cityPath,
		scopes:            scopes,
		controllerRunning: controllerRunning,
		staleFactor:       beadsCacheStaleFactor,
		staleFloor:        beadsCacheStaleFloor,
		now:               time.Now,
		readHeartbeat:     beads.ReadReconcileHeartbeat,
		processAlive:      processAliveByPID,
	}
}

// Name returns the check identifier.
func (c *BeadsCacheReconcileCheck) Name() string { return "beads-cache-reconcile" }

// WarmupEligible returns false: a just-started city has no reconcile history
// yet, so this check has nothing to say during `gc start`'s warm-up scan.
func (c *BeadsCacheReconcileCheck) WarmupEligible() bool { return false }

// CanFix returns false. Restarting a wedged controller is an operator
// decision with live-session consequences, not a mechanical repair.
func (c *BeadsCacheReconcileCheck) CanFix() bool { return false }

// Fix is a no-op; the check is report-only.
func (c *BeadsCacheReconcileCheck) Fix(_ *CheckContext) error { return nil }

// Run evaluates each expected scope's durable reconcile heartbeat.
func (c *BeadsCacheReconcileCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}
	if !c.controllerRunning {
		r.Status = StatusOK
		r.Message = "controller not running — beads cache reconcile watch skipped"
		return r
	}

	now := c.now()
	var findings []string
	var details []string
	watched := 0
	// skipped counts expected scopes that produced no verdict. It is reported
	// alongside the healthy count because "2 caches reconciling" reads as a
	// clean bill of health even when a third expected scope was never looked
	// at — and an unwatched scope is precisely the state this check exists to
	// make visible. Legacy shared-file rigs publish no record by design and
	// land here permanently.
	skipped := 0
	anyNeverReconciled := false
	for _, scope := range c.scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		hb, ok, err := c.readHeartbeat(c.cityPath, scope)
		if err != nil || !ok {
			skipped++
			details = append(details, fmt.Sprintf("%s: no usable heartbeat record — not evaluated", scope))
			continue
		}
		if !c.processAlive(hb.PID) {
			skipped++
			details = append(details, fmt.Sprintf("%s: heartbeat was written by pid %d, which is gone — not evaluated", scope, hb.PID))
			continue
		}
		verdict := evaluateReconcileHeartbeat(scope, hb, now, c.staleFactor, c.staleFloor)
		if !verdict.evaluated {
			skipped++
			details = append(details, fmt.Sprintf("%s: %s — not evaluated", scope, verdict.detail))
			continue
		}
		watched++
		if verdict.stale {
			findings = append(findings, verdict.detail)
			anyNeverReconciled = anyNeverReconciled || verdict.neverReconciled
			continue
		}
		details = append(details, fmt.Sprintf("%s: %s", scope, verdict.detail))
	}

	if len(findings) == 0 {
		r.Status = StatusOK
		if watched == 0 {
			// The controller IS running and not one expected scope published a
			// record. That is not a clean bill of health, it is the watch being
			// INACTIVE — a gc binary older than this check, a city path that
			// does not match the controller's, or every store still priming.
			// Keep the status quiet (an ambiguous input must never gate), but
			// never let the tick read as "the reconcilers are fine".
			r.Message = "WATCH INACTIVE — controller is running but no beads cache published a reconcile heartbeat; " +
				"nothing was evaluated (this is not a healthy verdict)"
		} else {
			r.Message = fmt.Sprintf("%d beads cache(s) reconciling within %d x their adaptive interval", watched, c.staleFactor)
			if skipped > 0 {
				r.Message += fmt.Sprintf("; %d expected scope(s) NOT evaluated (see details)", skipped)
			}
		}
		r.Details = details
		return r
	}

	sort.Strings(findings)
	r.Status = StatusError
	r.Message = strings.Join(findings, "; ")
	r.Details = append(details,
		"A cache that stops reconciling goes silently blind: CachingStore.List answers",
		"IncludeClosed=false purely from cache, and the reconciler is the only path by which",
		"a bead created through another store instance or another gc process enters it.",
		"Cache-age signals do NOT catch this — LastFreshAt is bumped by every local write,",
		"so X-GC-Cache-Age-S stays near zero while the reconciler is dead.",
	)
	r.FixHint = reconcileStaleFixHint(anyNeverReconciled)
	return r
}

// reconcileStaleFixHint returns the operator next step for a stalled scope.
//
// The two shapes need OPPOSITE advice, and getting it wrong is expensive on a
// live fleet, so they are split:
//
//   - NEVER reconciled since arming is the first-reconcile starvation defect
//     (ga-yc0chj): nextReconcileDelay anchors the first full scan on lastFreshAt
//     while stats.LastReconcileAt is still zero, and ~30 write paths bump
//     lastFreshAt, so a store whose write traffic is denser than its cadence
//     never becomes due. Restarting re-arms straight back into the same window —
//     on the incident fleet the 17:33:04 restart was followed by 75 more minutes
//     of silence before the first scan landed. Recommending a restart here
//     spends live agent sessions to reproduce the fault.
//   - Went stale AFTER reconciling normally is a different fault: a wedged bd
//     full scan, or a backing-store outage holding the loop in its
//     sync-failure backoff (up to 10 minutes between attempts). Read the cache
//     state and LastProblem before touching the controller.
func reconcileStaleFixHint(neverReconciled bool) string {
	const common = "cross-check ~/.gc/supervisor.log: 'beads cache: stagger=' marks the arm, " +
		"'beads cache: reconciled rig=<prefix>' marks each completed scan (rate-limited to one per minute)"
	if neverReconciled {
		return common + ". The named scope has NEVER completed a scan since it armed: this is the " +
			"first-reconcile starvation defect (ga-yc0chj), where local write traffic keeps pushing " +
			"the first full scan out of reach. Do NOT restart the controller to clear it — a restart " +
			"re-arms into the same starvation window and costs every live session for nothing. It " +
			"self-clears on a write lull longer than the cadence, and is immune once one scan lands"
	}
	return common + ". The named scope reconciled normally and then stopped, so check the cache " +
		"state and last problem first (a degraded state means the backing store is failing and the " +
		"loop is in sync-failure backoff, which no restart fixes). A restart re-arms the reconciler " +
		"only when the loop itself is wedged with a healthy backing store"
}

// reconcileHeartbeatVerdict is the pure classification of ONE heartbeat record.
// evaluated=false means the record could not be judged (unknown), which callers
// must treat as quiet, never as a fault.
type reconcileHeartbeatVerdict struct {
	evaluated bool
	stale     bool
	// neverReconciled reports that the scope has not completed a single
	// reconcile since it armed, as opposed to having reconciled normally and
	// then stopped. The two need opposite operator advice — see
	// reconcileStaleFixHint.
	neverReconciled bool
	detail          string
}

// evaluateReconcileHeartbeat decides whether one scope's reconciler has missed
// its heartbeat. It is pure — no clock read, no I/O — so every branch is
// directly enumerable in tests.
//
// The staleness clock runs from the later of LastReconcileAt and ArmedAt: a
// store armed 3 seconds ago that has not completed its first cycle is healthy,
// while a store armed 3 hours ago that has never completed one is exactly the
// fault this check exists for.
func evaluateReconcileHeartbeat(scope string, hb beads.ReconcileHeartbeat, now time.Time, factor int, floor time.Duration) reconcileHeartbeatVerdict {
	if hb.ArmedAt.IsZero() {
		return reconcileHeartbeatVerdict{detail: "record has no arm timestamp"}
	}
	if hb.IntervalMs <= 0 {
		return reconcileHeartbeatVerdict{detail: "record has no reconcile interval"}
	}
	if factor <= 0 {
		factor = beadsCacheStaleFactor
	}
	interval := time.Duration(hb.IntervalMs) * time.Millisecond
	window := time.Duration(factor) * interval
	if window < floor {
		window = floor
	}

	last := hb.LastReconcileAt
	neverReconciled := last.IsZero()
	if neverReconciled || last.Before(hb.ArmedAt) {
		last = hb.ArmedAt
	}
	age := now.Sub(last)
	if age < 0 {
		// Clock skew, or a record stamped in the future. Unknown, not a fault.
		return reconcileHeartbeatVerdict{detail: "heartbeat is stamped in the future"}
	}
	if age <= window {
		return reconcileHeartbeatVerdict{
			evaluated: true,
			detail: fmt.Sprintf("last reconcile %s ago (window %s, interval %s)",
				age.Round(time.Second), window.Round(time.Second), interval),
		}
	}

	label := "has not completed a reconcile"
	if neverReconciled {
		label = "was armed but has NEVER completed a reconcile"
	}
	prefix := ""
	if p := strings.TrimSpace(hb.Prefix); p != "" {
		prefix = fmt.Sprintf(" (rig=%s)", p)
	}
	return reconcileHeartbeatVerdict{
		evaluated:       true,
		stale:           true,
		neverReconciled: neverReconciled,
		detail: fmt.Sprintf("beads cache %s%s %s in %s — over %s (%d x its %s adaptive interval); cache state=%s",
			scope, prefix, label, age.Round(time.Second), window.Round(time.Second), factor, interval, heartbeatState(hb)),
	}
}

func heartbeatState(hb beads.ReconcileHeartbeat) string {
	if s := strings.TrimSpace(hb.State); s != "" {
		return s
	}
	return "unknown"
}

// processAliveByPID reports whether pid names a live process. Signal 0 performs
// permission and existence checks without delivering a signal — no fork, no
// /proc walk, no subprocess.
func processAliveByPID(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
