package config

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The assigned-ready reader must not let an assigned dispatch hold hide a
// second ready assignment or the unassigned pool queue behind it.
func TestAssignedReadySkipsDispatchHeldRows(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	for _, tc := range []struct {
		name      string
		assigned  string
		ephemeral string
		wantID    string
	}{
		{"held only falls through to routed work", `[{"id":"held","labels":["` + beadmeta.HoldExternalLabel + `"]}]`, `[]`, "routed"},
		{"held first serves second assignment", `[{"id":"held","labels":["` + beadmeta.HoldMayorLabel + `"]},{"id":"assigned","labels":[]}]`, `[]`, "assigned"},
		{"held ephemeral fallback does not mask routed work", `[{"id":"held","labels":["` + beadmeta.HoldExternalLabel + `"]}]`, `[{"id":"held","assignee":"sess-1","labels":["` + beadmeta.HoldExternalLabel + `"],"dependency_count":0}]`, "routed"},
		{"ordinary hold is still served", `[{"id":"assigned","labels":["hold:human"]}]`, `[]`, "assigned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bdScript := `#!/bin/sh
case "$1" in
  ready)
    case "$*" in
      *--assignee=*) printf '%s' '` + tc.assigned + `' ;;
      *gc.root_bead_id*) printf '[]' ;;
      *) printf '%s' '[{"id":"routed","metadata":{"gc.routed_to":"rig/worker"}}]' ;;
    esac ;;
  query) printf '%s' '` + tc.ephemeral + `' ;;
esac
`
			a := &Agent{Name: "worker", Dir: "rig"}
			out := runShellWithFakeBd(t, a.EffectiveWorkQuery(), map[string]string{
				"GC_SESSION_ID": "sess-1", "GC_SESSION_ORIGIN": "ephemeral",
			}, bdScript)
			var rows []struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
				t.Fatalf("invalid work-query output %q: %v", out, err)
			}
			if len(rows) != 1 || rows[0].ID != tc.wantID {
				t.Fatalf("work query returned %q, want only %s", out, tc.wantID)
			}
		})
	}
}

// readerLimitFakeBd answers the assigned-ready read the way bd does: it applies
// every --exclude-label it is given to rows, THEN cuts to --limit. The fixture
// therefore decides the outcome, not the mere presence of a flag: a reader that
// excluded holds only after its page bound would see 20 held rows and never the
// runnable 21st.
func readerLimitFakeBd(rows string) string {
	return `#!/bin/sh
case "$1" in
  ready)
    case "$*" in
      *--assignee=*)
        ex='[]'; n=0
        for a in "$@"; do
          if [ "${want_label:-}" = 1 ]; then ex=$(printf '%s' "$ex" | jq -c --arg l "$a" '. + [$l]'); want_label=; continue; fi
          case "$a" in
            --exclude-label) want_label=1 ;;
            --exclude-label=*) ex=$(printf '%s' "$ex" | jq -c --arg l "${a#--exclude-label=}" '. + [$l]') ;;
            --limit=*) n=${a#--limit=} ;;
          esac
        done
        printf '%s' '` + rows + `' | jq -c --argjson ex "$ex" --argjson n "$n" \
          '[.[] | select(([(.labels // [])[] as $l | $ex[] | select(. == $l)] | length) == 0)] | if $n > 0 then .[:$n] else . end' ;;
      *gc.root_bead_id*) printf '[]' ;;
      *) printf '%s' '[{"id":"routed","metadata":{"gc.routed_to":"rig/worker"}}]' ;;
    esac ;;
  *) printf '[]' ;;
esac
`
}

func TestAssignedReadyFiltersHoldsBeforeReaderLimit(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	holds := []string{beadmeta.HoldMayorLabel, beadmeta.HoldExternalLabel}
	var rows []string
	for i := 1; i <= 20; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"held-%02d","labels":["%s"]}`, i, holds[i%2]))
	}
	rows = append(rows, `{"id":"assigned","labels":[]}`)
	fixture := "[" + strings.Join(rows, ",") + "]"

	out := runShellWithFakeBd(t, (&Agent{Name: "worker", Dir: "rig"}).EffectiveWorkQuery(), map[string]string{
		"GC_SESSION_ID": "sess-1", "GC_SESSION_ORIGIN": "ephemeral",
	}, readerLimitFakeBd(fixture))
	var got []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("invalid work-query output %q: %v", out, err)
	}
	if len(got) != 1 || got[0].ID != "assigned" {
		t.Fatalf("work query returned %q, want the runnable 21st assignment behind 20 held rows", out)
	}
}

// The ephemeral fallback serves assigned molecule rows the ready reader cannot
// see. Its candidate stream must drop dispatch holds BEFORE the fast and slow
// cuts take their first row: otherwise a held head (oldest first) is cut, then
// dropped, and the unheld assignment behind it is never examined.
//
// The fake bd answers the open and in_progress ephemeral queries differently,
// so only the open (ready) fallback can serve b-ready: the in_progress tier
// sees nothing and the assignee-scoped ready reader returns [].
func ephemeralHoldFallbackFakeBd(openRows string) string {
	const closedDep = `"dependencies":[{"id":"dep-1","status":"closed","dependency_type":"blocks"}]`
	return `#!/bin/sh
case "$1" in
  ready) printf '[]' ;;
  query)
    case "$*" in
      *status=in_progress*) printf '[]' ;;
      *status=open*) printf '%s' '` + openRows + `' ;;
      *) printf '[]' ;;
    esac ;;
  show)
    case "$2" in
      a-held) printf '%s' '[{"id":"a-held",` + closedDep + `}]' ;;
      b-ready) printf '%s' '[{"id":"b-ready",` + closedDep + `}]' ;;
      *) printf '[]' ;;
    esac ;;
  *) printf '[]' ;;
esac
`
}

func ephemeralHoldFallbackRows(assignee string, dependencyCount int) string {
	return fmt.Sprintf(`[`+
		`{"id":"a-held","status":"open","issue_type":"step","assignee":%q,"labels":[%q],"created_at":"2026-10-30T00:00:00Z","dependency_count":%d},`+
		`{"id":"b-ready","status":"open","issue_type":"step","assignee":%q,"labels":[],"created_at":"2026-10-30T00:00:01Z","dependency_count":%d}`+
		`]`, assignee, beadmeta.HoldExternalLabel, dependencyCount, assignee, dependencyCount)
}

func TestAssignedReadyEphemeralFallbackSkipsHeldHead(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	bd104 := QueryTopology{}
	bd105 := QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}}
	for _, tc := range []struct {
		name     string
		script   string
		env      map[string]string
		assignee string
		deps     int
	}{
		{"standard fast path", standardAssignedReadyWorkQueryScript(bd104), map[string]string{"GC_SESSION_ID": "sess-1"}, "sess-1", 0},
		{"standard dependency-enriched", standardAssignedReadyWorkQueryScript(bd104), map[string]string{"GC_SESSION_ID": "sess-1"}, "sess-1", 1},
		{"standard bd105 assignee read", standardAssignedReadyWorkQueryScript(bd105), map[string]string{"GC_SESSION_ID": "sess-1"}, "sess-1", 0},
		{"standard bd105 dependency-enriched", standardAssignedReadyWorkQueryScript(bd105), map[string]string{"GC_SESSION_ID": "sess-1"}, "sess-1", 1},
		{"legacy control fast path", legacyControlAssignedReadyWorkQueryScript(bd104), map[string]string{"GC_SESSION_ID": "rig/control-dispatcher"}, "rig/workflow-control", 0},
		{"legacy control dependency-enriched", legacyControlAssignedReadyWorkQueryScript(bd104), map[string]string{"GC_SESSION_ID": "rig/control-dispatcher"}, "rig/workflow-control", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runShellWithFakeBd(t, tc.script+`printf "[]"`, tc.env,
				ephemeralHoldFallbackFakeBd(ephemeralHoldFallbackRows(tc.assignee, tc.deps)))
			var rows []struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
				t.Fatalf("invalid work-query output %q: %v", out, err)
			}
			if len(rows) != 1 || rows[0].ID != "b-ready" {
				t.Fatalf("work query returned %q, want only b-ready behind the held a-held", out)
			}
		})
	}
}

// The full effective work query, with routed work on offer: the held ephemeral
// head must not hand the session routed work while its own b-ready waits.
func TestEffectiveWorkQueryEphemeralFallbackServesUnheldAssignment(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	bdScript := strings.Replace(ephemeralHoldFallbackFakeBd(ephemeralHoldFallbackRows("sess-1", 0)),
		`  ready) printf '[]' ;;`,
		`  ready)
    case "$*" in
      *--assignee=*|*gc.root_bead_id*) printf '[]' ;;
      *) printf '%s' '[{"id":"routed","metadata":{"gc.routed_to":"rig/worker"}}]' ;;
    esac ;;`, 1)
	out := runShellWithFakeBd(t, (&Agent{Name: "worker", Dir: "rig"}).EffectiveWorkQuery(), map[string]string{
		"GC_SESSION_ID": "sess-1", "GC_SESSION_ORIGIN": "ephemeral",
	}, bdScript)
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("invalid work-query output %q: %v", out, err)
	}
	if len(rows) != 1 || rows[0].ID != "b-ready" {
		t.Fatalf("work query returned %q, want only b-ready", out)
	}
}
