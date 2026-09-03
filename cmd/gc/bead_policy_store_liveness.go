package main

import (
	"log"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/liveness"
)

// This file is the session-liveness half of the bead-policy store: the WRITE
// splitter and the READ overlay.
//
// It lives at the policy wrapper because that wrapper is the OUTERMOST store
// every gc caller holds. Splitting here — rather than at each of the ~15 known
// direct-writer sites (session.Store.ApplyPatch / UpdateMetadataInfo /
// setMetadataValue, Manager's lifecycle batches, chat.go's instance_token,
// usage_compute's marker, cmd_session kill sync, session_beads' Tx close) —
// means no call site changes and no site can be missed. Symmetrically, merging
// liveness values back onto Bead.Metadata at materialization means every raw
// `b.Metadata["state"]` reader (session_wake fencing, providers recovery,
// manager state/lease reads, usage accounting, Store.PersistedMarkers) keeps
// working with zero per-site edits.
//
// Ordering note: this layer must stay ABOVE the CachingStore, not below it. The
// cache holds bead rows whose liveness keys are frozen at whatever was last
// committed; overlaying on the way OUT of the cache is what makes those rows
// read fresh. wrapWithCachingStore preserves the order by re-wrapping the cache
// with this same policy store (carrying this binding along).

// livenessWriteFailureLogWindow rate-limits the degrade log so a Dolt outage
// cannot turn every session transition into a log line.
const livenessWriteFailureLogWindow = 60 // seconds; compared against a monotonic counter below

// livenessFailureLog rate-limits liveness degrade logging process-wide.
var livenessFailureLog struct {
	sync.Mutex
	suppressed int
	lastUnix   int64
}

func noteLivenessFailure(op string, err error) {
	now := nowUnix()
	livenessFailureLog.Lock()
	defer livenessFailureLog.Unlock()
	if livenessFailureLog.lastUnix != 0 && now-livenessFailureLog.lastUnix < livenessWriteFailureLogWindow {
		livenessFailureLog.suppressed++
		return
	}
	suppressed := livenessFailureLog.suppressed
	livenessFailureLog.suppressed = 0
	livenessFailureLog.lastUnix = now
	if suppressed > 0 {
		log.Printf("session liveness: %s failed (%d similar suppressed): %v", op, suppressed, err)
		return
	}
	log.Printf("session liveness: %s failed: %v", op, err)
}

// livenessRoute writes the liveness half of patch to the non-versioned store and
// returns what still has to reach versioned bead metadata.
//
// Every failure path returns the FULL patch: with no liveness store, or after a
// failed liveness write, the legacy versioned write is the only way the value
// survives. A degrade therefore costs a Dolt commit, never a lost transition.
func (s *beadPolicyStore) livenessRoute(id string, patch map[string]string) map[string]string {
	rest, _ := s.livenessRouteStatus(id, patch)
	return rest
}

// livenessRouteStatus is livenessRoute plus whether the liveness half actually
// landed. ok is false only for a genuine degrade (no store, or a failed write) —
// a patch with no liveness keys at all routes fine and reports true.
func (s *beadPolicyStore) livenessRouteStatus(id string, patch map[string]string) (rest map[string]string, ok bool) {
	if s == nil || len(patch) == 0 {
		return patch, true
	}
	store := s.lv.Store()
	if store == nil {
		return patch, false
	}
	plan := liveness.PlanWrite(s.lv.Mode(), patch)
	if len(plan.Liveness) == 0 {
		return plan.Versioned, true
	}
	ctx, cancel := livenessOpContext()
	err := store.SetBatch(ctx, id, plan.Liveness)
	cancel()
	if err != nil {
		noteLivenessFailure("write", err)
		return patch, false
	}
	return plan.Versioned, true
}

// SetMetadataBatch splits the patch. When nothing but liveness keys were in it,
// the underlying store is NOT called at all — that skipped call is the Dolt
// commit this whole change exists to remove.
//
// Contract note: a fully-diverted batch no longer surfaces ErrNotFound for an
// unknown bead, matching the long-standing SetLocalString contract ("callers
// must not rely on this method to validate bead existence"). Callers that need
// existence validated already Get first (Manager.checkTransition does).
func (s *beadPolicyStore) SetMetadataBatch(id string, kvs map[string]string) error {
	rest := s.livenessRoute(id, kvs)
	if len(rest) == 0 {
		return nil
	}
	return s.Store.SetMetadataBatch(id, rest)
}

// SetMetadata routes a single-key write through the same splitter.
func (s *beadPolicyStore) SetMetadata(id, key, value string) error {
	rest := s.livenessRoute(id, map[string]string{key: value})
	if len(rest) == 0 {
		return nil
	}
	// The remainder of a one-key patch is either empty or that same key, so a
	// single-key write stays a single-key write on the backend.
	return s.Store.SetMetadata(id, key, value)
}

// Update splits opts.Metadata. The Update itself still runs whenever any other
// field is set — only a metadata-ONLY update whose every key is a liveness key
// can be skipped.
func (s *beadPolicyStore) Update(id string, opts beads.UpdateOpts) error {
	if len(opts.Metadata) == 0 {
		return s.Store.Update(id, opts)
	}
	rest := s.livenessRoute(id, opts.Metadata)
	if len(rest) == 0 && updateOptsMetadataOnly(opts) {
		return nil
	}
	opts.Metadata = rest
	return s.Store.Update(id, opts)
}

// updateOptsMetadataOnly reports whether opts carries nothing but metadata.
func updateOptsMetadataOnly(opts beads.UpdateOpts) bool {
	return opts.Title == nil && opts.Status == nil && opts.Type == nil &&
		opts.Priority == nil && opts.Description == nil && opts.ParentID == nil &&
		opts.Assignee == nil && len(opts.Labels) == 0 && len(opts.RemoveLabels) == 0
}

// CloseAll splits the close metadata. The close itself always runs: a status
// change is genuine lifecycle history and must keep committing.
//
// If the liveness write degrades for ANY id, the full metadata goes versioned
// for ALL of them. A per-id remainder would be wrong: CloseAll applies one
// metadata map to every bead, so the safe resolution of a partial degrade is the
// one that loses nothing.
func (s *beadPolicyStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if len(metadata) == 0 || len(ids) == 0 {
		return s.Store.CloseAll(ids, metadata)
	}
	rest, degraded := metadata, false
	for _, id := range ids {
		routed, ok := s.livenessRouteStatus(id, metadata)
		if !ok {
			degraded = true
			continue
		}
		rest = routed
	}
	if degraded {
		rest = metadata
	}
	return s.Store.CloseAll(ids, rest)
}

// Tx routes the transaction's metadata writes through the same splitter.
//
// The liveness half is written OUTSIDE the transaction, so a rolled-back Tx
// leaves it applied. That is correct for this data: liveness is last-write-wins
// telemetry with no rollback semantics of its own, and the alternative — holding
// liveness writes until commit — would make a lifecycle transition's telemetry
// invisible to a concurrently-reading process for the whole transaction.
func (s *beadPolicyStore) Tx(commitMsg string, fn func(tx beads.Tx) error) error {
	if fn == nil {
		return s.Store.Tx(commitMsg, fn)
	}
	return s.Store.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&livenessTx{inner: tx, policy: s})
	})
}

// livenessTx applies the write splitter inside a Store.Tx callback.
type livenessTx struct {
	inner  beads.Tx
	policy *beadPolicyStore
}

var _ beads.Tx = (*livenessTx)(nil)

func (t *livenessTx) Create(b beads.Bead) (beads.Bead, error) { return t.inner.Create(b) }

func (t *livenessTx) Close(id string) error { return t.inner.Close(id) }

func (t *livenessTx) Update(id string, opts beads.UpdateOpts) error {
	if len(opts.Metadata) == 0 {
		return t.inner.Update(id, opts)
	}
	rest := t.policy.livenessRoute(id, opts.Metadata)
	if len(rest) == 0 && updateOptsMetadataOnly(opts) {
		return nil
	}
	opts.Metadata = rest
	return t.inner.Update(id, opts)
}

func (t *livenessTx) SetMetadataBatch(id string, kvs map[string]string) error {
	rest := t.policy.livenessRoute(id, kvs)
	if len(rest) == 0 {
		return nil
	}
	return t.inner.SetMetadataBatch(id, rest)
}

// Get overlays the bead's liveness values onto its committed metadata. This is a
// POINT query against the liveness table, so single-bead reads — the fencing and
// lifecycle paths — are always exact, never served from a snapshot.
func (s *beadPolicyStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if err != nil {
		return b, err
	}
	return s.overlayBead(b), nil
}

func (s *beadPolicyStore) overlayBead(b beads.Bead) beads.Bead {
	store := s.lv.Store()
	if store == nil {
		return b
	}
	ctx, cancel := livenessOpContext()
	snap, err := store.Get(ctx, b.ID)
	cancel()
	if err != nil {
		noteLivenessFailure("read", err)
		return b
	}
	return applyLivenessSnapshot(b, snap)
}

// overlayBeads merges liveness rows onto a whole result set with ONE batched
// query (chunked inside the store). A read failure is fail-open: the beads come
// back with their committed metadata, which is the same fallback a bead with no
// liveness rows gets.
func (s *beadPolicyStore) overlayBeads(list []beads.Bead) []beads.Bead {
	if s == nil || len(list) == 0 {
		return list
	}
	store := s.lv.Store()
	if store == nil {
		return list
	}
	ids := make([]string, 0, len(list))
	for _, b := range list {
		ids = append(ids, b.ID)
	}
	ctx, cancel := livenessOpContext()
	snaps, err := store.GetMany(ctx, ids)
	cancel()
	if err != nil {
		noteLivenessFailure("read", err)
		return list
	}
	if len(snaps) == 0 {
		return list
	}
	for i := range list {
		if snap, ok := snaps[list[i].ID]; ok {
			list[i] = applyLivenessSnapshot(list[i], snap)
		}
	}
	return list
}

// applyLivenessSnapshot merges one snapshot onto one bead. Absent keys fall back
// to the committed metadata, which is what lets pre-existing session beads carry
// over with no migration step.
func applyLivenessSnapshot(b beads.Bead, snap liveness.Snapshot) beads.Bead {
	if len(snap.Values) == 0 {
		return b
	}
	b.Metadata = beads.StringMap(liveness.Overlay(b.Metadata, snap))
	return b
}

// overlayResult applies the batched overlay to a list-shaped result, leaving an
// error result untouched.
func (s *beadPolicyStore) overlayResult(list []beads.Bead, err error) ([]beads.Bead, error) {
	if err != nil {
		return list, err
	}
	return s.overlayBeads(list), nil
}

// nowUnix is a seam for the rate-limiter's clock.
var nowUnix = func() int64 { return time.Now().Unix() }
