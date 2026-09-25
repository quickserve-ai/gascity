package doctor

import (
	"fmt"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/orders"
)

// orderFiringGatedHintFmt is the fix hint for an order held by open work. It
// names the bead because the bead is the fix: closing or finishing it is what
// lets the order fire, and `gc order history` of the order says nothing
// about it (ga-puy7n0).
const orderFiringGatedHintFmt = "%s is gated on open work %s: inspect it with gc bd show %s"

// OrderFiringOpenWork is the open work bead holding an order's single-flight
// gate, as the dispatcher's open-work gate sees it.
type OrderFiringOpenWork struct {
	// ID is the gating bead's ID.
	ID string
	// CreatedAt is when the gating bead was created; the detail line prints
	// its age.
	CreatedAt time.Time
	// Holder names who holds the bead's claim, or "" when nothing does.
	Holder string
	// Note is the caller's verdict on the holder, printed after it. Empty when
	// there is nothing to add.
	Note string
}

// OrderFiringCurrentOpenWorkFunc returns the open work bead gating an order's
// dispatch, and false when nothing gates it. Implementations MUST be safe for
// concurrent use: the check resolves its non-OK orders in parallel.
type OrderFiringCurrentOpenWorkFunc func(order orders.Order) (OrderFiringOpenWork, bool, error)

// WithOrderFiringCurrentOpenWorkFunc lets callers name the open work bead that
// gates a stale or overdue order, so the detail line says why the order is
// not firing. Without it, one unclaimed wisp reads exactly like a scheduler
// that stopped: a blocking FAIL naming the order and not the bead (ga-puy7n0,
// red for 41h). The lookup runs only for orders already judged not OK, so a
// healthy city pays nothing for it (ga-9ymvnl).
func WithOrderFiringCurrentOpenWorkFunc(fn OrderFiringCurrentOpenWorkFunc) OrderFiringCurrentOption {
	return func(c *OrderFiringCurrentCheck) {
		c.openWork = fn
	}
}

// orderFiringNonOK is one order the check judged not OK, with the index of its
// detail line in the result.
type orderFiringNonOK struct {
	order  orders.Order
	detail int
}

// orderFiringOpenWorkResult is one open-work lookup outcome.
type orderFiringOpenWorkResult struct {
	work  OrderFiringOpenWork
	found bool
	err   error
}

// attributeOpenWork appends the gating bead to the detail line of each non-OK
// order that open work holds, and points the fix hint at the first such bead.
// An order stale for any other reason keeps its line unchanged. A failed
// lookup is reported on the line rather than dropped: the check still judged
// the order, and a silent miss would read as "not gated".
func (c *OrderFiringCurrentCheck) attributeOpenWork(result *CheckResult, nonOK []orderFiringNonOK, now time.Time) {
	if c.openWork == nil || len(nonOK) == 0 {
		return
	}
	found := c.lookupOpenWork(nonOK)
	hinted := false
	for i, n := range nonOK {
		r := found[i]
		if r.err != nil {
			result.Details[n.detail] += fmt.Sprintf(", open-work lookup failed: %v", r.err)
			continue
		}
		if !r.found {
			continue
		}
		result.Details[n.detail] += orderFiringGatedSuffix(r.work, now)
		if !hinted {
			result.FixHint = fmt.Sprintf(orderFiringGatedHintFmt, orderDisplayName(n.order), r.work.ID, r.work.ID)
			hinted = true
		}
	}
}

// orderFiringGatedSuffix renders ", gated on open work <id> (<age> old,
// <holder|unassigned>[; <note>])".
func orderFiringGatedSuffix(work OrderFiringOpenWork, now time.Time) string {
	holder := work.Holder
	if holder == "" {
		holder = "unassigned"
	}
	age := "age unknown"
	if !work.CreatedAt.IsZero() {
		age = formatOrderFiringDuration(nonNegativeDuration(now.Sub(work.CreatedAt))) + " old"
	}
	if work.Note != "" {
		return fmt.Sprintf(", gated on open work %s (%s, %s; %s)", work.ID, age, holder, work.Note)
	}
	return fmt.Sprintf(", gated on open work %s (%s, %s)", work.ID, age, holder)
}

// lookupOpenWork resolves the non-OK orders' open-work lookups in parallel,
// under the same cap as the order-run lookups and for the same reason: each
// is a store round trip, and serially they can blow the check budget.
func (c *OrderFiringCurrentCheck) lookupOpenWork(nonOK []orderFiringNonOK) []orderFiringOpenWorkResult {
	out := make([]orderFiringOpenWorkResult, len(nonOK))
	limit := orderFiringLastRunConcurrency
	if len(nonOK) < limit {
		limit = len(nonOK)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, limit)
	for i, n := range nonOK {
		wg.Add(1)
		go func(i int, order orders.Order) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			work, found, err := c.openWork(order)
			out[i] = orderFiringOpenWorkResult{work: work, found: found, err: err}
		}(i, n.order)
	}
	wg.Wait()
	return out
}
