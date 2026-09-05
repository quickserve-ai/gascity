package tmux

import (
	"slices"
	"testing"
	"time"
)

func TestProviderEnvSkipsEscapeForPiAlias(t *testing.T) {
	if !providerEnvSkipsEscape("my-pi/tmux") {
		t.Fatal("pi provider alias should skip pre-enter Escape")
	}
}

func TestProviderEnvSkipsEscapeForCopilot(t *testing.T) {
	if !providerEnvSkipsEscape("copilot") {
		t.Fatal("copilot provider should skip pre-enter Escape")
	}
}

// TestComputeExcludingKillSet_SelfCloseExcludesCallerKeepsAgent locks in the
// fix for the self-close wedge: when `gc session close` runs from inside the
// pane it is tearing down, the caller is a descendant of the pane leader (the
// agent). The caller must be excluded from the TERM list so it survives long
// enough to finish cleanup, while the pane leader (agent) is still reached.
func TestComputeExcludingKillSet_SelfCloseExcludesCallerKeepsAgent(t *testing.T) {
	const (
		agentPID  = "100" // pane leader (e.g. the coding agent) — must be killed
		shellPID  = "101" // intermediate shell spawned by the agent
		callerPID = "102" // gc session close — the excluded caller
	)
	exclude := map[string]bool{callerPID: true}

	killList, killPaneLeader := computeExcludingKillSet(
		agentPID,
		[]string{shellPID, callerPID},
		nil,
		exclude,
	)

	if !killPaneLeader {
		t.Error("pane leader (agent) must be killed, but it was reported excluded")
	}
	if slices.Contains(killList, callerPID) {
		t.Errorf("caller %s must be excluded from TERM list, got %v", callerPID, killList)
	}
	if !slices.Contains(killList, shellPID) {
		t.Errorf("non-excluded descendant %s must be in TERM list, got %v", shellPID, killList)
	}
}

// TestComputeExcludingKillSet_ExternalCallerKillsEverything verifies that when
// the caller lives outside the pane (e.g. the supervisor running the close),
// excluding its PID is a harmless no-op: every process in the pane's tree is
// still terminated.
func TestComputeExcludingKillSet_ExternalCallerKillsEverything(t *testing.T) {
	const agentPID = "200"
	exclude := map[string]bool{"999": true} // external caller, not in the pane tree

	killList, killPaneLeader := computeExcludingKillSet(
		agentPID,
		[]string{"201"},
		[]string{"202"},
		exclude,
	)

	if !killPaneLeader {
		t.Error("pane leader must be killed for an external caller")
	}
	if !slices.Contains(killList, "201") || !slices.Contains(killList, "202") {
		t.Errorf("all pane descendants must be killed, got %v", killList)
	}
}

// TestComputeExcludingKillSet_ExcludedPaneLeaderSurvives guards the degenerate
// case where the pane leader itself is in the exclusion set: it must not be
// signaled directly (the final tmux kill-session reaps it instead).
func TestComputeExcludingKillSet_ExcludedPaneLeaderSurvives(t *testing.T) {
	const agentPID = "300"
	exclude := map[string]bool{agentPID: true}

	_, killPaneLeader := computeExcludingKillSet(agentPID, nil, nil, exclude)

	if killPaneLeader {
		t.Error("an excluded pane leader must not be killed directly")
	}
}

func TestTerminateProcessSetReturnsWhenTerminatedProcessesExit(t *testing.T) {
	alive := map[string]bool{"101": true, "102": true}
	var signals []string
	var sleeps []time.Duration
	now := time.Unix(0, 0)

	terminateProcessSet(
		[]string{"101", "102"},
		time.Second,
		func(pid, signal string) {
			signals = append(signals, signal+":"+pid)
			if signal == "TERM" {
				alive[pid] = false
			}
		},
		func(pid string) bool { return alive[pid] },
		func(delay time.Duration) {
			sleeps = append(sleeps, delay)
			now = now.Add(delay)
		},
		func() time.Time { return now },
	)

	if want := []string{"TERM:101", "TERM:102"}; !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	if len(sleeps) != 0 {
		t.Fatalf("sleep calls = %v, want none after TERM made every process exit", sleeps)
	}
}

func TestTerminateProcessSetKillsOnlyProcessesStillAliveAfterGracePeriod(t *testing.T) {
	alive := map[string]bool{"201": true, "202": true}
	var signals []string
	var slept time.Duration
	now := time.Unix(0, 0)

	terminateProcessSet(
		[]string{"201", "202"},
		2*processExitCheckInterval,
		func(pid, signal string) {
			signals = append(signals, signal+":"+pid)
			if signal == "TERM" && pid == "201" {
				alive[pid] = false
			}
		},
		func(pid string) bool { return alive[pid] },
		func(delay time.Duration) {
			slept += delay
			now = now.Add(delay)
		},
		func() time.Time { return now },
	)

	want := []string{"TERM:201", "TERM:202", "KILL:202"}
	if !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	if slept != 2*processExitCheckInterval {
		t.Fatalf("slept = %s, want full grace period %s for surviving process", slept, 2*processExitCheckInterval)
	}
}

func TestTerminateProcessSetReturnsWhenProcessExitsDuringGracePeriod(t *testing.T) {
	var signals []string
	checks := 0
	slept := time.Duration(0)
	now := time.Unix(0, 0)

	terminateProcessSet(
		[]string{"301"},
		time.Second,
		func(pid, signal string) { signals = append(signals, signal+":"+pid) },
		func(string) bool {
			checks++
			return checks < 3
		},
		func(delay time.Duration) {
			slept += delay
			now = now.Add(delay)
		},
		func() time.Time { return now },
	)

	if want := []string{"TERM:301"}; !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	if slept != 2*processExitCheckInterval {
		t.Fatalf("slept = %s, want two observations (%s)", slept, 2*processExitCheckInterval)
	}
}

func TestTerminateProcessSetCountsProbeTimeAgainstGracePeriod(t *testing.T) {
	var signals []string
	slept := time.Duration(0)
	now := time.Unix(0, 0)
	probeDuration := 2 * processExitCheckInterval

	terminateProcessSet(
		[]string{"401"},
		3*processExitCheckInterval,
		func(pid, signal string) { signals = append(signals, signal+":"+pid) },
		func(string) bool {
			now = now.Add(probeDuration)
			return true
		},
		func(delay time.Duration) {
			slept += delay
			now = now.Add(delay)
		},
		func() time.Time { return now },
	)

	if want := []string{"TERM:401", "KILL:401"}; !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
	if slept != processExitCheckInterval {
		t.Fatalf("slept = %s, want remaining grace budget %s after slow probe", slept, processExitCheckInterval)
	}
}

// knownSet builds a descendant-set lookup from the given pids.
func knownSet(pids ...string) map[string]bool {
	m := make(map[string]bool, len(pids))
	for _, p := range pids {
		m[p] = true
	}
	return m
}

func TestReparentedOrphans_CollectsInitAndSubreaperOrphans(t *testing.T) {
	// leader=100, one live descendant=200. Group also holds:
	//   300 reparented to init (ppid 1) — classic case
	//   400 reparented to systemd --user subreaper (ppid 900) — the case the
	//        old PPID==1 test missed
	//   500 still a child of a live descendant (ppid 200) — owned elsewhere
	//   600 whose parent read failed ("") — must be skipped
	known := knownSet("100", "200")
	parents := map[string]string{
		"300": "1",
		"400": "900", // systemd --user pid, not init
		"500": "200",
		"600": "",
	}
	parentOf := func(pid string) string { return parents[pid] }

	got := reparentedOrphans([]string{"200", "300", "400", "500", "600"}, known, parentOf)
	slices.Sort(got)
	want := []string{"300", "400"}
	if !slices.Equal(got, want) {
		t.Fatalf("reparentedOrphans = %v, want %v", got, want)
	}
}

func TestReparentedOrphans_SkipsKnownDescendants(t *testing.T) {
	known := knownSet("100", "200", "300")
	parentOf := func(string) string { return "1" }
	if got := reparentedOrphans([]string{"200", "300"}, known, parentOf); len(got) != 0 {
		t.Fatalf("reparentedOrphans = %v, want empty (all are known descendants)", got)
	}
}

// TestFilterTmuxServerPIDs_SparesTheServerKillsThePane is the ga-03ixvj guard.
// The mayor is started at wave 0 with no server running, so its new-session
// BECOMES the server: server and pane are founded in the same instant, two PIDs
// apart, and the server keeps the mayor's argv. Killing the mayor took the
// whole city down three times. Whatever path put the server PID in a kill list
// was never identified, so the guard refuses it at the choke point.
func TestFilterTmuxServerPIDs_SparesTheServerKillsThePane(t *testing.T) {
	// PIDs from the 21:54Z repro: server 71744, mayor pane 71746.
	servers := map[string]bool{"71744": true}
	got := filterTmuxServerPIDs([]string{"71744", "71746", "71750"}, servers)
	want := []string{"71746", "71750"}
	if !slices.Equal(got, want) {
		t.Fatalf("filterTmuxServerPIDs = %v, want %v — the server must be spared and everything else killed", got, want)
	}
}

// TestFilterTmuxServerPIDs_NoServersIsAPassthrough pins that the guard cannot
// block an ordinary kill. If ps fails, liveTmuxServerPIDs returns nil and the
// kill list must survive intact — degrading to the old behavior beats refusing
// to clean up a session.
func TestFilterTmuxServerPIDs_NoServersIsAPassthrough(t *testing.T) {
	pids := []string{"100", "200"}
	got := filterTmuxServerPIDs(pids, nil)
	if !slices.Equal(got, pids) {
		t.Fatalf("filterTmuxServerPIDs with no known servers = %v, want %v unchanged", got, pids)
	}
}

// TestIsTmuxServerCommand covers the argv half of server detection. The server
// keeps the argv of whatever founded it, which for this city is literally the
// mayor's new-session line — so matching must key on the binary, not on the
// presence of a subcommand.
func TestIsTmuxServerCommand(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{"tmux -u -L gastown new-session -d -s gastown__mayor -c /x/.gc/agents/mayor", true},
		{"/opt/homebrew/bin/tmux -u -L gastown new-session -d -s gastown__mayor", true},
		{"tmux", true},
		{"claude --model opus[1m]", false},
		{"node /usr/local/bin/tmuxinator", false},
		{"", false},
	} {
		if got := isTmuxServerCommand(tc.command); got != tc.want {
			t.Errorf("isTmuxServerCommand(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

// TestTerminateProcessesNeverSignalsTheTmuxServer proves the ga-03ixvj guard is
// WIRED IN, not just implemented. It drives the real terminateProcesses with a
// stubbed server lookup and a captured kill, and asserts no signal of any kind
// — TERM or KILL — ever reaches the server PID, while the session processes are
// still terminated normally.
func TestTerminateProcessesNeverSignalsTheTmuxServer(t *testing.T) {
	const serverPID = "71744"

	origServers, origKill := tmuxServerPIDsFn, killSignalFn
	t.Cleanup(func() { tmuxServerPIDsFn, killSignalFn = origServers, origKill })

	tmuxServerPIDsFn = func() map[string]bool { return map[string]bool{serverPID: true} }

	var signaled []string
	killSignalFn = func(pid, signal string) { signaled = append(signaled, signal+":"+pid) }

	// The mayor's own teardown set: the server and its pane were founded in the
	// same instant, two PIDs apart, so both land in the kill list.
	terminateProcesses([]string{serverPID, "71746", "71750"})

	for _, s := range signaled {
		if s == "TERM:"+serverPID || s == "KILL:"+serverPID {
			t.Fatalf("terminateProcesses signaled the tmux server (%s); signals=%v", s, signaled)
		}
	}
	if len(signaled) == 0 {
		t.Fatal("guard blocked everything — session processes must still be terminated")
	}
	for _, want := range []string{"TERM:71746", "TERM:71750"} {
		if !slices.Contains(signaled, want) {
			t.Errorf("expected %s among signals, got %v", want, signaled)
		}
	}
}
