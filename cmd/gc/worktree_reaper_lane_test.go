package main

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// wedgedGetStore is a rig store whose Get blocks until release is closed —
// the 2026-09-27 outage shape, a native Dolt handle stuck on a store lock that
// the reaper's context-free store call can never escape.
type wedgedGetStore struct {
	beads.Store
	release chan struct{}
	gets    atomic.Int32
	lists   atomic.Int32
}

func (s *wedgedGetStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists.Add(1)
	return s.Store.List(q)
}

func (s *wedgedGetStore) Get(id string) (beads.Bead, error) {
	s.gets.Add(1)
	<-s.release
	return s.Store.Get(id)
}

func newWedgedGetStore(t *testing.T, seed []beads.Bead) (*wedgedGetStore, func()) {
	t.Helper()
	store := &wedgedGetStore{
		Store:   beads.NewMemStoreFrom(1, seed, nil),
		release: make(chan struct{}),
	}
	var once sync.Once
	release := func() { once.Do(func() { close(store.release) }) }
	return store, release
}

// waitWorktreeReaperIdle blocks until the lane has no pass in flight.
func waitWorktreeReaperIdle(t *testing.T, cr *CityRuntime) {
	t.Helper()
	lane := cr.worktreeReaperLaneOf()
	lane.mu.Lock()
	done := lane.done
	lane.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("worktree reaper pass did not finish")
	}
}

func worktreeReaperLaneState(cr *CityRuntime) (inflight bool, started, skipped uint64) {
	lane := cr.worktreeReaperLaneOf()
	lane.mu.Lock()
	defer lane.mu.Unlock()
	return lane.inflight, lane.seq, lane.skippedTotal
}

// TestWorktreeReaperLane_WedgedPassDoesNotHoldTheTick is the outage regression
// (ga-yuiof4 item 3): a reaper pass wedged forever in a rig-store call must not
// hold the controller tick, so the NEXT tick still dispatches orders, and the
// wedged pass must not be joined by a second one.
//
// Against the pre-lane inline reap phase this fails at tick 1 — the tick
// never returns, because the reap phase runs inline and blocks in store.Get.
func TestWorktreeReaperLane_WedgedPassDoesNotHoldTheTick(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
	store, release := newWedgedGetStore(t, []beads.Bead{{ID: "ga-abc123", Status: "closed"}})

	cfg := reapTestConfig(rigRoot)
	enabled := true
	cfg.Daemon = config.DaemonConfig{AutoReapClosedBeadWorktrees: &enabled}
	injectLiveness(t, liveWorktreeState{scanned: true})

	cr := newReapTickRuntime(cityPath, cfg, store, io.Discard)
	// Unwedge and drain the pass before the temp dirs are removed.
	t.Cleanup(func() { waitWorktreeReaperIdle(t, cr) })
	t.Cleanup(release)
	od := &recordingOrderDispatcher{}
	cr.od = od

	for i := 1; i <= 3; i++ {
		done := make(chan struct{})
		go func() {
			defer close(done)
			runReapTickNoWait(t, cr)
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("tick %d did not return while a reaper pass is wedged in a store call: the reap phase is holding the tick (order dispatch ran %d time(s))", i, od.calls.Load())
		}
		if got := od.calls.Load(); got != int32(i) {
			t.Fatalf("after tick %d order dispatch ran %d time(s), want %d", i, got, i)
		}
	}
	waitForReaperCond(t, func() bool { return store.gets.Load() >= 1 }, "the first pass to reach the wedged store")
	if got := store.gets.Load(); got != 1 {
		t.Fatalf("wedged store saw %d Get call(s), want exactly 1: a second pass started beside the wedged one", got)
	}
	inflight, started, skipped := worktreeReaperLaneState(cr)
	if !inflight || started != 1 || skipped != 2 {
		t.Fatalf("lane inflight=%t started=%d skipped=%d, want the one wedged pass in flight and ticks 2-3 skipped", inflight, started, skipped)
	}

	// Unwedged, the pass completes normally and still reaps.
	release()
	waitWorktreeReaperIdle(t, cr)
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree %s survived the unwedged pass, want it reaped (stat err=%v)", wt, err)
	}
}

// TestWorktreeReaperLane_TriggerSkipsWhileInflight drives the trigger directly
// with an injected pass that never returns: every later trigger must return at
// once without starting a pass, and must report the skip and the in-flight
// pass's age in its trace fields.
func TestWorktreeReaperLane_TriggerSkipsWhileInflight(t *testing.T) {
	var passes atomic.Int32
	block := make(chan struct{})
	prev := runWorktreeReaperPassFn
	runWorktreeReaperPassFn = func(worktreeReaperPassInput) worktreeReaperPassResult {
		passes.Add(1)
		<-block
		return worktreeReaperPassResult{}
	}
	cr := newReapTickRuntime(t.TempDir(), &config.City{}, beads.NewMemStore(), io.Discard)
	t.Cleanup(func() {
		close(block)
		waitWorktreeReaperIdle(t, cr)
		runWorktreeReaperPassFn = prev
	})

	first := cr.triggerWorktreeReaperPass(context.Background(), cr.cfg, false, nil)
	if !first.started || first.skippedInflight {
		t.Fatalf("first trigger = %+v, want it to start a pass", first)
	}
	waitForReaperCond(t, func() bool { return passes.Load() == 1 }, "the injected pass to start")

	for i := 0; i < 5; i++ {
		start := time.Now()
		trig := cr.triggerWorktreeReaperPass(context.Background(), cr.cfg, false, nil)
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("trigger %d took %s while a pass is in flight, want it non-blocking", i, took)
		}
		if trig.started || !trig.skippedInflight || trig.seq != first.seq {
			t.Fatalf("trigger %d = %+v, want a skip naming in-flight pass %d", i, trig, first.seq)
		}
		f := trig.fields()
		if f["skipped_inflight"] != true || f["triggered"] != false {
			t.Fatalf("trigger %d fields = %v, want skipped_inflight=true triggered=false", i, f)
		}
		if _, ok := f["inflight_age_ms"].(int64); !ok {
			t.Fatalf("trigger %d fields = %v, want inflight_age_ms", i, f)
		}
	}
	if got := passes.Load(); got != 1 {
		t.Fatalf("injected pass ran %d time(s), want 1 (single flight)", got)
	}
}

// TestWorktreeReaperLane_CompletedPassAllowsNextTrigger proves the latch
// releases: once a pass returns, the next trigger starts a fresh pass, and it
// reports the completed pass under last_pass_*.
func TestWorktreeReaperLane_CompletedPassAllowsNextTrigger(t *testing.T) {
	var passes atomic.Int32
	var gotDryRun []bool
	var mu sync.Mutex
	prev := runWorktreeReaperPassFn
	runWorktreeReaperPassFn = func(in worktreeReaperPassInput) worktreeReaperPassResult {
		passes.Add(1)
		mu.Lock()
		gotDryRun = append(gotDryRun, !in.reapEnabled)
		mu.Unlock()
		return worktreeReaperPassResult{report: reapReport{Reaped: []reapDecision{{BeadID: "ga-x"}}}}
	}
	t.Cleanup(func() { runWorktreeReaperPassFn = prev })
	cr := newReapTickRuntime(t.TempDir(), &config.City{}, beads.NewMemStore(), io.Discard)

	first := cr.triggerWorktreeReaperPass(context.Background(), cr.cfg, false, nil)
	if !first.started {
		t.Fatalf("first trigger = %+v, want it to start a pass", first)
	}
	waitWorktreeReaperIdle(t, cr)

	second := cr.triggerWorktreeReaperPass(context.Background(), cr.cfg, true, nil)
	if !second.started || second.skippedInflight || second.seq != first.seq+1 {
		t.Fatalf("second trigger = %+v, want a new pass after the first completed", second)
	}
	f := second.fields()
	if f["last_pass_seq"] != first.seq || f["last_pass_reaped"] != 1 || f["last_pass_dry_run"] != true {
		t.Fatalf("second trigger fields = %v, want last_pass_* describing the completed dry-run pass %d", f, first.seq)
	}
	waitWorktreeReaperIdle(t, cr)
	if got := passes.Load(); got != 2 {
		t.Fatalf("pass ran %d time(s), want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotDryRun) != 2 || gotDryRun[0] != true || gotDryRun[1] != false {
		t.Fatalf("pass dry-run flags = %v, want [true false] (flags snapshotted per trigger)", gotDryRun)
	}
}

// TestWorktreeReaperLane_PanickingPassReleasesLatch: a panic inside a pass is
// contained by safeTick and must not leave the lane latched forever.
func TestWorktreeReaperLane_PanickingPassReleasesLatch(t *testing.T) {
	prev := runWorktreeReaperPassFn
	runWorktreeReaperPassFn = func(worktreeReaperPassInput) worktreeReaperPassResult { panic("boom") }
	t.Cleanup(func() { runWorktreeReaperPassFn = prev })
	cr := newReapTickRuntime(t.TempDir(), &config.City{}, beads.NewMemStore(), io.Discard)

	cr.triggerWorktreeReaperPass(context.Background(), cr.cfg, true, nil)
	waitWorktreeReaperIdle(t, cr)
	next := cr.triggerWorktreeReaperPass(context.Background(), cr.cfg, true, nil)
	waitWorktreeReaperIdle(t, cr)
	if !next.started || next.fields()["last_pass_panicked"] != true {
		t.Fatalf("trigger after a panicking pass = %+v, want a new pass and last_pass_panicked", next)
	}
}

// TestReaperStoreFence covers the two ways a background pass can outlive its
// handles: the controller context ends, or a reload publishes a different
// store for the rig (and closes the old one shortly after).
func TestReaperStoreFence(t *testing.T) {
	handle := beads.NewMemStore()
	cs := &controllerState{beadStores: map[string]beads.Store{"mrig": handle}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fence := reaperStoreFence(ctx, cs, "mrig", handle)
	if err := fence(); err != nil {
		t.Fatalf("fence on the published handle = %v, want nil", err)
	}

	cs.mu.Lock()
	cs.beadStores = map[string]beads.Store{"mrig": beads.NewMemStore()}
	cs.mu.Unlock()
	if err := fence(); !errors.Is(err, errReaperStoreRetired) {
		t.Fatalf("fence after a reload replaced the handle = %v, want errReaperStoreRetired", err)
	}
	fenced := &reaperFencedStore{Store: handle, fence: fence}
	if _, err := fenced.Get("ga-x"); !errors.Is(err, errReaperStoreRetired) {
		t.Fatalf("fenced Get = %v, want errReaperStoreRetired", err)
	}
	if _, err := fenced.List(beads.ListQuery{AllowScan: true}); !errors.Is(err, errReaperStoreRetired) {
		t.Fatalf("fenced List = %v, want errReaperStoreRetired", err)
	}

	standalone := reaperStoreFence(ctx, nil, "mrig", handle)
	if err := standalone(); err != nil {
		t.Fatalf("standalone fence = %v, want nil", err)
	}
	cancel()
	if err := standalone(); !errors.Is(err, errReaperPassCanceled) {
		t.Fatalf("fence after ctx cancel = %v, want errReaperPassCanceled", err)
	}
}

// TestWorktreeReaperLane_ReloadMidPassFailsClosed is the end-to-end safety
// case for running off-tick: a pass whose pass-1 Get was already inside the
// handle when a reload replaced it must not reap. It pins the POST-call fence
// on Get specifically: the Get's answer must be discarded, dropping the
// candidate before the borrow-veto List is ever issued. (If only the List's
// pre-call fence caught it, the worktree would survive too — so survival alone
// cannot tell the two apart; the List count can.)
func TestWorktreeReaperLane_ReloadMidPassFailsClosed(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
	store, release := newWedgedGetStore(t, []beads.Bead{{ID: "ga-abc123", Status: "closed"}})

	cfg := reapTestConfig(rigRoot)
	enabled := true
	cfg.Daemon = config.DaemonConfig{AutoReapClosedBeadWorktrees: &enabled}
	injectLiveness(t, liveWorktreeState{scanned: true})

	cr := newReapTickRuntime(cityPath, cfg, store, io.Discard)
	cs := &controllerState{cityName: "test", beadStores: map[string]beads.Store{"mrig": store}}
	cr.cs = cs
	t.Cleanup(func() { waitWorktreeReaperIdle(t, cr) })
	t.Cleanup(release)

	trig := cr.triggerWorktreeReaperPass(context.Background(), cfg, true, nil)
	if !trig.started {
		t.Fatalf("trigger = %+v, want a pass", trig)
	}
	// The pass is now inside Get (past the fence). Reload swaps the handle.
	waitForReaperCond(t, func() bool { return store.gets.Load() == 1 }, "the pass to enter Get")
	cs.mu.Lock()
	cs.beadStores = map[string]beads.Store{"mrig": beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)}
	cs.mu.Unlock()
	release()
	waitWorktreeReaperIdle(t, cr)

	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree %s was removed by a pass whose store was retired mid-pass (stat err=%v), want it kept", wt, err)
	}
	if res := lastReaperResult(cr); len(res.report.Reaped) != 0 || len(res.report.Protected) != 0 {
		t.Fatalf("report reaped=%d protected=%d, want 0 and 0: the retired Get's answer must drop the candidate in pass 1", len(res.report.Reaped), len(res.report.Protected))
	}
	if got := store.lists.Load(); got != 0 {
		t.Fatalf("borrow-veto List ran %d time(s) on the retired handle's candidate, want 0: the Get's post-call fence did not discard its answer", got)
	}
}

func waitForReaperCond(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for !cond() {
		select {
		case <-poll.C:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestCleanupClosedBeadAgentHomeWorktreesGuarded_StopsWhenDisabledOrBranchMoved
// covers the agent-home half of the round: the reset is skipped when real
// reaping was turned off mid-pass, and when the home's branch changed after
// its bead was read.
func TestCleanupClosedBeadAgentHomeWorktreesGuarded_StopsWhenDisabledOrBranchMoved(t *testing.T) {
	for _, tc := range []struct {
		name         string
		enabled      bool
		branchAfter  string
		wantDetached bool
	}{
		{name: "enabled and unchanged resets", enabled: true, branchAfter: "builder/ga-abc123", wantDetached: true},
		{name: "disabled by reload skips", enabled: false, branchAfter: "builder/ga-abc123"},
		{name: "branch moved skips", enabled: true, branchAfter: "builder/ga-zzz999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, builderWTPath, _ := setupAgentHomeWorktreeCleanupTest(t)
			store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)
			if err := os.WriteFile(builderWTPath+"/"+worktreeStaleFileName, []byte("branch=builder/ga-abc123\n"), 0o644); err != nil {
				t.Fatalf("write stale marker: %v", err)
			}
			probe := &branchSequenceProbe{fakeAgentWorktreeGit: fakeAgentWorktreeGit{isRepo: true}, branches: []string{"builder/ga-abc123", tc.branchAfter}}
			orig := newAgentWorktreeGitProbe
			newAgentWorktreeGitProbe = func(string) agentWorktreeGitProbe { return probe }
			t.Cleanup(func() { newAgentWorktreeGitProbe = orig })

			cleaned := cleanupClosedBeadAgentHomeWorktreesGuarded(cityPath, agentHomeConfig(), map[string]beads.Store{"ga-rig": store}, io.Discard, &agentHomeResetGuard{stillEnabled: func() bool { return tc.enabled }, startFence: &sessionStartFence{}})
			if detached := probe.checkoutDetachRef != ""; detached != tc.wantDetached || (cleaned == 1) != tc.wantDetached {
				t.Fatalf("cleaned=%d detached=%t, want detached=%t", cleaned, detached, tc.wantDetached)
			}
		})
	}
}

// branchSequenceProbe answers CurrentBranch from a sequence, repeating the
// last entry, so a test can move the branch between two reads.
type branchSequenceProbe struct {
	fakeAgentWorktreeGit
	branches []string
	reads    int
}

func (p *branchSequenceProbe) CurrentBranch() (string, error) {
	i := p.reads
	if i >= len(p.branches) {
		i = len(p.branches) - 1
	}
	p.reads++
	return p.branches[i], nil
}

// TestWorktreeReaperLane_SessionDirsReachThePass: the trigger-time session
// snapshot reaches the pass input, and a later tick's snapshot — published by
// a trigger that SKIPS because the pass is in flight — reaches the pass's
// pre-removal check.
func TestWorktreeReaperLane_SessionDirsReachThePass(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	got := make(chan worktreeReaperPassInput, 1)
	block := make(chan struct{})
	prev := runWorktreeReaperPassFn
	runWorktreeReaperPassFn = func(in worktreeReaperPassInput) worktreeReaperPassResult {
		got <- in
		<-block
		return worktreeReaperPassResult{}
	}
	cr := newReapTickRuntime(t.TempDir(), &config.City{}, beads.NewMemStore(), io.Discard)
	t.Cleanup(func() {
		close(block)
		waitWorktreeReaperIdle(t, cr)
		runWorktreeReaperPassFn = prev
	})

	cr.triggerWorktreeReaperPass(context.Background(), cr.cfg, true, newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{ID: "s1", WorkDir: first}}))
	in := <-got
	if len(in.liveSessionDirs) != 1 || in.liveSessionDirs[0] != first {
		t.Fatalf("pass liveSessionDirs = %v, want [%s]", in.liveSessionDirs, first)
	}
	if skip := cr.triggerWorktreeReaperPass(context.Background(), cr.cfg, true, newSessionBeadSnapshotFromInfos([]sessionpkg.Info{{ID: "s2", WorkDir: second}})); !skip.skippedInflight {
		t.Fatalf("second trigger = %+v, want a skip", skip)
	}
	if in.pre == nil || in.pre.currentSessionDirs == nil {
		t.Fatal("pass input carries no pre-removal session-dir source")
	}
	if cur, valid := in.pre.currentSessionDirs(); !valid || len(cur) != 1 || cur[0] != second {
		t.Fatalf("pre-removal session dirs = %v (valid=%t), want the later tick's [%s]", cur, valid, second)
	}
}

// fenceObservingProvider records whether the session-start fence showed a
// start in flight at the moment the provider was asked to Start.
type fenceObservingProvider struct {
	*runtime.Fake
	fence       *sessionStartFence
	sawInflight atomic.Bool
	starts      atomic.Int32
}

func (p *fenceObservingProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.starts.Add(1)
	p.fence.mu.Lock()
	inflight := p.fence.inflight
	p.fence.mu.Unlock()
	if inflight > 0 {
		p.sawInflight.Store(true)
	}
	return p.Fake.Start(ctx, name, cfg)
}

// TestSessionStartFence_BracketsControllerStarts pins the wiring the fence's
// ordering argument depends on: every controller runtime start, on the sync
// wave path and on the async enqueue path (whose starts outlive the tick),
// happens inside the bracket, and the bracket closes afterwards.
func TestSessionStartFence_BracketsControllerStarts(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			fence := &sessionStartFence{}
			sp := &fenceObservingProvider{Fake: runtime.NewFake(), fence: fence}
			mgr := newSessionManagerWithConfig("", store, sp, nil)
			info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{BeadOnly: true, Template: "worker", Title: "Worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			bead, err := store.Get(info.ID)
			if err != nil {
				t.Fatalf("Get bead: %v", err)
			}
			item := preparedStart{
				candidate: startCandidate{info: sessiontest.SeedBead(t, bead), tp: TemplateParams{TemplateName: "worker"}},
				cfg:       runtime.Config{Command: "claude", WorkDir: info.WorkDir},
			}
			genBefore := fence.generation()
			if async {
				done := make(chan struct{})
				enqueuePreparedStartWaveForCity(context.Background(), []asyncPreparedStart{{item: item, done: func() { close(done) }}},
					"", sp, store, nil, clock.Real{}, events.Discard, 10*time.Second, 0, io.Discard, io.Discard, nil, nil, nil, nil, nil, fence)
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					t.Fatal("async start did not finish")
				}
			} else {
				executePreparedStartWave(context.Background(), []preparedStart{item}, sp, store, 10*time.Second, withSessionStartFence(fence))
			}
			if sp.starts.Load() == 0 {
				t.Fatal("provider Start was never called; the test did not exercise a start")
			}
			if !sp.sawInflight.Load() {
				t.Fatal("provider Start ran outside the session-start fence bracket")
			}
			fence.mu.Lock()
			gen, inflight := fence.gen, fence.inflight
			fence.mu.Unlock()
			if inflight != 0 || gen != genBefore+2 {
				t.Fatalf("fence after one start: gen=%d inflight=%d, want gen=%d inflight=0", gen, inflight, genBefore+2)
			}
		})
	}
}
