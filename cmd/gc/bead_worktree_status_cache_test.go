package main

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// ga-singc6 follow-up: pass-1 discovery pays one remote store.Get per
// bead-shaped worktree on every reconciler tick, for statuses that change
// roughly never. These tests pin the cross-tick memo the reconciler wraps the
// rig stores with: Get verdicts (including not-found) are reused within the
// TTL, transient errors are never cached, an expired entry is re-fetched so a
// status flip is honored within one TTL, and the borrow-veto List — a safety
// gate — is NEVER memoized.

// reapGetCountingStore counts Get and List traffic reaching the real store
// through the cache wrapper, and can force Get to fail transiently.
type reapGetCountingStore struct {
	beads.Store
	gets      int
	lists     int
	liveLists int
	getErr    error
}

func (s *reapGetCountingStore) Get(id string) (beads.Bead, error) {
	s.gets++
	if s.getErr != nil {
		return beads.Bead{}, s.getErr
	}
	return s.Store.Get(id)
}

func (s *reapGetCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	if q.Live {
		s.liveLists++
	}
	return s.Store.List(q)
}

// newTestStatusCache returns a cache on a manual clock the caller advances.
func newTestStatusCache(ttl time.Duration) (*beadStatusCache, *time.Time) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c := newBeadStatusCache(ttl)
	c.now = func() time.Time { return now }
	return c, &now
}

func reapOnceWithCache(t *testing.T, cityPath string, cfg *config.City, store beads.Store, cache *beadStatusCache) reapReport {
	t.Helper()
	var stderr bytes.Buffer
	wrapped := map[string]beads.Store{reapTestRigName: cache.wrap(reapTestRigName, store)}
	return reapClosedBeadWorktrees(cityPath, cfg, wrapped, nil, false, events.Discard, &stderr)
}

// A second pass inside the TTL must not touch the store for statuses the
// first pass already read. Open beads keep their worktrees, so the same
// worktree is re-discovered every pass — the uncached world pays one Get per
// pass, the cached world exactly one per TTL window.
func TestReapStatusCache_SecondPassWithinTTLSkipsGets(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-cache001")
	store := &reapGetCountingStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-cache001", Status: "open"}}, nil)}
	cfg := reapTestConfig(rigRoot)
	cache, now := newTestStatusCache(5 * time.Minute)
	injectCountingLiveness(t, liveWorktreeState{scanned: true})

	reapOnceWithCache(t, cityPath, cfg, store, cache)
	if store.gets != 1 {
		t.Fatalf("first pass issued %d Get(s), want 1", store.gets)
	}
	*now = now.Add(20 * time.Second) // one tick later, well inside the TTL
	report := reapOnceWithCache(t, cityPath, cfg, store, cache)
	if store.gets != 1 {
		t.Fatalf("second pass within TTL issued %d total Get(s), want still 1 (verdict memoized)", store.gets)
	}
	if len(report.Reaped) != 0 {
		t.Fatalf("open bead was reaped: %+v", report.Reaped)
	}
}

// An expired entry is re-fetched, so a bead closed after the cached "open"
// read becomes reapable within one TTL — the staleness contract is a bounded
// DELAY, never a permanently wrong verdict.
func TestReapStatusCache_StatusFlipHonoredAfterTTL(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-cache002")
	mem := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-cache002", Status: "open"}}, nil)
	store := &reapGetCountingStore{Store: mem}
	cfg := reapTestConfig(rigRoot)
	cache, now := newTestStatusCache(5 * time.Minute)
	injectCountingLiveness(t, liveWorktreeState{scanned: true})

	reapOnceWithCache(t, cityPath, cfg, store, cache)

	// The bead closes in the store; the cached "open" verdict hides it...
	closedStatus := "closed"
	if err := mem.Update("ga-cache002", beads.UpdateOpts{Status: &closedStatus}); err != nil {
		t.Fatalf("closing bead in mem store: %v", err)
	}
	*now = now.Add(1 * time.Minute)
	report := reapOnceWithCache(t, cityPath, cfg, store, cache)
	if len(report.Reaped) != 0 {
		t.Fatalf("reap happened inside the TTL off a stale read: %+v", report.Reaped)
	}
	if store.gets != 1 {
		t.Fatalf("pass inside TTL issued %d total Get(s), want 1", store.gets)
	}

	// ...until the TTL lapses, when the flip is observed and the reap lands.
	*now = now.Add(5 * time.Minute)
	report = reapOnceWithCache(t, cityPath, cfg, store, cache)
	if store.gets != 2 {
		t.Fatalf("post-TTL pass issued %d total Get(s), want 2 (entry expired)", store.gets)
	}
	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-cache002" {
		t.Fatalf("Reaped = %+v, want exactly ga-cache002", report.Reaped)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still present after reap (stat err=%v)", err)
	}
}

// Not-found verdicts are memoized too: a bead-shaped worktree whose ID does
// not exist in the store must not re-interrogate the hub every tick.
func TestReapStatusCache_NotFoundCached(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-cache003")
	store := &reapGetCountingStore{Store: beads.NewMemStoreFrom(1, nil, nil)}
	cfg := reapTestConfig(rigRoot)
	cache, now := newTestStatusCache(5 * time.Minute)
	injectCountingLiveness(t, liveWorktreeState{scanned: true})

	reapOnceWithCache(t, cityPath, cfg, store, cache)
	*now = now.Add(20 * time.Second)
	reapOnceWithCache(t, cityPath, cfg, store, cache)
	if store.gets != 1 {
		t.Fatalf("not-found verdict re-fetched: %d total Get(s), want 1", store.gets)
	}
}

// A transient store error is NOT cached — the next pass must retry, exactly
// as the uncached reaper did.
func TestReapStatusCache_TransientErrorsNotCached(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-cache004")
	store := &reapGetCountingStore{
		Store:  beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-cache004", Status: "open"}}, nil),
		getErr: errors.New("hub connection reset"),
	}
	cfg := reapTestConfig(rigRoot)
	cache, now := newTestStatusCache(5 * time.Minute)
	injectCountingLiveness(t, liveWorktreeState{scanned: true})

	reapOnceWithCache(t, cityPath, cfg, store, cache)
	*now = now.Add(20 * time.Second)
	reapOnceWithCache(t, cityPath, cfg, store, cache)
	if store.gets != 2 {
		t.Fatalf("errored Get was cached: %d total Get(s), want 2 (retry every pass)", store.gets)
	}

	// Once the store recovers, the verdict caches normally again.
	store.getErr = nil
	*now = now.Add(20 * time.Second)
	reapOnceWithCache(t, cityPath, cfg, store, cache)
	*now = now.Add(20 * time.Second)
	reapOnceWithCache(t, cityPath, cfg, store, cache)
	if store.gets != 3 {
		t.Fatalf("post-recovery caching broken: %d total Get(s), want 3", store.gets)
	}
}

// The borrow-veto List is a SAFETY gate and must stay fresh on every pass the
// candidate survives to it, even while the Get verdict is served from cache.
// An indeterminate liveness scan keeps the candidate protected (not reaped),
// so the same worktree reaches the borrow-veto gate pass after pass.
func TestReapStatusCache_BorrowVetoListNeverMemoized(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-cache005")
	store := &reapGetCountingStore{Store: beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-cache005", Status: "closed"}}, nil)}
	cfg := reapTestConfig(rigRoot)
	cache, now := newTestStatusCache(5 * time.Minute)
	injectCountingLiveness(t, liveWorktreeState{}) // scanned=false: indeterminate, protects

	report := reapOnceWithCache(t, cityPath, cfg, store, cache)
	if len(report.Reaped) != 0 {
		t.Fatalf("reap happened under an indeterminate liveness scan: %+v", report.Reaped)
	}
	*now = now.Add(20 * time.Second)
	reapOnceWithCache(t, cityPath, cfg, store, cache)

	if store.gets != 1 {
		t.Fatalf("closed verdict re-fetched: %d total Get(s), want 1", store.gets)
	}
	if store.lists != 2 {
		t.Fatalf("borrow-veto List ran %d time(s) over two passes, want 2 — the safety scan must never be memoized", store.lists)
	}
	// The scan must also be Live: a production rig store is a CachingStore,
	// and only a Live query bypasses its in-memory active set. Without it
	// the "borrow-veto runs fresh" half of the staleness argument silently
	// depends on another cache's reconcile cadence.
	if store.liveLists != 2 {
		t.Fatalf("borrow-veto issued %d Live List(s) of %d, want all Live", store.liveLists, store.lists)
	}
}

// A negative cache hit must return the ORIGINAL error, not a flattened
// ErrNotFound: ErrIDCollision wraps ErrNotFound but stays distinguishable,
// and a caller checking for the collision sub-case must see it on hits too.
func TestReapStatusCache_PreservesNotFoundErrorIdentity(t *testing.T) {
	cache, now := newTestStatusCache(5 * time.Minute)
	store := &reapGetCountingStore{
		Store:  beads.NewMemStoreFrom(1, nil, nil),
		getErr: beads.ErrIDCollision,
	}
	wrapped := cache.wrap(reapTestRigName, store)

	if _, err := wrapped.Get("ga-fuzzy01"); !errors.Is(err, beads.ErrIDCollision) {
		t.Fatalf("first Get err = %v, want ErrIDCollision", err)
	}
	*now = now.Add(20 * time.Second)
	if _, err := wrapped.Get("ga-fuzzy01"); !errors.Is(err, beads.ErrIDCollision) {
		t.Fatalf("cached Get err = %v, want the ORIGINAL ErrIDCollision preserved", err)
	}
	if store.gets != 1 {
		t.Fatalf("gets = %d, want 1 (collision verdict memoized like any not-found)", store.gets)
	}
}
