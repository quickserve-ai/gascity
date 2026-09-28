package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// Review round 3 (ga-yuiof4 item 3; Astra r3 1, new-1): session starts that
// bypassed the prepared-start bracket — the control-dispatcher tick, the
// launch-drift Relaunch, and in-process API wakes — must all move the
// session-start fence.

func r3CleanSessions() *sessionBeadSnapshot { return newSessionBeadSnapshotFromInfos(nil) }

func anyStartCall(sp *runtime.Fake) bool {
	for _, c := range sp.SnapshotCalls() {
		if c.Method == "Start" {
			return true
		}
	}
	return false
}

func waitForStartCall(t *testing.T, sp *runtime.Fake, what string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for !anyStartCall(sp) {
		select {
		case <-poll.C:
		case <-deadline:
			t.Fatalf("timed out waiting for %s to start a runtime", what)
		}
	}
}

// TestWorktreeReaperLane_ControlDispatcherStartAfterScanIsProtected (Astra r3
// finding 1, hole a): the control-dispatcher tick starts a dispatcher session
// after the pass's liveness scan started. Its start must move the fence, so the
// pass's removal is refused.
func TestWorktreeReaperLane_ControlDispatcherStartAfterScanIsProtected(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "100")
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
	reapStore := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)

	cityStore := beads.NewMemStore()
	dispatchRigStore := beads.NewMemStore()
	if _, err := dispatchRigStore.Create(beads.Bead{
		Title:  "Finalize rig workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:     "fixture/core.control-dispatcher",
			beadmeta.RootStoreRefMetadataKey: "rig:fixture",
		},
	}); err != nil {
		t.Fatalf("create control bead: %v", err)
	}
	maxActive := 1
	enabled := true
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Prefix: "ga"},
		Rigs: []config.Rig{
			{Name: reapTestRigName, Path: rigRoot},
			{Name: "fixture", Path: t.TempDir()},
		},
		Agents: []config.Agent{
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
			{
				Name:              config.ControlDispatcherAgentName,
				BindingName:       "core",
				Dir:               "fixture",
				StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
				MaxActiveSessions: &maxActive,
			},
		},
		Daemon: config.DaemonConfig{AutoReapClosedBeadWorktrees: &enabled},
	}
	sp := runtime.NewFake()
	cr := &CityRuntime{
		cityPath:      cityPath,
		cityName:      "test-city",
		cfg:           cfg,
		sp:            sp,
		dops:          newDrainOps(sp),
		rec:           events.Discard,
		sessionDrains: newDrainTracker(),
		logPrefix:     "gc test",
		stdout:        io.Discard,
		stderr:        io.Discard,
	}
	cr.buildFnWithSessionBeads = supervisorBuildAgentsFnWithSessionBeads(cityPath, "test-city", io.Discard)
	cr.setControllerState(&controllerState{
		cfg:           cfg,
		sp:            sp,
		beadStores:    map[string]beads.Store{reapTestRigName: reapStore, "fixture": dispatchRigStore},
		cityBeadStore: cityStore,
		eventProv:     events.NewFake(),
		cityName:      "test-city",
		cityPath:      cityPath,
	})
	cr.registerSessionStartFence(cr.sp)
	t.Cleanup(func() { cr.retireSessionStartFences() })

	var scans atomic.Int32
	prevScan := collectLiveWorktreeStateFn
	collectLiveWorktreeStateFn = func() liveWorktreeState {
		if scans.Add(1) == 1 {
			// The scan has started; the control dispatcher now starts a
			// session this scan cannot see.
			cr.controlDispatcherTick(context.Background())
			waitForStartCall(t, sp, "the control-dispatcher tick")
		}
		return liveWorktreeState{scanned: true}
	}
	t.Cleanup(func() { collectLiveWorktreeStateFn = prevScan })

	cr.triggerWorktreeReaperPass(context.Background(), cfg, true, r3CleanSessions())
	waitWorktreeReaperIdle(t, cr)

	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree %s was removed although the control dispatcher started a session after the scan (stat err=%v)", wt, err)
	}
	assertProtectedFor(t, cr, wt, "session-start fence")
}

// TestWorktreeReaperLane_LaunchDriftRelaunchRefusesAgentHomeReset (Astra r3
// new-1): the agent-home cleanup reads a home, pauses, and the reconciler's
// launch-drift path relaunches the agent in that home. The relaunch must move
// the fence, so the reset that follows is refused.
func TestWorktreeReaperLane_LaunchDriftRelaunchRefusesAgentHomeReset(t *testing.T) {
	env, _, session := setupLaunchDriftResumeEnv(t)
	fence := &sessionStartFence{}
	env.startOptions = append(env.startOptions, withSessionStartFence(fence))

	cityPath, builderWTPath, _ := setupAgentHomeWorktreeCleanupTest(t)
	homeStore := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)
	if err := os.WriteFile(filepath.Join(builderWTPath, worktreeStaleFileName), []byte("branch=builder/ga-abc123\n"), 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}
	probe := &relaunchDuringPauseProbe{
		fakeAgentWorktreeGit: fakeAgentWorktreeGit{isRepo: true, currentBranch: "builder/ga-abc123"},
		pause: func() {
			env.reconcile([]beads.Bead{session})
		},
	}
	orig := newAgentWorktreeGitProbe
	newAgentWorktreeGitProbe = func(string) agentWorktreeGitProbe { return probe }
	t.Cleanup(func() { newAgentWorktreeGitProbe = orig })

	cleaned := cleanupClosedBeadAgentHomeWorktreesGuarded(cityPath, agentHomeConfig(), map[string]beads.Store{"ga-rig": homeStore}, io.Discard,
		&agentHomeResetGuard{stillEnabled: func() bool { return true }, startFence: fence})

	if got := env.sp.CountCalls("Relaunch", "worker"); got != 1 {
		t.Fatalf("Relaunch calls = %d, want 1: the test did not drive a launch-drift relaunch; stderr=%s", got, env.stderr.String())
	}
	if probe.checkoutDetachRef != "" || cleaned != 0 {
		t.Fatalf("agent home was reset (detach=%q cleaned=%d) although the agent was relaunched in it after the home was read", probe.checkoutDetachRef, cleaned)
	}
}

// relaunchDuringPauseProbe runs pause from HasUncommittedWork — after the
// cleanup read the home's branch and bead, before the reset.
type relaunchDuringPauseProbe struct {
	fakeAgentWorktreeGit
	pause func()
}

func (p *relaunchDuringPauseProbe) HasUncommittedWork() bool {
	if p.pause != nil {
		p.pause()
		p.pause = nil
	}
	return false
}

// TestWorktreeReaperLane_InProcessAPIWakeAfterScanIsProtected (Astra r3
// finding 1): the in-process API server wakes a session through a worker
// factory built on the controller state's provider — the same construction as
// internal/api's workerFactory — after the pass's scan started. That start must
// move the fence.
func TestWorktreeReaperLane_InProcessAPIWakeAfterScanIsProtected(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
	reapStore := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)
	cfg := reviewReapConfig(rigRoot)

	sp := runtime.NewFake()
	cr := newReapTickRuntime(cityPath, cfg, reapStore, io.Discard)
	cr.sp = sp
	cs := &controllerState{cfg: cfg, sp: sp, cityName: "test", cityPath: cityPath, beadStores: map[string]beads.Store{reapTestRigName: reapStore}}
	cr.cs = cs
	cr.registerSessionStartFence(cr.sp)
	t.Cleanup(func() { cr.retireSessionStartFences() })

	sessionStore := beads.NewMemStore()
	mgr := sessionpkg.NewManagerWithOptions(sessionStore, sp)
	info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{BeadOnly: true, Template: "worker", Title: "Worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	var scans atomic.Int32
	prevScan := collectLiveWorktreeStateFn
	collectLiveWorktreeStateFn = func() liveWorktreeState {
		if scans.Add(1) == 1 {
			factory, ferr := worker.NewFactory(worker.FactoryConfig{Store: sessionStore, Provider: cs.SessionProvider(), CityPath: cityPath})
			if ferr != nil {
				t.Errorf("worker factory: %v", ferr)
				return liveWorktreeState{scanned: true}
			}
			handle, herr := factory.SessionByID(info.ID)
			if herr != nil {
				t.Errorf("worker handle: %v", herr)
				return liveWorktreeState{scanned: true}
			}
			if serr := handle.Start(context.Background()); serr != nil {
				t.Errorf("API-style wake: %v", serr)
			}
		}
		return liveWorktreeState{scanned: true}
	}
	t.Cleanup(func() { collectLiveWorktreeStateFn = prevScan })

	cr.triggerWorktreeReaperPass(context.Background(), cfg, true, r3CleanSessions())
	waitWorktreeReaperIdle(t, cr)

	if !anyStartCall(sp) {
		t.Fatal("the API-style wake never reached the provider's Start; the test did not exercise a start")
	}
	_, statErr := os.Stat(wt)
	if errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree %s was removed although an in-process API wake started a session after the scan", wt)
	}
	assertProtectedFor(t, cr, wt, "session-start fence")
}
