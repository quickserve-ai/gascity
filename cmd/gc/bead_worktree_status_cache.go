package main

import (
	"errors"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// reapBeadStatusCacheTTL bounds how stale a bead-status verdict the worktree
// reaper may act on. The reaper's pass-1 discovery calls store.Get once per
// bead-shaped worktree on EVERY reconciler tick, and for a hub-backed rig each
// Get is a multi-statement remote hydration — measured at ~200ms apiece on the
// live fleet, ~1.2s of every ~20s tick for statuses that change roughly never,
// and the dominant residual cost of the reap phase after the ga-singc6 lazy
// reordering. Worse, under hub slowness each Get carries a pool read deadline
// plus retry, so the phase amplifies hub latency into tick inflation.
//
// Five minutes trades ~15x fewer hub reads for a bounded staleness window
// whose both directions are safe:
//   - caching "open"/"not found" only DELAYS a newly closed bead's reap by at
//     most the TTL — the reaper's whole job is eventual cleanup;
//   - caching "closed" cannot reap a live tree: the git-safety gate (clean,
//     no unpushed — stashes deliberately excluded, ga-gsfxag), the
//     borrow-veto scan (any non-terminal bead
//     whose work_dir metadata references the path — including a reopened
//     bead), and the process/session liveness gate all still run FRESH on
//     every pass. The residual exposure is a bead reopened within the TTL
//     whose worktree is clean, unreferenced, and unused — reaping that loses
//     nothing a `worktree add` cannot recreate.
const reapBeadStatusCacheTTL = 5 * time.Minute

// beadStatusCacheEntry is one memoized Get verdict. err is non-nil only for
// negative entries and preserves the backing store's original error identity
// (ErrIDCollision wraps ErrNotFound but stays distinguishable — a hit must
// not flatten it).
type beadStatusCacheEntry struct {
	bead beads.Bead
	err  error
	at   time.Time
}

// beadStatusCache memoizes bead-store Get verdicts across reconciler ticks.
// It lives on the CityRuntime (one per controller process) so the reaper —
// a stateless per-tick function — can be handed wrapped stores without
// carrying cache state itself. Transient store errors are never cached: an
// errored Get stays invisible and is retried on the next pass, exactly as
// before.
type beadStatusCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]beadStatusCacheEntry
}

func newBeadStatusCache(ttl time.Duration) *beadStatusCache {
	return &beadStatusCache{
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]beadStatusCacheEntry),
	}
}

// wrap returns store with Get memoized through the cache under the rig's key
// space. Every other Store method — List included, which the borrow-veto
// safety gate depends on being fresh — passes straight through.
func (c *beadStatusCache) wrap(rig string, store beads.Store) beads.Store {
	if c == nil || store == nil {
		return store
	}
	return &statusCachingStore{Store: store, cache: c, rig: rig}
}

// prune drops expired entries. Caller holds c.mu. The map is bounded by the
// number of bead-shaped worktree names ever seen, so this is hygiene, not
// pressure relief.
func (c *beadStatusCache) prune(now time.Time) {
	for key, e := range c.entries {
		if now.Sub(e.at) >= c.ttl {
			delete(c.entries, key)
		}
	}
}

type statusCachingStore struct {
	beads.Store
	cache *beadStatusCache
	rig   string
}

func (s *statusCachingStore) Get(id string) (beads.Bead, error) {
	c := s.cache
	key := s.rig + "/" + id

	c.mu.Lock()
	now := c.now()
	if e, ok := c.entries[key]; ok && now.Sub(e.at) < c.ttl {
		c.mu.Unlock()
		return e.bead, e.err
	}
	c.mu.Unlock()

	bead, err := s.Store.Get(id)
	if err == nil || errors.Is(err, beads.ErrNotFound) {
		c.mu.Lock()
		c.prune(now)
		c.entries[key] = beadStatusCacheEntry{bead: bead, err: err, at: now}
		c.mu.Unlock()
	}
	return bead, err
}
