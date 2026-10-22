package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
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

// templateTickSummary is one template's tick-summary record: the verdict and the
// fields the trace records verbatim.
type templateTickSummary struct {
	status TraceEvaluationStatus
	reason TraceReasonCode
	fields map[string]any
}

// buildTemplateTickSummaries computes the per-template tick summaries from the
// tick's pre-reconcile inputs, returning the template names in record order and
// each template's summary. retained_for is present only when the template has
// open seats; it counts them by openSeatRetentionLabel.
func buildTemplateTickSummaries(
	cfg *config.City,
	openInfos []sessionpkg.Info,
	desiredState map[string]TemplateParams,
	poolDesired map[string]int,
	workSet map[string]bool,
	workRequested map[string]bool,
) ([]string, map[string]templateTickSummary) {
	templateNames := make(map[string]struct{})
	openCounts := make(map[string]int)
	retainedFor := make(map[string]map[string]int)
	desiredCounts := make(map[string]int)
	for _, info := range openInfos {
		template := normalizedSessionTemplateInfo(info, cfg)
		if template == "" {
			continue
		}
		templateNames[template] = struct{}{}
		openCounts[template]++
		if retainedFor[template] == nil {
			retainedFor[template] = make(map[string]int)
		}
		retainedFor[template][openSeatRetentionLabel(info)]++
	}
	for _, tp := range desiredState {
		if tp.TemplateName == "" {
			continue
		}
		templateNames[tp.TemplateName] = struct{}{}
		desiredCounts[tp.TemplateName]++
	}
	for template := range poolDesired {
		templateNames[template] = struct{}{}
	}
	for template := range workSet {
		templateNames[template] = struct{}{}
	}
	for template := range workRequested {
		templateNames[template] = struct{}{}
	}
	names := traceSetStrings(templateNames)
	summaries := make(map[string]templateTickSummary, len(names))
	for _, template := range names {
		status, reason := templateTickSummaryVerdict(desiredCounts[template], poolDesired[template], openCounts[template])
		fields := map[string]any{
			"desired_count":  desiredCounts[template],
			"open_count":     openCounts[template],
			"pool_desired":   poolDesired[template],
			"work_requested": workRequested[template],
		}
		if held := retainedFor[template]; len(held) > 0 {
			fields["retained_for"] = held
		}
		summaries[template] = templateTickSummary{status: status, reason: reason, fields: fields}
	}
	return names, summaries
}
