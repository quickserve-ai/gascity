package dolt_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The stale-db formula's terminal bd close must present the alias-first session
// identity and must escalate loudly when the close is refused. An unwrapped
// close under set -euo pipefail aborts the script, the EXIT trap still
// drain-acks, and the controller re-dispatches a fresh agent against the
// still-open bead forever (we-m34w5: 15 wakes in ~40 minutes). A refused
// close must leave evidence — an escalation event plus fail-open — never a
// clean drain.
func TestStaleDBFormulaTerminalCloseIsGuardedAndActorCarrying(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "formulas", "mol-dog-stale-db.toml"))
	if err != nil {
		t.Fatalf("read formula: %v", err)
	}
	script := string(raw)

	if regexp.MustCompile(`(?m)^bd close "\$WORK_BEAD"`).MatchString(script) {
		t.Fatalf("terminal bd close is unguarded: a refused close aborts the script, the EXIT trap drain-acks, and the controller re-dispatches forever")
	}
	if !strings.Contains(script, `actor="${GC_ALIAS:-${GC_SESSION_NAME:-${GC_SESSION_ID:-}}}"`) ||
		!strings.Contains(script, `--actor "$actor"`) {
		t.Fatalf("terminal close must present the alias-first session identity (GC_ALIAS, then GC_SESSION_NAME, then GC_SESSION_ID)")
	}
	guard := regexp.MustCompile(`if ! close_work_bead[\s\S]*?fail_open_after_drain`)
	if !guard.MatchString(script) {
		t.Fatalf("a refused terminal close must reach fail_open_after_drain (loud fail-open), not a clean drain")
	}
}
