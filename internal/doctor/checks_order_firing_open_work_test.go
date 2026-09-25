package doctor

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// openWorkRecorder is an injected open-work lookup that records every order
// it was asked about, so a test can assert who was looked up and who was not.
type openWorkRecorder struct {
	mu     sync.Mutex
	asked  []string
	answer func(order orders.Order) (OrderFiringOpenWork, bool, error)
}

func (r *openWorkRecorder) lookup(order orders.Order) (OrderFiringOpenWork, bool, error) {
	r.mu.Lock()
	r.asked = append(r.asked, order.ScopedName())
	r.mu.Unlock()
	if r.answer == nil {
		return OrderFiringOpenWork{}, false, nil
	}
	return r.answer(order)
}

func runOrderFiringCurrentWithOpenWork(t *testing.T, cfg *config.City, cityPath string, now time.Time, rec *openWorkRecorder) *CheckResult {
	t.Helper()
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentOpenWorkFunc(rec.lookup))
	check.clock = func() time.Time { return now }
	return check.Run(&CheckContext{CityPath: cityPath})
}

// TestOrderFiringCurrent_GatedStaleOrderNamesTheBead is ga-puy7n0 ask 2 in its
// field shape: an order stale for 41h behind one unclaimed wisp. The detail
// line names the wisp and its age, and the fix hint points at the bead rather
// than at the order history of whichever order happened to be listed first.
func TestOrderFiringCurrent_GatedStaleOrderNamesTheBead(t *testing.T) {
	now := time.Date(2026, 8, 15, 20, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stale-db", "cooldown", "4h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-72 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stale-db", Ts: now.Add(-41 * time.Hour)},
	)
	rec := &openWorkRecorder{answer: func(orders.Order) (OrderFiringOpenWork, bool, error) {
		return OrderFiringOpenWork{ID: "ga-wisp-kdjfbpa", CreatedAt: now.Add(-41 * time.Hour)}, true, nil
	}}

	result := runOrderFiringCurrentWithOpenWork(t, cfg, cityPath, now, rec)

	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; details = %v", result.Status, result.Details)
	}
	want := "mol-dog-stale-db: last fired 41h ago, expected every 4h (CRITICAL: stale), gated on open work ga-wisp-kdjfbpa (41h old, unassigned)"
	if len(result.Details) != 1 || result.Details[0] != want {
		t.Fatalf("details = %q, want [%q]", result.Details, want)
	}
	if !strings.Contains(result.FixHint, "gc bd show ga-wisp-kdjfbpa") || strings.Contains(result.FixHint, "gc order history") {
		t.Fatalf("fix hint = %q, want it to point at the gating bead", result.FixHint)
	}
}

// TestOrderFiringCurrent_GatedOverdueOrderNamesHolderAndNote covers the
// warning tier and the holder rendering: a claimed wisp shows its holder and
// the caller's note in place of "unassigned".
func TestOrderFiringCurrent_GatedOverdueOrderNamesHolderAndNote(t *testing.T) {
	now := time.Date(2026, 8, 15, 20, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "digest-generate", "cooldown", "4h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-72 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "digest-generate", Ts: now.Add(-7 * time.Hour)},
	)
	rec := &openWorkRecorder{answer: func(orders.Order) (OrderFiringOpenWork, bool, error) {
		return OrderFiringOpenWork{ID: "ga-wisp-ypurq90", CreatedAt: now.Add(-7 * time.Hour), Holder: "gastown__dog-1-pool", Note: "its session is live"}, true, nil
	}}

	result := runOrderFiringCurrentWithOpenWork(t, cfg, cityPath, now, rec)

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; details = %v", result.Status, result.Details)
	}
	want := ", gated on open work ga-wisp-ypurq90 (7h old, gastown__dog-1-pool; its session is live)"
	if len(result.Details) != 1 || !strings.HasSuffix(result.Details[0], want) {
		t.Fatalf("details = %q, want a line ending %q", result.Details, want)
	}
}

// TestOrderFiringCurrent_UngatedStaleOrderKeepsItsLine is the other half of
// ask 2's contract: an order stale for a reason other than open work (the
// scheduler never evaluated it) keeps the line and the hint it always had.
func TestOrderFiringCurrent_UngatedStaleOrderKeepsItsLine(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-6 * time.Hour)},
	)
	rec := &openWorkRecorder{}

	result := runOrderFiringCurrentWithOpenWork(t, cfg, cityPath, now, rec)
	plain := runOrderFiringCurrentTest(t, cfg, cityPath, now)

	if len(rec.asked) != 1 {
		t.Fatalf("lookup asked about %v, want the one stale order", rec.asked)
	}
	if strings.Join(result.Details, "\n") != strings.Join(plain.Details, "\n") {
		t.Fatalf("details = %q, want them unchanged from the check without a lookup: %q", result.Details, plain.Details)
	}
	if result.FixHint != plain.FixHint {
		t.Fatalf("fix hint = %q, want the unchanged %q", result.FixHint, plain.FixHint)
	}
}

// TestOrderFiringCurrent_OpenWorkLookupSkipsOKOrders pins the budget rule
// (ga-9ymvnl): the lookup is a store round trip, so it runs only for orders
// already judged not OK. One current order and one stale order must produce
// exactly one lookup, for the stale one.
func TestOrderFiringCurrent_OpenWorkLookupSkipsOKOrders(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "fresh-cooldown", "cooldown", "1h")
	writeOrderFiringTestOrder(t, cityPath, "stale-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "fresh-cooldown", Ts: now.Add(-10 * time.Minute)},
		events.Event{Type: events.OrderFired, Subject: "stale-cooldown", Ts: now.Add(-6 * time.Hour)},
	)
	rec := &openWorkRecorder{}

	runOrderFiringCurrentWithOpenWork(t, cfg, cityPath, now, rec)

	if len(rec.asked) != 1 || rec.asked[0] != "stale-cooldown" {
		t.Fatalf("lookup asked about %v, want exactly [stale-cooldown]", rec.asked)
	}
}

// TestOrderFiringCurrent_OpenWorkLookupNotCalledWhenAllCurrent is the same
// rule on a healthy city: nothing is looked up at all.
func TestOrderFiringCurrent_OpenWorkLookupNotCalledWhenAllCurrent(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "fresh-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "fresh-cooldown", Ts: now.Add(-10 * time.Minute)},
	)
	rec := &openWorkRecorder{}

	result := runOrderFiringCurrentWithOpenWork(t, cfg, cityPath, now, rec)

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK", result.Status)
	}
	if len(rec.asked) != 0 {
		t.Fatalf("lookup asked about %v, want no lookups on a healthy city", rec.asked)
	}
}

// TestOrderFiringCurrent_OpenWorkLookupFailureIsReported keeps a failed lookup
// visible: the line says the lookup failed rather than reading as "not gated".
func TestOrderFiringCurrent_OpenWorkLookupFailureIsReported(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "stale-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "stale-cooldown", Ts: now.Add(-6 * time.Hour)},
	)
	rec := &openWorkRecorder{answer: func(orders.Order) (OrderFiringOpenWork, bool, error) {
		return OrderFiringOpenWork{}, false, errors.New("store unreachable")
	}}

	result := runOrderFiringCurrentWithOpenWork(t, cfg, cityPath, now, rec)

	if len(result.Details) != 1 || !strings.HasSuffix(result.Details[0], ", open-work lookup failed: store unreachable") {
		t.Fatalf("details = %q, want the lookup failure on the stale order's line", result.Details)
	}
}
