package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// "gc agent is-foreign" exposes the pool sweeper's foreign-identity gate to
// callers outside the gc process — specifically the city-local witness patrol
// formula, which is a shell/LLM step and cannot call Go.
//
// WHY A VERB AND NOT A SECOND IMPLEMENTATION (ga-7dr90m, mayor's decision
// 2026-08-26 22:31 PDT). On 2026-08-26 21:52 PDT, right after a supervisor
// cycle, the witness patrol read "assignee absent from THIS city's session map"
// as "orphaned" and stripped 18 workflow steps held by westeros-side identities
// plus one of ours. The Go pool sweeper already had the gate that would have
// stopped it (pool_orphan_foreign_identity.go). Two rejected alternatives: port
// the shell orphan-sweep functions into the formula (a second copy that drifts
// from the sweeper), or hand-roll the logic in the formula — which reproduces
// ga-8yi7ne, where stripping ANY binding prefix makes westeros's canonical
// "qcore/pool.omp-1" resolve against our "qcore/omp" and their live claim reads
// as our dead instance.
//
// So this verb calls poolAssigneeObservability, the SAME function the in-process
// sweeper calls. The formula decides nothing about identities; it asks.
//
// EXIT CODES ARE THE CONTRACT, because a shell caller reads them before it reads
// JSON:
//
//	0  local     — this city can answer this identity's liveness; caller may
//	               apply its own liveness check and reap if genuinely dead
//	1  foreign   — well-formed identity NOT in this city's roster; PROTECT
//	2  unknown   — this city cannot answer (no config, unresolvable city, bad
//	               usage); PROTECT, and count as protected-unknown
//
// Exit 2 means "no answer", never "no match" — a caller that treats it as
// foreign is still safe, and one that treats it as local is not, which is why
// the degraded case has its own code rather than folding into 1.
//
// A NOTE ON WHAT "local" DOES NOT MEAN. It does not mean alive. This verb
// answers one question — whose roster does this identity belong to — and the
// 21:52 incident happened precisely because those two questions were conflated.
// The caller still has to establish liveness itself.

// AgentIsForeignJSON is the JSON output format for "gc agent is-foreign --json".
type AgentIsForeignJSON struct {
	SchemaVersion string `json:"schema_version"`
	Identity      string `json:"identity"`
	// Verdict is "local", "foreign" or "unknown", matching the exit code.
	Verdict string `json:"verdict"`
	// Reason names the narrowing that fired, so a protected identity can be
	// reported with WHY. "foreign_binding" (another city's naming) and
	// "absent_from_roster" (possibly one of ours, decommissioned) call for
	// different operator responses.
	Reason string `json:"reason"`
	// Detail is the narrowing's subject: the matched roster candidate, or the
	// foreign binding that blocked every candidate.
	Detail string `json:"detail,omitempty"`
	// RosterSource names where the roster came from, so a verdict is
	// reproducible from the JSON alone.
	RosterSource string `json:"roster_source"`
	CityName     string `json:"city_name,omitempty"`
}

// Verdict strings. These are the words the formula matches on, so they are part
// of the contract alongside the exit codes.
const (
	agentIsForeignVerdictLocal   = "local"
	agentIsForeignVerdictForeign = "foreign"
	agentIsForeignVerdictUnknown = "unknown"
)

// agentIsForeignExitUnknown is the degraded exit code. Named because the
// fail-closed contract in the witness formula keys on it.
const (
	agentIsForeignExitLocal   = 0
	agentIsForeignExitForeign = 1
	agentIsForeignExitUnknown = 2
)

func newAgentIsForeignCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "is-foreign <identity>",
		Short: "Report whether an assignee identity belongs to another city's roster",
		Long: `Report whether an assignee identity belongs to another city's roster.

Answers the question the pool sweeper's foreign-identity gate answers, using the
same code: is this <rig>/<name> identity one THIS city configures, or one it
merely SEES in a shared rig store?

Exit codes:
  0  local    this city can answer this identity's liveness
  1  foreign  well-formed identity absent from this city's roster — protect it
  2  unknown  this city cannot answer (no config, bad usage) — protect it

"local" is not a claim that the identity is ALIVE. Liveness is a separate
question and conflating the two is what stripped 18 of another city's workflow
steps on 2026-08-26 (ga-7dr90m).

Callers that reap on this verdict MUST fail closed: treat a missing verb, exit 2,
or unparseable output as "protect", never as "local".`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			// exitForCode preserves the distinction between 1 and 2; a bare
			// errExit would collapse "foreign" and "cannot answer" into one
			// code and break the fail-closed contract at the caller.
			return exitForCode(cmdAgentIsForeign(args, jsonOutput, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	return cmd
}

func cmdAgentIsForeign(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		// Bad usage is a degraded answer, not a foreign one: exit 2 so a caller
		// that mis-invokes the verb protects the claim instead of reaping it.
		fmt.Fprintln(stderr, "gc agent is-foreign: usage: gc agent is-foreign <identity> [--json]") //nolint:errcheck // best-effort stderr
		return agentIsForeignExitUnknown
	}
	identity := strings.TrimSpace(args[0])
	cityPath, err := resolveCity()
	if err != nil {
		return writeAgentIsForeignUnknown(identity, "", "", fmt.Sprintf("resolve city: %v", err), jsonOutput, stdout, stderr)
	}
	return doAgentIsForeign(fsys.OSFS{}, cityPath, identity, jsonOutput, stdout, stderr)
}

func doAgentIsForeign(fs fsys.FS, cityPath, identity string, jsonOutput bool, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")
	// Config-load warnings go to stderr, never stdout: stdout carries the JSON
	// a caller parses, and a warning mixed into it would make a healthy verdict
	// unparseable — which the fail-closed contract turns into a protect. Correct
	// but a silent loss of the sweeper.
	cfg, err := loadCityConfigFS(fs, tomlPath, stderr)
	if err != nil {
		return writeAgentIsForeignUnknown(identity, tomlPath, "", fmt.Sprintf("load city config: %v", err), jsonOutput, stdout, stderr)
	}
	cityName := cfg.EffectiveCityName()
	verdict := poolAssigneeObservability(cfg, cityName, identity)

	if verdict.Reason == poolRosterReasonNoConfig {
		return writeAgentIsForeignUnknown(identity, tomlPath, cityName, "resolved city config carries no roster", jsonOutput, stdout, stderr)
	}

	out := AgentIsForeignJSON{
		SchemaVersion: "1",
		Identity:      identity,
		Verdict:       agentIsForeignVerdictForeign,
		Reason:        string(verdict.Reason),
		Detail:        verdict.Detail,
		RosterSource:  agentIsForeignRosterSource(tomlPath, cfg),
		CityName:      cityName,
	}
	code := agentIsForeignExitForeign
	if verdict.Local {
		out.Verdict = agentIsForeignVerdictLocal
		code = agentIsForeignExitLocal
	}
	if !writeAgentIsForeignResult(out, jsonOutput, stdout, stderr) {
		return agentIsForeignExitUnknown
	}
	return code
}

// writeAgentIsForeignUnknown emits the degraded verdict. It still writes
// well-formed output so a caller can log WHY it protected a claim; a bare
// non-zero exit with nothing to read is a refusal nobody can diagnose, and a
// refusal nobody can diagnose becomes a refusal somebody disables.
func writeAgentIsForeignUnknown(identity, tomlPath, cityName, reason string, jsonOutput bool, stdout, stderr io.Writer) int {
	out := AgentIsForeignJSON{
		SchemaVersion: "1",
		Identity:      identity,
		Verdict:       agentIsForeignVerdictUnknown,
		Reason:        string(poolRosterReasonNoConfig),
		Detail:        reason,
		RosterSource:  tomlPath,
		CityName:      cityName,
	}
	writeAgentIsForeignResult(out, jsonOutput, stdout, stderr)
	return agentIsForeignExitUnknown
}

func writeAgentIsForeignResult(out AgentIsForeignJSON, jsonOutput bool, stdout, stderr io.Writer) bool {
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, out); err != nil {
			fmt.Fprintf(stderr, "gc agent is-foreign: %v\n", err) //nolint:errcheck // best-effort stderr
			return false
		}
		return true
	}
	detail := ""
	if strings.TrimSpace(out.Detail) != "" {
		detail = fmt.Sprintf(" (%s)", out.Detail)
	}
	fmt.Fprintf(stdout, "%s %s: %s%s\n", out.Identity, out.Verdict, out.Reason, detail) //nolint:errcheck // best-effort stdout
	return true
}

// agentIsForeignRosterSource describes the roster this verdict was read from.
// The counts are there so a surprising verdict can be triaged from the JSON
// alone: a roster reporting 0 agents is a config-resolution failure wearing a
// successful load, and it would make every identity read foreign.
func agentIsForeignRosterSource(tomlPath string, cfg *config.City) string {
	agents, rigs := 0, 0
	if cfg != nil {
		agents = len(cfg.Agents)
		rigs = len(cfg.Rigs)
	}
	return fmt.Sprintf("%s (resolved: %d agents, %d rigs)", tomlPath, agents, rigs)
}
