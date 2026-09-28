package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Review round 2 (ga-yuiof4 item 3; Astra r2 1, 3, 4): the session-start
// fence, the post-collection scan-age check, and a degraded session read that
// must not erase published protection.

func r2CleanSessions() *sessionBeadSnapshot { return newSessionBeadSnapshotFromInfos(nil) }

// TestWorktreeReaperLane_SessionStartFence covers Astra r2 finding 1: a
// controller session start that begins after the pass's liveness scan started
// — so that scan cannot see its process — must stop the removal; one still in
// flight at removal time must too; with no start the pass still reaps.
func TestWorktreeReaperLane_SessionStartFence(t *testing.T) {
	for _, tc := range []struct {
		name string
		// duringScan runs inside the pass's first liveness scan.
		duringScan func(f *sessionStartFence)
		// beforePass runs before the pass is triggered; its returned func
		// runs at cleanup.
		beforePass  func(f *sessionStartFence) func()
		wantRemoved bool
	}{
		{
			name:       "start begins and ends after the scan started",
			duringScan: func(f *sessionStartFence) { f.beginStart(); f.endStart() },
		},
		{
			name: "start still in flight at removal",
			beforePass: func(f *sessionStartFence) func() {
				f.beginStart()
				return f.endStart
			},
		},
		{
			name:        "no start: still reaps",
			wantRemoved: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, rigRoot := initReapRig(t)
			wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
			store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)
			cfg := reviewReapConfig(rigRoot)
			cr := newReapTickRuntime(cityPath, cfg, store, io.Discard)
			fence := cr.sessionStartFenceOf()

			var scans atomic.Int32
			prevScan := collectLiveWorktreeStateFn
			collectLiveWorktreeStateFn = func() liveWorktreeState {
				if scans.Add(1) == 1 && tc.duringScan != nil {
					tc.duringScan(fence)
				}
				return liveWorktreeState{scanned: true}
			}
			t.Cleanup(func() { collectLiveWorktreeStateFn = prevScan })
			if tc.beforePass != nil {
				t.Cleanup(tc.beforePass(fence))
			}

			cr.triggerWorktreeReaperPass(context.Background(), cfg, true, r2CleanSessions())
			waitWorktreeReaperIdle(t, cr)

			_, statErr := os.Stat(wt)
			removed := errors.Is(statErr, os.ErrNotExist)
			if removed != tc.wantRemoved {
				t.Fatalf("worktree removed=%t (stat err=%v), want removed=%t", removed, statErr, tc.wantRemoved)
			}
			if !tc.wantRemoved {
				assertProtectedFor(t, cr, wt, "session-start fence")
			}
		})
	}
}

// slowAfterFirstGetStore answers the first Get at once and every later Get
// after delay — the pre-removal uncached Get of a slow hub.
type slowAfterFirstGetStore struct {
	beads.Store
	delay time.Duration
	gets  atomic.Int32
}

func (s *slowAfterFirstGetStore) Get(id string) (beads.Bead, error) {
	if s.gets.Add(1) > 1 {
		<-time.After(s.delay)
	}
	return s.Store.Get(id)
}

// TestWorktreeReaperLane_RegatheredScanOverBoundIsProtected covers Astra r2
// finding 3: the scan used for a removal must have STARTED within the bound
// before the decision, checked after collection too. A re-gathered scan that
// takes longer than the bound comes back already stale and must protect; a
// fast re-gather still reaps.
func TestWorktreeReaperLane_RegatheredScanOverBoundIsProtected(t *testing.T) {
	const bound = 200 * time.Millisecond
	for _, tc := range []struct {
		name        string
		regather    time.Duration
		wantRemoved bool
	}{
		{name: "slow re-gather protects", regather: 3 * bound},
		{name: "fast re-gather reaps", regather: 0, wantRemoved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, rigRoot := initReapRig(t)
			wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
			// The pre-removal Get outlasts the bound, forcing a re-gather.
			store := &slowAfterFirstGetStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil), delay: 2 * bound}
			cfg := reviewReapConfig(rigRoot)

			var scans atomic.Int32
			prevScan, prevBound := collectLiveWorktreeStateFn, reapPreRemovalLivenessMaxAge
			collectLiveWorktreeStateFn = func() liveWorktreeState {
				if scans.Add(1) > 1 && tc.regather > 0 {
					<-time.After(tc.regather)
				}
				return liveWorktreeState{scanned: true}
			}
			reapPreRemovalLivenessMaxAge = bound
			t.Cleanup(func() {
				collectLiveWorktreeStateFn = prevScan
				reapPreRemovalLivenessMaxAge = prevBound
			})

			cr := newReapTickRuntime(cityPath, cfg, store, io.Discard)
			cr.triggerWorktreeReaperPass(context.Background(), cfg, true, r2CleanSessions())
			waitWorktreeReaperIdle(t, cr)

			if got := scans.Load(); got < 2 {
				t.Fatalf("scan ran %d time(s), want a re-gather before removal", got)
			}
			_, statErr := os.Stat(wt)
			removed := errors.Is(statErr, os.ErrNotExist)
			if removed != tc.wantRemoved {
				t.Fatalf("worktree removed=%t (stat err=%v), want removed=%t", removed, statErr, tc.wantRemoved)
			}
			if !tc.wantRemoved {
				assertProtectedFor(t, cr, wt, "over the", "bound")
			}
		})
	}
}

// TestWorktreeReaperLane_DegradedSessionReadKeepsProtection covers Astra r2
// finding 4: a failed (nil) or degraded (LoadError) session read published
// while a pass is in flight must not erase the last clean publication; the
// pass's removal is protected until a clean read arrives, and then reaps.
func TestWorktreeReaperLane_DegradedSessionReadKeepsProtection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		degraded *sessionBeadSnapshot
	}{
		{name: "failed read (nil snapshot)", degraded: nil},
		{name: "degraded read (load error)", degraded: newSessionBeadSnapshotWithError(errors.New("session list timed out"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, rigRoot := initReapRig(t)
			wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-abc123")
			store, release := newWedgedGetStore(t, []beads.Bead{{ID: "ga-abc123", Status: "closed"}})
			cfg := reviewReapConfig(rigRoot)
			injectLiveness(t, liveWorktreeState{scanned: true})

			cr := newReapTickRuntime(cityPath, cfg, store, io.Discard)
			t.Cleanup(func() { waitWorktreeReaperIdle(t, cr) })
			t.Cleanup(release)

			// Tick 1: clean read, pass starts and blocks in its first Get.
			cr.triggerWorktreeReaperPass(context.Background(), cfg, true, r2CleanSessions())
			waitForReaperCond(t, func() bool { return store.gets.Load() == 1 }, "the pass to block in its first Get")
			// Tick 2: the session read fails; the trigger skips but publishes.
			if skip := cr.triggerWorktreeReaperPass(context.Background(), cfg, true, tc.degraded); !skip.skippedInflight {
				t.Fatalf("second trigger = %+v, want a skip", skip)
			}
			release()
			waitWorktreeReaperIdle(t, cr)
			assertProtectedFor(t, cr, wt, "session snapshot degraded")

			// Tick 3: a clean read restores the publication; the next pass reaps.
			cr.triggerWorktreeReaperPass(context.Background(), cfg, true, r2CleanSessions())
			waitWorktreeReaperIdle(t, cr)
			if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
				res := lastReaperResult(cr)
				reasons := make([]string, 0, len(res.report.Protected))
				for _, p := range res.report.Protected {
					reasons = append(reasons, p.Reason)
				}
				t.Fatalf("worktree survived the pass after a clean session read (stat err=%v), want it reaped; protected: %s", err, strings.Join(reasons, "; "))
			}
		})
	}
}
