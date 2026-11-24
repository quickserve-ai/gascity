package main

// Town park labels in the generated work query (ga-v28pab). A town parks
// unclaimed work under any hold:* label or freeze:deferred, not only the two
// canonical dispatch holds. These tests run the REAL generated work query over
// the #8010 fake reader (assigned_hold_repro_test.go) and feed its output to
// the real claim path: a parked row is never claimed, and a parked row read
// first never hides actionable work behind it.

import (
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const assignedOpenParkActionableID = "test-actionable"

var assignedOpenParkTownLabels = []string{"hold:human", "hold:human-decision", "hold:clock", beadmeta.FreezeDeferredLabel}

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
	frozen := assignedHoldRoutedStep()
	frozen.ID = "test-frozen-routed"
	frozen.Labels = []string{beadmeta.FreezeDeferredLabel}
	for _, federated := range []bool{false, true} {
		t.Run("alone/federated="+strconv.FormatBool(federated), func(t *testing.T) {
			out, _ := assignedHoldRunWorkQuery(t, assignedHoldTopology(federated), []assignedHoldRow{frozen})
			res, claimed := assignedHoldClaim(t, out)
			if res.Action == "work" || len(claimed) != 0 {
				t.Fatalf("freeze:deferred routed bead served: action=%s bead=%q claimed=%v output %q", res.Action, res.BeadID, claimed, out)
			}
		})
		t.Run("first/federated="+strconv.FormatBool(federated), func(t *testing.T) {
			out, _ := assignedHoldRunWorkQuery(t, assignedHoldTopology(federated), []assignedHoldRow{frozen, assignedHoldRoutedStep()})
			res, claimed := assignedHoldClaim(t, out)
			assertAssignedHoldRoutedStepClaimed(t, out, res, claimed)
		})
	}
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
