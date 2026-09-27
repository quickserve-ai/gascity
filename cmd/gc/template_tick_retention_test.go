package main

import (
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestTemplateTickSummaryVerdict pins that "retained" is recorded only when an
// open seat was observed, never as the default for any template with demand.
func TestTemplateTickSummaryVerdict(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		desired, poolDesired, openSeat int
		wantStatus                     TraceEvaluationStatus
		wantReason                     TraceReasonCode
	}{
		{"open seats are retained", 2, 2, 2, TraceEvaluationEligible, TraceReasonRetained},
		{"demand with no open seat retains nothing", 2, 2, 0, TraceEvaluationEligible, TraceReasonNoMatchingSession},
		{"pool demand alone retains nothing", 0, 1, 0, TraceEvaluationEligible, TraceReasonNoMatchingSession},
		{"open seat without demand is retained", 0, 0, 1, TraceEvaluationEligible, TraceReasonRetained},
		{"nothing at all", 0, 0, 0, TraceEvaluationSkipped, TraceReasonNoDemand},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, reason := templateTickSummaryVerdict(tc.desired, tc.poolDesired, tc.openSeat)
			if status != tc.wantStatus || reason != tc.wantReason {
				t.Fatalf("verdict(desired=%d pool=%d open=%d) = %s/%s, want %s/%s",
					tc.desired, tc.poolDesired, tc.openSeat, status, reason, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// TestOpenSeatRetentionLabel pins the retained_for keys: a working seat and a
// seat asleep holding a slot must read differently.
func TestOpenSeatRetentionLabel(t *testing.T) {
	for _, tc := range []struct {
		info sessionpkg.Info
		want string
	}{
		{sessionpkg.Info{MetadataState: "active"}, "active"},
		{sessionpkg.Info{MetadataState: "asleep", SleepReason: "idle"}, "asleep:idle"},
		{sessionpkg.Info{MetadataState: "asleep"}, "asleep:unspecified"},
		{sessionpkg.Info{MetadataState: "draining"}, "draining"},
		{sessionpkg.Info{}, "unknown"},
	} {
		if got := openSeatRetentionLabel(tc.info); got != tc.want {
			t.Errorf("openSeatRetentionLabel(state=%q sleep_reason=%q) = %q, want %q",
				tc.info.MetadataState, tc.info.SleepReason, got, tc.want)
		}
	}
}
