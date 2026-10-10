package config

import (
	"encoding/json"
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

func TestAssignedReadyFiltersHoldsBeforeReaderLimit(t *testing.T) {
	held := "[" + strings.TrimSuffix(strings.Repeat(`{"id":"held","labels":["`+beadmeta.HoldMayorLabel+`"]},`, 20), ",") + "]"
	bdScript := `#!/bin/sh
case "$1:$*" in
  ready:*--assignee=*--exclude-label*) printf '%s' '[{"id":"assigned","labels":[]}]' ;;
  ready:*--assignee=*) printf '%s' '` + held + `' ;;
  ready:*) printf '%s' '[{"id":"routed","metadata":{"gc.routed_to":"rig/worker"}}]' ;;
  *) printf '[]' ;;
esac
`
	out := runShellWithFakeBd(t, (&Agent{Name: "worker", Dir: "rig"}).EffectiveWorkQuery(), map[string]string{
		"GC_SESSION_ID": "sess-1", "GC_SESSION_ORIGIN": "ephemeral",
	}, bdScript)
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		t.Fatalf("invalid work-query output %q: %v", out, err)
	}
	if len(rows) != 1 || rows[0].ID != "assigned" {
		t.Fatalf("work query returned %q, want the runnable assignment beyond held rows", out)
	}
}
