package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDetectBinaryDrift(t *testing.T) {
	cases := []struct {
		name         string
		localBuildID string
		supervisorID string
		wantDrift    bool
	}{
		{
			name:         "match",
			localBuildID: "acc19d24",
			supervisorID: "acc19d24",
			wantDrift:    false,
		},
		{
			name:         "mismatch",
			localBuildID: "acc19d24",
			supervisorID: "9e21abcd",
			wantDrift:    true,
		},
		{
			name:         "supervisor empty (older binary, no buildID exposed)",
			localBuildID: "acc19d24",
			supervisorID: "",
			wantDrift:    false,
		},
		{
			name:         "local empty (dev build)",
			localBuildID: "",
			supervisorID: "acc19d24",
			wantDrift:    false,
		},
		{
			name:         "both empty",
			localBuildID: "",
			supervisorID: "",
			wantDrift:    false,
		},
		{
			name:         "match with dirty suffix",
			localBuildID: "acc19d24-dirty",
			supervisorID: "acc19d24-dirty",
			wantDrift:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sv := SupervisorStatus{BuildID: tc.supervisorID}
			got := DetectBinaryDrift(tc.localBuildID, sv)
			if got != tc.wantDrift {
				t.Errorf("DetectBinaryDrift(%q, %q) = %v; want %v", tc.localBuildID, tc.supervisorID, got, tc.wantDrift)
			}
		})
	}
}

func TestDetectPackDrift(t *testing.T) {
	dir := t.TempDir()

	// Pack root with a single file. ParsedAt is set in the past — drift.
	packA := filepath.Join(dir, "packA")
	if err := os.MkdirAll(packA, 0o755); err != nil {
		t.Fatal(err)
	}
	fileA := filepath.Join(packA, "agent.toml")
	if err := os.WriteFile(fileA, []byte("name = \"a\""), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pack root B with ParsedAt in the future — no drift.
	packB := filepath.Join(dir, "packB")
	if err := os.MkdirAll(packB, 0o755); err != nil {
		t.Fatal(err)
	}
	fileB := filepath.Join(packB, "agent.toml")
	if err := os.WriteFile(fileB, []byte("name = \"b\""), 0o644); err != nil {
		t.Fatal(err)
	}

	past := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(1 * time.Hour)

	t.Run("no roots", func(t *testing.T) {
		drifted, err := DetectPackDrift(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(drifted) != 0 {
			t.Errorf("expected no drift; got %v", drifted)
		}
	})

	t.Run("one drifted, one not", func(t *testing.T) {
		drifted, err := DetectPackDrift([]PackRootStatus{
			{Dir: packA, ParsedAt: past},
			{Dir: packB, ParsedAt: future},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(drifted) != 1 || drifted[0] != packA {
			t.Errorf("expected only packA drifted; got %v", drifted)
		}
	})

	t.Run("zero ParsedAt skips check", func(t *testing.T) {
		drifted, err := DetectPackDrift([]PackRootStatus{
			{Dir: packA, ParsedAt: time.Time{}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(drifted) != 0 {
			t.Errorf("expected zero ParsedAt to skip check; got %v", drifted)
		}
	})

	t.Run("missing dir is reported as error", func(t *testing.T) {
		_, err := DetectPackDrift([]PackRootStatus{
			{Dir: filepath.Join(dir, "no-such-dir"), ParsedAt: past},
		})
		if err == nil {
			t.Errorf("expected error for missing dir")
		}
	})

	t.Run("both drifted", func(t *testing.T) {
		drifted, err := DetectPackDrift([]PackRootStatus{
			{Dir: packA, ParsedAt: past},
			{Dir: packB, ParsedAt: past},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(drifted) != 2 {
			t.Errorf("expected both drifted; got %v", drifted)
		}
	})
}

// fakeSupervisorClient implements SupervisorClient for tests.
type fakeSupervisorClient struct {
	pingErr   error
	pingDelay time.Duration
	pingCount int
}

func (f *fakeSupervisorClient) Status(_ context.Context) (SupervisorStatus, error) {
	return SupervisorStatus{}, errors.New("not implemented")
}

func (f *fakeSupervisorClient) Ping(_ context.Context) error {
	f.pingCount++
	if f.pingDelay > 0 {
		time.Sleep(f.pingDelay)
	}
	return f.pingErr
}

func TestPollReady_succeedsImmediately(t *testing.T) {
	c := &fakeSupervisorClient{pingErr: nil}
	if err := PollReady(c, 1*time.Second); err != nil {
		t.Errorf("expected nil; got %v", err)
	}
	if c.pingCount == 0 {
		t.Errorf("expected at least one ping; got %d", c.pingCount)
	}
}

func TestPollReady_timesOut(t *testing.T) {
	c := &fakeSupervisorClient{pingErr: errors.New("connection refused")}
	err := PollReady(c, 100*time.Millisecond)
	if err == nil {
		t.Errorf("expected timeout error; got nil")
	}
	if c.pingCount == 0 {
		t.Errorf("expected at least one ping attempt; got %d", c.pingCount)
	}
}

func TestPollReady_eventuallySucceeds(t *testing.T) {
	failsBefore := 3
	calls := 0
	c := &countingClient{
		ping: func() error {
			calls++
			if calls <= failsBefore {
				return errors.New("not ready")
			}
			return nil
		},
	}
	if err := PollReady(c, 2*time.Second); err != nil {
		t.Errorf("expected success after retries; got %v", err)
	}
	if calls < failsBefore+1 {
		t.Errorf("expected at least %d calls; got %d", failsBefore+1, calls)
	}
}

type countingClient struct {
	ping func() error
}

func (c *countingClient) Status(_ context.Context) (SupervisorStatus, error) {
	return SupervisorStatus{}, errors.New("not implemented")
}

func (c *countingClient) Ping(_ context.Context) error {
	return c.ping()
}

func TestRestartLoopGuard(t *testing.T) {
	g := newRestartLoopGuard(3, 60*time.Second)
	now := time.Now()

	// First three should succeed.
	for i := 0; i < 3; i++ {
		if !g.allowAt(now.Add(time.Duration(i) * time.Second)) {
			t.Errorf("restart %d: expected allowed", i+1)
		}
	}
	// Fourth within the window should be refused.
	if g.allowAt(now.Add(10 * time.Second)) {
		t.Errorf("restart 4 within window: expected refused")
	}
	// After the window expires, restarts should be allowed again.
	if !g.allowAt(now.Add(120 * time.Second)) {
		t.Errorf("restart after window: expected allowed")
	}
}

// fakeRestartHelpers captures invocations made by RestartSupervisor.
type fakeRestartHelpers struct {
	systemctlArgs []string
	systemctlErr  error
	launchctlArgs []string
	launchctlErr  error
	killedPID     int
	killErr       error
	spawnExe      string
	spawnArgv     []string
	spawnErr      error
}

func (f *fakeRestartHelpers) Systemctl(args ...string) error {
	f.systemctlArgs = append([]string(nil), args...)
	return f.systemctlErr
}

func (f *fakeRestartHelpers) Launchctl(args ...string) error {
	f.launchctlArgs = append([]string(nil), args...)
	return f.launchctlErr
}

func (f *fakeRestartHelpers) Kill(pid int) error {
	f.killedPID = pid
	return f.killErr
}

func (f *fakeRestartHelpers) Spawn(exe string, argv ...string) error {
	f.spawnExe = exe
	f.spawnArgv = append([]string(nil), argv...)
	return f.spawnErr
}

func TestRestartSupervisor_SystemdManaged(t *testing.T) {
	h := &fakeRestartHelpers{}
	spec := restartSpec{
		SystemdManaged: true,
		PID:            12345,
		ExePath:        "/home/op/.local/bin/gc",
		Argv:           []string{"supervisor", "run"},
		ServiceName:    "gascity-supervisor.service",
	}
	if err := restartSupervisor(spec, restartHelpersFromFake(h)); err != nil {
		t.Fatalf("restartSupervisor: %v", err)
	}
	wantArgs := []string{"--user", "restart", "gascity-supervisor.service"}
	if len(h.systemctlArgs) != len(wantArgs) {
		t.Fatalf("systemctl args = %v, want %v", h.systemctlArgs, wantArgs)
	}
	for i := range wantArgs {
		if h.systemctlArgs[i] != wantArgs[i] {
			t.Errorf("systemctl arg %d = %q, want %q", i, h.systemctlArgs[i], wantArgs[i])
		}
	}
	if h.killedPID != 0 {
		t.Errorf("Kill called for systemd-managed restart (pid=%d); should delegate to systemd only", h.killedPID)
	}
	if h.spawnExe != "" {
		t.Errorf("Spawn called for systemd-managed restart; should delegate to systemd only")
	}
	if len(h.launchctlArgs) != 0 {
		t.Errorf("launchctl invoked for systemd-managed restart: %v", h.launchctlArgs)
	}
}

func TestRestartSupervisor_LaunchdManagedRefreshesRegistration(t *testing.T) {
	var calls [][]string
	h := restartHelpers{
		Launchctl: func(args ...string) error {
			calls = append(calls, append([]string(nil), args...))
			return nil
		},
		ValidateLaunchdPlist: func(string, string, string) error { return nil },
		WaitLaunchdUnloaded:  func(string, time.Duration) error { return nil },
	}
	spec := restartSpec{
		LaunchdManaged: true,
		PID:            12345,
		LaunchdLabel:   "com.gascity.supervisor.test",
	}
	if err := restartSupervisor(spec, h); err != nil {
		t.Fatalf("restartSupervisor: %v", err)
	}
	target := supervisorLaunchdServiceTarget(spec.LaunchdLabel)
	domain := "gui/" + strconv.Itoa(os.Getuid())
	// enable precedes bootstrap: `gc supervisor stop` disables the job
	// (#5334) and launchd refuses to bootstrap a disabled service.
	want := [][]string{
		{"bootout", supervisorLaunchdServiceTarget(legacyGastownLaunchdLabel)},
		{"bootout", target},
		{"enable", target},
		{"bootstrap", domain, supervisorLaunchdPlistPath()},
		{"kickstart", "-p", target},
	}
	if !reflect.DeepEqual(want, calls) {
		t.Fatalf("launchctl calls = %v, want %v", calls, want)
	}
}

// TestRestartSupervisor_LaunchdManagedEnablesDisabledJobBeforeBootstrap
// models launchd's disabled set: a label that `gc supervisor stop` disabled
// (#5334) but that is still loaded reaches the refresh path, and launchd
// refuses to bootstrap it until it is enabled again. Enabling after bootstrap
// would leave the supervisor booted out and unregistered.
func TestRestartSupervisor_LaunchdManagedEnablesDisabledJobBeforeBootstrap(t *testing.T) {
	const label = "com.gascity.supervisor.test"
	target := supervisorLaunchdServiceTarget(label)
	disabled := true
	var calls [][]string
	h := restartHelpers{
		Launchctl: func(args ...string) error {
			calls = append(calls, append([]string(nil), args...))
			switch args[0] {
			case "enable":
				if len(args) == 2 && args[1] == target {
					disabled = false
				}
			case "bootstrap":
				if disabled {
					return errors.New("Bootstrap failed: 119: Service is disabled")
				}
			}
			return nil
		},
		ValidateLaunchdPlist: func(string, string, string) error { return nil },
		WaitLaunchdUnloaded:  func(string, time.Duration) error { return nil },
	}
	if err := restartSupervisor(restartSpec{LaunchdManaged: true, LaunchdLabel: label}, h); err != nil {
		t.Fatalf("restartSupervisor on a disabled job = %v, want nil; launchctl calls = %v", err, calls)
	}
	if disabled {
		t.Fatalf("job still disabled after refresh; launchctl calls = %v", calls)
	}
}

func TestRestartSupervisor_LaunchdManagedPostBootoutFailureIncludesRecovery(t *testing.T) {
	domain := supervisorLaunchdDomain()
	const label = "com.gascity.supervisor.test"
	target := supervisorLaunchdServiceTarget(label)
	const plistPath = "/tmp/Gas City/supervisor.plist"
	startRecovery := "launchctl kickstart -p " + shellSingleQuote(target)
	bootstrapRecovery := "launchctl bootstrap " + shellSingleQuote(domain) + " " + shellSingleQuote(plistPath) + " && " + startRecovery
	enableRecovery := "launchctl enable " + shellSingleQuote(target) + " && " + bootstrapRecovery
	tests := []struct {
		failingCommand string
		wantRecovery   string
	}{
		{"enable", enableRecovery},
		{"bootstrap", bootstrapRecovery},
		{"kickstart", startRecovery},
	}
	for _, tc := range tests {
		t.Run(tc.failingCommand, func(t *testing.T) {
			h := restartHelpers{
				Launchctl: func(args ...string) error {
					if args[0] == tc.failingCommand {
						return errors.New("injected failure")
					}
					return nil
				},
				ValidateLaunchdPlist: func(string, string, string) error { return nil },
				WaitLaunchdUnloaded:  func(string, time.Duration) error { return nil },
			}
			err := restartSupervisor(restartSpec{
				LaunchdManaged:   true,
				LaunchdLabel:     label,
				LaunchdPlistPath: plistPath,
			}, h)
			if err == nil {
				t.Fatalf("restartSupervisor succeeded, want %s failure", tc.failingCommand)
			}
			for _, want := range []string{tc.failingCommand, "supervisor is stopped", tc.wantRecovery} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("restartSupervisor error = %q, want recovery detail %q", err, want)
				}
			}
			if !launchdRestartRollbackSafe(err) {
				t.Fatalf("%s failure should be safe to roll back: %v", tc.failingCommand, err)
			}
		})
	}
}

func TestRestartSupervisor_LaunchdManagedStopsWhenBootoutFails(t *testing.T) {
	var calls [][]string
	h := restartHelpers{
		Launchctl: func(args ...string) error {
			calls = append(calls, append([]string(nil), args...))
			return errors.New("permission denied")
		},
		ValidateLaunchdPlist: func(string, string, string) error { return nil },
		WaitLaunchdUnloaded:  func(string, time.Duration) error { return nil },
	}
	spec := restartSpec{LaunchdManaged: true, LaunchdLabel: "com.gascity.supervisor.test"}
	err := restartSupervisor(spec, h)
	if err == nil || !strings.Contains(err.Error(), "bootout") {
		t.Fatalf("restartSupervisor error = %v, want bootout failure", err)
	}
	if !launchdRestartRollbackSafe(err) {
		t.Fatalf("bootout-reported failure after confirmed unload should be rollback-safe: %v", err)
	}
	wantCalls := [][]string{
		{"bootout", supervisorLaunchdServiceTarget(legacyGastownLaunchdLabel)},
		{"bootout", supervisorLaunchdServiceTarget(spec.LaunchdLabel)},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("launchctl calls = %v, want legacy cleanup then failed target bootout %v", calls, wantCalls)
	}
}

func TestRestartSupervisor_LaunchdManagedValidatesBeforeBootout(t *testing.T) {
	launchctlCalled := false
	err := restartSupervisor(restartSpec{LaunchdManaged: true}, restartHelpers{
		ValidateLaunchdPlist: func(string, string, string) error { return errors.New("malformed plist") },
		WaitLaunchdUnloaded:  func(string, time.Duration) error { return nil },
		Launchctl: func(...string) error {
			launchctlCalled = true
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Fatalf("restartSupervisor error = %v, want preflight failure", err)
	}
	if launchctlCalled {
		t.Fatal("launchctl called after plist preflight failed")
	}
}

func TestRestartSupervisor_LaunchdManagedWaitsForBootoutCompletion(t *testing.T) {
	var calls [][]string
	err := restartSupervisor(restartSpec{LaunchdManaged: true}, restartHelpers{
		ValidateLaunchdPlist: func(string, string, string) error { return nil },
		WaitLaunchdUnloaded: func(string, time.Duration) error {
			return errors.New("job still loaded")
		},
		Launchctl: func(args ...string) error {
			calls = append(calls, append([]string(nil), args...))
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "waiting for launchd bootout") {
		t.Fatalf("restartSupervisor error = %v, want bootout wait failure", err)
	}
	if launchdRestartRollbackSafe(err) {
		t.Fatalf("unknown bootout state must not be auto-rolled back: %v", err)
	}
	wantCalls := [][]string{
		{"bootout", supervisorLaunchdServiceTarget(legacyGastownLaunchdLabel)},
		{"bootout", supervisorLaunchdServiceTarget("")},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("launchctl calls = %v, want legacy cleanup then target bootout %v", calls, wantCalls)
	}
}

func TestRestartSupervisor_DirectLaunch(t *testing.T) {
	h := &fakeRestartHelpers{}
	spec := restartSpec{
		SystemdManaged: false,
		PID:            12345,
		ExePath:        "/home/op/.local/bin/gc",
		Argv:           []string{"supervisor", "run"},
	}
	if err := restartSupervisor(spec, restartHelpersFromFake(h)); err != nil {
		t.Fatalf("restartSupervisor: %v", err)
	}
	if len(h.systemctlArgs) != 0 {
		t.Errorf("systemctl invoked for direct restart: %v", h.systemctlArgs)
	}
	if len(h.launchctlArgs) != 0 {
		t.Errorf("launchctl invoked for direct restart: %v", h.launchctlArgs)
	}
	if h.killedPID != 12345 {
		t.Errorf("Kill called with pid %d, want 12345", h.killedPID)
	}
	if h.spawnExe != "/home/op/.local/bin/gc" {
		t.Errorf("Spawn exe = %q, want /home/op/.local/bin/gc", h.spawnExe)
	}
	wantArgv := []string{"supervisor", "run"}
	if len(h.spawnArgv) != len(wantArgv) {
		t.Fatalf("spawn argv = %v, want %v", h.spawnArgv, wantArgv)
	}
	for i := range wantArgv {
		if h.spawnArgv[i] != wantArgv[i] {
			t.Errorf("spawn argv[%d] = %q, want %q", i, h.spawnArgv[i], wantArgv[i])
		}
	}
}

// TestRestartSupervisor_SystemdFailureSurfaces ensures a systemctl
// failure is propagated rather than swallowed; without this the operator
// would see "Restarting... ready" while the supervisor stayed dead.
func TestRestartSupervisor_SystemdFailureSurfaces(t *testing.T) {
	h := &fakeRestartHelpers{systemctlErr: errors.New("unit not loaded")}
	spec := restartSpec{
		SystemdManaged: true,
		PID:            1,
		ServiceName:    "gascity-supervisor.service",
	}
	err := restartSupervisor(spec, restartHelpersFromFake(h))
	if err == nil {
		t.Fatal("expected systemctl error to surface")
	}
}

// TestRestartSupervisor_DirectKillFailureSurfaces guards against
// silently spawning a second supervisor when the first one wouldn't die.
func TestRestartSupervisor_DirectKillFailureSurfaces(t *testing.T) {
	h := &fakeRestartHelpers{killErr: errors.New("permission denied")}
	spec := restartSpec{
		SystemdManaged: false,
		PID:            12345,
		ExePath:        "/home/op/.local/bin/gc",
	}
	err := restartSupervisor(spec, restartHelpersFromFake(h))
	if err == nil {
		t.Fatal("expected kill error to surface; otherwise we'd race a duplicate supervisor")
	}
	if h.spawnExe != "" {
		t.Errorf("Spawn called after Kill failed; should abort instead")
	}
}

// restartHelpersFromFake adapts the test fake to the production
// restartHelpers contract.
func restartHelpersFromFake(f *fakeRestartHelpers) restartHelpers {
	return restartHelpers{
		Systemctl: f.Systemctl,
		Launchctl: f.Launchctl,
		Kill:      f.Kill,
		Spawn:     f.Spawn,
	}
}
