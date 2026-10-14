package main

import (
	"os"
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
	got, warn, err := runBdAssigneeCanonicalizeErr(t, args)
	if err != nil {
		t.Fatalf("canonicalize %v refused: %v", args, err)
	}
	return got, warn
}

func runBdAssigneeCanonicalizeErr(t *testing.T, args []string) ([]string, string, error) {
	t.Helper()
	var stderr strings.Builder
	// The doBd order: strip gc's flag, then canonicalize.
	args, allowUnknown := stripBdAllowUnknownAssignee(args)
	got, err := canonicalizeBdAssigneeArgs(args, allowUnknown, t.TempDir(), bdAssigneeTestConfig(), &stderr)
	return got, stderr.String(), err
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

func TestCanonicalizeBdAssigneeRefusesUnknown(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	// ga-6sm0d7: the 2026-09-06 ghost shape. "qcore/crew.lana" matches no live
	// identity; the write is refused and the refusal names the live form.
	for _, args := range [][]string{
		{"update", "qc-1", "--assignee", "qcore/crew.lana"},
		{"update", "qc-1", "--assignee=qcore/crew.lana"},
		{"create", "fix things", "-a", "qcore/crew.lana"},
		// One unknown among several tokens refuses the whole write.
		{"update", "qc-1", "-a", "qcore/lana", "--assignee", "qcore/crew.lana"},
	} {
		_, _, err := runBdAssigneeCanonicalizeErr(t, args)
		if err == nil {
			t.Fatalf("args %v: unknown assignee accepted, want refusal", args)
		}
		for _, want := range []string{`"qcore/crew.lana"`, "qcore/lana", bdAllowUnknownAssigneeFlag, "ga-i44k"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("args %v: refusal %q does not mention %q", args, err, want)
			}
		}
	}
}

func TestCanonicalizeBdAssigneeRefusalWithNoNearMatchSaysSo(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	_, _, err := runBdAssigneeCanonicalizeErr(t, []string{"update", "qc-1", "--assignee", "q_core/crew/jasnah"})
	if err == nil || !strings.Contains(err.Error(), "no near match") {
		t.Fatalf("err = %v, want a refusal that says no near match", err)
	}
}

func TestCanonicalizeBdAssigneeAllowUnknownPassesCrossTownAndStripsFlag(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	for _, args := range [][]string{
		{"update", "qc-1", bdAllowUnknownAssigneeFlag, "--assignee", "q_core/crew/jasnah"},
		{"update", "qc-1", "--assignee", "q_core/crew/jasnah", bdAllowUnknownAssigneeFlag},
	} {
		got, warn, err := runBdAssigneeCanonicalizeErr(t, args)
		if err != nil {
			t.Fatalf("args %v: refused despite %s: %v", args, bdAllowUnknownAssigneeFlag, err)
		}
		want := []string{"update", "qc-1", "--assignee", "q_core/crew/jasnah"}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("args %v forwarded as %v, want %v (gc-side flag must not reach bd)", args, got, want)
		}
		if !strings.Contains(warn, "WARNING") || !strings.Contains(warn, "find-work") {
			t.Fatalf("stderr = %q, want the unknown-assignee warning to stay loud", warn)
		}
	}
}

func TestDoBdStripsAllowUnknownAssigneeBeforeAnyArm(t *testing.T) {
	// The flag is gc's, not bd's. doBd strips it on its first line, ahead of
	// scope detection, the by-id door and the relocated-class checks, so no
	// arm parses it and bd never receives it — whatever the subcommand and
	// whether or not canonicalization runs.
	src, err := os.ReadFile("cmd_bd.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func doBd(")
	strip := strings.Index(body, "stripBdAllowUnknownAssignee(bdArgs)")
	firstUse := strings.Index(body, "rewriteBdHeartbeatArgs(bdArgs)")
	if start < 0 || strip < start || firstUse < strip {
		t.Fatalf("doBd must strip %s before the first bdArgs consumer", bdAllowUnknownAssigneeFlag)
	}
	for _, args := range [][]string{
		{"list", bdAllowUnknownAssigneeFlag},
		{"update", "qc-1", "--status", "open", bdAllowUnknownAssigneeFlag},
	} {
		got, found := stripBdAllowUnknownAssignee(args)
		if !found || strings.Contains(strings.Join(got, " "), bdAllowUnknownAssigneeFlag) {
			t.Fatalf("args %v stripped to %v (found=%v)", args, got, found)
		}
	}
}

func TestCanonicalizeBdAssigneeLeavesAllowFlagAfterDoubleDash(t *testing.T) {
	stubBdAssigneeSessionBeads(t, bdAssigneeTestSessionBeads(), nil)
	args := []string{"create", "--", bdAllowUnknownAssigneeFlag}
	got, _ := runBdAssigneeCanonicalize(t, args)
	if strings.Join(got, " ") != strings.Join(args, " ") {
		t.Fatalf("positional text after -- was rewritten: %v", got)
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
	// An unknown-looking assignee included: without an index nothing is
	// known to be unknown, so an index hiccup never blocks a write.
	for _, raw := range []string{"qcore/crew/lana", "q_core/crew/jasnah"} {
		args := []string{"update", "qc-1", "--assignee", raw}
		got, warn := runBdAssigneeCanonicalize(t, args)
		if got[3] != raw {
			t.Fatalf("assignee = %q, want untouched on store error", got[3])
		}
		if warn != "" {
			t.Fatalf("stderr = %q, want silence on store error", warn)
		}
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
