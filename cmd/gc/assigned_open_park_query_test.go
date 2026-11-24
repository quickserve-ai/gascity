package main

// Town park labels in the generated work query (ga-v28pab). A town parks
// unclaimed work under any hold:* label or freeze:deferred, not only the two
// canonical dispatch holds. These tests run the REAL generated work query over
// the #8010 fake reader (assigned_hold_repro_test.go) and feed its output to
// the real claim path: a parked row is never claimed, and a parked row read
// first never hides actionable work behind it.

import (
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const assignedOpenParkActionableID = "test-actionable"

// The padded and mixed-case spellings pin that the query's jq strip agrees
// with the hook's Go filter (TrimSpace; freeze:deferred case-insensitive), so a
// row the hook would reject is never the one the cut to one row keeps.
var assignedOpenParkTownLabels = []string{"hold:human", "hold:human-decision", "hold:clock", beadmeta.FreezeDeferredLabel, "  hold:human ", "Freeze:Deferred"}

func assignedOpenParkActionable(assignee string) assignedHoldRow {
	return assignedHoldRow{ID: assignedOpenParkActionableID, Status: "open", Type: "task", Assignee: assignee}
}

// A parked assignment read FIRST must not hide the seat's actionable
// assignment behind it: the cut to one row runs after the park strip.
func TestAssignedOpenParkFirstStillServesActionableAssignment(t *testing.T) {
	for _, federated := range []bool{false, true} {
		for _, label := range assignedOpenParkTownLabels {
			t.Run(label+"/federated="+strconv.FormatBool(federated), func(t *testing.T) {
				out, _ := assignedHoldRunWorkQuery(t, assignedHoldTopology(federated), []assignedHoldRow{
					assignedHoldDecoy("open", assignedHoldAlias, label),
					assignedOpenParkActionable(assignedHoldAlias),
				})
				res, claimed := assignedHoldClaim(t, out)
				if res.Action != "work" || res.BeadID != assignedOpenParkActionableID {
					t.Fatalf("parked head hid the actionable assignment: action=%s reason=%s bead=%q; output %q",
						res.Action, res.Reason, res.BeadID, out)
				}
				if len(claimed) != 1 || claimed[0] != assignedOpenParkActionableID {
					t.Fatalf("claim mutations = %v, want only %s", claimed, assignedOpenParkActionableID)
				}
			})
		}
	}
}

// A seat whose only assignment is parked falls through to its routed work,
// and the parked bead is never claimed (the #8010 shape, for town labels).
func TestAssignedOpenParkOnlyFallsThroughToRoutedStep(t *testing.T) {
	for _, federated := range []bool{false, true} {
		for _, label := range assignedOpenParkTownLabels {
			t.Run(label+"/federated="+strconv.FormatBool(federated), func(t *testing.T) {
				out, _ := assignedHoldRunWorkQuery(t, assignedHoldTopology(federated), []assignedHoldRow{
					assignedHoldDecoy("open", assignedHoldAlias, label),
					assignedHoldRoutedStep(),
				})
				res, claimed := assignedHoldClaim(t, out)
				assertAssignedHoldRoutedStepClaimed(t, out, res, claimed)
			})
		}
	}
}

// An unassigned routed bead parked on freeze:deferred is not served, and an
// actionable routed bead behind it is; bd excludes freeze:deferred ahead of
// the routed tier's limit, like the dispatch holds.
func TestAssignedOpenParkUnassignedRoutedFreezeDeferredNotServed(t *testing.T) {
	// "Freeze:Deferred" passes bd's exact --exclude-label, so it pins that the
	// routed tier's jq strip folds case as the hook and the demand predicate do:
	// a row the hook refuses must not hold a place in the routed read.
	for _, label := range []string{beadmeta.FreezeDeferredLabel, "Freeze:Deferred"} {
		frozen := assignedHoldRoutedStep()
		frozen.ID = "test-frozen-routed"
		frozen.Labels = []string{label}
		assignedOpenParkRoutedFrozenCases(t, label, frozen)
	}
	assignedOpenParkFreezeRulesAgree(t)
}

func assignedOpenParkRoutedFrozenCases(t *testing.T, label string, frozen assignedHoldRow) {
	t.Helper()
	for _, federated := range []bool{false, true} {
		t.Run(label+"/alone/federated="+strconv.FormatBool(federated), func(t *testing.T) {
			out, _ := assignedHoldRunWorkQuery(t, assignedHoldTopology(federated), []assignedHoldRow{frozen})
			res, claimed := assignedHoldClaim(t, out)
			if res.Action == "work" || len(claimed) != 0 {
				t.Fatalf("freeze:deferred routed bead served: action=%s bead=%q claimed=%v output %q", res.Action, res.BeadID, claimed, out)
			}
		})
		t.Run(label+"/first/federated="+strconv.FormatBool(federated), func(t *testing.T) {
			out, _ := assignedHoldRunWorkQuery(t, assignedHoldTopology(federated), []assignedHoldRow{frozen, assignedHoldRoutedStep()})
			res, claimed := assignedHoldClaim(t, out)
			assertAssignedHoldRoutedStepClaimed(t, out, res, claimed)
		})
	}
}

func assignedOpenParkFreezeRulesAgree(t *testing.T) {
	t.Helper()
	var found bool
	for _, l := range config.PoolDemandServeRulesForQuery().ExcludeLabels {
		if l == beadmeta.FreezeDeferredLabel {
			found = true
		}
	}
	if !found {
		t.Fatalf("PoolDemandServeRulesForQuery().ExcludeLabels = %v, want freeze:deferred so the controller's demand count agrees with the query",
			config.PoolDemandServeRulesForQuery().ExcludeLabels)
	}
	if got := filterReadyByRoute([]beads.Bead{{ID: "f", Status: "open", Type: "task", Labels: []string{beadmeta.FreezeDeferredLabel},
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: assignedHoldAlias}}}, beadmeta.RoutedToMetadataKey, assignedHoldAlias); len(got) != 0 {
		t.Fatalf("control dispatcher routed filter served a freeze:deferred bead: %v", got)
	}
}

// A seat's own in_progress park (hold:cert-wait is in_progress by town
// doctrine) is still served to it: claimed work keeps the canonical rule.
func TestAssignedOpenParkInProgressCertWaitStillServed(t *testing.T) {
	for _, federated := range []bool{false, true} {
		t.Run("federated="+strconv.FormatBool(federated), func(t *testing.T) {
			park := assignedHoldDecoy("in_progress", assignedHoldAlias, "hold:cert-wait")
			out, _ := assignedHoldRunWorkQuery(t, assignedHoldTopology(federated), []assignedHoldRow{park, assignedHoldRoutedStep()})
			res, _ := assignedHoldClaim(t, out)
			if res.BeadID != assignedHoldDecoyID {
				t.Fatalf("in_progress hold:cert-wait park not served to its seat: action=%s reason=%s bead=%q output %q",
					res.Action, res.Reason, res.BeadID, out)
			}
		})
	}
}

// The control dispatcher's Go mirrors agree with the generated tiers: its
// assigned mirror drops an OPEN park but keeps bd's human label and a claimed
// park; its routed mirror drops any hold:* park as the shell routed tier's jq
// strip does (ga-v28pab).
func TestControlReadyMirrorsDropOpenWorkParks(t *testing.T) {
	const who = "rig/control-dispatcher"
	ready := []beads.Bead{
		{ID: "frozen", Status: "open", Type: "task", Assignee: who, Labels: []string{beadmeta.FreezeDeferredLabel}},
		{ID: "clock", Status: "open", Type: "task", Assignee: who, Labels: []string{"hold:clock"}},
		{ID: "human", Status: "open", Type: "task", Assignee: who, Labels: []string{beadmeta.HumanLabel}},
		{ID: "claimed-park", Status: "in_progress", Type: "task", Assignee: who, Labels: []string{"hold:cert-wait"}},
		{ID: "plain", Status: "open", Type: "task", Assignee: who},
	}
	var got []string
	for _, b := range filterReadyByAssignee(ready, who, 0) {
		got = append(got, b.ID)
	}
	if want := "human,claimed-park,plain"; strings.Join(got, ",") != want {
		t.Fatalf("filterReadyByAssignee kept %v, want %s", got, want)
	}
	// The cut to N runs after the drop: a parked head does not use up the limit.
	if one := filterReadyByAssignee(ready, who, 1); len(one) != 1 || one[0].ID != "human" {
		t.Fatalf("filterReadyByAssignee(limit 1) = %+v, want the first unparked row", one)
	}
	routed := []beads.Bead{
		{ID: "routed-clock", Status: "open", Type: "task", Labels: []string{"hold:clock"},
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: who}},
		{ID: "routed-frozen-mixed-case", Status: "open", Type: "task", Labels: []string{"Freeze:Deferred"},
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: who}},
		{ID: "routed-plain", Status: "open", Type: "task",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: who}},
	}
	if r := filterReadyByRoute(routed, beadmeta.RoutedToMetadataKey, who); len(r) != 1 || r[0].ID != "routed-plain" {
		t.Fatalf("filterReadyByRoute = %+v, want only routed-plain", r)
	}
}
