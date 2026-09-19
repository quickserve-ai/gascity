package runtime

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// voidRecorder implements only events.Recorder — the best-effort, cannot-confirm
// shape (FileRecorder under a lock timeout, Discard, exec scripts).
type voidRecorder struct{ got []events.Event }

func (v *voidRecorder) Record(e events.Event) { v.got = append(v.got, e) }

// ackRecorder implements events.AckRecorder and can be told to drop.
type ackRecorder struct {
	got  []events.Event
	fail error
}

func (a *ackRecorder) Record(e events.Event) { a.got = append(a.got, e) }
func (a *ackRecorder) RecordAck(e events.Event) error {
	if a.fail != nil {
		return a.fail
	}
	a.got = append(a.got, e)
	return nil
}

func TestEventSinkWritesEveryRatioFieldWithoutAJoin(t *testing.T) {
	rec := &ackRecorder{}
	s := NewEventTerminationSink(rec, "controller")
	req := time.Unix(1700000000, 0).UTC()
	at := req.Add(9 * time.Minute)
	err := s.RecordTermination("qcore/worker-3", Termination{
		Kind: KindDrainTimeout, Actor: "controller", Reason: "config-drift",
		At: at, RequestedAt: req, SessionID: "ga-abc123",
	})
	if err != nil {
		t.Fatalf("RecordTermination: %v", err)
	}
	if len(rec.got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(rec.got))
	}
	ev := rec.got[0]
	if ev.Type != TerminationEventType {
		t.Errorf("type = %q, want %q", ev.Type, TerminationEventType)
	}
	if ev.Subject != "qcore/worker-3" || ev.SessionID != "ga-abc123" {
		t.Errorf("subject/session = %q/%q, want qcore/worker-3/ga-abc123", ev.Subject, ev.SessionID)
	}
	var p terminationPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	// Every field the ratio needs must be on the row: a reader that has to join
	// back to the bead to bucket a row cannot compute the ratio from the stream
	// alone, which is the stream's only job.
	if p.Kind != string(KindDrainTimeout) || p.Reason != "config-drift" ||
		p.SessionName != "qcore/worker-3" || p.At == "" || p.RequestedAt == "" {
		t.Errorf("payload is missing ratio fields: %+v", p)
	}
	if p.CountsNumerator {
		t.Error("drain-timeout must not count in the numerator")
	}
	if !p.CountsDenominator {
		t.Error("drain-timeout must count in the denominator")
	}
}

// TestEventSinkReportsADroppedEvent — the reconciliation rule says a missing
// bead record is filled by the event. That rests on knowing the event landed.
func TestEventSinkReportsADroppedEvent(t *testing.T) {
	boom := errors.New("ENOSPC")
	s := NewEventTerminationSink(&ackRecorder{fail: boom}, "controller")
	err := s.RecordTermination("seat", Termination{Kind: KindHandoff})
	if err == nil {
		t.Fatal("a dropped event must be REPORTED — otherwise the reconciliation rule rests on a record that may not exist")
	}
	if !errors.Is(err, boom) {
		t.Errorf("the underlying cause was lost: %v", err)
	}
	if IsUnacknowledgedEvent(err) {
		t.Error("a real drop must not be classified as merely unacknowledged")
	}
}

// TestEventSinkDoesNotClaimDurabilityItCannotObserve — a plain Recorder is
// void. Returning nil would assert a durable record that may not exist.
func TestEventSinkDoesNotClaimDurabilityItCannotObserve(t *testing.T) {
	v := &voidRecorder{}
	s := NewEventTerminationSink(v, "controller")
	err := s.RecordTermination("seat", Termination{Kind: KindHandoff})
	if err == nil {
		t.Fatal("emitting through a void Recorder must not report success: Record is best-effort and silently drops")
	}
	if !IsUnacknowledgedEvent(err) {
		t.Errorf("want the unacknowledged sentinel, got %v", err)
	}
	if len(v.got) != 1 {
		t.Error("the event should still have been attempted")
	}
}

// TestEventSinkNilIsSafe — a caller that has not wired events must still stop.
func TestEventSinkNilIsSafe(t *testing.T) {
	if s := NewEventTerminationSink(nil, ""); s != nil {
		t.Fatal("a nil recorder must yield a nil sink")
	}
	var s *EventTerminationSink
	if err := s.RecordTermination("seat", Termination{Kind: KindHandoff}); err != nil {
		t.Errorf("a nil sink must be a silent no-op, got %v", err)
	}
	// And StopRecorded must tolerate it end to end.
	p := NewFake()
	if err := StopRecorded(p, "seat", Termination{Kind: KindHandoff}, s); err != nil {
		t.Errorf("StopRecorded with a nil typed sink: %v", err)
	}
	if len(stoppedNames(p)) != 1 {
		t.Error("the stop did not happen")
	}
}

// TestEventSinkStopStillHappensWhenTheEventIsDropped is the end-to-end form of
// the contract: the record is bookkeeping, the stop is the operation.
func TestEventSinkStopStillHappensWhenTheEventIsDropped(t *testing.T) {
	s := NewEventTerminationSink(&ackRecorder{fail: errors.New("disk full")}, "controller")
	p := NewFake()
	err := StopRecorded(p, "seat", Termination{Kind: KindCityStop}, s)
	if len(stoppedNames(p)) != 1 {
		t.Fatalf("the stop must happen even when the event is dropped: %v", err)
	}
	if err == nil {
		t.Error("the drop must still be reported")
	}
}
