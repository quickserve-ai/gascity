package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const restartScript = "commands/restart/run.sh"

// writeFakeBeadsBDForRestart writes a stub gc-beads-bd that records each
// invocation's first argument and exits with the code specified for that
// op. ops that aren't in opExitCodes exit 0.
//
// No ENOSPC helper is written beside the stub, so run.sh resolves the real
// assets/scripts/dolt-enospc.sh from the bd pack (via GC_PACK_DIR), exactly as
// it does behind the city shim. A hand-copied fixture here could drift from
// the guard it stands in for.
func writeFakeBeadsBDForRestart(t *testing.T, cityPath string, opExitCodes map[string]int) string {
	t.Helper()
	scriptDir := filepath.Join(cityPath, ".gc", "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir fake bd dir: %v", err)
	}
	logPath := filepath.Join(cityPath, "bd.log")
	var cases strings.Builder
	for op, code := range opExitCodes {
		fmt.Fprintf(&cases, "  %s) exit %d ;;\n", op, code)
	}
	body := `#!/bin/sh
printf '%s\n' "$1" >> "` + logPath + `"
case "$1" in
` + cases.String() + `  *) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(scriptDir, "gc-beads-bd.sh"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake bd script: %v", err)
	}
	return logPath
}

func runRestart(t *testing.T, cityPath, root string, port int) ([]byte, error) {
	t.Helper()
	return runRestartWithEnv(t, cityPath, root, []string{fmt.Sprintf("GC_DOLT_PORT=%d", port)})
}

func runRestartWithEnv(t *testing.T, cityPath, root string, extraEnv []string, args ...string) ([]byte, error) {
	t.Helper()
	script := filepath.Join(root, restartScript)
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
		"GC_CITY_RUNTIME_DIR", "GC_PACK_STATE_DIR", "GC_DOLT_LOG_FILE",
		"GC_BEADS_BD_SCRIPT",
	),
		"PATH="+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		// The ENOSPC guard measures the temp volume with the real df. A 1 MB
		// floor still exercises that measurement (an unreadable df refuses)
		// without making these tests depend on the runner's free space.
		"GC_DOLT_RESTART_MIN_FREE_MB=1",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd.CombinedOutput()
}

func TestRestartCallsStopThenStart_HappyPath(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})

	out, err := runRestart(t, cityPath, root, port)
	if err != nil {
		t.Fatalf("gc dolt restart failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected ops in order 'stop start', got %q\noutput:\n%s", got, out)
	}
}

func TestRestartCallsStartWhenStopReportsNothingRunning(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	// op_stop exits 2 when no managed dolt PID is found. restart must
	// treat that as success and still invoke start.
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 2, "start": 0})

	out, err := runRestart(t, cityPath, root, port)
	if err != nil {
		t.Fatalf("gc dolt restart failed when stop reported nothing-running: %v\n%s", err, out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected ops in order 'stop start' (exit 2 on stop is recoverable), got %q\noutput:\n%s", got, out)
	}
}

func TestRestartDoesNotRequirePortWhenStopReportsNothingRunning(t *testing.T) {
	root := repoRoot(t)
	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 2, "start": 0})

	out, err := runRestartWithEnv(t, cityPath, root, nil)
	if err != nil {
		t.Fatalf("gc dolt restart failed without a resolved runtime port: %v\n%s", err, out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected ops in order 'stop start' without a runtime port, got %q\noutput:\n%s", got, out)
	}
}

func TestRestartAbortsAndDoesNotStartWhenStopFails(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	// op_stop exit code 1 is the genuine-failure path (e.g., couldn't kill
	// the managed PID). restart must abort without calling start so the
	// operator can investigate.
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 1, "start": 0})

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly succeeded when stop failed:\n%s", out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	if strings.Contains(string(data), "start") {
		t.Fatalf("restart called start after stop failed; ops log:\n%s\noutput:\n%s", data, out)
	}
}

func TestRestartPropagatesStartFailureWithDiagnostic(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 1})

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly succeeded when start failed:\n%s", out)
	}
	if !strings.Contains(string(out), "gc dolt restart: start failed (exit 1)") {
		t.Fatalf("restart did not report start failure; output:\n%s", out)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected restart to attempt stop and start before reporting start failure, got %q\noutput:\n%s", got, out)
	}
}

// writeDoltLogForRestart writes the fake Dolt server log restart consults, at
// the default pack-state path.
func writeDoltLogForRestart(t *testing.T, cityPath, content string) {
	t.Helper()
	logPath := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir dolt log dir: %v", err)
	}
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write dolt log: %v", err)
	}
}

// doltENOSPCLogLine renders a Dolt logrus ENOSPC line stamped at ts, in the
// RFC3339-with-offset form Dolt writes.
func doltENOSPCLogLine(ts time.Time) string {
	return fmt.Sprintf("time=%q level=error msg=\"error writing chunk\" error=\"write journal.idx: no space left on device\"\n", ts.Format(time.RFC3339))
}

func TestRestartRefusesRecentENOSPCUnlessForced(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})
	recent := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	writeDoltLogForRestart(t, cityPath, doltENOSPCLogLine(recent))

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly ignored recent ENOSPC:\n%s", out)
	}
	for _, want := range []string{
		"gc dolt restart: refusing restart: Dolt log shows 1 ENOSPC line(s) stamped within the last 60 min",
		"newest " + recent.Format(time.RFC3339),
		"live free space: ",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("restart refusal missing %q; output:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "--force") {
		t.Fatalf("restart refusal must not advertise --force; output:\n%s", out)
	}
	if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("restart invoked gc-beads-bd despite ENOSPC refusal; ops log:\n%s\noutput:\n%s", data, out)
	}

	out, err = runRestartWithEnv(t, cityPath, root, []string{fmt.Sprintf("GC_DOLT_PORT=%d", port)}, "--force")
	if err != nil {
		t.Fatalf("gc dolt restart --force failed despite fake stop/start success: %v\n%s", err, out)
	}
	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected forced restart to call stop then start, got %q\noutput:\n%s", got, out)
	}
}

// TestRestartProceedsPastStaleENOSPCEvidence pins the time bound: ENOSPC lines
// from 8 days ago on a healthy disk must not block a restart. The pre-fix
// guard counted any ENOSPC in the last 1000 log lines, which a quiet log
// stretched across 8 days.
func TestRestartProceedsPastStaleENOSPCEvidence(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})
	writeDoltLogForRestart(t, cityPath, doltENOSPCLogLine(time.Now().Add(-8*24*time.Hour)))

	out, err := runRestart(t, cityPath, root, port)
	if err != nil {
		t.Fatalf("gc dolt restart refused on 8-day-old ENOSPC evidence: %v\n%s", err, out)
	}
	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	got := strings.Join(strings.Fields(string(data)), " ")
	if got != "stop start" {
		t.Fatalf("expected ops in order 'stop start' past stale ENOSPC, got %q\noutput:\n%s", got, out)
	}
}

// TestRestartRefusesENOSPCLineWithoutTimestamp pins the fail-closed rule: an
// ENOSPC line whose stamp cannot be parsed counts as recent, and the refusal
// says so rather than passing it silently.
func TestRestartRefusesENOSPCLineWithoutTimestamp(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})
	writeDoltLogForRestart(t, cityPath, "fatal: no space left on device\n")

	out, err := runRestart(t, cityPath, root, port)
	if err == nil {
		t.Fatalf("gc dolt restart passed an unstamped ENOSPC line:\n%s", out)
	}
	if !strings.Contains(string(out), "unparseable timestamp") {
		t.Fatalf("restart refusal did not name the unparseable timestamp; output:\n%s", out)
	}
	if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("restart invoked gc-beads-bd despite ENOSPC refusal; ops log:\n%s\noutput:\n%s", data, out)
	}
}

// TestRestartTreatsLoopbackAndWildcardHostsAsLocalManaged pins the P0.5
// host-classification contract: the managed-server bind default is
// 127.0.0.1, and 0.0.0.0 remains the explicit wildcard opt-out — both must
// be treated as a GC-managed local server (restart proceeds), exactly like
// an unset GC_DOLT_HOST. Without 127.0.0.1 in the local set, adopting the
// loopback bind default would break managed-server detection.
func TestRestartTreatsLoopbackAndWildcardHostsAsLocalManaged(t *testing.T) {
	root := repoRoot(t)

	for _, host := range []string{"127.0.0.1", "0.0.0.0", "localhost", "::1"} {
		t.Run(host, func(t *testing.T) {
			port, cleanup := startReachableTCPListener(t)
			defer cleanup()

			cityPath := t.TempDir()
			bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})

			out, err := runRestartWithEnv(t, cityPath, root, []string{
				fmt.Sprintf("GC_DOLT_PORT=%d", port),
				"GC_DOLT_HOST=" + host,
			})
			if err != nil {
				t.Fatalf("gc dolt restart refused GC_DOLT_HOST=%s as if remote: %v\n%s", host, err, out)
			}

			data, err := os.ReadFile(bdLog)
			if err != nil {
				t.Fatalf("read fake bd log: %v", err)
			}
			got := strings.Join(strings.Fields(string(data)), " ")
			if got != "stop start" {
				t.Fatalf("expected ops in order 'stop start' for local host %s, got %q\noutput:\n%s", host, got, out)
			}
		})
	}
}

func TestRestartRejectsRemoteHostWithDiagnostic(t *testing.T) {
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	bdLog := writeFakeBeadsBDForRestart(t, cityPath, map[string]int{"stop": 0, "start": 0})

	out, err := runRestartWithEnv(t, cityPath, root, []string{
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_HOST=example.internal",
	})
	if err == nil {
		t.Fatalf("gc dolt restart unexpectedly succeeded for a remote host:\n%s", out)
	}
	if !strings.Contains(string(out), "gc dolt restart: not supported for remote dolt servers") {
		t.Fatalf("restart did not explain remote-host refusal; output:\n%s", out)
	}
	if data, err := os.ReadFile(bdLog); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("restart invoked gc-beads-bd despite remote-host refusal; ops log:\n%s\noutput:\n%s", data, out)
	}
}
