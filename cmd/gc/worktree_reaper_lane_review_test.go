package main

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Review round 1 (ga-yuiof4 item 3; Astra 1-2, Opus 1-2): an off-tick pass's
// gate verdicts can be as old as the pass, so each removal must be re-verified
// fresh, the store fence must also hold across a blocked call, and a reload
// that disables reaping must stop the next removal.

// reopenedAfterFirstGetStore answers the first Get with the stored (closed)
// status and every later Get with "open" — the bead was reopened, and a tick
// may dispatch into its worktree, after the pass's pass-1 status read.
type reopenedAfterFirstGetStore struct {
	beads.Store
	gets atomic.Int32
}

func (s *reopenedAfterFirstGetStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if s.gets.Add(1) > 1 && err == nil {
		b.Status = "open"
	}
	return b, err
}

// blockingListStore blocks the borrow-veto List until release, then answers
// it successfully from the backing store (no referencing beads).
type blockingListStore struct {
	beads.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.Store.List(q)
}

// cleanSessions is a session snapshot that loaded cleanly and lists no open
// session.
func cleanSessions() *sessionBeadSnapshot { return newSessionBeadSnapshotFromInfos(nil) }

func reviewReapConfig(rigRoot string) *config.City {
	cfg := reapTestConfig(rigRoot)
	enabled := true
	cfg.Daemon = config.DaemonConfig{AutoReapClosedBeadWorktrees: &enabled}
	return cfg
}

func lastReaperResult(cr *CityRuntime) worktreeReaperPassResult {
	lane := cr.worktreeReaperLaneOf()
	lane.mu.Lock()
	defer lane.mu.Unlock()
	return lane.lastResult
}

func assertProtectedFor(t *testing.T, cr *CityRuntime, wt string, reasonParts ...string) {
	t.Helper()
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree %s was removed (stat err=%v), want it protected", wt, err)
	}
	res := lastReaperResult(cr)
	if len(res.report.Reaped) != 0 || len(res.report.Protected) != 1 {
		t.Fatalf("report reaped=%d protected=%d, want 0 reaped and 1 protected", len(res.report.Reaped), len(res.report.Protected))
	}
	reason := res.report.Protected[0].Reason
	for _, part := range reasonParts {
		if !strings.Contains(reason, part) {
			t.Fatalf("protect reason = %q, want it to contain %q", reason, part)
		}
	}
}

// TestWorktreeReaperLane_BeadReopenedBeforeRemovalIsProtected: the bead reads
// closed at pass 1 and open by the time its removal comes up. The pre-removal
// uncached Get must see "open" and protect.
func TestWorktreeReaperLane_BeadReopenedBeforeRemovalIsProtected(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
	store := &reopenedAfterFirstGetStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)}
	cfg := reviewReapConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	cr := newReapTickRuntime(cityPath, cfg, store, io.Discard)
	cr.triggerWorktreeReaperPass(context.Background(), cfg, true, cleanSessions())
	waitWorktreeReaperIdle(t, cr)

	assertProtectedFor(t, cr, wt, `bead is now "open"`)
}

// TestWorktreeReaperLane_WorktreeGoneLiveBeforeRemovalIsProtected: the
// process scan at the liveness gate finds the tree idle; a process then starts
// in it. The pre-removal liveness read — re-gathered because the slow
// pre-removal Get outlasts the scan-age bound — must see it and protect.
func TestWorktreeReaperLane_WorktreeGoneLiveBeforeRemovalIsProtected(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
	const bound = 200 * time.Millisecond
	store := &slowAfterFirstGetStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil), delay: 2 * bound}
	cfg := reviewReapConfig(rigRoot)

	var scans atomic.Int32
	prevScan := collectLiveWorktreeStateFn
	collectLiveWorktreeStateFn = func() liveWorktreeState {
		if scans.Add(1) == 1 {
			return liveWorktreeState{scanned: true}
		}
		return liveWorktreeState{scanned: true, cwds: []string{canonicalTestPath(wt)}}
	}
	prevAge := reapPreRemovalLivenessMaxAge
	reapPreRemovalLivenessMaxAge = bound
	t.Cleanup(func() {
		collectLiveWorktreeStateFn = prevScan
		reapPreRemovalLivenessMaxAge = prevAge
	})

	cr := newReapTickRuntime(cityPath, cfg, store, io.Discard)
	cr.triggerWorktreeReaperPass(context.Background(), cfg, true, cleanSessions())
	waitWorktreeReaperIdle(t, cr)

	assertProtectedFor(t, cr, wt, "pre-removal re-check: live: live process cwd")
	if got := scans.Load(); got < 2 {
		t.Fatalf("process scan ran %d time(s), want a fresh re-gather before removal", got)
	}
}

// TestWorktreeReaperLane_ListRetiredDuringCallIsDiscarded (Astra P1-2): the
// borrow-veto List enters the handle, a reload retires the handle while the
// call is blocked, and the call then returns SUCCESS with no referencing
// beads. That answer came from a retired handle and must be discarded, so the
// worktree is protected rather than reaped on it.
func TestWorktreeReaperLane_ListRetiredDuringCallIsDiscarded(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
	seed := []beads.Bead{{ID: "ga-abc123", Status: "closed"}}
	store := &blockingListStore{
		Store:   beads.NewMemStoreFrom(1, seed, nil),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(store.release) }) }
	cfg := reviewReapConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	cr := newReapTickRuntime(cityPath, cfg, store, io.Discard)
	cs := &controllerState{cityName: "test", beadStores: map[string]beads.Store{"mrig": store}}
	cr.cs = cs
	t.Cleanup(func() { waitWorktreeReaperIdle(t, cr) })
	t.Cleanup(release)

	cr.triggerWorktreeReaperPass(context.Background(), cfg, true, cleanSessions())
	<-store.entered
	cs.mu.Lock()
	cs.beadStores = map[string]beads.Store{"mrig": beads.NewMemStoreFrom(1, seed, nil)}
	cs.mu.Unlock()
	release()
	waitWorktreeReaperIdle(t, cr)

	assertProtectedFor(t, cr, wt, "borrow-veto scan failed", errReaperStoreRetired.Error())
}

// TestWorktreeReaperLane_ReloadDisablingReapStopsNextRemoval (Opus concern
// 2): a pass starts with real reaping on; while it is blocked in a store call
// a reload turns auto_reap_closed_bead_worktrees off (leaving only dry-run).
// The in-flight pass's next removal must not happen.
func TestWorktreeReaperLane_ReloadDisablingReapStopsNextRemoval(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
	store, release := newWedgedGetStore(t, []beads.Bead{{ID: "ga-abc123", Status: "closed"}})
	cfg := reviewReapConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	cr := newReapTickRuntime(cityPath, cfg, store, io.Discard)
	t.Cleanup(func() { waitWorktreeReaperIdle(t, cr) })
	t.Cleanup(release)

	cr.triggerWorktreeReaperPass(context.Background(), cfg, true, cleanSessions())
	waitForReaperCond(t, func() bool { return store.gets.Load() == 1 }, "the pass to block in its first Get")

	reloaded := reapTestConfig(rigRoot)
	off, dry := false, true
	reloaded.Daemon = config.DaemonConfig{AutoReapClosedBeadWorktrees: &off, AutoReapClosedBeadWorktreesDryRun: &dry}
	cr.serviceStateMu.Lock()
	cr.cfg = reloaded
	cr.serviceStateMu.Unlock()
	release()
	waitWorktreeReaperIdle(t, cr)

	assertProtectedFor(t, cr, wt, "real reaping no longer enabled")
}
