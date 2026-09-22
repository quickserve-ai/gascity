package events

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestSessionTerminatedIsAKnownEventTypeWithATypedPayload pins both halves of
// the registration. TestEveryKnownEventTypeHasRegisteredPayload only iterates
// KnownEventTypes, so a constant missing from the list would be invisible to
// it — which is exactly how session.terminated shipped at first: emitted, read
// by the handoff ratio from events.jsonl, and carried on the SSE wire as an
// untyped envelope (ga-ksac39).
func TestSessionTerminatedIsAKnownEventTypeWithATypedPayload(t *testing.T) {
	t.Parallel()

	if !slices.Contains(KnownEventTypes, SessionTerminated) {
		t.Fatalf("%q is missing from KnownEventTypes; the SSE projection would carry it untyped", SessionTerminated)
	}
	sample, ok := LookupPayload(SessionTerminated)
	if !ok {
		t.Fatalf("%q has no registered payload", SessionTerminated)
	}
	if _, ok := sample.(SessionTerminatedPayload); !ok {
		t.Fatalf("%q registered payload is %T, want SessionTerminatedPayload", SessionTerminated, sample)
	}
}

func TestSessionTerminatedPayloadRoundTrips(t *testing.T) {
	t.Parallel()

	want := SessionTerminatedPayload{
		Kind:              "handoff",
		Actor:             "woodhouse",
		Reason:            "context 71%",
		At:                "2026-09-22T03:10:00Z",
		RequestedAt:       "2026-09-22T03:09:12Z",
		SessionName:       "woodhouse",
		SessionID:         "ga-wisp-abc123",
		CountsNumerator:   true,
		CountsDenominator: true,
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	decoded, typed, err := DecodePayload(SessionTerminated, raw)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !typed {
		t.Fatal("DecodePayload reported no registered type for session.terminated")
	}
	got, ok := decoded.(SessionTerminatedPayload)
	if !ok {
		t.Fatalf("DecodePayload returned %T, want SessionTerminatedPayload", decoded)
	}
	if got != want {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}

	// The ratio reader keys on these names in events.jsonl; renaming one would
	// silently zero a bucket, so the wire names are pinned here.
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, key := range []string{"kind", "actor", "reason", "at", "requested_at", "session_name", "session_id", "counts_numerator", "counts_denominator"} {
		if _, ok := shape[key]; !ok {
			t.Fatalf("payload JSON is missing %q: %s", key, raw)
		}
	}
}
