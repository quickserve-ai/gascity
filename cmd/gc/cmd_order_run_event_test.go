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
	// The marker proves the command itself ran and failed: a run that never
	// reached the shell (env build failure, missing sh) prints no marker.
	a := orders.Order{Name: "tick", Trigger: "cooldown", Interval: "1m", Exec: "echo manual-run-marker; exit 3"}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExecTracked(a, cityDir, nil, orders.NewStore(beads.OrdersStore{Store: store}), ep, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderRunExecTracked = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "manual-run-marker") || !strings.Contains(stderr.String(), "exec failed") {
		t.Fatalf("stderr = %q, want the command's own output after an exec failure", stderr.String())
	}
	all := trackingBeads(t, store, "order-run:tick")
	if len(all) != 1 || !slicesContain(all[0].Labels, "exec-failed") {
		t.Fatalf("tracking beads = %+v, want one labeled exec-failed", all)
	}
	got := outcomeEventsFor(t, ep, a.ScopedName())
	if len(got) != 1 || got[0].Type != events.OrderFailed {
		t.Fatalf("outcome events = %+v, want exactly one %s", got, events.OrderFailed)
	}
	if !strings.Contains(got[0].Message, "exit code 1 (manual gc order run") {
		t.Fatalf("order.failed message = %q, want the excerpt stamped on the bead", got[0].Message)
	}
}

// Cross-family read on #355 (1): a cursor-label failure stamps the failed
// outcome on the bead BEFORE recording order.failed, as the dispatcher does,
// so history and the event log agree.
func TestOrderRunExecTrackedCursorLabelFailureStampsFailedThenRecords(t *testing.T) {
	cityDir := t.TempDir()
	mem := beads.NewMemStore()
	ep := events.NewFake()
	ep.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	a := orders.Order{Name: "evt", Trigger: "event", On: events.BeadClosed, Exec: "echo should-not-run"}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExecTracked(a, cityDir, nil, orders.NewStore(beads.OrdersStore{Store: eventCursorUpdateFailStore{Store: mem}}), ep, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderRunExecTracked = %d, want 1", code)
	}
	if strings.Contains(stdout.String(), "should-not-run") {
		t.Fatalf("the command ran after its cursor label failed: %q", stdout.String())
	}
	all := trackingBeads(t, mem, "exec-failed")
	if len(all) != 1 {
		t.Fatalf("beads labeled exec-failed = %d, want 1 (outcome stamped before the event)", len(all))
	}
	got := outcomeEventsFor(t, ep, a.ScopedName())
	if len(got) != 1 || got[0].Type != events.OrderFailed || !strings.Contains(got[0].Message, "seq=") {
		t.Fatalf("outcome events = %+v, want exactly one %s carrying seq=", got, events.OrderFailed)
	}
}

// Cross-family read on #355 (2): an outcome-label failure on an
// event-triggered run carries the triggering seq, as the dispatcher's does.
func TestOrderRunExecTrackedOutcomeLabelFailureCarriesSeq(t *testing.T) {
	cityDir := t.TempDir()
	mem := beads.NewMemStore()
	ep := events.NewFake()
	ep.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	// The command must SUCCEED, so the only failure is the rejected "exec"
	// outcome label: a plain exec failure would also yield code 1 and a seq=
	// event, and must not satisfy this test.
	a := orders.Order{Name: "evt", Trigger: "event", On: events.BeadClosed, Exec: "echo outcome-label-run-ok"}

	var stdout, stderr bytes.Buffer
	code := doOrderRunExecTracked(a, cityDir, nil, orders.NewStore(beads.OrdersStore{Store: execLabelUpdateFailStore{Store: mem}}), ep, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doOrderRunExecTracked = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "outcome-label-run-ok") || !strings.Contains(stdout.String(), "executed (exec)") {
		t.Fatalf("stdout = %q, want the command to have run and succeeded", stdout.String())
	}
	got := outcomeEventsFor(t, ep, a.ScopedName())
	if len(got) != 1 || got[0].Type != events.OrderFailed || !strings.HasPrefix(got[0].Message, "seq=1: ") ||
		!strings.Contains(got[0].Message, "label failed") || !strings.Contains(got[0].Message, "exec label failed") {
		t.Fatalf("outcome events = %+v, want exactly one %s starting seq=1: naming the injected outcome-label failure", got, events.OrderFailed)
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
