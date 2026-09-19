package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// EventTerminationSink writes a `session.terminated` event for every ending.
//
// THIS IS THE SECOND OF THE TWO SINKS, AND ITS WHOLE VALUE IS THAT IT FAILS
// INDEPENDENTLY OF THE FIRST. The bead sink rides Dolt and can fail during
// exactly the incidents that produce force-exits; this one is a local file
// append that almost never does. Two sinks in one failure domain would be one
// sink with extra steps (katya, ga-ksac39 condition 2).
//
// RECONCILIATION RULE for whoever reads these, stated here because this is the
// file that produces the corroborating half: BEAD HISTORY IS THE SOURCE OF
// RECORD. Where a bead record is missing but an event exists, the event FILLS
// the hole and the week is FLAGGED — never silently patched. An event stream
// that quietly repaired the authoritative record would make the instrument
// unfalsifiable.
type EventTerminationSink struct {
	rec events.Recorder
	// actor is the fallback event actor when a Termination carries none.
	actor string
}

// NewEventTerminationSink returns a sink writing to rec. A nil rec yields a nil
// sink, which StopRecorded skips — callers that have not wired events yet must
// still be able to stop.
func NewEventTerminationSink(rec events.Recorder, actor string) *EventTerminationSink {
	if rec == nil {
		return nil
	}
	if actor == "" {
		actor = "controller"
	}
	return &EventTerminationSink{rec: rec, actor: actor}
}

// TerminationEventType is the event type the ratio reads.
const TerminationEventType = "session.terminated"

// terminationPayload is the event's payload. Field names are snake_case to match
// the rest of the event log, and every field the ratio needs is present so a
// reader never has to join back to the bead to bucket a row.
type terminationPayload struct {
	Kind        string `json:"kind"`
	Actor       string `json:"actor,omitempty"`
	Reason      string `json:"reason,omitempty"`
	At          string `json:"at"`
	RequestedAt string `json:"requested_at,omitempty"`
	SessionName string `json:"session_name"`
	SessionID   string `json:"session_id,omitempty"`
	// Numerator/Denominator are written out rather than recomputed by readers.
	// The bucket rules are a JUDGEMENT (handoff-target reports on its own line;
	// observed-dead is out of the denominator), and a judgement re-derived
	// independently by every consumer drifts. Recording the decision makes a
	// later change to it visible as a change in the data.
	CountsNumerator   bool `json:"counts_numerator"`
	CountsDenominator bool `json:"counts_denominator"`
}

// RecordTermination emits the event. It returns an error ONLY when the recorder
// can tell us the event was dropped.
//
// WHY THE ACK MATTERS HERE MORE THAN USUAL: events.Recorder.Record is
// best-effort and VOID — a FileRecorder silently drops on a cross-process lock
// timeout or ENOSPC, and Discard drops everything. If this sink used Record and
// returned nil, it would report success for an event that never landed, and the
// reconciliation rule above ("where the bead is missing, the event fills the
// hole") would be resting on a record that may not exist. So when the recorder
// implements AckRecorder we use RecordAck and surface its verdict; when it does
// not, we say so rather than implying durability we cannot observe.
func (s *EventTerminationSink) RecordTermination(sessionName string, t Termination) error {
	if s == nil || s.rec == nil {
		return nil
	}
	at := t.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	payload := terminationPayload{
		Kind:              string(t.Kind),
		Actor:             t.Actor,
		Reason:            t.Reason,
		At:                at.UTC().Format(time.RFC3339),
		SessionName:       sessionName,
		SessionID:         t.SessionID,
		CountsNumerator:   t.Kind.CountsInNumerator(),
		CountsDenominator: t.Kind.CountsInDenominator(),
	}
	if !t.RequestedAt.IsZero() {
		payload.RequestedAt = t.RequestedAt.UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		// Cannot happen for this struct, but a silent drop here would be the
		// one case where the "almost never fails" sink fails invisibly.
		return fmt.Errorf("marshalling the termination payload: %w", err)
	}
	actor := t.Actor
	if actor == "" {
		actor = s.actor
	}
	ev := events.Event{
		Type:      TerminationEventType,
		Ts:        at.UTC(),
		Actor:     actor,
		Subject:   sessionName,
		Message:   string(t.Kind),
		Payload:   raw,
		SessionID: t.SessionID,
	}
	if ack, ok := s.rec.(events.AckRecorder); ok {
		if err := ack.RecordAck(ev); err != nil {
			return fmt.Errorf("session.terminated event dropped: %w", err)
		}
		return nil
	}
	s.rec.Record(ev)
	// Not an error: a void Recorder is a legitimate configuration. But the
	// caller must not read a nil return here as "durably recorded", so this is
	// reported as an unacknowledged write rather than silent success.
	return errUnacknowledgedEvent
}

// errUnacknowledgedEvent marks an emit through a plain (void) Recorder. It is a
// sentinel so a caller can tell "the event was dropped" from "the event was
// written to a recorder that cannot confirm anything".
var errUnacknowledgedEvent = errors.New("session.terminated emitted to a recorder that cannot acknowledge it (best-effort; not proof of a durable record)")

// IsUnacknowledgedEvent reports whether err is the unacknowledged-emit sentinel
// rather than a real failure. A caller deciding whether a termination is
// durably recorded treats this as NOT durable; a caller deciding whether
// something went wrong treats it as benign.
func IsUnacknowledgedEvent(err error) bool { return errors.Is(err, errUnacknowledgedEvent) }
