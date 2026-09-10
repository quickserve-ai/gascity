package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func TestWaitForManagedDoltProcessExit(t *testing.T) {
	const pid = 4242
	tests := []struct {
		name         string
		timeout      time.Duration
		aliveThrough int
		wantCalls    int
		wantElapsed  time.Duration
	}{
		{name: "already dead", timeout: 45 * time.Millisecond, aliveThrough: 0, wantCalls: 1},
		{name: "exits after two polls", timeout: time.Second, aliveThrough: 2, wantCalls: 3, wantElapsed: 40 * time.Millisecond},
		{name: "never exits uses exact remainder", timeout: 45 * time.Millisecond, aliveThrough: -1, wantCalls: 4, wantElapsed: 45 * time.Millisecond},
		{name: "nonpositive bound probes once", timeout: 0, aliveThrough: -1, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				started := time.Now()
				waitForManagedDoltProcessExit(pid, tt.timeout, func(gotPID int) bool {
					if gotPID != pid {
						t.Fatalf("alive pid = %d, want %d", gotPID, pid)
					}
					calls++
					return tt.aliveThrough < 0 || calls <= tt.aliveThrough
				})

				if calls != tt.wantCalls {
					t.Errorf("alive calls = %d, want %d", calls, tt.wantCalls)
				}
				if elapsed := time.Since(started); elapsed != tt.wantElapsed {
					t.Errorf("elapsed = %v, want %v", elapsed, tt.wantElapsed)
				}
			})
		})
	}
}

func TestManagedDoltStopPollInterval(t *testing.T) {
	cases := []struct {
		name  string
		grace time.Duration
		want  time.Duration
	}{
		{"default grace keeps 500ms", 30 * time.Second, 500 * time.Millisecond},
		{"exactly 500ms keeps 500ms", 500 * time.Millisecond, 500 * time.Millisecond},
		{"sub-poll grace shrinks to grace", 200 * time.Millisecond, 200 * time.Millisecond},
		{"tiny grace shrinks to grace", 100 * time.Millisecond, 100 * time.Millisecond},
		{"zero grace keeps 500ms", 0, 500 * time.Millisecond},
		{"negative grace keeps 500ms", -1 * time.Second, 500 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltStopPollInterval(tc.grace); got != tc.want {
				t.Errorf("managedDoltStopPollInterval(%v) = %v, want %v", tc.grace, got, tc.want)
			}
		})
	}
}

func TestResolveManagedDoltStopTimeoutDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got := resolveManagedDoltStopTimeout(dir)
	if got != config.DefaultDoltStopTimeout {
		t.Errorf("resolveManagedDoltStopTimeout() = %v, want %v (default)", got, config.DefaultDoltStopTimeout)
	}
}

func TestResolveManagedDoltStopTimeoutCustom(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test"

[daemon]
dolt_stop_timeout = "1m"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got := resolveManagedDoltStopTimeout(dir)
	if got != time.Minute {
		t.Errorf("resolveManagedDoltStopTimeout() = %v, want 1m", got)
	}
}

func TestResolveManagedDoltStopTimeoutMissingCityFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	// No city.toml — loadCityConfig should fail and we should fall back.
	got := resolveManagedDoltStopTimeout(dir)
	if got != config.DefaultDoltStopTimeout {
		t.Errorf("resolveManagedDoltStopTimeout() with no city.toml = %v, want %v (default)", got, config.DefaultDoltStopTimeout)
	}
}

func TestResolveManagedDoltStopTimeoutEmptyCityPathReturnsDefault(t *testing.T) {
	// An empty cityPath must NOT trigger loadCityConfig("", …), which would
	// resolve "city.toml" relative to cwd and materialize builtin packs
	// there. Plant a stray ./city.toml with a non-default dolt_stop_timeout;
	// resolveManagedDoltStopTimeout("") must ignore it and return the default.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "stray"

[daemon]
dolt_stop_timeout = "1m"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatalf("write stray city.toml: %v", err)
	}
	t.Chdir(dir)

	got := resolveManagedDoltStopTimeout("")
	if got != config.DefaultDoltStopTimeout {
		t.Errorf("resolveManagedDoltStopTimeout(\"\") = %v, want %v (default — must not read stray ./city.toml)", got, config.DefaultDoltStopTimeout)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gc")); err == nil {
		t.Error("resolveManagedDoltStopTimeout(\"\") materialized .gc/ under cwd; empty cityPath must not load config")
	}
}

func TestResolveManagedDoltStopTimeoutInvalidValueFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`
[workspace]
name = "test"

[daemon]
dolt_stop_timeout = "not-a-duration"

[[agent]]
name = "mayor"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	got := resolveManagedDoltStopTimeout(dir)
	if got != config.DefaultDoltStopTimeout {
		t.Errorf("resolveManagedDoltStopTimeout() with invalid duration = %v, want %v (default)", got, config.DefaultDoltStopTimeout)
	}
}

// --- ga-drkbcd: the production stop path and its stop-intent marker ---------
//
// The tests below drive the REAL stopManagedDoltProcessWithOptions against a
// stand-in server, rather than writing the marker from the test itself: the
// marker only defends against a false "exited cleanly" if the production stop
// is the thing that writes it, and only stays safe if a FAILED stop takes it
// back down again.

// startFakeOwnedManagedDolt starts a stand-in for the managed dolt sql-server
// that every check in the stop path accepts as ours. Ownership is decided by
// the process's argv carrying "--config <configFile>"
// (inspectManagedDoltOwnership → containsProcessConfig), so a shell spawned
// with those trailing arguments is indistinguishable from the real server to
// the inspection, and it can be scripted to answer SIGTERM the way dolt does —
// or to refuse it. The process is reaped in the background so it never lingers
// as a zombie that pidAlive would still report as alive.
func startFakeOwnedManagedDolt(t *testing.T, configFile, body string, env ...string) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	cmd := exec.Command("/bin/sh", "-c", body, "dolt", "sql-server", "--config", configFile)
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake owned managed dolt: %v", err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
		}
	})
	return pid
}

// waitForFakeManagedDoltReady blocks until the stand-in server has installed
// its signal disposition and touched its ready file. Without it a SIGTERM can
// land on a shell that has not yet run `trap`, which is a different scenario
// from the one under test.
func waitForFakeManagedDoltReady(t *testing.T, readyPath string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake managed dolt never became ready (%s)", readyPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newManagedDoltStopFixture builds an isolated city + pack-state layout for a
// stop-path test. GC_PACK_STATE_DIR is the single knob every other layout entry
// derives from, so the whole managed layout lands under tmpdir.
func newManagedDoltStopFixture(t *testing.T, cityTOML string) (string, managedDoltRuntimeLayout) {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	packStateDir := filepath.Join(cityPath, "pack-state")
	if err := os.MkdirAll(packStateDir, 0o755); err != nil {
		t.Fatalf("mkdir pack state dir: %v", err)
	}
	t.Setenv("GC_PACK_STATE_DIR", packStateDir)
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolve managed dolt layout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	return cityPath, layout
}

// writeFakeManagedDoltPIDFile points the layout's pid file at pid, which is how
// findManagedDoltPID locates the server without a port probe.
func writeFakeManagedDoltPIDFile(t *testing.T, layout managedDoltRuntimeLayout, pid int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatalf("mkdir pid dir: %v", err)
	}
	if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
}

const fakeManagedDoltStopCityTOML = `
[workspace]
name = "test"

[daemon]
dolt_stop_timeout = "300ms"

[[agent]]
name = "mayor"
`

// TestStopManagedDoltProcessWritesTheStopIntentThroughTheProductionPath is the
// ga-drkbcd D9 regression: the marker that decides whether a status-0 exit
// alarms must be written by stopManagedDoltProcessWithOptions itself. The
// stand-in server captures the marker from inside its own SIGTERM handler, so
// the assertion is about the file that existed at the instant gc signalled —
// not one the test wrote, and not one reconstructed afterwards (the stop clears
// it on success, which is asserted too).
func TestStopManagedDoltProcessWritesTheStopIntentThroughTheProductionPath(t *testing.T) {
	cityPath, layout := newManagedDoltStopFixture(t, fakeManagedDoltStopCityTOML)
	capturePath := filepath.Join(t.TempDir(), "intent-at-signal.json")
	readyPath := filepath.Join(t.TempDir(), "ready")

	pid := startFakeOwnedManagedDolt(t, layout.ConfigFile,
		"trap 'cp \"$GC_TEST_INTENT_MARKER\" \"$GC_TEST_INTENT_CAPTURE\" 2>/dev/null; exit 0' TERM\n"+
			": > \"$GC_TEST_READY\"\n"+
			"while : ; do sleep 0.05; done\n",
		"GC_TEST_INTENT_MARKER="+managedDoltStopIntentPath(layout.ConfigFile),
		"GC_TEST_INTENT_CAPTURE="+capturePath,
		"GC_TEST_READY="+readyPath,
	)
	waitForFakeManagedDoltReady(t, readyPath)
	writeFakeManagedDoltPIDFile(t, layout, pid)

	report, err := stopManagedDoltProcessWithOptions(cityPath, "", false)
	if err != nil {
		t.Fatalf("stop the stand-in managed dolt: %v", err)
	}
	if !report.HadPID || report.PID != pid {
		t.Fatalf("stop report = %+v; expected it to target pid %d", report, pid)
	}

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("the production stop never wrote a marker the server could see: %v", err)
	}
	var intent managedDoltStopIntent
	if err := json.Unmarshal(captured, &intent); err != nil {
		t.Fatalf("marker written by the production stop is not parseable: %v (%s)", err, captured)
	}
	if intent.PID != pid {
		t.Errorf("marker pid = %d, want %d (the pid the stop signalled)", intent.PID, pid)
	}
	if intent.RequesterPID != os.Getpid() {
		t.Errorf("marker requester pid = %d, want %d (the stopping process)", intent.RequesterPID, os.Getpid())
	}
	if intent.Reason != "gc managed dolt stop" {
		t.Errorf("marker reason = %q, want %q", intent.Reason, "gc managed dolt stop")
	}
	if !managedDoltStopIntentCovers(intent, pid, time.Now()) {
		t.Errorf("the marker the production stop wrote does not cover the exit it was written for: %+v", intent)
	}

	// The marker must OUTLIVE the stop. The watchdog reads it from another
	// process after its child exits, so a stop that deleted the marker on its
	// way out would race that read and turn a shutdown gc requested into a
	// CRITICAL alarm. Expiry is the TTL's job, not the stop's.
	survivor, found := readManagedDoltStopIntent(layout.ConfigFile)
	if !found {
		t.Fatal("the completed stop deleted its own marker; the watchdog can no longer explain the exit it caused")
	}
	exitReport := classifyManagedDoltWatchdogChildExit(
		observeManagedDoltWatchdogChildExit(pid, layout.ConfigFile, layout.LogFile, time.Now().Add(-time.Minute), nil, false))
	if exitReport.Alarm || exitReport.Cause != managedDoltExitCauseRequested {
		t.Errorf("the watchdog read the completed stop as %q (alarm=%v); want %q (marker: %+v)",
			exitReport.Cause, exitReport.Alarm, managedDoltExitCauseRequested, survivor)
	}
}

// TestStopManagedDoltProcessMarkerOutlivesALongConfiguredShutdown is the
// ga-drkbcd P2b regression. The marker's default 10-minute TTL is shorter than
// a `[daemon] dolt_stop_timeout` an operator is allowed to configure, so a
// graceful shutdown that legitimately takes longer than the default would
// outlive its own authorization and alarm as if nobody had asked. The stop path
// therefore sizes the marker to its own grace window.
func TestStopManagedDoltProcessMarkerOutlivesALongConfiguredShutdown(t *testing.T) {
	cityPath, layout := newManagedDoltStopFixture(t, `
[workspace]
name = "test"

[daemon]
dolt_stop_timeout = "15m"

[[agent]]
name = "mayor"
`)
	readyPath := filepath.Join(t.TempDir(), "ready")
	pid := startFakeOwnedManagedDolt(t, layout.ConfigFile,
		"trap 'exit 0' TERM\n"+
			": > \"$GC_TEST_READY\"\n"+
			"while : ; do sleep 0.05; done\n",
		"GC_TEST_READY="+readyPath,
	)
	waitForFakeManagedDoltReady(t, readyPath)
	writeFakeManagedDoltPIDFile(t, layout, pid)

	if _, err := stopManagedDoltProcessWithOptions(cityPath, "", false); err != nil {
		t.Fatalf("stop the stand-in managed dolt: %v", err)
	}
	intent, found := readManagedDoltStopIntent(layout.ConfigFile)
	if !found {
		t.Fatal("the stop wrote no marker")
	}
	// A shutdown that took eleven minutes — inside the configured fifteen — is
	// still a stop gc asked for.
	if !managedDoltStopIntentCovers(intent, pid, time.Now().Add(11*time.Minute)) {
		t.Errorf("a marker written under a 15m stop timeout stopped vouching after 11m: %+v", intent)
	}
	// It must still expire: past the configured window plus its slack, nothing
	// vouches for the pid any more.
	if managedDoltStopIntentCovers(intent, pid, time.Now().Add(30*time.Minute)) {
		t.Errorf("the marker never expires: %+v", intent)
	}
}

// TestStopManagedDoltProcessClearsTheStopIntentWhenTheStopFails is the
// ga-drkbcd D2 regression. The intent is recorded BEFORE the signal, so every
// error return after that point leaves a fresh marker naming a still-live pid.
// Inside the marker's TTL the next genuinely unexpected status-0 exit of that
// pid would then be vouched for and suppressed — which is the 2026-08-15
// failure mode this branch exists to remove.
func TestStopManagedDoltProcessClearsTheStopIntentWhenTheStopFails(t *testing.T) {
	cityPath, layout := newManagedDoltStopFixture(t, fakeManagedDoltStopCityTOML)

	// Hold the dolt exclusive store lock so the SIGKILL gate refuses to escalate
	// and the stop fails AFTER the intent has been recorded. This is a real
	// production shape: a mid-flush holder is exactly why that gate exists.
	lockDir := filepath.Join(layout.DataDir, "gc", ".dolt", "noms")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatalf("mkdir lock dir: %v", err)
	}
	lockPath := filepath.Join(lockDir, "LOCK")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("create store lock: %v", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold store lock: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	origLockWindow := managedDoltLockReleaseTimeoutFn
	managedDoltLockReleaseTimeoutFn = func(string) time.Duration { return 300 * time.Millisecond }
	t.Cleanup(func() { managedDoltLockReleaseTimeoutFn = origLockWindow })

	readyPath := filepath.Join(t.TempDir(), "ready")
	pid := startFakeOwnedManagedDolt(t, layout.ConfigFile,
		// A server that does NOT answer SIGTERM: the stop runs its grace out and
		// then hits the lock gate.
		"trap '' TERM\n"+
			": > \"$GC_TEST_READY\"\n"+
			"while : ; do sleep 0.05; done\n",
		"GC_TEST_READY="+readyPath,
	)
	waitForFakeManagedDoltReady(t, readyPath)
	writeFakeManagedDoltPIDFile(t, layout, pid)

	if _, err := stopManagedDoltProcessWithOptions(cityPath, "", false); err == nil {
		t.Fatal("expected the stop to fail while the store lock is held by a live process")
	}
	if !pidAlive(pid) {
		t.Fatal("the stand-in server died; the failed-stop scenario did not happen")
	}

	if intent, found := readManagedDoltStopIntent(layout.ConfigFile); found && managedDoltStopIntentCovers(intent, pid, time.Now()) {
		t.Errorf("a FAILED stop left a marker vouching for still-live pid %d: %+v", pid, intent)
	}

	// The consequence, stated as the watchdog would see it: the very next
	// status-0 exit of that pid must still be an alarm.
	exitReport := classifyManagedDoltWatchdogChildExit(
		observeManagedDoltWatchdogChildExit(pid, layout.ConfigFile, layout.LogFile, time.Now().Add(-time.Minute), nil, false))
	if !exitReport.Alarm || exitReport.Cause != managedDoltExitCauseUnexpectedClean {
		t.Errorf("after a FAILED stop an unexpected clean exit was classified %q (alarm=%v); want %q with an alarm",
			exitReport.Cause, exitReport.Alarm, managedDoltExitCauseUnexpectedClean)
	}
}
