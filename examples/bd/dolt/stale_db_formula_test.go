package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/orders"
)

// staleDBFilteredEnv scrubs the host's Gas City environment before a rendered
// formula shell runs. BEADS_ACTOR is named explicitly: it carries no GC_ prefix,
// so filteredEnv's prefix scrub misses it, and the terminal close's --actor
// falls back to it — a host that exports it (any gc session running this
// suite) would otherwise decide what the recorded close line says.
func staleDBFilteredEnv(keys ...string) []string {
	keys = append(keys,
		"GC_BEAD_ID",
		"GC_TRIGGER_BEAD_ID",
		"GC_ESCALATE_SCRIPT",
		"GC_ESCALATE_SEARCH_PACKS",
		"GC_ESCALATION_RECIPIENT",
		"GC_SYSTEM_PACKS_DIR",
		"GC_MAINTENANCE_DONE_TARGET",
		"BEADS_ACTOR",
	)
	return filteredEnv(keys...)
}

func TestStaleDBFormulaRuntimeContract(t *testing.T) {
	root := repoRoot(t)
	f, err := formula.NewParser().ParseFile(filepath.Join(root, "formulas", "mol-dog-stale-db.toml"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	if len(f.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1 so shell state stays inside one formula step", len(f.Steps))
	}

	desc := f.Steps[0].Description
	for _, want := range []string{
		`set -euo pipefail`,
		`WORK_BEAD="${GC_BEAD_ID:-${GC_TRIGGER_BEAD_ID:-$(gc hook current --id-only)}}"`,
		`TMP_DIR=$(mktemp -d`,
		`trap cleanup EXIT`,
		`drain_ack_once()`,
		`gc dolt-cleanup --json --probe > "$SCAN_FILE"`,
		`gc dolt-cleanup --json --probe --force --max-orphan-dbs "{{max_orphans_for_sql}}" > "$APPLY_FILE"`,
		`jq -r '.dropped.count // 0'`,
		`jq -r '[.dropped.skipped[]? | select(.reason == "invalid-identifier")] | length'`,
		`jq -r '[.force_blockers[]?] | length'`,
		`jq -r '.reaped.targets | length'`,
		`gc event emit mol-dog-stale-db.scan`,
		`gc event emit mol-dog-stale-db.drop`,
		`gc event emit mol-dog-stale-db.purge`,
		`gc event emit mol-dog-stale-db.reap`,
		`gc event emit mol-dog-stale-db.done`,
		`gc event emit mol-dog-stale-db.escalate`,
		`if [ "$APPLIED" -eq 1 ] && [ "$MISSED_PURGE_BYTES" -gt 0 ]; then`,
		`leaving work bead open`,
		`maintenance_notice "MAINTENANCE_WARN: $ORPHAN_TOTAL Dolt orphan(s) seen this scan`,
		`maintenance_notice "MAINTENANCE_DONE: stale-db - orphans: ${ORPHAN_TOTAL}, applied: ${APPLIED}, escalated: ${ESCALATED}"`,
		`escalated=${ESCALATED}`,
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("formula step missing %q", want)
		}
	}
	for _, bad := range []string{
		`/tmp/dolt-cleanup`,
		`gc nudge deacon`,
		`gc session nudge deacon`,
		`GC_BEAD_ID:-<work-bead>`,
		`GC_BEAD_ID:?`,
		`Dolt orphan(s) detected`,
	} {
		if strings.Contains(desc, bad) {
			t.Errorf("formula step still contains retired or leaky pattern %q", bad)
		}
	}
}

func TestStaleDBFormulaRenderedShellIsStrictAndValid(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	for _, want := range []string{
		`set -euo pipefail`,
		`WORK_BEAD="${GC_BEAD_ID:-${GC_TRIGGER_BEAD_ID:-$(gc hook current --id-only)}}"`,
		`if [ "$PROTECTED_NON_BASELINE" -ge "10" ]; then`,
		`protected_non_baseline: ((.summary.protected_total // 0) - (.summary.protected_by_kind["active-rig"] // 0))`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("rendered script missing %q", want)
		}
	}

	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

func TestStaleDBFormulaApplyErrorsLeaveWorkOpen(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[{"name":"dolt_tmp","error":"drop failed"}]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    case " $* " in
      *" --force "*) cat "$GC_TEST_APPLY_JSON" ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err == nil {
		t.Fatalf("rendered script exited successfully; want apply errors to fail before success close\nlog:\n%s\noutput:\n%s", log, out)
	}
	for _, want := range []string{
		"bd update bead-1 --append-notes",
		"## apply (--force, refused)",
		"gc event emit mol-dog-stale-db.escalate",
		"gc runtime drain-ack",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	for _, forbidden := range []string{
		"gc event emit mol-dog-stale-db.drop",
		"gc event emit mol-dog-stale-db.purge",
		"gc event emit mol-dog-stale-db.reap",
		"gc event emit mol-dog-stale-db.done",
	} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("rendered script logged forbidden success path %q despite apply errors\nlog:\n%s\noutput:\n%s", forbidden, log, out)
		}
	}
	if strings.Contains(log, "bd close bead-1") {
		t.Fatalf("rendered script closed bead successfully despite apply errors\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestStaleDBFormulaApplyCommandFailureAppendsApplyJSON(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[{"name":"dolt_tmp","error":"drop failed"}]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1},"errors":[{"stage":"drop","error":"drop failed"}]}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    case " $* " in
      *" --force "*)
        cat "$GC_TEST_APPLY_JSON"
        exit 42
        ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err == nil {
		t.Fatalf("rendered script exited successfully; want failed apply command to keep work open\nlog:\n%s\noutput:\n%s", log, out)
	}
	if !strings.Contains(log, "## apply (--force, failed)") {
		t.Fatalf("failed apply JSON was not appended to work bead\nlog:\n%s\noutput:\n%s", log, out)
	}
	if !strings.Contains(log, `"stage":"drop"`) {
		t.Fatalf("appended apply note missing JSON errors\nlog:\n%s\noutput:\n%s", log, out)
	}
	if strings.Contains(log, "bd close bead-1") {
		t.Fatalf("rendered script closed bead despite failed apply command\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestStaleDBFormulaDryRunFailureAppendsScanJSON(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    cat "$GC_TEST_SCAN_JSON"
    exit 42
    ;;
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err == nil {
		t.Fatalf("rendered script exited successfully; want dry-run failure to keep work open\nlog:\n%s\noutput:\n%s", log, out)
	}
	if !strings.Contains(log, "bd update bead-1 --append-notes") {
		t.Fatalf("dry-run failure did not append scan JSON to work bead\nlog:\n%s\noutput:\n%s", log, out)
	}
	if strings.Contains(log, "bd close bead-1") {
		t.Fatalf("rendered script closed bead despite dry-run failure\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestStaleDBFormulaCleanApplyClosesWorkAndUsesDBThreshold(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":20,"failed":[]},"purge":{"bytes_reclaimed":1000},"reaped":{"count":0,"targets":[{"pid":1},{"pid":2}]},"summary":{"bytes_freed_disk":1000,"bytes_freed_rss":200,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":20,"failed":[]},"purge":{"bytes_reclaimed":1000},"reaped":{"count":2,"targets":[{"pid":1},{"pid":2}]},"summary":{"bytes_freed_disk":1000,"bytes_freed_rss":200,"errors_total":0}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    echo "gc $*" >> "$GC_TEST_LOG"
    case " $* " in
      *" --force "*) cat "$GC_TEST_APPLY_JSON" ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"GC_BEAD_ID=bead-1",
		"GC_ALIAS=dog-alpha",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err != nil {
		t.Fatalf("rendered script failed: %v\nlog:\n%s\noutput:\n%s", err, log, out)
	}
	for _, want := range []string{
		"gc dolt-cleanup --json --probe --force --max-orphan-dbs 20",
		"gc event emit mol-dog-stale-db.done --message 1200 bytes freed; 0 errors",
		"bd close bead-1 --actor dog-alpha",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	if strings.Contains(log, "mol-dog-stale-db.escalate") {
		t.Fatalf("rendered script escalated at dropped.count == max_orphans_for_sql; want apply because threshold is >\nlog:\n%s\noutput:\n%s", log, out)
	}
}

// TestStaleDBFormulaCloseUsesClaimIdentityForUnaliasedPool pins the close
// actor chain for an unaliased pool dog, the shape every bd-pack dog has. Such
// a session claims its wisp under its session bead ID (gc hook --claim records
// alias > GC_SESSION_ID) and exports that same ID as BEADS_ACTOR, while
// GC_SESSION_NAME stays the runtime name (<template>-<beadID>). bd fences the
// close on actor == assignee byte for byte, so closing as the session name is
// rejected and the successful run is left open (the #5716 shape). GC_ALIAS is
// exported as an empty string rather than left unset, so every tier must use
// `:-` (empty-or-unset) and not `-`.
func TestStaleDBFormulaCloseUsesClaimIdentityForUnaliasedPool(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{
			name: "beads actor",
			env:  []string{"GC_ALIAS=", "BEADS_ACTOR=gc-dog7", "GC_SESSION_ID=gc-dog7", "GC_SESSION_NAME=dolt__dog-gc-dog7"},
			want: "bd close bead-1 --actor gc-dog7",
		},
		{
			name: "session id when beads actor absent",
			env:  []string{"GC_ALIAS=", "GC_SESSION_ID=gc-dog7", "GC_SESSION_NAME=dolt__dog-gc-dog7"},
			want: "bd close bead-1 --actor gc-dog7",
		},
		{
			name: "session name last",
			env:  []string{"GC_ALIAS=", "BEADS_ACTOR=", "GC_SESSION_NAME=dog-session-7"},
			want: "bd close bead-1 --actor dog-session-7",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runStaleDBFormulaCloseActorCase(t, tc.env, tc.want)
		})
	}
}

func runStaleDBFormulaCloseActorCase(t *testing.T, identityEnv []string, wantClose string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":20,"failed":[]},"purge":{"bytes_reclaimed":1000},"reaped":{"count":0,"targets":[{"pid":1},{"pid":2}]},"summary":{"bytes_freed_disk":1000,"bytes_freed_rss":200,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":20,"failed":[]},"purge":{"bytes_reclaimed":1000},"reaped":{"count":2,"targets":[{"pid":1},{"pid":2}]},"summary":{"bytes_freed_disk":1000,"bytes_freed_rss":200,"errors_total":0}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    echo "gc $*" >> "$GC_TEST_LOG"
    case " $* " in
      *" --force "*) cat "$GC_TEST_APPLY_JSON" ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	cmd.Env = append(cmd.Env, identityEnv...)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err != nil {
		t.Fatalf("rendered script failed: %v\nlog:\n%s\noutput:\n%s", err, log, out)
	}
	for _, want := range []string{
		"gc dolt-cleanup --json --probe --force --max-orphan-dbs 20",
		"gc event emit mol-dog-stale-db.done --message 1200 bytes freed; 0 errors",
		wantClose,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	if strings.Contains(log, "mol-dog-stale-db.escalate") {
		t.Fatalf("rendered script escalated at dropped.count == max_orphans_for_sql; want apply because threshold is >\nlog:\n%s\noutput:\n%s", log, out)
	}
}

// TestStaleDBFormulaResolvesWorkBeadViaHookCurrentChain proves the formula
// runs in a real pool shell, where neither GC_BEAD_ID nor GC_TRIGGER_BEAD_ID
// is guaranteed: the bead-id chain must fall through to
// `gc hook current --id-only`, the back-channel stamped by `gc hook --claim`.
// Before this chain the formula required GC_BEAD_ID — an env var no session
// shell has ever received — so even a correctly claiming dog aborted at the
// WORK_BEAD guard (ga-2q2r0).
func TestStaleDBFormulaResolvesWorkBeadViaHookCurrentChain(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":100},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":100,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":100},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":100,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "hook current")
    echo "gc $*" >> "$GC_TEST_LOG"
    echo "bead-hook-1"
    ;;
  "dolt-cleanup "*)
    echo "gc $*" >> "$GC_TEST_LOG"
    case " $* " in
      *" --force "*) cat "$GC_TEST_APPLY_JSON" ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err != nil {
		t.Fatalf("rendered script failed: %v\nlog:\n%s\noutput:\n%s", err, log, out)
	}
	for _, want := range []string{
		"gc hook current --id-only",
		"bd close bead-hook-1",
		"gc runtime drain-ack",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
}

// TestStaleDBFormulaFailsLoudlyWhenNoWorkBeadIDResolvable pins the chain's
// failure mode: a shell that cannot name its bead through any chain element
// must abort before touching Dolt, not run the cleanup and silently skip the
// close it owes.
func TestStaleDBFormulaFailsLoudlyWhenNoWorkBeadIDResolvable(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	writeTestFile(t, logPath, "")
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
echo "gc $*" >> "$GC_TEST_LOG"
case "${1:-} ${2:-}" in
  "hook current")
    echo "gc hook current: session has no current claim" >&2
    exit 1
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("PATH", "TMPDIR", "GC_TEST_LOG"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err == nil {
		t.Fatalf("rendered script succeeded with no resolvable work bead id\nlog:\n%s\noutput:\n%s", log, out)
	}
	if strings.Contains(log, "dolt-cleanup") {
		t.Fatalf("script must abort before running cleanup when it cannot name its bead\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestStaleDBFormulaPurgeOnlyScanApplies(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    echo "gc $*" >> "$GC_TEST_LOG"
    case " $* " in
      *" --force "*) cat "$GC_TEST_APPLY_JSON" ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err != nil {
		t.Fatalf("rendered script failed: %v\nlog:\n%s\noutput:\n%s", err, log, out)
	}
	for _, want := range []string{
		"gc dolt-cleanup --json --probe --force --max-orphan-dbs 20",
		"gc event emit mol-dog-stale-db.done --message 4096 bytes freed; 0 errors",
		"bd close bead-1",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	if strings.Contains(log, "mol-dog-stale-db.escalate") {
		t.Fatalf("rendered script escalated purge-only cleanup\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestStaleDBFormulaPurgeOnlyApplySQLFailureLeavesWorkOpen(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	writeTestFile(t, scanPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`)
	writeTestFile(t, applyPath, `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"ok":false,"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":2},"errors":[{"stage":"drop","error":"open dolt connection: refused"},{"stage":"purge","error":"open dolt connection: refused"}]}`)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    echo "gc $*" >> "$GC_TEST_LOG"
    case " $* " in
      *" --force "*)
        cat "$GC_TEST_APPLY_JSON"
        exit 42
        ;;
      *) cat "$GC_TEST_SCAN_JSON" ;;
    esac
    ;;
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    echo "gc $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  update|close)
    echo "bd $*" >> "$GC_TEST_LOG"
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_APPLY_JSON"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_APPLY_JSON="+applyPath,
	)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	log := string(logData)
	if err == nil {
		t.Fatalf("rendered script exited successfully; want SQL-backed apply failure to keep work open\nlog:\n%s\noutput:\n%s", log, out)
	}
	for _, want := range []string{
		"gc dolt-cleanup --json --probe --force --max-orphan-dbs 20",
		"bd update bead-1 --append-notes",
		"## apply (--force, failed)",
		`"stage":"purge"`,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("command log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	if strings.Contains(log, "bd close bead-1") {
		t.Fatalf("rendered script closed bead despite SQL-backed apply failure\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestStaleDBFormulaExitZeroMaxOrphanRefusalLeavesWorkOpenWithoutSuccessEvents(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
		scanJSON:  `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":20,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`,
		applyJSON: `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":21,"failed":[]},"purge":{"ok":false,"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1},"errors":[{"stage":"drop","kind":"max-orphan-refusal","error":"stale database threshold tripped"}]}`,
	})
	if err == nil {
		t.Fatalf("rendered script exited successfully; want exit-zero max-orphan refusal to keep work open\nlog:\n%s\noutput:\n%s", log, out)
	}
	for _, forbidden := range []string{
		"gc event emit mol-dog-stale-db.drop",
		"gc event emit mol-dog-stale-db.purge",
		"gc event emit mol-dog-stale-db.reap",
		"bd close bead-1",
	} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("exit-zero max-orphan refusal logged forbidden success path %q\nlog:\n%s\noutput:\n%s", forbidden, log, out)
		}
	}
	for _, want := range []string{
		"bd update bead-1 --append-notes",
		"## apply (--force, refused)",
		"apply refused by max-orphan safety guard",
		"gc event emit mol-dog-stale-db.escalate",
		"gc runtime drain-ack",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("exit-zero max-orphan refusal log missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
}

func TestStaleDBFormulaDryRunForceBlockersLeaveWorkOpenBeforeApply(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
		scanJSON: `{"schema":"gc.dolt.cleanup.v1","force_blockers":[{"kind":"rig-protection","name":"missing","error":"missing metadata"}],"dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`,
		wantNote: "## scan (dry-run)",
		wantLog:  "force blocker",
	})
	if err == nil {
		t.Fatalf("rendered script exited successfully; want dry-run force blockers to keep work open\nlog:\n%s\noutput:\n%s", log, out)
	}
	for _, want := range []string{
		"gc event emit mol-dog-stale-db.escalate",
		"gc mail send human -s",
		"gc runtime drain-ack",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("force-blocker path missing %q\nlog:\n%s\noutput:\n%s", want, log, out)
		}
	}
	for _, forbidden := range []string{
		"gc dolt-cleanup --json --probe --force",
		"gc event emit mol-dog-stale-db.drop",
		"gc event emit mol-dog-stale-db.purge",
		"gc event emit mol-dog-stale-db.reap",
		"gc event emit mol-dog-stale-db.done",
		"bd close bead-1",
	} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("force-blocker path logged forbidden command %q\nlog:\n%s\noutput:\n%s", forbidden, log, out)
		}
	}
}

type staleDBFailureCase struct {
	scanJSON     string
	scanExit     string
	applyJSON    string
	applyExit    string
	failContains string
	wantNote     string
	wantLog      string
	forbidLog    string
	forbidOutput string
	// env is appended after the scrubbed harness environment, so a case can
	// stand in for one runtime identity shape.
	env []string
	// dir is the cwd the rendered script starts in ("" = the test's own).
	dir string
}

func TestStaleDBFormulaFailurePathsDrainAck(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	applyScanJSON := `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`
	for _, tc := range []struct {
		name string
		spec staleDBFailureCase
	}{
		{
			name: "dry run command failure",
			spec: staleDBFailureCase{
				scanJSON: `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1}}`,
				scanExit: "42",
				wantNote: "## scan (dry-run failed)",
			},
		},
		{
			name: "invalid scan JSON",
			spec: staleDBFailureCase{
				scanJSON: `{"schema":"wrong"}`,
				wantNote: "## scan (invalid JSON)",
			},
		},
		{
			name: "apply command failure",
			spec: staleDBFailureCase{
				scanJSON:  applyScanJSON,
				applyJSON: `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[{"name":"dolt_tmp","error":"drop failed"}]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":1},"errors":[{"stage":"drop","error":"drop failed"}]}`,
				applyExit: "42",
				wantNote:  "## apply (--force, failed)",
				wantLog:   "gc dolt cleanup --server-down-ok",
				forbidLog: "agent with --server-down-ok",
			},
		},
		{
			name: "invalid apply JSON",
			spec: staleDBFailureCase{
				scanJSON:  applyScanJSON,
				applyJSON: `{"schema":"wrong"}`,
				wantNote:  "## apply (--force, invalid JSON)",
			},
		},
		{
			name: "apply misses dry-run reclaimable bytes",
			spec: staleDBFailureCase{
				scanJSON:     `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"bytes_reclaimed":4096},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":4096,"bytes_freed_rss":0,"errors_total":0}}`,
				applyJSON:    `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":1,"failed":[]},"purge":{"ok":true,"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`,
				wantNote:     "## apply (--force)",
				wantLog:      "apply missed 4096 reclaimable bytes",
				forbidLog:    "apply reported",
				forbidOutput: "apply reported",
			},
		},
		{
			name: "invalid identifier skipped in scan",
			spec: staleDBFailureCase{
				scanJSON: `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[],"skipped":[{"name":"testdb_bad;drop","reason":"invalid-identifier"}]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`,
				wantNote: "## scan (dry-run)",
				wantLog:  "invalid stale database identifier",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, out, err := runStaleDBFormulaFailureCase(t, tc.spec)
			if err == nil {
				t.Fatalf("rendered script exited successfully; want failure path to keep work open\nlog:\n%s\noutput:\n%s", log, out)
			}
			if !strings.Contains(log, "gc runtime drain-ack") {
				t.Fatalf("failure path did not drain-ack before exit\nlog:\n%s\noutput:\n%s", log, out)
			}
			if tc.spec.wantNote != "" && !strings.Contains(log, tc.spec.wantNote) {
				t.Fatalf("failure path did not append expected note %q\nlog:\n%s\noutput:\n%s", tc.spec.wantNote, log, out)
			}
			if tc.spec.wantLog != "" && !strings.Contains(log, tc.spec.wantLog) {
				t.Fatalf("failure path log missing %q\nlog:\n%s\noutput:\n%s", tc.spec.wantLog, log, out)
			}
			if tc.spec.forbidLog != "" && strings.Contains(log, tc.spec.forbidLog) {
				t.Fatalf("failure path log still contains unsupported copy %q\nlog:\n%s\noutput:\n%s", tc.spec.forbidLog, log, out)
			}
			if tc.spec.forbidOutput != "" && strings.Contains(string(out), tc.spec.forbidOutput) {
				t.Fatalf("failure path output still contains unsupported copy %q\nlog:\n%s\noutput:\n%s", tc.spec.forbidOutput, log, out)
			}
			if strings.Contains(log, "bd close bead-1") {
				t.Fatalf("failure path closed bead despite non-zero outcome\nlog:\n%s\noutput:\n%s", log, out)
			}
		})
	}
}

func TestStaleDBFormulaSuccessPathFailuresDrainAck(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	cleanScan := `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[],"skipped":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`
	for _, tc := range []struct {
		name        string
		fail        string
		env         []string
		wantFailure bool
		// wantLog lines must all be recorded after the injected failure, and
		// wantMail must appear on the escalation mail line itself.
		wantLog  []string
		wantMail string
	}{
		{
			name: "scan event failure",
			fail: "gc event emit mol-dog-stale-db.scan",
		},
		{
			name: "scan note failure",
			fail: "bd update bead-1 --append-notes",
		},
		{
			// The terminal close is the one call whose failure is not
			// nonessential: the bead stays open behind a drained session and
			// the controller re-dispatches against it forever (we-m34w5). A
			// refusal must reach the operator on every channel the script's
			// other operator paths use — escalation mail carrying bd's own
			// refusal text, the escalate event, and the maintenance nudge —
			// before the fail-open exit, never as a bare non-zero status.
			name:        "close failure",
			fail:        "bd close bead-1",
			env:         []string{"GC_MAINTENANCE_DONE_TARGET=health"},
			wantFailure: true,
			wantLog: []string{
				"gc mail send human -s ESCALATION: stale-db terminal close refused for bead-1 [HIGH] -m terminal close refused for bead-1 (actor presented: none): ",
				"gc event emit mol-dog-stale-db.escalate --message terminal close refused for bead-1 (actor presented: none): ",
				"gc session nudge health MAINTENANCE_ESCALATE: stale-db terminal close REFUSED for bead-1",
			},
			wantMail: `assignee is "dog-2", actor is "dog-1"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
				scanJSON:     cleanScan,
				failContains: tc.fail,
				env:          tc.env,
			})
			if tc.wantFailure && err == nil {
				t.Fatalf("rendered script exited successfully; want %q failure to preserve non-zero status\nlog:\n%s\noutput:\n%s", tc.fail, log, out)
			}
			if !tc.wantFailure && err != nil {
				t.Fatalf("rendered script failed; want %q to be non-fatal\nlog:\n%s\noutput:\n%s", tc.fail, log, out)
			}
			if !strings.Contains(log, "gc runtime drain-ack") {
				t.Fatalf("%q path did not drain-ack\nlog:\n%s\noutput:\n%s", tc.fail, log, out)
			}
			if !strings.Contains(log, tc.fail) {
				t.Fatalf("command log missing injected failure %q\nlog:\n%s\noutput:\n%s", tc.fail, log, out)
			}
			for _, want := range tc.wantLog {
				if !strings.Contains(log, want) {
					t.Fatalf("%q path did not record %q\nlog:\n%s\noutput:\n%s", tc.fail, want, log, out)
				}
			}
			if tc.wantMail != "" {
				mail := staleDBLogLine(log, "gc mail send ")
				if !strings.Contains(mail, tc.wantMail) {
					t.Fatalf("%q path escalation mail does not carry bd's refusal %q\nmail: %q\nlog:\n%s\noutput:\n%s", tc.fail, tc.wantMail, mail, log, out)
				}
			}
			if !tc.wantFailure && !strings.Contains(log, "bd close bead-1") {
				t.Fatalf("%q path did not close work after nonessential failure\nlog:\n%s\noutput:\n%s", tc.fail, log, out)
			}
		})
	}
}

// TestStaleDBFormulaTerminalCloseCarriesClaimIdentity pins the --actor the
// terminal close presents to the identity `gc hook --claim` recorded the
// assignee under: GC_ALIAS, then the session bead id, then BEADS_ACTOR,
// GC_AGENT and GC_SESSION_NAME (hookClaimAssigneeIdentity in cmd/gc). bd's
// ownership guard compares the two verbatim. The pool row is the shipped dog —
// max_active_sessions with no namepool — whose spawn carries a blank GC_ALIAS,
// its session bead id in GC_SESSION_ID, and the slot name every occupant of
// that slot reuses in BEADS_ACTOR, GC_AGENT and GC_SESSION_NAME. Its claim is
// recorded under the bead id, and presenting the slot name instead is exactly
// the refusal that re-dispatched a fresh dog every firing (we-m34w5).
func TestStaleDBFormulaTerminalCloseCarriesClaimIdentity(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}

	const (
		sessionID = "gcg-session-557fc1017792caa9a01355325b212416"
		slot      = "test-city--dog-1-pool"
		alias     = "test-city/dog"
	)
	cleanScan := `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[],"skipped":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`
	for _, tc := range []struct {
		name        string
		env         []string
		wantClose   string
		forbidClose string
	}{
		{
			name:        "unaliased pool dog closes as its session bead id, not the slot name",
			env:         []string{"GC_ALIAS=", "GC_SESSION_ID=" + sessionID, "BEADS_ACTOR=" + slot, "GC_AGENT=" + slot, "GC_SESSION_NAME=" + slot},
			wantClose:   "bd close bead-1 --actor " + sessionID + " --reason ",
			forbidClose: "--actor " + slot,
		},
		{
			name:      "aliased session keeps its alias",
			env:       []string{"GC_ALIAS=" + alias, "GC_SESSION_ID=" + sessionID, "BEADS_ACTOR=" + alias, "GC_AGENT=" + alias, "GC_SESSION_NAME=test-city--dog"},
			wantClose: "bd close bead-1 --actor " + alias + " --reason ",
		},
		{
			name:      "no session bead id falls back to BEADS_ACTOR",
			env:       []string{"GC_ALIAS=", "GC_SESSION_ID=", "BEADS_ACTOR=" + slot, "GC_AGENT=" + slot},
			wantClose: "bd close bead-1 --actor " + slot + " --reason ",
		},
		{
			name:        "no identity at all leaves the actor to bd",
			wantClose:   "bd close bead-1 --reason ",
			forbidClose: "--actor",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{scanJSON: cleanScan, env: tc.env})
			if err != nil {
				t.Fatalf("rendered script failed: %v\nlog:\n%s\noutput:\n%s", err, log, out)
			}
			closeLine := staleDBLogLine(log, "bd close ")
			if !strings.HasPrefix(closeLine, tc.wantClose) {
				t.Fatalf("terminal close = %q, want prefix %q\nlog:\n%s\noutput:\n%s", closeLine, tc.wantClose, log, out)
			}
			if tc.forbidClose != "" && strings.Contains(closeLine, tc.forbidClose) {
				t.Fatalf("terminal close %q presents %q\nlog:\n%s\noutput:\n%s", closeLine, tc.forbidClose, log, out)
			}
		})
	}
}

// TestStaleDBFormulaClosesFromStoreRootWhateverTheCwd pins ga-w8ptue's first
// defect: a dog that ran the script from a temp dir had its bare `bd close`
// find no store. The script now enters the first store root (rig root, then
// the city in gc's own order) that holds a .beads store, so every bd call runs
// in that store from any starting cwd, and the close is the work bead's.
func TestStaleDBFormulaClosesFromStoreRootWhateverTheCwd(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}
	newStore := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".beads"), 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("EvalSymlinks: %v", err)
		}
		return resolved
	}
	city := newStore(t)
	otherCity := newStore(t)
	rig := newStore(t)
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{name: "city path", env: []string{"GC_CITY_PATH=" + city}, want: city},
		{name: "rig root wins over the city", env: []string{"GC_RIG_ROOT=" + rig, "GC_CITY_PATH=" + city}, want: rig},
		{name: "empty rig root falls back to the city", env: []string{"GC_RIG_ROOT=", "GC_CITY_PATH=" + city}, want: city},
		{name: "GC_CITY outranks GC_CITY_PATH, as in gc", env: []string{"GC_CITY=" + otherCity, "GC_CITY_PATH=" + city}, want: otherCity},
		{name: "GC_CITY_ROOT as the last resort", env: []string{"GC_CITY_ROOT=" + city}, want: city},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The stores are shared across cases: start each with no close recorded.
			for _, store := range []string{city, otherCity, rig} {
				if err := os.Remove(filepath.Join(store, ".beads", "closed")); err != nil && !os.IsNotExist(err) {
					t.Fatalf("Remove: %v", err)
				}
			}
			log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
				scanJSON: staleDBCleanScan,
				dir:      t.TempDir(), // no .beads here: the dog's temp-dir cwd
				env:      append([]string{"GC_TEST_BD_NEEDS_STORE=1"}, tc.env...),
			})
			if err != nil {
				t.Fatalf("rendered script failed from a store-less cwd: %v\nlog:\n%s\noutput:\n%s", err, log, out)
			}
			if argv := staleDBLogLine(log, "bd-close-argv "); argv != "bd-close-argv [close] [bead-1] [--reason] [Stale DB scan complete (orphans=0, applied=0, escalated=0)]" {
				t.Fatalf("terminal close argv = %q, want exactly close bead-1 --reason <one argument>\nlog:\n%s", argv, log)
			}
			closed, err := os.ReadFile(filepath.Join(tc.want, ".beads", "closed"))
			if err != nil || string(closed) != "bead-1\n" {
				t.Fatalf("store %s recorded closed %q (err %v), want exactly bead-1\nlog:\n%s", tc.want, closed, err, log)
			}
			cwds := 0
			for _, line := range strings.Split(log, "\n") {
				if cwd, ok := strings.CutPrefix(line, "bd-cwd "); ok {
					cwds++
					if cwd != tc.want {
						t.Fatalf("a bd call ran in %q, want the store root %q\nlog:\n%s", cwd, tc.want, log)
					}
				}
			}
			if cwds < 2 {
				t.Fatalf("recorded %d bd working directories, want the note and the close at least\nlog:\n%s", cwds, log)
			}
		})
	}
}

// TestStaleDBFormulaStoreResolutionFailureIsNotAHumanRefusal pins ga-w8ptue's
// second defect: "no beads database found" is not an ownership refusal, so it
// must not reach the human escalation route as a HIGH "close refused". It
// still fails open (the bead stays open for the next firing), emits its own
// event, nudges the maintenance target, and mails the store-error route only
// when one is configured.
func TestStaleDBFormulaStoreResolutionFailureIsNotAHumanRefusal(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}
	for _, tc := range []struct {
		name     string
		env      []string
		wantMail string
	}{
		// No route: one plain infrastructure mail to the escalation recipient,
		// never a HIGH refusal (ga-w8ptue, ga-7ws7gu).
		{name: "no store-error route", env: nil, wantMail: "gc mail send human -s stale-db: terminal close could not reach a bead store for bead-1"},
		{name: "store-error route configured", env: []string{"GC_STORE_ERROR_RECIPIENT=woodhouse"}, wantMail: "gc mail send woodhouse -s stale-db: terminal close could not reach a bead store for bead-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No store root in the environment and none under the cwd.
			log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
				scanJSON: staleDBCleanScan,
				dir:      t.TempDir(),
				env:      append([]string{"GC_TEST_BD_NEEDS_STORE=1", "GC_MAINTENANCE_DONE_TARGET=health"}, tc.env...),
			})
			if err == nil {
				t.Fatalf("rendered script exited successfully; want the unclosed bead to fail open\nlog:\n%s\noutput:\n%s", log, out)
			}
			for _, forbidden := range []string{"gc mail send human -s ESCALATION", "[HIGH]", "mol-dog-stale-db.escalate"} {
				if strings.Contains(log, forbidden) {
					t.Fatalf("store-resolution failure took the refusal path (%q)\nlog:\n%s\noutput:\n%s", forbidden, log, out)
				}
			}
			for _, want := range []string{
				"bd close bead-1",
				"gc event emit mol-dog-stale-db.store-unresolved --message terminal close for bead-1 could not reach a bead store from ",
				"gc session nudge health MAINTENANCE_WARN: stale-db terminal close for bead-1 could not reach a bead store",
				"gc runtime drain-ack",
			} {
				if !strings.Contains(log, want) {
					t.Fatalf("store-resolution failure did not record %q\nlog:\n%s\noutput:\n%s", want, log, out)
				}
			}
			mail := staleDBLogLine(log, "gc mail send ")
			if !strings.HasPrefix(mail, tc.wantMail) || strings.Count(log, "gc mail send ") != 1 {
				t.Fatalf("store-error mail = %q (%d mail lines), want exactly one with prefix %q\nlog:\n%s", mail, strings.Count(log, "gc mail send "), tc.wantMail, log)
			}
		})
	}
}

// TestStaleDBFormulaUnusableStoreRootStopsBeforeAnyWork pins the review fix
// on #363: a store root the session names but that holds no store (missing,
// a file, a rig root with no .beads of its own) stops the run as
// store-unresolved before the scan, the notes or the close, with no fallback
// to another store or the cwd, and never on the human route.
func TestStaleDBFormulaUnusableStoreRootStopsBeforeAnyWork(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}
	city := t.TempDir()
	if err := os.Mkdir(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	bareRig := filepath.Join(city, "rigs", "bare") // under a real store, so bd could walk up into it
	if err := os.MkdirAll(bareRig, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// The starting cwd holds a store too: a fallback to the cwd would be caught.
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".beads"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{name: "city path names a missing directory", env: []string{"GC_CITY_PATH=/nonexistent/gc-city"}},
		{name: "city path names a file", env: []string{"GC_CITY_PATH=" + staleDBRegularFile(t)}},
		{name: "rig root without its own store", env: []string{"GC_RIG_ROOT=" + bareRig, "GC_CITY_PATH=" + city}},
		{name: "store-error route configured", env: []string{"GC_CITY_PATH=/nonexistent/gc-city", "GC_STORE_ERROR_RECIPIENT=woodhouse"}},
		// gc would resolve a NAME through its registry, maybe to another city: stop, never guess
		{name: "a city name in GC_CITY ahead of a valid city path", env: []string{"GC_CITY=mycity", "GC_CITY_PATH=" + city}},
		{name: "a relative city path", env: []string{"GC_CITY_PATH=."}},
		{name: "a relative rig root", env: []string{"GC_RIG_ROOT=rigs/bare", "GC_CITY_PATH=" + city}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
				scanJSON: staleDBCleanScan,
				dir:      cwd,
				env:      append([]string{"GC_TEST_BD_NEEDS_STORE=1", "GC_MAINTENANCE_DONE_TARGET=health"}, tc.env...),
			})
			if err == nil {
				t.Fatalf("rendered script exited successfully; want a stop before any work\nlog:\n%s\noutput:\n%s", log, out)
			}
			for _, forbidden := range []string{"bd ", "bd-cwd", "gc dolt-cleanup", "gc mail send human -s ESCALATION", "[HIGH]", "mol-dog-stale-db.escalate"} {
				if strings.Contains(log, forbidden) {
					t.Fatalf("an unusable store root still reached %q\nlog:\n%s\noutput:\n%s", forbidden, log, out)
				}
			}
			for _, want := range []string{
				"gc event emit mol-dog-stale-db.store-unresolved --message ",
				"gc session nudge health MAINTENANCE_WARN: stale-db ",
				"gc runtime drain-ack",
			} {
				if !strings.Contains(log, want) {
					t.Fatalf("an unusable store root did not record %q\nlog:\n%s\noutput:\n%s", want, log, out)
				}
			}
			// With a store-error route the notice goes there; with none, one
			// plain infrastructure mail goes to the escalation recipient
			// (ga-w8ptue: an unread event alone reaches nobody).
			mail := staleDBLogLine(log, "gc mail send ")
			wantMail := "gc mail send human -s stale-db: "
			if strings.Contains(strings.Join(tc.env, " "), "GC_STORE_ERROR_RECIPIENT=") {
				wantMail = "gc mail send woodhouse -s stale-db: "
			}
			if !strings.HasPrefix(mail, wantMail) || strings.Count(log, "gc mail send ") != 1 {
				t.Fatalf("store-error mail = %q (%d mail lines), want exactly one starting %q\nlog:\n%s", mail, strings.Count(log, "gc mail send "), wantMail, log)
			}
			if !strings.HasPrefix(mail, "gc mail send woodhouse") && !strings.Contains(mail, "an infrastructure failure, sent here because no store-error route") {
				t.Fatalf("fallback mail does not say why it came to the escalation recipient: %q", mail)
			}
		})
	}
}

// TestStaleDBFormulaUnreachableStoreAtCloseIsNotAHumanRefusal: the store was
// found, but bd could not open or reach it at the close (texts seen live on
// 2026-11-18). That is not an ownership refusal, so it takes the non-human
// route; a real refusal still goes to human as HIGH.
func TestStaleDBFormulaUnreachableStoreAtCloseIsNotAHumanRefusal(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}
	city := t.TempDir()
	if err := os.Mkdir(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	for _, tc := range []struct {
		name  string
		err   string
		human bool
	}{
		{name: "circuit breaker open", err: "Error: failed to open database: dolt circuit breaker is open: server appears down, failing fast (cooldown 5s)"},
		{name: "server unreachable", err: "Error: failed to open database: Dolt server unreachable at 100.122.207.22:3307: dial tcp 100.122.207.22:3307: i/o timeout"},
		{name: "connection refused", err: "Error: dial tcp 127.0.0.1:51361: connect: connection refused"},
		{name: "connection lost mid-close", err: "Error: close bead-1: driver: bad connection"},
		{name: "connection reset", err: "Error: read tcp 127.0.0.1:53110->127.0.0.1:51361: read: connection reset by peer"},
		{name: "an ownership refusal still reaches human", err: `Error: cannot close bead-1: assignee is "dog-2", actor is "dog-1"; reclaim or use --force to override`, human: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
				scanJSON: staleDBCleanScan,
				dir:      t.TempDir(),
				env:      []string{"GC_TEST_BD_NEEDS_STORE=1", "GC_TEST_BD_CLOSE_ERR=" + tc.err, "GC_CITY_PATH=" + city},
			})
			if err == nil {
				t.Fatalf("rendered script exited successfully; want the unclosed bead to fail open\nlog:\n%s\noutput:\n%s", log, out)
			}
			if !strings.Contains(log, "gc runtime drain-ack") {
				t.Fatalf("no drain-ack\nlog:\n%s", log)
			}
			tookHuman := strings.Contains(log, "gc mail send human -s ESCALATION: stale-db terminal close refused for bead-1 [HIGH]")
			tookStore := strings.Contains(log, "gc event emit mol-dog-stale-db.store-unresolved --message terminal close for bead-1 could not reach a bead store")
			if tookHuman != tc.human || tookStore == tc.human {
				t.Fatalf("human route %v, store-unresolved route %v; want human=%v\nlog:\n%s\noutput:\n%s", tookHuman, tookStore, tc.human, log, out)
			}
		})
	}
}

// TestStaleDBFormulaTerminalCloseRoutesByRefusalText pins ga-7ws7gu: only an
// authority refusal of the terminal close (bd's ownership guard, or the pinned
// flag) reaches the human route as HIGH, and it wins over an availability text
// printed in the same output. Every other failure takes the platform route as
// close-failed, with no advice to close the bead by hand. The texts are bd's
// own at 1.3.1-fleet.20261005.1 (internal/validation/issue.go,
// cmd/bd/close_direct.go).
func TestStaleDBFormulaTerminalCloseRoutesByRefusalText(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not found: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not found: %v", err)
	}
	city := t.TempDir()
	if err := os.Mkdir(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	const (
		human       = "human"
		store       = "store-unresolved"
		closeFailed = "close-failed"
	)
	for _, tc := range []struct {
		name  string
		err   string
		route string
	}{
		{name: "ownership refusal", err: `Error: cannot close bead-1: assignee is "dog-2", actor is "dog-1"; reclaim or use --force to override`, route: human},
		{name: "pinned refusal", err: "Error: cannot modify pinned issue bead-1 (use --force to override)", route: human},
		{name: "refusal beside an availability text", err: "warning: driver: bad connection (retried)\nError: cannot close bead-1: assignee is \"dog-2\", actor is \"dog-1\"; reclaim or use --force to override", route: human},
		{name: "store unreachable", err: "Error: dial tcp 127.0.0.1:51361: connect: connection refused", route: store},
		{name: "gate refusal", err: "Error: cannot close bead-1: gate condition not satisfied: expires in 4m59s (use --force to override)", route: closeFailed},
		{name: "blank error text", err: " ", route: closeFailed},
		{name: "no error text at all", err: "", route: closeFailed},
		{name: "template", err: "Error: cannot modify template bead-1: templates are read-only; use 'bd mol pour' to create a work item", route: closeFailed},
		{name: "unknown failure", err: "Error: something nobody anticipated", route: closeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closeErr := "GC_TEST_BD_CLOSE_ERR=" + tc.err
			if tc.err == "" {
				closeErr = "GC_TEST_BD_CLOSE_SILENT=1" // fail with zero bytes of output
			}
			log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
				scanJSON: staleDBCleanScan,
				dir:      t.TempDir(),
				env: []string{
					"GC_TEST_BD_NEEDS_STORE=1", closeErr, "GC_CITY_PATH=" + city,
					"GC_STORE_ERROR_RECIPIENT=platform-seat", "GC_MAINTENANCE_DONE_TARGET=health",
				},
			})
			if tc.err == "" && !strings.Contains(log, "failed, not an ownership refusal: no error text") {
				t.Fatalf("a silent close failure did not say so\nlog:\n%s", log)
			}
			if err == nil {
				t.Fatalf("rendered script exited successfully; want the unclosed bead to fail open\nlog:\n%s\noutput:\n%s", log, out)
			}
			if !strings.Contains(log, "gc runtime drain-ack") {
				t.Fatalf("no drain-ack\nlog:\n%s", log)
			}
			got := map[string]bool{
				human:       strings.Contains(log, "gc mail send human -s ESCALATION: stale-db terminal close refused for bead-1 [HIGH]"),
				store:       strings.Contains(log, "gc event emit mol-dog-stale-db.store-unresolved --message terminal close for bead-1 could not reach a bead store"),
				closeFailed: strings.Contains(log, "gc event emit mol-dog-stale-db.close-failed --message terminal close for bead-1 failed, not an ownership refusal: "),
			}
			for route, took := range got {
				if took != (route == tc.route) {
					t.Fatalf("routes taken %v; want only %q\nlog:\n%s\noutput:\n%s", got, tc.route, log, out)
				}
			}
			if tc.route == human {
				return
			}
			for _, forbidden := range []string{"[HIGH]", "mol-dog-stale-db.escalate", "--actor <assignee>"} {
				if strings.Contains(log, forbidden) {
					t.Fatalf("non-refusal close failure carried %q\nlog:\n%s", forbidden, log)
				}
			}
			if tc.route == closeFailed {
				for _, want := range []string{
					"gc mail send platform-seat -s stale-db: terminal close failed for bead-1",
					"gc session nudge health MAINTENANCE_WARN: stale-db terminal close for bead-1 failed, not an ownership refusal",
				} {
					if !strings.Contains(log, want) {
						t.Fatalf("close-failed route did not record %q\nlog:\n%s", want, log)
					}
				}
			}
		})
	}
	// With no store-error route, close-failed still tells someone: one plain
	// infrastructure mail to the escalation recipient, not a HIGH refusal.
	t.Run("close-failed with no store-error route", func(t *testing.T) {
		log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
			scanJSON: staleDBCleanScan,
			dir:      t.TempDir(),
			env: []string{
				"GC_TEST_BD_NEEDS_STORE=1", "GC_TEST_BD_CLOSE_ERR=Error: something nobody anticipated",
				"GC_CITY_PATH=" + city, "GC_ESCALATION_RECIPIENT=ops",
			},
		})
		if err == nil {
			t.Fatalf("rendered script exited successfully; want fail open\nlog:\n%s\noutput:\n%s", log, out)
		}
		mail := staleDBLogLine(log, "gc mail send ")
		if !strings.HasPrefix(mail, "gc mail send ops -s stale-db: terminal close failed for bead-1") ||
			!strings.Contains(mail, "an infrastructure failure, sent here because no store-error route") ||
			strings.Count(log, "gc mail send ") != 1 || strings.Contains(log, "[HIGH]") {
			t.Fatalf("no-route close-failed mail = %q; want one plain infrastructure mail to ops\nlog:\n%s", mail, log)
		}
	})
	// A notice that stalls (a store open, a recipient wake) is abandoned at
	// its bound, so the run still drain-acks and fails open (Astra's finding
	// on #370). Without the bound this case waits out the fake's 60 s sleep.
	t.Run("a stalled notice cannot hold the drain-ack", func(t *testing.T) {
		start := time.Now()
		log, out, err := runStaleDBFormulaFailureCase(t, staleDBFailureCase{
			scanJSON: staleDBCleanScan,
			dir:      t.TempDir(),
			env: []string{
				"GC_TEST_BD_NEEDS_STORE=1", "GC_TEST_BD_CLOSE_ERR=Error: something nobody anticipated",
				"GC_CITY_PATH=" + city, "GC_MAINTENANCE_DONE_TARGET=health",
				"GC_TEST_HANG_NOTICES=1", "GC_STALE_DB_NOTICE_TIMEOUT_SECS=1",
			},
		})
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("rendered script exited successfully; want fail open\nlog:\n%s\noutput:\n%s", log, out)
		}
		// Three stalls (the completion nudge, the mail, the warning nudge) at a
		// 1 s bound plus the 2 s TERM grace: about 9 s, measured 21 s on a
		// loaded host. Unbounded, the fake's sleeps take 180 s. 90 s keeps
		// the two apart with room for a busy CI runner.
		if elapsed > 90*time.Second {
			t.Fatalf("run took %s with stalled notices; the bound did not hold\nlog:\n%s", elapsed, log)
		}
		for _, want := range []string{"gc mail send human -s stale-db: terminal close failed for bead-1", "gc session nudge health ", "gc runtime drain-ack"} {
			if !strings.Contains(log, want) {
				t.Fatalf("stalled-notice run did not record %q\nlog:\n%s\noutput:\n%s", want, log, out)
			}
		}
		if !strings.Contains(string(out), "notice abandoned after 1s: gc mail send") {
			t.Fatalf("stalled mail was not reported as abandoned\noutput:\n%s", out)
		}
	})
}

// staleDBRegularFile returns the path of a plain file, for a store-root
// variable that names something which is not a directory.
func staleDBRegularFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "not-a-dir")
	writeTestFile(t, path, "")
	return path
}

const staleDBCleanScan = `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[],"skipped":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`

// staleDBLogLine returns the first recorded command line starting with prefix,
// or "" when the fake binaries never logged one.
func staleDBLogLine(log, prefix string) string {
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func runStaleDBFormulaFailureCase(t *testing.T, tc staleDBFailureCase) (string, []byte, error) {
	t.Helper()
	script := renderStaleDBFormulaShell(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	logPath := filepath.Join(dir, "commands.log")
	scanPath := filepath.Join(dir, "scan.json")
	applyPath := filepath.Join(dir, "apply.json")
	if tc.applyJSON == "" {
		tc.applyJSON = `{"schema":"gc.dolt.cleanup.v1","dropped":{"count":0,"failed":[]},"purge":{"bytes_reclaimed":0},"reaped":{"count":0,"targets":[]},"summary":{"bytes_freed_disk":0,"bytes_freed_rss":0,"errors_total":0}}`
	}
	writeTestFile(t, scanPath, tc.scanJSON)
	writeTestFile(t, applyPath, tc.applyJSON)
	writeTestFile(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
set -euo pipefail
maybe_fail() {
  local rendered="$1"
  if [ -n "${GC_TEST_FAIL_CONTAINS:-}" ] && [[ "$rendered" == *"$GC_TEST_FAIL_CONTAINS"* ]]; then
    exit 70
  fi
}
case "${1:-} ${2:-}" in
  "dolt-cleanup "*)
    echo "gc $*" >> "$GC_TEST_LOG"
    case " $* " in
      *" --force "*)
        cat "$GC_TEST_APPLY_JSON"
        if [ -n "${GC_TEST_APPLY_EXIT:-}" ]; then exit "$GC_TEST_APPLY_EXIT"; fi
        ;;
      *)
        cat "$GC_TEST_SCAN_JSON"
        if [ -n "${GC_TEST_SCAN_EXIT:-}" ]; then exit "$GC_TEST_SCAN_EXIT"; fi
        ;;
    esac
    ;;
  "event emit"|"session nudge"|"runtime drain-ack"|"mail send")
    rendered="gc $*"
    echo "$rendered" >> "$GC_TEST_LOG"
    maybe_fail "$rendered"
    if [ -n "${GC_TEST_HANG_NOTICES:-}" ] && { [ "${1:-} ${2:-}" = "mail send" ] || [ "${1:-} ${2:-}" = "session nudge" ]; }; then
      # Stall the way a send waiting on a store open or a recipient wake does,
      # and ignore TERM (an ignored signal survives exec), so only the bound's
      # KILL ends it. exec, so no orphan holds the test's output pipe open.
      trap '' TERM
      exec sleep 60
    fi
    ;;
  *)
    echo "unexpected gc command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)
	writeTestFile(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env bash
set -euo pipefail
maybe_fail() {
  local rendered="$1"
  if [ -n "${GC_TEST_FAIL_CONTAINS:-}" ] && [[ "$rendered" == *"$GC_TEST_FAIL_CONTAINS"* ]]; then
    if [ "${rendered#bd close }" != "$rendered" ]; then
      # Refuse a close in bd's own ownership-guard words
      # (internal/validation/issue.go, bd 1.3.1-fleet.20261005.1).
      local id
      id=$(printf '%s\n' "$rendered" | awk '{print $3}')
      echo "Error: cannot close ${id}: assignee is \"dog-2\", actor is \"dog-1\"; reclaim or use --force to override" >&2
    else
      echo "bd: ${rendered} failed" >&2
    fi
    exit 1
  fi
}
case "${1:-}" in
  update|close)
    rendered="bd $*"
    echo "$rendered" >> "$GC_TEST_LOG"
    # Like the real bd, find the store from the cwd when asked to.
    if [ -n "${GC_TEST_BD_NEEDS_STORE:-}" ]; then
      echo "bd-cwd $(pwd -P)" >> "$GC_TEST_LOG"
      if [ ! -d .beads ]; then
        echo "Error: no beads database found" >&2
        exit 1
      fi
    fi
    maybe_fail "$rendered"
    if [ -n "${GC_TEST_BD_CLOSE_ERR:-}" ] && [ "${1:-}" = close ]; then
      echo "$GC_TEST_BD_CLOSE_ERR" >&2
      exit 1
    fi
    if [ -n "${GC_TEST_BD_CLOSE_SILENT:-}" ] && [ "${1:-}" = close ]; then
      exit 1
    fi
    if [ -n "${GC_TEST_BD_NEEDS_STORE:-}" ] && [ "${1:-}" = close ]; then
      # Argument boundaries kept, and the close recorded in the store it ran in.
      printf 'bd-close-argv' >> "$GC_TEST_LOG"
      printf ' [%s]' "$@" >> "$GC_TEST_LOG"
      printf '\n' >> "$GC_TEST_LOG"
      printf '%s\n' "${2:-}" >> .beads/closed
    fi
    ;;
  *)
    echo "unexpected bd command: $*" >&2
    exit 64
    ;;
esac
`, 0o755)

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Dir = tc.dir
	cmd.Env = append(staleDBFilteredEnv("GC_BEAD_ID", "PATH", "TMPDIR", "GC_TEST_LOG", "GC_TEST_SCAN_JSON", "GC_TEST_SCAN_EXIT", "GC_TEST_APPLY_JSON", "GC_TEST_APPLY_EXIT", "GC_TEST_FAIL_CONTAINS", "GC_TEST_BD_NEEDS_STORE", "GC_TEST_BD_CLOSE_ERR", "GC_TEST_BD_CLOSE_SILENT", "GC_TEST_HANG_NOTICES", "GC_STALE_DB_NOTICE_TIMEOUT_SECS"),
		"GC_BEAD_ID=bead-1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+dir,
		"GC_TEST_LOG="+logPath,
		"GC_TEST_SCAN_JSON="+scanPath,
		"GC_TEST_SCAN_EXIT="+tc.scanExit,
		"GC_TEST_APPLY_JSON="+applyPath,
		"GC_TEST_APPLY_EXIT="+tc.applyExit,
		"GC_TEST_FAIL_CONTAINS="+tc.failContains,
	)
	cmd.Env = append(cmd.Env, tc.env...)
	out, err := cmd.CombinedOutput()
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("ReadFile(%s): %v\noutput:\n%s", logPath, readErr, out)
	}
	return string(logData), out, err
}

func renderStaleDBFormulaShell(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	f, err := formula.NewParser().ParseFile(filepath.Join(root, "formulas", "mol-dog-stale-db.toml"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(f.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(f.Steps))
	}

	vars := make(map[string]string, len(f.Vars))
	for name, def := range f.Vars {
		if def.Default != nil {
			vars[name] = *def.Default
		}
	}
	rendered := formula.Substitute(f.Steps[0].Description, vars)
	if residual := formula.CheckResidualVars(rendered); len(residual) != 0 {
		t.Fatalf("rendered formula has residual vars: %v", residual)
	}
	return extractFirstBashFence(t, rendered)
}

func writeTestFile(t *testing.T, path string, data string, perm ...os.FileMode) {
	t.Helper()
	mode := os.FileMode(0o644)
	if len(perm) > 0 {
		mode = perm[0]
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func extractFirstBashFence(t *testing.T, markdown string) string {
	t.Helper()
	start := strings.Index(markdown, "```bash\n")
	if start < 0 {
		t.Fatal("missing bash code fence")
	}
	start += len("```bash\n")
	end := strings.LastIndex(markdown, "\n```")
	if end < 0 {
		t.Fatal("missing closing code fence")
	}
	if end <= start {
		t.Fatal("closing code fence appears before bash body")
	}
	return markdown[start:end]
}

func TestStaleDBOrderUsesParsedFieldsOnly(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "orders", "mol-dog-stale-db.toml"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "\n[vars]\n") {
		t.Fatal("order contains [vars], but order parsing ignores that table")
	}

	order, err := orders.Parse(data)
	if err != nil {
		t.Fatalf("orders.Parse: %v", err)
	}
	if err := orders.Validate(order); err != nil {
		t.Fatalf("orders.Validate: %v", err)
	}
	if order.Trigger != "cron" {
		t.Fatalf("Trigger = %q, want cron", order.Trigger)
	}
	if order.Schedule != "0 */4 * * *" {
		t.Fatalf("Schedule = %q, want 0 */4 * * *", order.Schedule)
	}
}
