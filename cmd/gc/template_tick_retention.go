package main

import (
	"strings"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The per-template tick summary used to record reason "retained" for every
// template with any demand or any open session: a bare default, true or not.
// On 2026-09-25/26 it read "retained" for 178 ticks while both of a pool's open
// seats held work they were not doing (ga-x99xh0), and nothing in the record
// said what the seats were retained FOR. The verdict below is derived from the
// counts the summary observed, and when open seats are retained the summary
// carries retained_for: the open seats keyed by the state each was observed in.

// templateTickSummaryVerdict derives a template summary's status and reason from
// the counts it records. Open seats are "retained"; demand with no open seat has
// nothing retained and reads "no_matching_session"; no demand and no seats is
// "no_demand".
func templateTickSummaryVerdict(desiredCount, poolDesired, openCount int) (TraceEvaluationStatus, TraceReasonCode) {
	switch {
	case openCount > 0:
		return TraceEvaluationEligible, TraceReasonRetained
	case desiredCount > 0 || poolDesired > 0:
		return TraceEvaluationEligible, TraceReasonNoMatchingSession
	default:
		return TraceEvaluationSkipped, TraceReasonNoDemand
	}
}

// openSeatRetentionLabel names the state an open session was observed in, which
// is what the tick summary's retained_for counts it under: its lifecycle state,
// and for a sleeping seat the reason it sleeps ("asleep:idle" is a seat holding
// a slot while doing nothing; "active" is one that is running).
func openSeatRetentionLabel(info sessionpkg.Info) string {
	state := strings.TrimSpace(info.MetadataState)
	if state == "" {
		state = "unknown"
	}
	if state == string(sessionpkg.StateAsleep) {
		reason := strings.TrimSpace(info.SleepReason)
		if reason == "" {
			reason = "unspecified"
		}
		return state + ":" + reason
	}
	return state
}
