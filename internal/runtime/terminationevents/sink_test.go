package terminationevents

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
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
	s := New(rec, "controller")
	req := time.Unix(1700000000, 0).UTC()
	at := req.Add(9 * time.Minute)
	err := s.RecordTermination("qcore/worker-3", runtime.Termination{
		Kind: runtime.KindDrainTimeout, Actor: "controller", Reason: "config-drift",
		At: at, RequestedAt: req, SessionID: "ga-abc123",
		EventID: "01JABCDEFGHJKMNPQRSTVWXYZ0",
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
	var p events.SessionTerminatedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	// Every field the ratio needs must be on the row: a reader that has to join
	// back to the bead to bucket a row cannot compute the ratio from the stream
	// alone, which is the stream's only job.
	if p.Kind != string(runtime.KindDrainTimeout) || p.Reason != "config-drift" ||
		p.SessionName != "qcore/worker-3" || p.At == "" || p.RequestedAt == "" {
		t.Errorf("payload is missing ratio fields: %+v", p)
	}
	if p.EventID != "01JABCDEFGHJKMNPQRSTVWXYZ0" {
		t.Errorf("event_id = %q, want the record's: it is the reader's dedup key", p.EventID)
	}
	// Facts only: the bucket judgement belongs to the reader's versioned
	// classifier, so it must not ride on the row (katya, PR #106 finding 5).
	var shape map[string]any
	if err := json.Unmarshal(ev.Payload, &shape); err != nil {
		t.Fatalf("payload: %v", err)
	}
	for _, k := range []string{"counts_numerator", "counts_denominator"} {
		if _, ok := shape[k]; ok {
			t.Errorf("payload carries %q; the ratio rule belongs to the reader, not the row", k)
		}
	}
}

// TestEventSinkReportsADroppedEvent — the reconciliation rule says a missing
// bead record is filled by the event. That rests on knowing the event landed.
func TestEventSinkReportsADroppedEvent(t *testing.T) {
	boom := errors.New("ENOSPC")
	s := New(&ackRecorder{fail: boom}, "controller")
	err := s.RecordTermination("seat", runtime.Termination{Kind: runtime.KindHandoff})
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
	s := New(v, "controller")
	err := s.RecordTermination("seat", runtime.Termination{Kind: runtime.KindHandoff})
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
	if s := New(nil, ""); s != nil {
		t.Fatal("a nil recorder must yield a nil sink")
	}
	var s *Sink
	if err := s.RecordTermination("seat", runtime.Termination{Kind: runtime.KindHandoff}); err != nil {
		t.Errorf("a nil sink must be a silent no-op, got %v", err)
	}
	// And StopRecorded must tolerate it end to end.
	p := runtime.NewFake()
	if err := runtime.StopRecorded(p, "seat", runtime.Termination{Kind: runtime.KindHandoff}, s); err != nil {
		t.Errorf("StopRecorded with a nil typed sink: %v", err)
	}
	if len(stoppedNames(p)) != 1 {
		t.Error("the stop did not happen")
	}
}

// stoppedNames mirrors the parent package's test helper. It is duplicated
// rather than exported: a test-only accessor on the contract package would be
// public API that only tests want, and this is nine lines.
func stoppedNames(f *runtime.Fake) []string {
	var out []string
	for _, c := range f.Calls {
		if c.Method == "Stop" {
			out = append(out, c.Name)
		}
	}
	return out
}

// TestEventSinkStopStillHappensWhenTheEventIsDropped is the end-to-end form of
// the contract: the record is bookkeeping, the stop is the operation.
func TestEventSinkStopStillHappensWhenTheEventIsDropped(t *testing.T) {
	s := New(&ackRecorder{fail: errors.New("disk full")}, "controller")
	p := runtime.NewFake()
	err := runtime.StopRecorded(p, "seat", runtime.Termination{Kind: runtime.KindCityStop}, s)
	if len(stoppedNames(p)) != 1 {
		t.Fatalf("the stop must happen even when the event is dropped: %v", err)
	}
	if err == nil {
		t.Error("the drop must still be reported")
	}
}
