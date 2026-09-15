package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// ga-<pool-alias>: a pool worker's ALIAS is the canonical assignee.
// bd_assignee_canonicalize.go rewrites a pool worker's session-name and
// bead-ID assignment forms INTO its alias ("qcore/gastown.furiosa"), so every
// bead a polecat claims carries the alias as assignee. The reconciler's
// assigned-work guards, however, only recognized {bead ID, session_name,
// configured-named identity} — never the alias — so a pool worker mid-claim
// looked like it had NO assigned work. It fell out of the desired set,
// reached the live-session orphan branch, and was drained ~70-90s into real
// work; the pool then respawned and repeated the loop indefinitely.
//
// Observed 2026-08-13 21:02-21:22Z: 8+ spawn->claim->die cycles on
// qcore/gastown.furiosa against qc-sfk9y, close_reason "session drained:
// pool slot retired by reconciler".

func TestPoolAliasAssignedWorkKeepsWorkerAwake(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "qcore/gastown.polecat"}},
		SessionBeads: []AwakeSessionBead{{
			ID:          "ga-f93lvp",
			SessionName: "gastown__polecat-ga-f93lvp",
			Alias:       "qcore/gastown.furiosa",
			Template:    "qcore/gastown.polecat",
			State:       "active",
		}},
		WorkBeads: []AwakeWorkBead{{
			ID:       "qc-sfk9y",
			Assignee: "qcore/gastown.furiosa",
			Status:   "in_progress",
		}},
		RunningSessions: map[string]bool{"gastown__polecat-ga-f93lvp": true},
		Now:             now,
	})

	assertAwake(t, result, "gastown__polecat-ga-f93lvp")
	assertReason(t, result, "gastown__polecat-ga-f93lvp", "assigned-work")
	if got := result["gastown__polecat-ga-f93lvp"].AssignedWorkBeadID; got != "qc-sfk9y" {
		t.Fatalf("AssignedWorkBeadID = %q, want %q", got, "qc-sfk9y")
	}
}

// The orphan-drain guard (sessionHasOpenAssignedWorkForConfig*) resolves a
// session's assignment identities through these helpers. If the alias is
// absent the guard cannot see the worker's own claimed bead, and the live
// session is drained as "orphaned" mid-work.
func TestPoolAliasIsAnAssignmentIdentity(t *testing.T) {
	bead := beads.Bead{
		ID: "ga-f93lvp",
		Metadata: map[string]string{
			"session_name": "gastown__polecat-ga-f93lvp",
			"alias":        "qcore/gastown.furiosa",
			"pool_managed": "true",
		},
	}
	info := session.Info{
		ID:                  "ga-f93lvp",
		SessionNameMetadata: "gastown__polecat-ga-f93lvp",
		Alias:               "qcore/gastown.furiosa",
	}

	for name, got := range map[string][]string{
		"bead": sessionAssignmentIdentifiersForConfig(bead, nil),
		"info": sessionAssignmentIdentifiersForConfigInfo(info, nil),
	} {
		if !poolAliasIdentifiersContain(got, "qcore/gastown.furiosa") {
			t.Fatalf("%s identifiers %v omit the pool alias — the orphan-drain guard cannot see the worker's claimed work", name, got)
		}
	}
}

// A numbered slot of a pool identified by rebinding slots
// (usesTransientPoolSlotIdentity) passes to a fresh session whenever its holder
// dies, so a claim recorded under the slot alias belongs to whichever session
// held the slot then. It must not keep the current holder awake, and it must not
// count as that holder's assigned work in the drain guards
// (TestAssignmentGuardsIgnoreTransientPoolSlotAliases). A namepool member's alias
// is its own stable name, so its claim still does both, as ga-rht4v5 requires.
func TestTransientPoolSlotAliasDoesNotKeepSessionAwakeOnAnotherSessionsClaim(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{
		{Name: "polecat", Dir: "qcore", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), NamepoolNames: []string{"furiosa", "nux"}},
		{Name: "worker", Dir: "qcore", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)},
	}}
	for _, tc := range []struct {
		name         string
		template     string
		alias        string
		wantIdentity bool
	}{
		{name: "namepool alias is the session's own", template: "qcore/polecat", alias: "qcore/furiosa", wantIdentity: true},
		{name: "transient slot alias is not", template: "qcore/worker", alias: "qcore/worker-2", wantIdentity: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const sessionName = "qcore--pool-ga-f93lvp"
			bead := beads.Bead{
				ID:     "ga-f93lvp",
				Status: "open",
				Type:   session.BeadType,
				Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"template":     tc.template,
					"session_name": sessionName,
					"alias":        tc.alias,
					"pool_managed": "true",
					"pool_slot":    "2",
					"state":        "active",
				},
			}
			info := sessiontest.SeedBead(t, bead)

			for form, got := range map[string][]string{
				"bead": sessionAssignmentIdentifiersForConfig(bead, cfg),
				"info": sessionAssignmentIdentifiersForConfigInfo(info, cfg),
			} {
				if has := poolAliasIdentifiersContain(got, tc.alias); has != tc.wantIdentity {
					t.Fatalf("%s identifiers %v contain alias %q = %v, want %v", form, got, tc.alias, has, tc.wantIdentity)
				}
			}

			claimed := []beads.Bead{{ID: "qc-sfk9y", Assignee: tc.alias, Status: "in_progress"}}
			input := buildAwakeInputFromReconciler(cfg, "", []session.Info{info},
				nil, nil, nil, nil, nil, claimed, nil, nil, nil, now)
			input.RunningSessions = map[string]bool{sessionName: true}
			decision := ComputeAwakeSet(input)[sessionName]
			if decision.HasAssignedWork != tc.wantIdentity {
				t.Fatalf("HasAssignedWork = %v, want %v for a claim under alias %q (decision %+v)", decision.HasAssignedWork, tc.wantIdentity, tc.alias, decision)
			}
		})
	}
}

func poolAliasIdentifiersContain(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
