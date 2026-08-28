package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isForeignTestCity writes a city.toml whose roster mirrors the shapes this
// city actually runs, so the verb is exercised against resolved config rather
// than a hand-built config.City — the verb's job includes LOADING the roster,
// and a test that skips that step would not catch a verb that resolves nothing
// and calls every identity foreign.
//
// The "pack"-bound sibling is what makes "pack" a binding THIS city mints
// (ga-8yi7ne). Without it the fixture would describe a city that dropped the
// import, where bound forms SHOULD be unresolvable.
func isForeignTestCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "repo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	// The namepool is a FILE at load time (Agent.NamepoolNames is toml:"-"), so
	// the themed-instance narrowing can only be exercised through one.
	if err := os.WriteFile(filepath.Join(cityPath, "namepool.txt"), []byte("furiosa\nnux\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(namepool): %v", err)
	}
	cityToml := `[workspace]
name = "testcity"
prefix = "tc"

[[rigs]]
name = "repo"
path = "repo"
prefix = "rp"

[[agent]]
name = "worker"
dir = "repo"
min_active_sessions = 0
max_active_sessions = 2
namepool = "namepool.txt"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	return cityPath
}

func runIsForeign(t *testing.T, cityPath, identity string) (code int, out AgentIsForeignJSON, raw string, stderr string) {
	t.Helper()
	var stdoutBuf, stderrBuf bytes.Buffer
	code = run([]string{"--city", cityPath, "agent", "is-foreign", identity, "--json"}, &stdoutBuf, &stderrBuf)
	raw = stdoutBuf.String()
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &out); err != nil {
			t.Fatalf("is-foreign %q: stdout is not parseable JSON (%v):\n%s", identity, err, raw)
		}
	}
	return code, out, raw, stderrBuf.String()
}

// TestAgentIsForeign_VerdictsAndExitCodes is the non-vacuity test: local and
// foreign identities are asserted in ONE table. Either assertion alone is
// satisfiable by a verb that answers the same way for everything.
func TestAgentIsForeign_VerdictsAndExitCodes(t *testing.T) {
	cityPath := isForeignTestCity(t)

	cases := []struct {
		name     string
		identity string
		wantCode int
		wantVerd string
		wantWhy  string
	}{
		{
			name:     "configured agent",
			identity: "repo/worker",
			wantCode: agentIsForeignExitLocal,
			wantVerd: agentIsForeignVerdictLocal,
			wantWhy:  string(poolRosterReasonAgentTemplate),
		},
		{
			name:     "namepool instance of a local agent",
			identity: "repo/furiosa",
			wantCode: agentIsForeignExitLocal,
			wantVerd: agentIsForeignVerdictLocal,
			wantWhy:  string(poolRosterReasonNamepoolInstance),
		},
		{
			name:     "numeric slot instance of a local agent",
			identity: "repo/worker-3",
			wantCode: agentIsForeignExitLocal,
			wantVerd: agentIsForeignVerdictLocal,
			wantWhy:  string(poolRosterReasonAgentInstance),
		},
		{
			// Not a <rig>/<name> identity: a bare alias, a session bead ID, a
			// runtime session name. This city's own naming, so the caller keeps
			// its existing liveness path. Reporting these as foreign would
			// protect every local claim and silently disable the sweeper.
			name:     "bare alias is not the cross-city hazard",
			identity: "worker",
			wantCode: agentIsForeignExitLocal,
			wantVerd: agentIsForeignVerdictLocal,
			wantWhy:  string(poolRosterReasonNotQualified),
		},
		{
			name:     "well-formed identity absent from the roster",
			identity: "repo/stranger",
			wantCode: agentIsForeignExitForeign,
			wantVerd: agentIsForeignVerdictForeign,
			wantWhy:  string(poolRosterReasonAbsentFromRoster),
		},
		{
			name:     "identity under a rig this city does not configure",
			identity: "otherrig/worker",
			wantCode: agentIsForeignExitForeign,
			wantVerd: agentIsForeignVerdictForeign,
			wantWhy:  string(poolRosterReasonAbsentFromRoster),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, raw, stderr := runIsForeign(t, cityPath, tc.identity)
			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, tc.wantCode, raw, stderr)
			}
			if out.Verdict != tc.wantVerd {
				t.Fatalf("verdict = %q, want %q (raw: %s)", out.Verdict, tc.wantVerd, raw)
			}
			if out.Reason != tc.wantWhy {
				t.Fatalf("reason = %q, want %q (raw: %s)", out.Reason, tc.wantWhy, raw)
			}
			if out.Identity != tc.identity {
				t.Fatalf("identity echoed as %q, want %q", out.Identity, tc.identity)
			}
			if strings.TrimSpace(out.RosterSource) == "" {
				t.Fatalf("roster_source is empty; a verdict must name the roster it was read from (raw: %s)", raw)
			}
		})
	}
}

// TestAgentIsForeign_ForeignBindingCollision is the ga-8yi7ne case, named
// because the mayor's decision on ga-7dr90m requires it.
//
// A neighboring city's canonical "<rig>/<binding>.<name>-<slot>" must NOT
// resolve against our same-named unbound agent. Stripping ANY binding prefix is
// what turned this gate into a cross-city hazard in the field on 2026-08-25:
// westeros's "qcore/pool.omp-1" strips to "qcore/omp-1", matches our configured
// "qcore/omp", and their LIVE claim reads as our dead instance. The bead
// oscillated — released and re-claimed twice.
//
// The assertion is on the REASON as well as the verdict: "foreign_binding" is
// the narrowing that must fire. A verb that returned foreign for the right
// identity via the wrong narrowing would pass a verdict-only test while the
// binding gate was broken.
func TestAgentIsForeign_ForeignBindingCollision(t *testing.T) {
	cityPath := isForeignTestCity(t)

	// "pool" is NOT a binding this city mints; "worker" IS a local agent. So a
	// binding-stripping implementation resolves this to repo/worker and calls it
	// local. That is the bug.
	code, out, raw, stderr := runIsForeign(t, cityPath, "repo/pool.worker-1")
	if code != agentIsForeignExitForeign {
		t.Fatalf("ga-8yi7ne: exit = %d, want %d (foreign) — a foreign binding resolved against our own agent\nstdout: %s\nstderr: %s",
			code, agentIsForeignExitForeign, raw, stderr)
	}
	if out.Verdict != agentIsForeignVerdictForeign {
		t.Fatalf("ga-8yi7ne: verdict = %q, want foreign (raw: %s)", out.Verdict, raw)
	}
	if out.Reason != string(poolRosterReasonForeignBinding) {
		t.Fatalf("ga-8yi7ne: reason = %q, want %q — the binding gate must be the narrowing that fires (raw: %s)",
			out.Reason, poolRosterReasonForeignBinding, raw)
	}
	if out.Detail != "pool" {
		t.Fatalf("ga-8yi7ne: detail = %q, want the offending binding %q so the witness mail can name it", out.Detail, "pool")
	}

	// Non-vacuity: the same identity WITHOUT the binding prefix is local, so
	// the binding is demonstrably what flipped the verdict rather than the
	// dash, the slot, or a verb hard-coded to call dotted names foreign.
	// (The other half — a binding this city DOES mint stays local — needs
	// Agent.BindingName, which is toml:"-" and cannot be expressed in
	// city.toml; it is asserted at the gate level in
	// TestPoolAssigneeObservability_ReasonsMatchTheNarrowingThatFired.)
	code, out, raw, _ = runIsForeign(t, cityPath, "repo/worker-1")
	if code != agentIsForeignExitLocal || out.Verdict != agentIsForeignVerdictLocal {
		t.Fatalf("the same instance identity without a binding prefix must stay local: exit = %d verdict = %q (raw: %s)", code, out.Verdict, raw)
	}
}

// TestAgentIsForeign_DegradedAnswersAreExitTwo pins the fail-closed contract's
// other half. Exit 2 must mean "cannot answer", distinct from 1 ("foreign"), so
// a caller can count protected-unknown separately — and so a mis-invocation
// never reads as "local" and reaps.
func TestAgentIsForeign_DegradedAnswersAreExitTwo(t *testing.T) {
	cityPath := isForeignTestCity(t)

	t.Run("missing identity argument", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--city", cityPath, "agent", "is-foreign", "--json"}, &stdout, &stderr)
		if code != agentIsForeignExitUnknown {
			t.Fatalf("exit = %d, want %d for bad usage\nstderr: %s", code, agentIsForeignExitUnknown, stderr.String())
		}
	})

	t.Run("unloadable city config", func(t *testing.T) {
		broken := t.TempDir()
		if err := os.WriteFile(filepath.Join(broken, "city.toml"), []byte("this is not = valid = toml ["), 0o644); err != nil {
			t.Fatalf("WriteFile(city.toml): %v", err)
		}
		var stdout, stderr bytes.Buffer
		code := run([]string{"--city", broken, "agent", "is-foreign", "repo/worker", "--json"}, &stdout, &stderr)
		if code != agentIsForeignExitUnknown {
			t.Fatalf("exit = %d, want %d when the roster cannot be loaded\nstdout: %s\nstderr: %s",
				code, agentIsForeignExitUnknown, stdout.String(), stderr.String())
		}
		// The degraded answer still has to be readable: a refusal nobody can
		// diagnose becomes a refusal somebody disables.
		var out AgentIsForeignJSON
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &out); err != nil {
			t.Fatalf("degraded verdict is not parseable JSON (%v):\n%s", err, stdout.String())
		}
		if out.Verdict != agentIsForeignVerdictUnknown {
			t.Fatalf("verdict = %q, want %q", out.Verdict, agentIsForeignVerdictUnknown)
		}
		if strings.TrimSpace(out.Detail) == "" {
			t.Fatalf("degraded verdict carries no detail; the caller cannot say why it protected the claim")
		}
	})
}

// TestAgentIsForeign_StdoutCarriesOnlyTheVerdict guards the parse contract: the
// witness formula reads stdout as JSON, so a config-load warning on stdout
// would make a healthy verdict unparseable — which the fail-closed contract
// turns into a protect. Correct, and a silent loss of orphan recovery.
func TestAgentIsForeign_StdoutCarriesOnlyTheVerdict(t *testing.T) {
	cityPath := isForeignTestCity(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "agent", "is-foreign", "repo/worker", "--json"}, &stdout, &stderr)
	// Non-vacuity for this test: it only proves anything if a warning was
	// actually produced. This fixture declares the rig path in city.toml, which
	// the loader warns about, so stderr must be non-empty here — otherwise the
	// assertion below passes for a build that emits no warnings at all.
	if strings.TrimSpace(stderr.String()) == "" {
		t.Fatalf("fixture produced no config warning, so this test cannot show that warnings stay off stdout")
	}
	var out AgentIsForeignJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &out); err != nil {
		t.Fatalf("stdout is not a single parseable JSON line (%v); exit was %d\nstdout: %s\nstderr: %s", err, code, stdout.String(), stderr.String())
	}
	if out.Identity != "repo/worker" {
		t.Fatalf("identity = %q, want repo/worker", out.Identity)
	}
}

// TestPoolAssigneeObservability_ReasonsMatchTheNarrowingThatFired pins the
// reason plumbing at the gate, where a minted binding can be expressed
// (Agent.BindingName is toml:"-", so city.toml cannot). This is the half of
// ga-8yi7ne the verb test cannot reach: the bound form of a binding THIS city
// mints must stay local, and say which resolver answered.
func TestPoolAssigneeObservability_ReasonsMatchTheNarrowingThatFired(t *testing.T) {
	cfg := foreignIdentityTestCity(t)

	cases := []struct {
		name       string
		identity   string
		wantLocal  bool
		wantReason poolRosterReason
		wantDetail string
	}{
		{
			// "pack" IS minted here (the fixture's bound sibling), so the
			// binding is stripped and the unbound agent answers.
			name:       "bound form of a binding this city mints",
			identity:   "repo/pack.worker",
			wantLocal:  true,
			wantReason: poolRosterReasonAgentTemplate,
		},
		{
			// ga-8yi7ne: "pool" is NOT minted here, so no resolver may run even
			// though stripping it would match our own "repo/worker".
			name:       "bound form of a binding this city does not mint",
			identity:   "repo/pool.worker-1",
			wantLocal:  false,
			wantReason: poolRosterReasonForeignBinding,
			wantDetail: "pool",
		},
		{
			name:       "namepool instance",
			identity:   "repo/furiosa",
			wantLocal:  true,
			wantReason: poolRosterReasonNamepoolInstance,
		},
		{
			name:       "absent from the roster",
			identity:   "repo/stranger",
			wantLocal:  false,
			wantReason: poolRosterReasonAbsentFromRoster,
		},
		{
			name:       "not a qualified identity",
			identity:   "some-runtime-session-name",
			wantLocal:  true,
			wantReason: poolRosterReasonNotQualified,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := poolAssigneeObservability(cfg, "testcity", tc.identity)
			if got.Local != tc.wantLocal {
				t.Fatalf("Local = %v, want %v (reason %q)", got.Local, tc.wantLocal, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if tc.wantDetail != "" && got.Detail != tc.wantDetail {
				t.Fatalf("Detail = %q, want %q", got.Detail, tc.wantDetail)
			}
			// The boolean predicate the in-process sweeper calls must agree with
			// the explained form in every case — that agreement is what makes
			// this ONE implementation rather than two that can drift.
			if observable := poolAssigneeIsLocallyObservable(cfg, "testcity", tc.identity); observable != got.Local {
				t.Fatalf("poolAssigneeIsLocallyObservable = %v but poolAssigneeObservability.Local = %v — the predicate and the explained form have drifted", observable, got.Local)
			}
		})
	}
}
