package doctor

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// ga-fxuyqh (b): an order the look-back could not settle in its time budget
// is undetermined, not "failing repeatedly". The summary must count it
// separately and name it, so the label never claims more than the predicate
// computed.
func TestOrderOutcomeHealthy_BudgetCutOrderIsUndeterminedNotFailing(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), "daily", "cron", "0 0 * * *")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFailed, Ts: now, Subject: "daily", Message: "boom"},
	)
	result := outcomeCheckAt(cityPath, cfg, now).Run(&CheckContext{CityPath: cityPath, Deadline: time.Now().Add(-time.Second)})
	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want Warning", result.Status)
	}
	want := "1 order(s) undetermined, older runs not read: daily"
	if result.Message != want {
		t.Fatalf("message = %q, want %q", result.Message, want)
	}
}

func TestOrderOutcomeHealthy_FailedLookBackIsUndeterminedNotFailing(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), "daily", "cron", "0 0 * * *")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFailed, Ts: now, Subject: "daily", Message: "boom"},
	)
	check := outcomeCheckAt(cityPath, cfg, now)
	check.walkBack = func(string, events.Filter, []string, func([]events.Event, time.Time) bool) error {
		return errors.New("disk on fire")
	}
	result := check.Run(&CheckContext{CityPath: cityPath})
	want := "1 order(s) undetermined, older runs not read: daily"
	if result.Status != StatusWarning || result.Message != want {
		t.Fatalf("got %v %q, want Warning %q", result.Status, result.Message, want)
	}
}

// A real failing streak and a budget-cut order in one sweep: each is counted
// under its own label.
func TestOrderOutcomeHealthy_FailingAndUndeterminedCountedSeparately(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	ordersDir := filepath.Join(cityPath, "orders")
	writeOrderFiringTestOrderInDir(t, ordersDir, "daily", "cron", "0 0 * * *")
	writeOrderFiringTestOrderInDir(t, ordersDir, "hourly", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFailed, Ts: now.Add(-3 * time.Hour), Subject: "hourly", Message: "boom"},
		events.Event{Type: events.OrderFailed, Ts: now.Add(-2 * time.Hour), Subject: "hourly", Message: "boom"},
		events.Event{Type: events.OrderFailed, Ts: now.Add(-1 * time.Hour), Subject: "hourly", Message: "boom"},
		events.Event{Type: events.OrderFailed, Ts: now, Subject: "daily", Message: "boom"},
	)
	result := outcomeCheckAt(cityPath, cfg, now).Run(&CheckContext{CityPath: cityPath, Deadline: time.Now().Add(-time.Second)})
	want := "1 order(s) failing repeatedly; 1 undetermined, older runs not read: daily"
	if result.Status != StatusWarning || result.Message != want {
		t.Fatalf("got %v %q, want Warning %q; details %v", result.Status, result.Message, want, result.Details)
	}
}

// Cross-family read on #354: the look-back can leave an order unresolved while
// the outcomes it did read add up to a provisional streak at threshold, because
// an older controller start it never reached could still grace the oldest
// failure. Threshold 3, grace 10m: daily failed at now-7d and now; the walk
// reads one more failure at T+2m and stops (budget) before the start at T.
// That streak is not confirmed, so the order is undetermined, not failing.
func TestOrderOutcomeHealthy_ProvisionalStreakFromACutWalkIsUndetermined(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), "daily", "cron", "0 0 * * *")
	writeOutcomeLog(t, cityPath,
		events.Event{Seq: 3, Type: events.OrderFailed, Ts: now.Add(-7 * 24 * time.Hour), Subject: "daily", Message: "boom"},
		events.Event{Seq: 4, Type: events.OrderFailed, Ts: now, Subject: "daily", Message: "boom"},
	)
	boundary := now.Add(-12*24*time.Hour + 2*time.Minute)
	check := outcomeCheckAt(cityPath, cfg, now)
	check.walkBack = func(_ string, _ events.Filter, _ []string, fn func([]events.Event, time.Time) bool) error {
		// One source read, then the budget stops the walk.
		fn([]events.Event{{Seq: 2, Type: events.OrderFailed, Ts: boundary, Subject: "daily", Message: "spurious"}}, boundary)
		return nil
	}
	result := check.Run(&CheckContext{CityPath: cityPath})
	want := "1 order(s) undetermined, older runs not read: daily"
	if result.Status != StatusWarning || result.Message != want {
		t.Fatalf("got %v %q, want Warning %q; details %v", result.Status, result.Message, want, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "not confirmed: older runs were not read within the time budget") {
		t.Fatalf("detail must say the streak is unconfirmed: %v", result.Details)
	}
}
