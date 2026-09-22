package events

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestSessionNudgedIsAKnownEventTypeWithATypedPayload pins both halves of the
// registration: TestEveryKnownEventTypeHasRegisteredPayload iterates
// KnownEventTypes, so a constant missing from the list would be invisible to
// it and the SSE wire would carry session.nudged untyped.
func TestSessionNudgedIsAKnownEventTypeWithATypedPayload(t *testing.T) {
	t.Parallel()

	if !slices.Contains(KnownEventTypes, SessionNudged) {
		t.Fatalf("%q is missing from KnownEventTypes", SessionNudged)
	}
	sample, ok := LookupPayload(SessionNudged)
	if !ok {
		t.Fatalf("%q has no registered payload", SessionNudged)
	}
	if _, ok := sample.(SessionNudgedPayload); !ok {
		t.Fatalf("%q registered payload is %T, want SessionNudgedPayload", SessionNudged, sample)
	}
}

// TestSessionNudgedPayloadCarriesNoTextField pins the privacy property: the
// wire shape has a hash and a length, and no field that could hold the body.
func TestSessionNudgedPayloadCarriesNoTextField(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(SessionNudgedPayload{
		TargetAgent: "cheryl", Delivery: "immediate", Outcome: "delivered",
		ErrorClass: "error", TextBytes: 5, TextSHA256: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"target_agent", "delivery", "outcome", "text_bytes", "text_sha256"} {
		if _, ok := shape[key]; !ok {
			t.Fatalf("payload JSON is missing %q: %s", key, raw)
		}
	}
	for _, forbidden := range []string{"text", "message", "body", "content", "error"} {
		if _, ok := shape[forbidden]; ok {
			t.Fatalf("payload JSON carries %q; the nudge text must never be recorded: %s", forbidden, raw)
		}
	}
}
