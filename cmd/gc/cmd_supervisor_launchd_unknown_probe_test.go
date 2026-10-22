package main

import (
	"strings"
	"testing"
	"time"
)

// probeScript drives supervisorLaunchdLoaded through a fixed sequence of
// (loaded, absent, detail) states, repeating the last state once the script
// runs out, so the tests below assert on state transitions rather than on
// wall-clock timing.
type probeState struct {
	loaded, absent bool
	detail         string
}

func scriptLaunchdProbe(t *testing.T, states ...probeState) *int {
	t.Helper()
	oldLoaded := supervisorLaunchdLoaded
	oldPoll := supervisorLaunchdStopPollInterval
	oldBudget := supervisorLaunchdUnknownProbeBudget
	t.Cleanup(func() {
		supervisorLaunchdLoaded = oldLoaded
		supervisorLaunchdStopPollInterval = oldPoll
		supervisorLaunchdUnknownProbeBudget = oldBudget
	})
	supervisorLaunchdStopPollInterval = time.Millisecond
	supervisorLaunchdUnknownProbeBudget = 20 * time.Millisecond
	calls := 0
	supervisorLaunchdLoaded = func(string) (bool, bool, string) {
		i := calls
		calls++
		if i >= len(states) {
			i = len(states) - 1
		}
		st := states[i]
		return st.loaded, st.absent, st.detail
	}
	return &calls
}

var (
	probeUnknown = probeState{false, false, "exit status 1"}
	probeLoaded  = probeState{true, false, "pid = 42"}
	probeAbsent  = probeState{false, true, ""}
)

// TestWaitForSupervisorLaunchdAbsentBoundsNeverLoadedUnknown is the red test
// for gc-pk1x: a job the probe has never seen loaded, with the probe failing
// on every poll (launchctl missing, a non-service error, the acceptance
// harness's exit-1 shim), returns after supervisorLaunchdUnknownProbeBudget
// instead of the unload deadline. Before the fix every darwin `gc init`
// under the harness spent launchdRefreshWaitTimeout (90s) here.
func TestWaitForSupervisorLaunchdAbsentBoundsNeverLoadedUnknown(t *testing.T) {
	calls := scriptLaunchdProbe(t, probeUnknown)
	err := waitForSupervisorLaunchdAbsent("com.example.never", "gui/501/com.example.never", time.Now().Add(time.Hour))
	if err == nil {
		t.Fatal("want an error when the probe never answers, got nil")
	}
	if !strings.Contains(err.Error(), "could not be confirmed unloaded") || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error = %v, want the unconfirmed-unload wording with the probe detail", err)
	}
	if *calls < 2 {
		t.Fatalf("probes = %d, want more than one poll before giving up (transient errors get retried)", *calls)
	}
}

// TestWaitForSupervisorLaunchdAbsentTransientUnknownThenAbsent pins that a
// short probe failure before the job reads absent is retried, not fatal.
func TestWaitForSupervisorLaunchdAbsentTransientUnknownThenAbsent(t *testing.T) {
	scriptLaunchdProbe(t, probeUnknown, probeUnknown, probeAbsent)
	if err := waitForSupervisorLaunchdAbsent("com.example.blip", "gui/501/com.example.blip", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("a probe that recovers to absent inside the budget must return nil, got %v", err)
	}
}

// TestWaitForSupervisorLaunchdAbsentKeepsDeadlineOnceLoaded pins the other
// arm: once the probe has confirmed LOADED, a stretch of failing probes longer
// than the unknown budget does not abort the wait; the job unloading later
// still returns nil. The deadline (which must outlast
// supervisorLaunchdExitTimeout) is the only bound for a real unload.
func TestWaitForSupervisorLaunchdAbsentKeepsDeadlineOnceLoaded(t *testing.T) {
	states := []probeState{probeLoaded, probeLoaded}
	for i := 0; i < 60; i++ { // 60 x 1ms poll = 3x the 20ms budget
		states = append(states, probeUnknown)
	}
	states = append(states, probeAbsent)
	calls := scriptLaunchdProbe(t, states...)
	if err := waitForSupervisorLaunchdAbsent("com.example.live", "gui/501/com.example.live", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("loaded -> unknown (past the budget) -> absent must return nil, got %v", err)
	}
	if *calls < len(states) {
		t.Fatalf("probes = %d, want the loop to reach the absent state at poll %d instead of giving up on the unknown budget", *calls, len(states))
	}
}

// TestWaitForSupervisorLaunchdAbsentLoadedThenUnknownRunsToDeadline pins that
// after loaded was seen, an endless unknown reads as the deadline error, with
// the "still loaded" wording that names what was last confirmed.
func TestWaitForSupervisorLaunchdAbsentLoadedThenUnknownRunsToDeadline(t *testing.T) {
	scriptLaunchdProbe(t, probeLoaded, probeUnknown)
	err := waitForSupervisorLaunchdAbsent("com.example.stuck", "gui/501/com.example.stuck", time.Now().Add(60*time.Millisecond))
	if err == nil {
		t.Fatal("want the deadline error, got nil")
	}
	if !strings.Contains(err.Error(), "could not be confirmed unloaded") {
		t.Fatalf("error = %v, want the unconfirmed wording (last probe was unknown)", err)
	}
}

// TestLoadAndStartSupervisorLaunchdSkipsLegacyBootoutWithoutPlist pins that a
// machine with no legacy gastown plist never boots out the legacy label: no
// launchctl call and no unload wait for a job launchd never saw.
func TestLoadAndStartSupervisorLaunchdSkipsLegacyBootoutWithoutPlist(t *testing.T) {
	oldRun := supervisorLaunchctlRun
	oldPresent := legacyGastownLaunchdPlistPresent
	t.Cleanup(func() {
		supervisorLaunchctlRun = oldRun
		legacyGastownLaunchdPlistPresent = oldPresent
	})
	scriptLaunchdProbe(t, probeAbsent)
	legacyGastownLaunchdPlistPresent = func() bool { return false }
	var targets []string
	supervisorLaunchctlRun = func(args ...string) error {
		if len(args) > 0 && args[0] == "bootout" {
			targets = append(targets, args[1])
		}
		return nil
	}
	if err := loadAndStartSupervisorLaunchd("/nonexistent/gc.plist", "com.example.gc"); err != nil {
		t.Fatalf("loadAndStart with absent jobs: %v", err)
	}
	for _, tgt := range targets {
		if strings.Contains(tgt, legacyGastownLaunchdLabel) {
			t.Fatalf("legacy label %s was booted out although no legacy plist exists (targets %v)", legacyGastownLaunchdLabel, targets)
		}
	}
	if len(targets) != 1 || !strings.Contains(targets[0], "com.example.gc") {
		t.Fatalf("bootout targets = %v, want exactly the new label", targets)
	}
}
