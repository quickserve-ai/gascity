package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// outcomeEventsFor returns the order.completed / order.failed events recorded
// for subject, in order.
func outcomeEventsFor(t *testing.T, ep *events.Fake, subject string) []events.Event {
	t.Helper()
	all, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var out []events.Event
	for _, e := range all {
		if e.Subject != subject {
			continue
		}
		if e.Type == events.OrderCompleted || e.Type == events.OrderFailed {
			out = append(out, e)
		}
	}
	return out
}

// A manual `gc order run` of a tracked exec order must record the same
// outcome event the controller's dispatcher records, so `gc doctor` (which
// reads only the event log) and `gc order history` (which reads the tracking
// bead) agree about the run (ga-fxuyqh (a)).
func TestOrderRunExecTrackedRecordsOrderCompletedEvent(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStore()
	ep := events.NewFake()
	a := orders.Order{Name: "tick", Trigger: "cooldown", Interval: "1m", Exec: "true"}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExecTracked(a, cityDir, nil, orders.NewStore(beads.OrdersStore{Store: store}), ep, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRunExecTracked = %d, want 0; stderr: %s", code, stderr.String())
	}
	got := outcomeEventsFor(t, ep, a.ScopedName())
	if len(got) != 1 || got[0].Type != events.OrderCompleted {
		t.Fatalf("outcome events = %+v, want exactly one %s", got, events.OrderCompleted)
	}
}

func TestOrderRunExecTrackedRecordsOrderFailedEvent(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStore()
	ep := events.NewFake()
	a := orders.Order{Name: "tick", Trigger: "cooldown", Interval: "1m", Exec: "exit 3"}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExecTracked(a, cityDir, nil, orders.NewStore(beads.OrdersStore{Store: store}), ep, nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doOrderRunExecTracked = 0, want failure; stdout=%q", stdout.String())
	}
	got := outcomeEventsFor(t, ep, a.ScopedName())
	if len(got) != 1 || got[0].Type != events.OrderFailed {
		t.Fatalf("outcome events = %+v, want exactly one %s", got, events.OrderFailed)
	}
	if !strings.Contains(got[0].Message, "manual gc order run") {
		t.Fatalf("order.failed message = %q, want it to say the run was manual", got[0].Message)
	}
}

// With no event log available (nil provider) the run still completes and
// records its tracking bead; the event is best-effort.
func TestOrderRunExecTrackedNilProviderStillRuns(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStore()
	a := orders.Order{Name: "tick", Trigger: "cooldown", Interval: "1m", Exec: "true"}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExecTracked(a, cityDir, nil, orders.NewStore(beads.OrdersStore{Store: store}), nil, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRunExecTracked = %d, want 0; stderr: %s", code, stderr.String())
	}
	if n := len(trackingBeads(t, store, "order-run:tick")); n != 1 {
		t.Fatalf("tracking bead count = %d, want 1", n)
	}
}
