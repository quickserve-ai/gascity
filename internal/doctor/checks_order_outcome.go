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
	// The window alone cannot settle an order whose every outcome in it
	// failed, fewer than threshold times: how often an order ran is not
	// bounded by its schedule (a controller that was down, or a run holding
	// the single-flight gate), so its streak can reach back past any fixed
	// window. Those orders, and only those, are looked back past the window
	// for (lookBack).
	orderOutcomeLookback = 8 * 24 * time.Hour

	// orderOutcomeLookBackBudget bounds the look-back past the window, cut to
	// two thirds of the time left when the runner will abandon the check
	// sooner (sizingBudget). An order the look-back cannot settle in time is
	// reported as undetermined, a Warning, never as OK.
	orderOutcomeLookBackBudget = 20 * time.Second
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
	// walkBack reads the log newest-first for the look-back past the window;
	// a seam so tests can observe whether the look-back ran.
	walkBack func(path string, filter events.Filter, types []string, fn func([]events.Event, time.Time) bool) error
}

// NewOrderOutcomeHealthyCheck creates the repeated-order-failure check.
func NewOrderOutcomeHealthyCheck(cfg *config.City, cityPath string) *OrderOutcomeHealthyCheck {
	return &OrderOutcomeHealthyCheck{
		cfg:       cfg,
		cityPath:  cityPath,
		threshold: orderOutcomeFailureThreshold,
		grace:     orderOutcomeStartGrace,
		now:       time.Now,
		walkBack:  events.WalkFilteredTypesNewestFirst,
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
// store without accepting a context. This one reads only the event log: the
// last orderOutcomeLookback of it, and further back only for the orders that
// window cannot settle, within orderOutcomeLookBackBudget.
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

	outcomes, starts, unresolved, lookErr := c.lookBack(ctx, eventPath, scheduled, outcomes, starts)

	worst := StatusOK
	failing := 0
	var firstFailingHint string

	for _, order := range scheduled {
		streak, lastMessage, sawOutcome, skipped := consecutiveOrderFailures(outcomes, order.ScopedName(), starts, c.grace)
		status, detail := classifyOrderOutcome(order, streak, c.threshold, lastMessage, sawOutcome, skipped)
		if status == StatusOK && unresolved[order.ScopedName()] {
			why := "were not read within the time budget"
			if lookErr != nil {
				why = fmt.Sprintf("could not be read (%v)", lookErr)
			}
			status, detail = StatusWarning, fmt.Sprintf(
				"%s: every run read failed (%d counted); older runs %s, so the streak may reach threshold %d",
				orderDisplayName(order), streak, why, c.threshold)
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

// lookBack extends outcomes and starts past the window for the orders the
// window cannot settle, and returns the subjects it could not settle either.
//
// Only an order whose every outcome in the window failed, fewer than threshold
// times, needs it (Codex r3 on #136: a daily order that ran only weekly failed
// at -14d, -7d and now, and the window saw two). Older outcomes cannot change
// any other verdict: a success in the window ends the streak, a streak at
// threshold already warns, and an order with no outcome in the window is
// reported as having none there, not as succeeding (whether it should have run
// is order-firing-current's question). When no order needs the look-back,
// nothing more is read, so the common case costs the window read alone.
//
// The walk goes newest-first, one log source at a time, and stops as soon as
// every such order is settled (settledOrder), the log is exhausted, or the
// budget is spent. Sizing the read from the schedule instead cannot work: a
// schedule bounds how often an order may run, not how often it did.
func (c *OrderOutcomeHealthyCheck) lookBack(ctx *CheckContext, eventPath string, scheduled []orders.Order,
	outcomes []events.Event, starts []time.Time,
) ([]events.Event, []time.Time, map[string]bool, error) {
	unresolved := map[string]bool{}
	for _, order := range scheduled {
		subject := order.ScopedName()
		streak, _, sawOutcome, _ := consecutiveOrderFailures(outcomes, subject, starts, c.grace)
		if sawOutcome && streak < c.threshold && !orderSucceededIn(outcomes, subject) {
			unresolved[subject] = true
		}
	}
	if len(unresolved) == 0 {
		return outcomes, starts, nil, nil
	}
	budget := sizingBudget(ctx, orderOutcomeLookBackBudget)
	if budget <= 0 {
		return outcomes, starts, unresolved, nil
	}
	stopAt := time.Now().Add(budget)

	pending := make(map[string]bool, len(unresolved))
	for subject := range unresolved {
		pending[subject] = true
	}
	// outcomes is seq-ordered and, with an unresolved order, non-empty: every
	// outcome before the window has a lower seq than its first.
	filter := events.Filter{BeforeSeq: outcomes[0].Seq}
	var older []events.Event
	err := c.walkBack(eventPath, filter, []string{events.OrderCompleted, events.OrderFailed, events.ControllerStarted},
		func(evts []events.Event, through time.Time) bool {
			var batch []events.Event
			for _, e := range evts {
				if e.Type == events.ControllerStarted {
					starts = append(starts, e.Ts)
				} else if pending[e.Subject] {
					batch = append(batch, e)
				}
			}
			older = append(batch, older...)
			merged := make([]events.Event, 0, len(older)+len(outcomes))
			merged = append(merged, older...)
			merged = append(merged, outcomes...)
			for subject := range unresolved {
				if c.settledOrder(merged, subject, starts, through) {
					delete(unresolved, subject)
				}
			}
			return len(unresolved) > 0 && time.Now().Before(stopAt)
		})
	return append(older, outcomes...), starts, unresolved, err
}

// settledOrder reports whether outcomes older than those read can no longer
// change subject's verdict. through is from the newest-first walk: every
// outcome and start at or after it has been read, and zero means all of them.
//
// A failure at or after through+grace is counted whatever older starts hold,
// since a start that could grace it would already have been read; so threshold
// such failures in a row settle the order as failing. A success read settles
// it as under threshold when fewer than threshold failures follow it, since a
// start not yet read can only grace, and so remove, one of them.
func (c *OrderOutcomeHealthyCheck) settledOrder(outcomes []events.Event, subject string, starts []time.Time, through time.Time) bool {
	if through.IsZero() {
		return true
	}
	streak, _, _, _ := consecutiveOrderFailures(outcomes, subject, starts, c.grace)
	if streak < c.threshold && orderSucceededIn(outcomes, subject) {
		return true
	}
	confirmedFrom := through.Add(c.grace)
	var confirmed []events.Event
	for _, e := range outcomes {
		if e.Subject == subject && !e.Ts.Before(confirmedFrom) {
			confirmed = append(confirmed, e)
		}
	}
	streak, _, _, _ = consecutiveOrderFailures(confirmed, subject, starts, c.grace)
	return streak >= c.threshold
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
