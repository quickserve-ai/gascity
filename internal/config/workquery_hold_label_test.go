package config

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// This file expresses the ga-x9kptu / ga-5736js acceptance criteria at the
// shell-generator level: route-scoped, unassigned pool-demand queries (Tier
// 3, and the reconciler's count-form) must exclude beads carrying a
// beadmeta.DispatchHoldLabels value. The generated assignee-scoped tiers serve
// work as well, so they exclude the dispatch holds too but never the human
// label; the assigned-ready tier's exclusion (#8010) is pinned in
// workquery_assigned_ready_hold_test.go.

func TestBdReadyPoolDemandShellExcludesDispatchHoldLabels(t *testing.T) {
	got := bdReadyPoolDemandShell("--limit 0", QueryTopology{})
	for _, label := range beadmeta.DispatchHoldLabels {
		want := `--exclude-label "` + label + `"`
		if !strings.Contains(got, want) {
			t.Errorf("bdReadyPoolDemandShell() = %q, missing %q", got, want)
		}
	}
}

func TestBdReadyPoolDemandMigrationShellExcludesDispatchHoldLabels(t *testing.T) {
	got := bdReadyPoolDemandMigrationShell("--limit=20", QueryTopology{})
	for _, label := range beadmeta.DispatchHoldLabels {
		want := `--exclude-label "` + label + `"`
		if !strings.Contains(got, want) {
			t.Errorf("bdReadyPoolDemandMigrationShell() = %q, missing %q", got, want)
		}
	}
}

func TestLegacyEphemeralPoolDemandShellRouteScopedExcludesDispatchHoldLabels(t *testing.T) {
	got := legacyEphemeralPoolDemandShell(20, QueryTopology{}, true)
	if !strings.Contains(got, ".labels") {
		t.Errorf("legacyEphemeralPoolDemandShell() = %q, missing a .labels reference", got)
	}
	for _, label := range beadmeta.DispatchHoldLabels {
		if !strings.Contains(got, `"`+label+`"`) {
			t.Errorf("legacyEphemeralPoolDemandShell() = %q, missing hold label %q", got, label)
		}
	}
}

// The bd-1.0.4 assigned-ready wisp probe serves its first row and exits the
// work query, so its jq selection drops every open-work park, the dispatch
// holds included (#8010, ga-v28pab); the behavior is pinned in
// workquery_assigned_ready_hold_test.go.
func TestEphemeralAssignedReadyProbeScriptExcludesDispatchHoldLabels(t *testing.T) {
	got := ephemeralAssignedReadyProbeScript("cand", QueryTopology{})
	if !strings.Contains(got, beadmeta.OpenWorkParkExcludeSelectJQ) {
		t.Errorf("ephemeralAssignedReadyProbeScript() = %q, missing the open-work park selector %q", got, beadmeta.OpenWorkParkExcludeSelectJQ)
	}
	for _, label := range beadmeta.DispatchHoldLabels {
		if !strings.HasPrefix(label, beadmeta.HoldLabelPrefix) {
			t.Errorf("dispatch hold %q does not start with %q, so the open-work park selector would not drop it", label, beadmeta.HoldLabelPrefix)
		}
	}
}

func TestEffectiveRoutedPoolQueryCarriesHoldLabelExclusionForLegacyAlias(t *testing.T) {
	a := &Agent{Name: ControlDispatcherAgentName, Dir: "rig"}
	got := a.EffectiveRoutedPoolQuery()
	for _, label := range beadmeta.DispatchHoldLabels {
		want := `--exclude-label "` + label + `"`
		if !strings.Contains(got, want) {
			t.Errorf("EffectiveRoutedPoolQuery() (legacy-alias agent) = %q, missing %q", got, want)
		}
	}
}

func TestPoolDemandCountShellInheritsDispatchHoldLabelExclusion(t *testing.T) {
	got := poolDemandCountShell("hello-world/worker", QueryTopology{})
	for _, label := range beadmeta.DispatchHoldLabels {
		want := `--exclude-label "` + label + `"`
		if !strings.Contains(got, want) {
			t.Errorf("poolDemandCountShell() = %q, missing %q (reconciler count-form must inherit the claim-path fix)", got, want)
		}
	}
}

// bd's native human label parks a bead for a person. It is not a dispatch hold:
// beadmeta.DispatchHoldLabels names only the two canonical holds, and the hook's
// held-candidate filter and the in_progress serve gate iterate that list, so the
// exclusion rides the pool-demand serve rules alone. Every route-scoped query
// renders from those rules and must carry it; the assignee-scoped probes must
// stay transparent to it.

func TestPoolDemandServeRulesExcludeHumanLabelWithoutMakingItADispatchHold(t *testing.T) {
	if rules := PoolDemandServeRulesForQuery(); !slices.Contains(rules.ExcludeLabels, "human") {
		t.Errorf("PoolDemandServeRulesForQuery().ExcludeLabels = %v, missing the human label", rules.ExcludeLabels)
	}
	if slices.Contains(beadmeta.DispatchHoldLabels, "human") {
		t.Errorf("beadmeta.DispatchHoldLabels = %v, must not name the human label: it is not a dispatch hold", beadmeta.DispatchHoldLabels)
	}
}

func TestRouteScopedPoolDemandShellsExcludeHumanLabel(t *testing.T) {
	want := `--exclude-label "human"`
	for name, got := range map[string]string{
		"bdReadyPoolDemandShell":          bdReadyPoolDemandShell("--limit 0", QueryTopology{}),
		"bdReadyPoolDemandMigrationShell": bdReadyPoolDemandMigrationShell("--limit=20", QueryTopology{}),
		"poolDemandCountShell":            poolDemandCountShell("hello-world/worker", QueryTopology{}),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%s() = %q, missing %q", name, got, want)
		}
	}
	if got := legacyEphemeralPoolDemandShell(20, QueryTopology{}, true); !strings.Contains(got, `"human"`) {
		t.Errorf("legacyEphemeralPoolDemandShell() = %q, missing the human label in its jq label filter", got)
	}
}

func TestAssigneeScopedProbesStayHumanTransparent(t *testing.T) {
	for name, got := range map[string]string{
		"ephemeralAssignedReadyProbeScript":       ephemeralAssignedReadyProbeScript("cand", QueryTopology{}),
		"ephemeralAssignedInProgressProbeScript":  ephemeralAssignedInProgressProbeScript("cand", QueryTopology{}),
		"assignedReadyTierCommand/bd104":          assignedReadyTierCommand("cand", QueryTopology{}),
		"assignedReadyTierCommand/bd105":          assignedReadyTierCommand("cand", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}}),
		"assignedReadyTierCommand/bd105federated": assignedReadyTierCommand("cand", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}, FederatedReady: true}),
	} {
		if strings.Contains(got, `"human"`) {
			t.Errorf("%s() = %q, assignee-scoped tier must stay human-transparent", name, got)
		}
	}
}

// openWorkParkFirstRowScript replaces $r only when jq parsed the whole payload:
// jq can print a result and still fail on what follows it, and a partial result
// must not stand in for the payload the hook's fail-open handling reads.
func TestOpenWorkParkFirstRowScriptKeepsAPayloadJQCannotParseWhole(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required to run the generated script")
	}
	cases := map[string]struct{ in, want string }{
		"parsed, parked head stripped": {
			in:   `[{"id":"p","labels":["  hold:human "]},{"id":"a","labels":[]}]`,
			want: `[{"id":"a","labels":[]}]`,
		},
		"parsed, mixed-case freeze stripped": {
			in:   `[{"id":"f","labels":["Freeze:Deferred"]}]`,
			want: `[]`,
		},
		"trailing garbage kept whole": {
			in:   `[] trailing-garbage`,
			want: `[] trailing-garbage`,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			script := `r=` + shellquote.Quote(c.in) + `; ` + openWorkParkFirstRowScript() + `printf "%s" "$r"`
			out, err := exec.Command("sh", "-c", script).Output()
			if err != nil {
				t.Fatalf("script failed: %v", err)
			}
			if got := string(out); got != c.want {
				t.Fatalf("r = %q, want %q", got, c.want)
			}
		})
	}
}
