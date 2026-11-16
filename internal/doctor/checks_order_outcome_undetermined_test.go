package doctor

import (
	"errors"
	"path/filepath"
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
