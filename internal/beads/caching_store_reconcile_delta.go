package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"time"
)

// THE DELTA RECONCILE (ga-ckbnpj). A full reconcile re-lists every active row
// with every column to learn of a handful of changes. Over a shared hub that
// was ~500 MiB per pass every 120 s for the qcore rig (7,700 rows, ~10 changed
// per pass): 190 GB/day to one laptop. A backing that can answer a per-row
// digest (cacheReconcileDigestStore) lets a pass list only the rows whose
// digest moved since the last pass, and leave every other row exactly as the
// cache holds it.
//
// A row the cache itself changed since the last pass is listed again even
// when its digest has not moved: each pass fingerprints the cached row and its
// edges as it leaves them (reconcileFingerprint), and the next pass re-lists
// any row whose cached content no longer matches. That covers every install
// path at once (writes, applied events, read-through installs, a late DepList)
// without each one having to stamp anything, so a stale payload is corrected
// by the next pass, not by the next full list.
//
// Unchanged rows are a KEEP set, never re-absorbed: absorbing the cache's own
// copy as "fresh" would clear a dirty mark or a write fence the backing never
// confirmed. The eviction loop and the missing-row recovery skip the keep set,
// so a kept row is neither evicted nor re-read.
//
// A pass lists in full when the backing has no digest, the digest read fails,
// there is no previous digest, the cache is not live, or the last full list is
// cacheReconcileFullListEvery old: the digest's residual blind spots (see
// reconcileDigestForCache) are bounded by that interval.

// cacheReconcileFullListEvery bounds how long a field the digest misses can
// stay stale in the cache.
const cacheReconcileFullListEvery = 30 * time.Minute

// reconcilePlan is one pass's read: the digest it read first (nil when none),
// and, for a delta pass, the ids to list and the cached rows to leave alone.
type reconcilePlan struct {
	digest map[string]string
	delta  bool
	fetch  []string
	keep   map[string]struct{}
}

// planReconcile reads the backing's digest BEFORE anything is listed, so a row
// that changes after the read is newer than the digest the pass records and
// the next pass lists it again.
func (c *CachingStore) planReconcile(now time.Time) reconcilePlan {
	reader, ok := c.backing.(cacheReconcileDigestStore)
	if !ok {
		return reconcilePlan{}
	}
	digest, err := reader.reconcileDigestForCache()
	if err != nil {
		if !errors.Is(err, errReconcileDigestUnsupported) {
			c.recordProblem("reconcile digest", err)
		}
		return reconcilePlan{}
	}
	plan := reconcilePlan{digest: digest}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.reconcileDigest == nil || c.state != cacheLive || c.lastFullListAt.IsZero() ||
		now.Sub(c.lastFullListAt) >= cacheReconcileFullListEvery {
		return plan
	}
	plan.delta = true
	plan.keep = make(map[string]struct{}, len(digest))
	for id, h := range digest {
		prev, seen := c.reconcileDigest[id]
		_, dirty := c.dirty[id]
		cached, resident := c.beads[id]
		// A row the digest lists but the cache does not hold is listed too:
		// it may have been evicted by an event since the last pass.
		if !seen || prev != h || dirty || !resident ||
			c.reconcileFingerprints[id] != reconcileFingerprint(cached, c.deps[id]) {
			plan.fetch = append(plan.fetch, id)
			continue
		}
		plan.keep[id] = struct{}{}
	}
	sort.Strings(plan.fetch)
	return plan
}

// recordedDigestLocked is the digest a completed pass may stand on, with the
// fingerprint of the cached content each entry certifies. An entry is kept
// only for content the pass actually verified:
//
//   - a KEPT row keeps the fingerprint it was last certified with, and only
//     while the cache still holds exactly that content (an event can rewrite
//     it between the plan and the merge);
//   - a LISTED row is certified only if the cache now holds exactly the row
//     and edges the pass listed (a recency guard or fence can keep a local
//     copy instead, and the notification diff beadChanged ignores labels);
//   - a deferred row is the cache's own copy, verified by nothing.
//
// Every other row loses its entry, so the next pass lists it again. A pass
// whose ready projection failed records nothing: the rows it installed may
// carry a preserved is_blocked the digest no longer describes. Caller holds
// c.mu.
func (c *CachingStore) recordedDigestLocked(
	plan reconcilePlan, freshByID map[string]Bead, deferred map[string]struct{},
	depMap map[string][]Dep, useFreshDeps bool, projectionErr error,
) map[string]string {
	previous := c.reconcileFingerprints
	c.reconcileFingerprints = nil
	if plan.digest == nil || projectionErr != nil {
		return nil
	}
	recorded := make(map[string]string, len(plan.digest))
	fingerprints := make(map[string]uint64, len(plan.digest))
	for id, h := range plan.digest {
		if _, held := deferred[id]; held {
			continue
		}
		cached, ok := c.beads[id]
		if !ok {
			continue
		}
		current := reconcileFingerprint(cached, c.deps[id])
		if _, kept := plan.keep[id]; kept {
			if prev, had := previous[id]; !had || prev != current {
				continue
			}
		} else {
			fresh, listed := freshByID[id]
			if !listed || reconcileFingerprint(cached, nil) != reconcileFingerprint(fresh, nil) ||
				depsChanged(c.deps[id], c.depsForReconcileLocked(id, fresh, depMap, useFreshDeps)) {
				continue
			}
		}
		recorded[id] = h
		fingerprints[id] = current
	}
	c.reconcileFingerprints = fingerprints
	return recorded
}

// reconcileFingerprint hashes a cached row and its edges, including the fields
// Bead's JSON omits. Two equal fingerprints mean the cache has not changed the
// row since it was recorded.
func reconcileFingerprint(b Bead, deps []Dep) uint64 {
	h := fnv.New64a()
	row, _ := json.Marshal(b)      //nolint:errcheck // Bead always marshals
	edges, _ := json.Marshal(deps) //nolint:errcheck // Dep always marshals
	_, _ = h.Write(row)
	_, _ = h.Write(edges)
	_, _ = fmt.Fprintf(h, "|%t|%d|%d", b.IndefinitelyDeferred, b.Revision, b.ClaimFence)
	return h.Sum64()
}
