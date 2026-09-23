package doctor

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

const (
	orderOutcomeHealthyName = "order-outcome-healthy"

	// orderOutcomeInspectHintFmt mirrors the sibling check's
	// orderFiringInspectHintFmt so the pair emits consistently-shaped hints.
	orderOutcomeInspectHintFmt = "Inspect with: gc order check && gc order history %s"

	// orderOutcomeFailureThreshold is the consecutive-failure count that flags an
	// order. Three, not two: two in a row is a plausible transient for anything
	// touching the network or a lock. Three, not five: on a 6h order five failures
	// is 30h before anything surfaces.
	orderOutcomeFailureThreshold = 3

	// orderOutcomeStartGrace covers gastownhall/gascity#3898 — for ~5 minutes
	// after a supervisor start, exec orders fail spuriously because dispatch
	// begins before pack staging completes. 2x margin on the observed window.
	orderOutcomeStartGrace = 10 * time.Minute

	// orderOutcomeLookback is how far back the check reads the event log. The
	// window is fixed. Sizing it per order from the schedule under-covers: for
	// cron the computed interval is the SMALLEST gap between fires, so a
	// weekday or business-hours schedule, a restart-graced failure, or a
	// cooldown order whose runs outlast their interval can leave fewer than
	// threshold outcomes inside a schedule-sized window and read OK.
	//
	// The bound is what lets the check finish under load (ga-4mu4k5). An
	// unbounded read gunzips and decodes every retained archive: 58s per pass
	// on a 173 MB active log plus 26 archives at load ~6, and the check made
	// three passes, so it was abandoned at a 5-minute budget at load ~30.
	// Eight days in one prefiltered walk measured 19-21s at load ~19.
	//
	// An order that fires too rarely for threshold runs to fit in the window,
	// and failed at every run the window holds, is reported as such (a
	// Warning) rather than as under threshold: the check cannot see the
	// streak it would need.
	orderOutcomeLookback = 8 * 24 * time.Hour
)

// nearControllerStart reports whether ts falls within grace after any controller
// start. Every start is checked, not just the newest: two restarts with no
// successful run between them would otherwise leave the older burst counted and
// manufacture a false positive.
func nearControllerStart(ts time.Time, starts []time.Time, grace time.Duration) bool {
	if grace <= 0 {
		return false
	}
	for _, start := range starts {
		if start.IsZero() || ts.Before(start) {
			continue
		}
		if ts.Sub(start) <= grace {
			return true
		}
	}
	return false
}

// consecutiveOrderFailures counts trailing order.failed events for one order.
//
// outcomes must hold order.completed and order.failed events ordered by Seq
// ascending; the walk runs newest-first and stops at the first success.
//
// A success always ends the streak, even if it falls within the post-start grace
// window. The grace window skips only spurious FAILURES (neither counting them nor
// allowing them to break the streak); a success is proof the order works.
//
// Failures inside the post-start grace window are SKIPPED, not reset. Resetting
// would let a frequently-restarting city zero a genuinely broken order's streak
// on every restart, which is the opposite of what this check is for.
//
// sawOutcome distinguishes "ran and succeeded" from "never produced an outcome";
// order-firing-current already owns the never-fired case.
//
// skipped counts trailing failures that were inside the post-start grace
// window and therefore excluded from streak. Callers need this to avoid
// reporting "last run succeeded" when every trailing run actually failed but
// was suppressed as a spurious post-restart burst — see classifyOrderOutcome.
func consecutiveOrderFailures(outcomes []events.Event, subject string, starts []time.Time, grace time.Duration) (streak int, lastMessage string, sawOutcome bool, skipped int) {
	lastMessageSet := false

	for i := len(outcomes) - 1; i >= 0; i-- {
		event := outcomes[i]
		if event.Subject != subject {
			continue
		}
		sawOutcome = true
		if event.Type != events.OrderFailed {
			break
		}
		if nearControllerStart(event.Ts, starts, grace) {
			skipped++
			continue
		}
		streak++
		if !lastMessageSet {
			lastMessage = event.Message
			lastMessageSet = true
		}
	}

	return streak, lastMessage, sawOutcome, skipped
}

// classifyOrderOutcome turns one order's failure streak into a doctor result.
//
// skipped is the grace-window-skipped-failure count from consecutiveOrderFailures.
// When streak == 0 but skipped > 0, every trailing run actually failed (just
// inside the post-start grace window); reporting "last run succeeded" would be
// a false statement in the diagnostic tool at exactly the moment an operator is
// most likely reading it — just after a restart.
func classifyOrderOutcome(order orders.Order, streak int, threshold int, lastMessage string, sawOutcome bool, skipped int) (CheckStatus, string) {
	name := orderDisplayName(order)

	if !sawOutcome {
		return StatusOK, fmt.Sprintf("%s: no completed runs in the lookback window", name)
	}
	if streak == 0 {
		if skipped > 0 {
			return StatusOK, fmt.Sprintf("%s: %d recent failure(s) within controller-start grace window", name, skipped)
		}
		return StatusOK, fmt.Sprintf("%s: last run succeeded", name)
	}
	if streak < threshold {
		return StatusOK, fmt.Sprintf("%s: %d consecutive failure(s), under threshold %d", name, streak, threshold)
	}

	detail := fmt.Sprintf("%s: %d consecutive failures", name, streak)
	if strings.TrimSpace(lastMessage) != "" {
		detail = fmt.Sprintf("%s, last %q", detail, lastMessage)
	}
	return StatusWarning, detail
}

// OrderOutcomeHealthyCheck reports scheduled orders failing repeatedly.
//
// Sibling to OrderFiringCurrentCheck, which answers "did it run?" while this
// answers "did it succeed?". An order that fires faithfully on schedule and fails
// every time leaves order-firing-current green, so the failure is invisible: the
// only trace is order.failed in the event log, which nobody reads unprompted.
type OrderOutcomeHealthyCheck struct {
	cfg       *config.City
	cityPath  string
	threshold int
	grace     time.Duration
	now       func() time.Time
}

// NewOrderOutcomeHealthyCheck creates the repeated-order-failure check.
func NewOrderOutcomeHealthyCheck(cfg *config.City, cityPath string) *OrderOutcomeHealthyCheck {
	return &OrderOutcomeHealthyCheck{
		cfg:       cfg,
		cityPath:  cityPath,
		threshold: orderOutcomeFailureThreshold,
		grace:     orderOutcomeStartGrace,
		now:       time.Now,
	}
}

// Name returns the check identifier shown by gc doctor.
func (c *OrderOutcomeHealthyCheck) Name() string { return orderOutcomeHealthyName }

// CanFix reports whether the check can repair a failing order. It cannot:
// remediation depends entirely on why the order fails.
func (c *OrderOutcomeHealthyCheck) CanFix() bool { return false }

// Fix is a no-op for the reason given on CanFix.
func (c *OrderOutcomeHealthyCheck) Fix(_ *CheckContext) error { return nil }

// Run counts each scheduled order's trailing consecutive failures.
//
// Unlike order-firing-current this needs no goroutine-plus-timeout guard: that
// check wraps its work because the order-history resolver opens the beads/Dolt
// store without accepting a context. This one reads only the event log, and
// only the last orderOutcomeLookback of it.
func (c *OrderOutcomeHealthyCheck) Run(ctx *CheckContext) *CheckResult {
	// This check is advisory on every path: a failing order must not gate gc doctor.
	// Blocking would fail gc doctor outright and gate every clean-doctor dependency
	// on transient order breakage, including during maintenance. SeverityAdvisory
	// is the only source of truth, set here at construction.
	result := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}
	if c.cfg == nil {
		result.Status = StatusOK
		result.Message = "no city config loaded"
		return result
	}

	cityPath := c.cityPath
	if cityPath == "" && ctx != nil {
		cityPath = ctx.CityPath
	}
	if cityPath == "" {
		result.Status = StatusError
		result.Message = "city path unavailable"
		return result
	}

	// Same helper order-firing-current uses, so the two checks can never
	// disagree about which orders are in scope.
	allOrders, err := scanOrderFiringCurrentOrders(cityPath, c.cfg)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("scan orders: %v", err)
		return result
	}

	suspendedRigs := orderFiringCurrentSuspendedRigs(c.cfg, cityPath)
	var scheduled []orders.Order
	for _, order := range allOrders {
		// Manual and event-triggered orders are out of scope by construction,
		// matching the sibling check. This is what keeps a manual order that was
		// abandoned mid-failure from alarming forever, with no recency heuristic
		// needed: it simply is not in the monitored set.
		if order.Trigger != "cron" && order.Trigger != "cooldown" {
			continue
		}
		if orderFiringCurrentOrderSuspended(suspendedRigs, order) {
			continue
		}
		scheduled = append(scheduled, order)
	}
	if len(scheduled) == 0 {
		result.Status = StatusOK
		result.Message = "no cron or cooldown orders"
		return result
	}

	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	since := now.Add(-orderOutcomeLookback)
	eventPath := filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl")
	outcomes, starts, err := readOrderOutcomeWindow(eventPath, since, c.grace)
	if err != nil {
		result.Status = StatusError
		result.Message = err.Error()
		return result
	}

	worst := StatusOK
	failing := 0
	var firstFailingHint string
	cronSpans := map[string]time.Duration{}

	for _, order := range scheduled {
		streak, lastMessage, sawOutcome, skipped := consecutiveOrderFailures(outcomes, order.ScopedName(), starts, c.grace)
		status, detail := classifyOrderOutcome(order, streak, c.threshold, lastMessage, sawOutcome, skipped)
		if status == StatusOK && streak > 0 && !orderSucceededIn(outcomes, order.ScopedName()) &&
			tooRareForLookback(order, c.threshold, cronSpans) {
			status, detail = StatusWarning, fmt.Sprintf(
				"%s: every run in the last %s failed (%d), and it fires too rarely for %d runs to fit; older runs not read",
				orderDisplayName(order), formatOrderFiringDuration(orderOutcomeLookback), streak, c.threshold)
			if strings.TrimSpace(lastMessage) != "" {
				detail = fmt.Sprintf("%s, last %q", detail, lastMessage)
			}
		}
		worst = worseStatus(worst, status)
		result.Details = append(result.Details, detail)
		if status != StatusOK {
			failing++
			if firstFailingHint == "" {
				// gc order history takes a bare name positionally and filters
				// on a.Name; the scoped form matches zero orders on the
				// local-iterator path used when the supervisor API is
				// unavailable — exactly when someone is debugging a broken
				// city. orderHistoryHintTarget yields the --rig form instead.
				firstFailingHint = orderHistoryHintTarget(order)
			}
		}
	}

	result.Status = worst
	if worst == StatusOK {
		result.Message = "all scheduled orders succeeding"
	} else {
		result.Message = fmt.Sprintf("%d order(s) failing repeatedly", failing)
		result.FixHint = fmt.Sprintf(orderOutcomeInspectHintFmt, firstFailingHint)
	}
	return result
}

// orderSucceededIn reports whether outcomes hold a success for subject.
func orderSucceededIn(outcomes []events.Event, subject string) bool {
	for _, e := range outcomes {
		if e.Subject == subject && e.Type == events.OrderCompleted {
			return true
		}
	}
	return false
}

// tooRareForLookback reports whether threshold consecutive runs of order can
// span more than orderOutcomeLookback, so a streak that long may not be seen.
// For cron that is the LONGEST span of threshold consecutive fires, not the
// smallest gap between two: `0 0 1-3 * *` fires a day apart, but its runs on
// the 2nd, 3rd and next month's 1st span four weeks. A schedule that cannot be
// evaluated counts as too rare: the check cannot vouch for it.
func tooRareForLookback(order orders.Order, threshold int, spanCache map[string]time.Duration) bool {
	switch order.Trigger {
	case "cooldown":
		interval, err := time.ParseDuration(order.Interval)
		if err != nil || interval <= 0 {
			return true
		}
		return time.Duration(threshold)*interval > orderOutcomeLookback
	case "cron":
		span, ok := spanCache[order.Schedule]
		if !ok {
			var err error
			span, err = cronLongestRunSpan(order.Schedule, threshold)
			if err != nil {
				return true
			}
			spanCache[order.Schedule] = span
		}
		return span > orderOutcomeLookback
	default:
		return true
	}
}

// cronLongestRunSpan is the longest time spanned by runs consecutive fires of
// schedule over a leap year plus the lookback (so a span crossing the year
// boundary is seen), scanned minute by minute in UTC. It stops as soon as a
// span exceeds orderOutcomeLookback, and a schedule with fewer than runs fires
// in that horizon returns the whole horizon. Only an order already failing at
// every visible run reaches this, so the scan is rare.
func cronLongestRunSpan(schedule string, runs int) (time.Duration, error) {
	fields := strings.Fields(schedule)
	if len(fields) != orders.CronFieldCount {
		return 0, fmt.Errorf("invalid cron schedule: want %d fields, got %d", orders.CronFieldCount, len(fields))
	}
	if runs < 1 {
		runs = 1
	}
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	horizon := 366*24*time.Hour + orderOutcomeLookback
	var fires []time.Time
	var longest time.Duration
	for m := time.Duration(0); m < horizon; m += time.Minute {
		ts := base.Add(m)
		matched, err := orders.CronScheduleMatchesAt(fields, ts)
		if err != nil {
			return 0, fmt.Errorf("invalid cron schedule: %w", err)
		}
		if !matched {
			continue
		}
		fires = append(fires, ts)
		if len(fires) >= runs {
			if span := ts.Sub(fires[len(fires)-runs]); span > longest {
				longest = span
				if longest > orderOutcomeLookback {
					return longest, nil
				}
			}
		}
	}
	if len(fires) < runs {
		return horizon, nil
	}
	return longest, nil
}

// readOrderOutcomeWindow returns order.completed and order.failed at or after
// since, in Seq order, and the controller start times that can grace them, in
// ONE walk of the log from grace before since: a start up to grace before the
// window can still cover a failure just inside it. Outcomes before since are
// dropped, so every outcome read also has its graces read. One walk matters because the
// cost of a walk is gunzipping each archive in the window; a walk per type
// paid it three times (ga-4mu4k5). since also lets the reader skip every
// archive rotated before the window.
func readOrderOutcomeWindow(eventPath string, since time.Time, grace time.Duration) ([]events.Event, []time.Time, error) {
	evts, err := events.ReadFilteredTypes(eventPath, events.Filter{Since: since.Add(-grace)},
		events.OrderCompleted, events.OrderFailed, events.ControllerStarted)
	if err != nil {
		return nil, nil, fmt.Errorf("read order outcome and controller start events: %w", err)
	}
	var outcomes []events.Event
	var starts []time.Time
	for _, e := range evts {
		if e.Type == events.ControllerStarted {
			starts = append(starts, e.Ts)
			continue
		}
		if e.Ts.Before(since) {
			continue
		}
		outcomes = append(outcomes, e)
	}
	// Seq, not Ts: the log is append-only and seq-ordered, and two events in the
	// same second would otherwise sort arbitrarily.
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].Seq < outcomes[j].Seq })
	return outcomes, starts, nil
}
