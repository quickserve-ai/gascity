package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func stubBdAssigneeSessionBeads(t *testing.T, sessionBeads []beads.Bead, err error) {
	t.Helper()
	prev := bdListSessionBeadsForAssigneeIndex
	bdListSessionBeadsForAssigneeIndex = func(string) ([]beads.Bead, error) {
		return sessionBeads, err
	}
	t.Cleanup(func() { bdListSessionBeadsForAssigneeIndex = prev })
}

func bdAssigneeTestConfig() *config.City {
	one := 1
	return &config.City{
		Workspace: config.Workspace{Name: "gastown"},
		Rigs:      []config.Rig{{Name: "qcore", Path: "/tmp/qcore"}},
		Agents: []config.Agent{
			// Singleton (max 1): a bound identity, valid assignee. An agent
			// with max unset is instance-expanding = pool template.
			{Name: "gastown.refinery", Dir: "qcore", MaxActiveSessions: &one},
			{Name: "gastown.polecat", Dir: "qcore"},
		},
		NamedSessions: []config.NamedSession{
			{Name: "lana", Template: "claude", Dir: "qcore"},
			{Name: "cheryl", Template: "claude"},
		},
	}
}

func bdAssigneeTestSessionBeads() []beads.Bead {
	return []beads.Bead{
		{
			ID:     "ga-wisp-lana1",
			Status: "open",
			Type:   sessionBeadType,
			Metadata: map[string]string{
				"alias":                     "qcore/lana",
				"configured_named_identity": "qcore/lana",
				"session_name":              "qcore--lana",
			},
		},
		{
			// Alias-less pool worker: session-name forms are canonical for it.
			ID:     "ga-wisp-pool1",
			Status: "open",
			Type:   sessionBeadType,
			Metadata: map[string]string{
				"session_name": "gastown__polecat-ga-2e0p",
			},
		},
	}
}

func runBdAssigneeCanonicalize(t *testing.T, args []string) ([]string, string) {
	t.Helper()
	var stderr strings.Builder
	got := canonicalizeBdAssigneeArgs(args, t.TempDir(), bdAssigneeTestConfig(), &stderr)
	return got, stderr.String()
}

func TestCanonicalizeBdAssigneeRewritesCrewFormToAlias(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	got, warn := runBdAssigneeCanonicalize(t, []string{"update", "qc-1", "--assignee", "qcore/crew/lana"})
	if got[3] != "qcore/lana" {
		t.Fatalf("assignee = %q, want qcore/lana (args %v)", got[3], got)
	}
	if !strings.Contains(warn, "canonicalized") || !strings.Contains(warn, "ga-i44k") {
		t.Fatalf("stderr = %q, want canonicalization notice", warn)
	}
}

func TestCanonicalizeBdAssigneeRewritesSessionNameForm(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	// qcore--lana matches the assigned-work scope but NOT lana's own hook
	// query (exact $GC_AGENT) — half-visible, must rewrite to the alias.
	got, _ := runBdAssigneeCanonicalize(t, []string{"update", "qc-1", "--assignee=qcore--lana"})
	if got[2] != "--assignee=qcore/lana" {
		t.Fatalf("arg = %q, want --assignee=qcore/lana", got[2])
	}
}

func TestCanonicalizeBdAssigneeRewritesBareNameOnCreate(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	got, _ := runBdAssigneeCanonicalize(t, []string{"create", "fix things", "-a", "lana", "-p", "1"})
	if got[3] != "qcore/lana" {
		t.Fatalf("assignee = %q, want qcore/lana (args %v)", got[3], got)
	}
}

func TestCanonicalizeBdAssigneeRewritesDeadTemplateFormToBoundAgent(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	// "qcore/refinery" is the dead identity from the mol-formula strand
	// class; the bound agent is qcore/gastown.refinery.
	got, _ := runBdAssigneeCanonicalize(t, []string{"update", "qc-1", "--assignee", "qcore/refinery"})
	if got[3] != "qcore/gastown.refinery" {
		t.Fatalf("assignee = %q, want qcore/gastown.refinery", got[3])
	}
}

func TestCanonicalizeBdAssigneeLeavesCanonicalAndPoolIdentitiesAlone(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	for _, canonical := range []string{"qcore/lana", "cheryl", "gastown__polecat-ga-2e0p", "ga-wisp-pool1", "qcore/gastown.refinery"} {
		got, warn := runBdAssigneeCanonicalize(t, []string{"update", "qc-1", "--assignee", canonical})
		if got[3] != canonical {
			t.Fatalf("assignee %q rewritten to %q, want untouched", canonical, got[3])
		}
		if warn != "" {
			t.Fatalf("assignee %q produced stderr %q, want silence", canonical, warn)
		}
	}
}

func TestCanonicalizeBdAssigneeWarnsPoolTemplateWithoutRewriting(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	got, warn := runBdAssigneeCanonicalize(t, []string{"update", "qc-1", "--assignee", "qcore/gastown.polecat"})
	if got[3] != "qcore/gastown.polecat" {
		t.Fatalf("pool template rewritten to %q, want untouched", got[3])
	}
	if !strings.Contains(warn, "pool TEMPLATE") {
		t.Fatalf("stderr = %q, want pool-template warning", warn)
	}
}

func TestCanonicalizeBdAssigneeWarnsUnknownWithoutRewriting(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	got, warn := runBdAssigneeCanonicalize(t, []string{"update", "qc-1", "--assignee", "q_core/crew/jasnah"})
	if got[3] != "q_core/crew/jasnah" {
		t.Fatalf("cross-town assignee rewritten to %q, want untouched", got[3])
	}
	if !strings.Contains(warn, "WARNING") || !strings.Contains(warn, "find-work") {
		t.Fatalf("stderr = %q, want unknown-assignee warning", warn)
	}
}

func TestCanonicalizeBdAssigneeSkipsOtherSubcommandsAndEmptyValues(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	for _, args := range [][]string{
		{"list", "--assignee", "qcore/crew/lana"},
		{"ready", "--assignee=qcore/crew/lana"},
		{"update", "qc-1", "--assignee", ""},
	} {
		got, warn := runBdAssigneeCanonicalize(t, args)
		for i := range args {
			if got[i] != args[i] {
				t.Fatalf("args %v mutated to %v", args, got)
			}
		}
		if warn != "" {
			t.Fatalf("args %v produced stderr %q, want silence", args, warn)
		}
	}
}

func TestCanonicalizeBdAssigneeFailsOpenOnStoreError(t *testing.T) {
	stubBdAssigneeSessionBeads(t, nil, errBdAssigneeTestStore)
	args := []string{"update", "qc-1", "--assignee", "qcore/crew/lana"}
	got, warn := runBdAssigneeCanonicalize(t, args)
	if got[3] != "qcore/crew/lana" {
		t.Fatalf("assignee = %q, want untouched on store error", got[3])
	}
	if warn != "" {
		t.Fatalf("stderr = %q, want silence on store error", warn)
	}
}

var errBdAssigneeTestStore = beads.ErrNotFound

func TestCanonicalizeBdAssigneeDisabledByEnv(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	t.Setenv(bdAssigneeCanonicalizeEnv, "off")
	got, warn := runBdAssigneeCanonicalize(t, []string{"update", "qc-1", "--assignee", "qcore/crew/lana"})
	if got[3] != "qcore/crew/lana" || warn != "" {
		t.Fatalf("canonicalization ran despite %s=off (assignee=%q stderr=%q)", bdAssigneeCanonicalizeEnv, got[3], warn)
	}
}

func TestCanonicalizeBdAssigneeValueLookingLikeFlagIsNotMisread(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	// "--assignee" appearing as the VALUE of another flag must not trigger.
	args := []string{"update", "qc-1", "--notes", "--assignee", "--priority", "1"}
	got, _ := runBdAssigneeCanonicalize(t, args)
	for i := range args {
		if got[i] != args[i] {
			t.Fatalf("args %v mutated to %v", args, got)
		}
	}
}
