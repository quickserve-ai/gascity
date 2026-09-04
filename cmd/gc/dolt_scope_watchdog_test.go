package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestManagedDoltScopeGone(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "dolt-config.yaml")
	if err := os.WriteFile(existing, []byte("log_level: warning\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		configFile string
		want       bool
	}{
		{"existing config is alive", existing, false},
		{"missing config is gone", filepath.Join(dir, "removed", "dolt-config.yaml"), true},
		{"empty path never reaps", "", false},
		{"blank path never reaps", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltScopeGone(tc.configFile); got != tc.want {
				t.Errorf("managedDoltScopeGone(%q) = %v, want %v", tc.configFile, got, tc.want)
			}
		})
	}
}

func TestManagedDoltScopeWatchdogEnabledFor(t *testing.T) {
	cases := []struct {
		name     string
		testMode bool
		env      string
		want     bool
	}{
		{"production default on", false, "", true},
		{"production explicit off", false, "0", false},
		{"production explicit on", false, "1", true},
		{"test mode always off", true, "", false},
		{"test mode off even when forced", true, "1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltScopeWatchdogEnabledFor(tc.testMode, tc.env); got != tc.want {
				t.Errorf("managedDoltScopeWatchdogEnabledFor(%v, %q) = %v, want %v", tc.testMode, tc.env, got, tc.want)
			}
		})
	}
}

func TestManagedDoltScopeWatchdogEnabled_OffInTestBinary(t *testing.T) {
	// The test binary is always in managed-dolt test mode, so the scope
	// watchdog must never interpose on test-spawned servers.
	if managedDoltScopeWatchdogEnabled() {
		t.Fatal("scope watchdog enabled inside the test binary; test scopes are owned by the test watchdog")
	}
}

func TestManagedDoltScopeWatchdogInterval(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", managedDoltScopeWatchdogDefaultInterval},
		{"50", 50 * time.Millisecond},
		{"0", managedDoltScopeWatchdogDefaultInterval},
		{"-5", managedDoltScopeWatchdogDefaultInterval},
		{"nonsense", managedDoltScopeWatchdogDefaultInterval},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv(managedDoltScopeWatchdogIntervalEnv, tc.env)
			if got := managedDoltScopeWatchdogInterval(); got != tc.want {
				t.Errorf("interval for %q = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// TestManagedDoltScopeWatchdogKillsServerWhenScopeDeleted exercises the full
// production supervision loop: a helper process starts a fake dolt server
// under the scope watchdog, the test deletes the config file (the scope
// anchor), and the watchdog must terminate the server after the two-check
// confirmation window.
func TestManagedDoltScopeWatchdogKillsServerWhenScopeDeleted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	dir := t.TempDir()
	fakeDoltDir := writeFakeDoltSQLServer(t)
	statePath := filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "dolt-config.yaml")
	logPath := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestManagedDoltScopeWatchdogHelper", "-test.v")
	cmd.Env = sanitizedBaseEnv(
		"GC_TEST_MANAGED_DOLT_HELPER=scope-watchdog",
		"GC_TEST_MANAGED_DOLT_HELPER_STATE="+statePath,
		"GC_TEST_MANAGED_DOLT_HELPER_CONFIG="+configPath,
		"GC_TEST_MANAGED_DOLT_HELPER_LOG="+logPath,
		"GC_TEST_MANAGED_DOLT_HELPER_FAKE_DOLT_DIR="+fakeDoltDir,
		// TestMain scrubs non-GC_TEST_ GC_* keys, so the interval rides a
		// GC_TEST_ control var and the helper re-exports it for the watchdog.
		"GC_TEST_MANAGED_DOLT_HELPER_SCOPE_WD_INTERVAL_MS=50",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output)
	}
	doltPID, watchdogPID := readManagedDoltTestState(t, statePath)
	t.Cleanup(func() {
		cleanupManagedDoltTestPID(t, doltPID)
		cleanupManagedDoltTestPID(t, watchdogPID)
	})

	// Control window: with the config present, the server must stay alive
	// across several poll intervals — and so must its watchdog (the spawner
	// helper has already exited, the production lifecycle shape).
	time.Sleep(300 * time.Millisecond)
	if !pidAlive(doltPID) {
		logData, _ := os.ReadFile(logPath)
		t.Fatalf("fake dolt pid %d exited while scope was alive; helper output:\n%s\nwatchdog log:\n%s", doltPID, output, logData)
	}
	if !pidAlive(watchdogPID) {
		logData, _ := os.ReadFile(logPath)
		t.Fatalf("watchdog pid %d died while scope was alive; watchdog log:\n%s", watchdogPID, logData)
	}

	// Delete the scope anchor; the watchdog should confirm twice and reap.
	if err := os.Remove(configPath); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(doltPID) {
		if time.Now().After(deadline) {
			logData, _ := os.ReadFile(logPath)
			t.Fatalf("fake dolt pid %d still alive after scope deletion; watchdog log:\n%s", doltPID, logData)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for pidAlive(watchdogPID) {
		if time.Now().After(deadline) {
			t.Fatalf("watchdog pid %d still alive after reaping its server", watchdogPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	logData, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logData), "gone for") {
		t.Errorf("watchdog log missing the scope-gone termination decision; log:\n%s", logData)
	}
}

// TestManagedDoltScopeWatchdogHelper runs in a child process: it starts a
// fake dolt server under the scope watchdog and records both PIDs, then
// exits — proving the watchdog supervises independently of its spawner,
// exactly the production lifecycle (gc exits, the watchdog stays).
func TestManagedDoltScopeWatchdogHelper(t *testing.T) {
	if os.Getenv("GC_TEST_MANAGED_DOLT_HELPER") != "scope-watchdog" {
		t.Skip("helper process only")
	}
	fakeDoltDir := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_FAKE_DOLT_DIR"))
	if fakeDoltDir == "" {
		t.Fatal("missing fake dolt dir")
	}
	t.Setenv("PATH", fakeDoltDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if interval := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_SCOPE_WD_INTERVAL_MS")); interval != "" {
		t.Setenv(managedDoltScopeWatchdogIntervalEnv, interval)
	}
	statePath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_STATE"))
	configPath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_CONFIG"))
	logPath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_LOG"))
	if statePath == "" || configPath == "" || logPath == "" {
		t.Fatal("missing helper paths")
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	defer logFile.Close() //nolint:errcheck

	// Opt-in: run the watchdog inside a real city so the ga-drkbcd escalation
	// path (emergency spool + .gc/events.jsonl) is exercised end to end.
	// Defaults to "" — the original shape — so existing callers are unchanged.
	cityPath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_CITY"))
	started, err := startManagedDoltSQLServerWithScopeWatchdog(cityPath, configPath, logPath, logFile)
	if err != nil {
		t.Fatalf("start managed dolt with scope watchdog: %v", err)
	}
	state := fmt.Sprintf("%d %d\n", started.PID, started.WatchdogPID)
	if err := os.WriteFile(statePath, []byte(state), 0o644); err != nil {
		t.Fatalf("write helper state: %v", err)
	}
	// Opt-in: record the reported start identity so a caller test can assert the
	// scope-watchdog path populates it (the PR #4004 PID-reuse guard input).
	// Two lines: start-time ticks, then the ps-lstart identity (possibly empty).
	if identityPath := strings.TrimSpace(os.Getenv("GC_TEST_MANAGED_DOLT_HELPER_IDENTITY")); identityPath != "" {
		identity := fmt.Sprintf("%d\n%s\n", started.StartTimeTicks, started.StartIdentity)
		if err := os.WriteFile(identityPath, []byte(identity), 0o644); err != nil {
			t.Fatalf("write helper identity: %v", err)
		}
	}
}

// readManagedDoltScopeIdentityState parses the two-line identity file the scope
// watchdog helper writes when GC_TEST_MANAGED_DOLT_HELPER_IDENTITY is set:
// start-time ticks on line 1, the ps-lstart identity (possibly empty) on line 2.
func readManagedDoltScopeIdentityState(t *testing.T, path string) (uint64, string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read helper identity: %v", err)
	}
	lines := strings.SplitN(strings.TrimRight(string(data), "\n"), "\n", 2)
	ticks, err := strconv.ParseUint(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil {
		t.Fatalf("parse helper identity ticks %q: %v", lines[0], err)
	}
	identity := ""
	if len(lines) >= 2 {
		identity = lines[1]
	}
	return ticks, identity
}

// TestManagedDoltScopeWatchdogReportsStartIdentity is the PR #4004 F1 regression
// for the production scope-watchdog path: the returned managedDoltStartedProcess
// must carry the dolt child's OS start identity, snapshotted by the watchdog
// before it can reap the child. Without it the startup-failure cleanup guard
// (terminateManagedDoltStartedProcess) falls through to unconditional bare-PID
// signaling and can kill an unrelated process that reused the numeric PID.
func TestManagedDoltScopeWatchdogReportsStartIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	dir := t.TempDir()
	fakeDoltDir := writeFakeDoltSQLServer(t)
	statePath := filepath.Join(dir, "state")
	identityPath := filepath.Join(dir, "identity")
	configPath := filepath.Join(dir, "dolt-config.yaml")
	logPath := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestManagedDoltScopeWatchdogHelper", "-test.v")
	cmd.Env = sanitizedBaseEnv(
		"GC_TEST_MANAGED_DOLT_HELPER=scope-watchdog",
		"GC_TEST_MANAGED_DOLT_HELPER_STATE="+statePath,
		"GC_TEST_MANAGED_DOLT_HELPER_IDENTITY="+identityPath,
		"GC_TEST_MANAGED_DOLT_HELPER_CONFIG="+configPath,
		"GC_TEST_MANAGED_DOLT_HELPER_LOG="+logPath,
		"GC_TEST_MANAGED_DOLT_HELPER_FAKE_DOLT_DIR="+fakeDoltDir,
		"GC_TEST_MANAGED_DOLT_HELPER_SCOPE_WD_INTERVAL_MS=50",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output)
	}
	doltPID, watchdogPID := readManagedDoltTestState(t, statePath)
	t.Cleanup(func() {
		cleanupManagedDoltTestPID(t, doltPID)
		cleanupManagedDoltTestPID(t, watchdogPID)
	})

	ticks, identity := readManagedDoltScopeIdentityState(t, identityPath)
	if ticks == 0 && identity == "" {
		logData, _ := os.ReadFile(logPath)
		t.Fatalf("scope watchdog reported no start identity (ticks=%d identity=%q); PID-reuse guard disabled; log:\n%s", ticks, identity, logData)
	}
}

// TestManagedDoltScopeWatchdogServerSurvivesScopePresent asserts the
// watchdog never reaps a server whose scope stays on disk, and exits
// cleanly when the server itself goes away.
func TestManagedDoltScopeWatchdogServerSurvivesScopePresent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	dir := t.TempDir()
	fakeDoltDir := writeFakeDoltSQLServer(t)
	statePath := filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "dolt-config.yaml")
	logPath := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestManagedDoltScopeWatchdogHelper", "-test.v")
	cmd.Env = sanitizedBaseEnv(
		"GC_TEST_MANAGED_DOLT_HELPER=scope-watchdog",
		"GC_TEST_MANAGED_DOLT_HELPER_STATE="+statePath,
		"GC_TEST_MANAGED_DOLT_HELPER_CONFIG="+configPath,
		"GC_TEST_MANAGED_DOLT_HELPER_LOG="+logPath,
		"GC_TEST_MANAGED_DOLT_HELPER_FAKE_DOLT_DIR="+fakeDoltDir,
		"GC_TEST_MANAGED_DOLT_HELPER_SCOPE_WD_INTERVAL_MS=50",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output)
	}
	doltPID, watchdogPID := readManagedDoltTestState(t, statePath)
	t.Cleanup(func() {
		cleanupManagedDoltTestPID(t, doltPID)
		cleanupManagedDoltTestPID(t, watchdogPID)
	})

	time.Sleep(300 * time.Millisecond)
	if !pidAlive(doltPID) {
		logData, _ := os.ReadFile(logPath)
		t.Fatalf("fake dolt pid %d reaped while scope present; watchdog log:\n%s", doltPID, logData)
	}
	if !pidAlive(watchdogPID) {
		logData, _ := os.ReadFile(logPath)
		t.Fatalf("watchdog pid %d died while scope present; watchdog log:\n%s", watchdogPID, logData)
	}

	// Kill the server directly (the `gc stop` shape); the watchdog must
	// notice and exit instead of lingering.
	proc, err := os.FindProcess(doltPID)
	if err != nil {
		t.Fatalf("find dolt pid: %v", err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill dolt pid: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(watchdogPID) {
		if time.Now().After(deadline) {
			t.Fatalf("watchdog pid %d still alive after its server exited", watchdogPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunManagedDoltScopeWatchdogUsage pins the argv contract.
func TestRunManagedDoltScopeWatchdogUsage(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close() //nolint:errcheck
	if code := runManagedDoltScopeWatchdog(nil, devnull, devnull); code != 2 {
		t.Errorf("no args exit = %d, want 2", code)
	}
	if code := runManagedDoltScopeWatchdog([]string{"a", "b"}, devnull, devnull); code != 2 {
		t.Errorf("two args exit = %d, want 2", code)
	}
	if code := runManagedDoltScopeWatchdog([]string{" ", "log", "city"}, devnull, devnull); code != 2 {
		t.Errorf("blank config exit = %d, want 2", code)
	}
}

// TestTerminateManagedDoltScopeWatchdogChildSkipsReusedPID is the PR #4004
// completeness regression for the watchdog's own reap path: the scope-gone and
// signal-forward branches terminate the dolt child through
// terminateManagedDoltScopeWatchdogChild, which must skip the signal when the
// child's numeric PID was reaped and reused (identity mismatch) while still
// terminating a child whose start identity still matches. Without the guard the
// production scope reap could SIGKILL an unrelated process that reused the PID.
func TestTerminateManagedDoltScopeWatchdogChildSkipsReusedPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics required")
	}

	// The live re-read is mocked to a fixed identity (3333); the guard compares
	// each snapshot against it, exactly as the watchdog runloop does.
	oldTicks := managedDoltTestReadStartTimeTicks
	oldIdent := managedDoltTestReadStartIdentity
	managedDoltTestReadStartTimeTicks = func(int) uint64 { return 3333 }
	managedDoltTestReadStartIdentity = func(int) string { return "" }
	t.Cleanup(func() {
		managedDoltTestReadStartTimeTicks = oldTicks
		managedDoltTestReadStartIdentity = oldIdent
	})

	// Matching snapshot (3333 == mocked re-read): the child is signaled and a
	// sleep dies on SIGTERM (a zombie reads as not-alive).
	matching := exec.Command("sleep", "60")
	if err := matching.Start(); err != nil {
		t.Fatalf("start matching child: %v", err)
	}
	matchingPID := matching.Process.Pid
	t.Cleanup(func() {
		_ = matching.Process.Kill()
		_ = matching.Wait()
	})
	if err := terminateManagedDoltScopeWatchdogChild("", matchingPID, 3333, ""); err != nil {
		t.Fatalf("guarded terminate of matching child: %v", err)
	}
	if pidAlive(matchingPID) {
		t.Fatalf("watchdog reap did not signal matching dolt child pid %d", matchingPID)
	}

	// Reused snapshot (1111 != mocked re-read 3333): the PID was reaped and the
	// number reused, so the guard must leave the live process untouched.
	reused := exec.Command("sleep", "60")
	if err := reused.Start(); err != nil {
		t.Fatalf("start reused child: %v", err)
	}
	reusedPID := reused.Process.Pid
	t.Cleanup(func() {
		_ = reused.Process.Kill()
		_ = reused.Wait()
	})
	if err := terminateManagedDoltScopeWatchdogChild("", reusedPID, 1111, ""); err != nil {
		t.Fatalf("guarded terminate of reused child: %v", err)
	}
	// Give any erroneous SIGTERM time to land before asserting survival.
	time.Sleep(200 * time.Millisecond)
	if !pidAlive(reusedPID) {
		t.Fatalf("watchdog reap signaled reused dolt child pid %d; identity guard not enforced", reusedPID)
	}
}

// --- ga-drkbcd: attribution and loud unexpected stops --------------------
//
// The 2026-08-15 defect: the data plane stopped twice with zero attribution.
// A dolt sql-server exited status 0 mid-service and the watchdog printed
// "exited cleanly"; seven minutes later an external SIGTERM reached the
// watchdog and nothing recorded who sent it. The tests below drive the real
// supervision loop through both shapes.

// writeScriptedFakeDoltSQLServer writes a fake `dolt` whose sql-server body is
// caller-supplied, so a test can make the server exit on its own terms —
// something writeFakeDoltSQLServer's `exec sleep 60` cannot express.
func writeScriptedFakeDoltSQLServer(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake requires POSIX sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "dolt")
	content := "#!/bin/sh\n" +
		"if [ \"$1\" != \"sql-server\" ]; then\n" +
		"  echo \"unexpected dolt args: $*\" >&2\n" +
		"  exit 2\n" +
		"fi\n" +
		body
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write scripted fake dolt: %v", err)
	}
	return dir
}

// waitForWatchdogLogText polls the watchdog log until want appears.
func waitForWatchdogLogText(t *testing.T, logPath, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), want) {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("watchdog log never contained %q within %s; log:\n%s", want, timeout, data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runScopeWatchdogHelper starts a fake dolt under the production scope watchdog
// through the helper process and returns (doltPID, watchdogPID, cityPath).
func runScopeWatchdogHelper(t *testing.T, fakeDoltDir, dir, configPath, logPath string) (int, int, string) {
	t.Helper()
	statePath := filepath.Join(dir, "state")
	cityPath := filepath.Join(dir, "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatalf("create city dir: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestManagedDoltScopeWatchdogHelper", "-test.v")
	cmd.Env = sanitizedBaseEnv(
		"GC_TEST_MANAGED_DOLT_HELPER=scope-watchdog",
		"GC_TEST_MANAGED_DOLT_HELPER_STATE="+statePath,
		"GC_TEST_MANAGED_DOLT_HELPER_CONFIG="+configPath,
		"GC_TEST_MANAGED_DOLT_HELPER_LOG="+logPath,
		"GC_TEST_MANAGED_DOLT_HELPER_CITY="+cityPath,
		"GC_TEST_MANAGED_DOLT_HELPER_FAKE_DOLT_DIR="+fakeDoltDir,
		"GC_TEST_MANAGED_DOLT_HELPER_SCOPE_WD_INTERVAL_MS=50",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output)
	}
	doltPID, watchdogPID := readManagedDoltTestState(t, statePath)
	t.Cleanup(func() {
		cleanupManagedDoltTestPID(t, doltPID)
		cleanupManagedDoltTestPID(t, watchdogPID)
	})
	return doltPID, watchdogPID, cityPath
}

// TestManagedDoltScopeWatchdogAlarmsOnUnexpectedCleanExit is the ga-drkbcd
// regression for the silent branch. A server that exits status 0 while its
// scope is intact and nobody asked for a stop must produce an unmistakable
// alarm, not the reassuring "exited cleanly" line the 2026-08-15 outage got —
// and the alarm must reach a channel something actually reads.
func TestManagedDoltScopeWatchdogAlarmsOnUnexpectedCleanExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	dir := t.TempDir()
	// A server that announces its connection count and then simply leaves.
	fakeDoltDir := writeScriptedFakeDoltSQLServer(t, "echo 'INFO client connections served: 3370000'\nsleep 1\nexit 0\n")
	configPath := filepath.Join(dir, "dolt-config.yaml")
	logPath := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	doltPID, watchdogPID, cityPath := runScopeWatchdogHelper(t, fakeDoltDir, dir, configPath, logPath)

	logData := waitForWatchdogLogText(t, logPath, "ALARM UNEXPECTED CLEAN EXIT", 15*time.Second)
	for _, want := range []string{
		fmt.Sprintf("pid %d exited with status 0", doltPID),
		"no stop request from gc",
		fmt.Sprintf("watchdog pid %d", watchdogPID),
		"INFO client connections served: 3370000",
	} {
		if !strings.Contains(logData, want) {
			t.Errorf("alarm is missing %q; log:\n%s", want, logData)
		}
	}
	// The healthy wording must not be reachable on an unrequested exit.
	if strings.Contains(logData, "exited cleanly") {
		t.Errorf("an unrequested status-0 exit still rendered as a clean exit; log:\n%s", logData)
	}

	// The alarm must be durable, not a log line nothing reads: an emergency
	// spool record plus an emergency.signaled event in the city log. The
	// escalation runs after the log lines are flushed, so wait for it to
	// report rather than racing the atomic spool write.
	logData = waitForWatchdogLogText(t, logPath, "escalated to the emergency spool at", 15*time.Second)
	spoolEntries, err := os.ReadDir(filepath.Join(cityPath, ".gc", "emergency"))
	if err != nil {
		t.Fatalf("read emergency spool: %v; log:\n%s", err, logData)
	}
	spoolFound := ""
	for _, entry := range spoolEntries {
		// .tmp files are writeFileAtomic's in-flight staging names.
		if strings.HasSuffix(entry.Name(), ".json") {
			spoolFound = entry.Name()
		}
	}
	if spoolFound == "" {
		t.Fatalf("no emergency spool record written; entries=%v log:\n%s", spoolEntries, logData)
	}
	spoolData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "emergency", spoolFound))
	if err != nil {
		t.Fatalf("read emergency record: %v", err)
	}
	for _, want := range []string{`"severity": "critical"`, managedDoltWatchdogAlarmActor, "no stop request from gc"} {
		if !strings.Contains(string(spoolData), want) {
			t.Errorf("emergency record missing %q:\n%s", want, spoolData)
		}
	}
	eventData, err := os.ReadFile(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("read city event log: %v", err)
	}
	if !strings.Contains(string(eventData), "emergency.signaled") {
		t.Errorf("the alarm never reached the city event log:\n%s", eventData)
	}
}

// TestManagedDoltScopeWatchdogQuietOnRequestedStop is the false-alarm guard.
// `gc dolt stop` signals the dolt PID directly and dolt exits 0, so without the
// stop-intent marker every requested shutdown would raise the new alarm — which
// would be worse than the silence it replaces.
func TestManagedDoltScopeWatchdogQuietOnRequestedStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics required")
	}
	dir := t.TempDir()
	// A server that shuts down gracefully on SIGTERM, exiting status 0 —
	// exactly what dolt does, and exactly what made the two cases identical.
	fakeDoltDir := writeScriptedFakeDoltSQLServer(t, "trap 'exit 0' TERM\necho 'INFO dolt: ready'\nwhile : ; do sleep 0.1; done\n")
	configPath := filepath.Join(dir, "dolt-config.yaml")
	logPath := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	doltPID, watchdogPID, _ := runScopeWatchdogHelper(t, fakeDoltDir, dir, configPath, logPath)
	// The fake installs its SIGTERM trap before announcing readiness, so this
	// wait is what makes the signal below land on a server that shuts down
	// gracefully rather than on one still starting up.
	waitForWatchdogLogText(t, logPath, "INFO dolt: ready", 10*time.Second)

	// Record the intent the way stopManagedDoltProcessWithOptions does, then
	// stop the server the way it does: SIGTERM straight to the dolt PID.
	if err := recordManagedDoltStopIntent(configPath, doltPID, "gc managed dolt stop"); err != nil {
		t.Fatalf("record stop intent: %v", err)
	}
	if err := syscall.Kill(doltPID, syscall.SIGTERM); err != nil {
		t.Fatalf("signal fake dolt: %v", err)
	}

	logData := waitForWatchdogLogText(t, logPath, "exited cleanly", 15*time.Second)
	if strings.Contains(logData, "ALARM") {
		t.Errorf("a requested stop raised an alarm; log:\n%s", logData)
	}
	for _, want := range []string{"stop requested by", "gc managed dolt stop", fmt.Sprintf("requester pid %d", os.Getpid())} {
		if !strings.Contains(logData, want) {
			t.Errorf("requested stop is missing attribution %q; log:\n%s", want, logData)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(watchdogPID) {
		if time.Now().After(deadline) {
			t.Fatalf("watchdog pid %d still alive after its server exited", watchdogPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestManagedDoltScopeWatchdogAttributesAnExternalStopSignal is the ga-drkbcd
// ask-A regression. On 2026-08-15 the watchdog logged "received terminated" and
// nothing else; the sender is unrecoverable through os/signal, so the watchdog
// must instead record everything that narrows it — and record it durably.
func TestManagedDoltScopeWatchdogAttributesAnExternalStopSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics required")
	}
	dir := t.TempDir()
	fakeDoltDir := writeFakeDoltSQLServer(t)
	configPath := filepath.Join(dir, "dolt-config.yaml")
	logPath := filepath.Join(dir, "dolt.log")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	doltPID, watchdogPID, cityPath := runScopeWatchdogHelper(t, fakeDoltDir, dir, configPath, logPath)
	waitForWatchdogLogText(t, logPath, "supervising dolt sql-server", 10*time.Second)

	// The 2026-08-15 event: an external SIGTERM to the watchdog itself.
	if err := syscall.Kill(watchdogPID, syscall.SIGTERM); err != nil {
		t.Fatalf("signal watchdog: %v", err)
	}

	logData := waitForWatchdogLogText(t, logPath, "stop signal attribution", 15*time.Second)
	for _, want := range []string{
		"received terminated",
		fmt.Sprintf("terminating dolt sql-server pid %d", doltPID),
		"signal terminated received at",
		fmt.Sprintf("watchdog pid %d ppid", watchdogPID),
		"the sending pid is NOT recoverable",
	} {
		if !strings.Contains(logData, want) {
			t.Errorf("signal attribution missing %q; log:\n%s", want, logData)
		}
	}
	// The snapshot must at least see the dolt server it supervises; a snapshot
	// that saw nothing would be indistinguishable from one that never ran.
	if !strings.Contains(logData, "lifecycle actor") && !strings.Contains(logData, "ancestry[") {
		t.Errorf("signal attribution recorded neither ancestry nor a lifecycle actor; log:\n%s", logData)
	}

	eventPath := filepath.Join(cityPath, ".gc", "events.jsonl")
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(eventPath)
		if strings.Contains(string(data), "external-stop-signal") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the external stop signal never reached the city event log:\n%s\nwatchdog log:\n%s", data, logData)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
