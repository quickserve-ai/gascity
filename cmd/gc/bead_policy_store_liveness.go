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
//
// EVERY degraded return is FENCED. The returned patch carries the liveness keys
// AND a FallbackAtKey stamp, so once the pool recovers the pre-outage rows it
// left behind cannot shadow what this write just committed. Returning the bare
// patch instead is the stale-shadow defect: the overlay would let any surviving
// row win, and wake fencing would compare against a resurrected instance_token.
func (s *beadPolicyStore) livenessRouteStatus(id string, patch map[string]string) (rest map[string]string, ok bool) {
	if s == nil || len(patch) == 0 {
		return patch, true
	}
	store := s.lv.Store()
	if store == nil {
		return liveness.FallbackPlan(patch, s.lv.Now()), false
	}
	plan := liveness.PlanWrite(s.lv.Mode(), patch, store.Now())
	if len(plan.Liveness) == 0 {
		return plan.Versioned, true
	}
	ctx, cancel := livenessOpContext()
	err := store.SetBatch(ctx, id, plan.Liveness)
	cancel()
	if err != nil {
		// Mint the fence from the store we STILL hold. noteOpError retires the
		// pool, and a stamp taken after that would fall back to the raw local
		// clock — the wrong timebase for the server-minted written_at values it
		// has to fence.
		now := store.Now()
		noteLivenessFailure("write", err)
		s.lv.noteOpError(err)
		return liveness.FallbackPlan(patch, now), false
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
		// Fenced, and a fresh map — never the caller's.
		rest = liveness.FallbackPlan(metadata, s.lv.Now())
	}
	return s.Store.CloseAll(ids, rest)
}

// Tx does NOT split. Inside a transaction every metadata key — liveness
// included — goes to versioned bead metadata, fenced with a FallbackAtKey stamp.
//
// WHY NOT SPLIT. Two Tx call sites carry explicit invariants that a split
// breaks, and both are about a bead's status and its liveness state being
// observable together or not at all:
//
//   - session_beads.go closeSessionBeadInTx (ga-igcny0.1.1): a closed session
//     bead must ALWAYS carry its terminal state.
//   - session_beads.go the reopen path: the reopen must not be observable split
//     from the metadata that accompanies it.
//
// Writing liveness before the transaction inverts the atomicity outright — a
// rolled-back Tx leaves the telemetry applied. Buffering it and flushing after
// commit is better but still leaves a real window: a crash between COMMIT and
// FLUSH leaves a bead whose status changed and whose liveness rows still hold
// the pre-transition values, which the overlay would then serve over the
// committed terminal state. That is precisely the split both comments forbid.
//
// Going fully versioned closes the window instead of narrowing it, and it costs
// nothing: every one of these call sites is a low-frequency lifecycle event
// (close, reopen, failed-create rollback) whose transaction commits regardless.
// The commit-churn this change exists to remove comes from the non-Tx transition
// patches, which still split. The fence stamp is what keeps the stale table rows
// these writes leave behind from shadowing the committed values afterwards.
//
// The reviewers' requirement — "a failed versioned write must leave liveness
// untouched" — holds trivially: no liveness write is issued at all.
func (s *beadPolicyStore) Tx(commitMsg string, fn func(tx beads.Tx) error) error {
	if fn == nil {
		return s.Store.Tx(commitMsg, fn)
	}
	return s.Store.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&livenessTx{inner: tx, policy: s})
	})
}

// livenessTx fences a Store.Tx callback's metadata writes. See Tx.
type livenessTx struct {
	inner  beads.Tx
	policy *beadPolicyStore
}

var _ beads.Tx = (*livenessTx)(nil)

func (t *livenessTx) Create(b beads.Bead) (beads.Bead, error) { return t.inner.Create(b) }

func (t *livenessTx) Close(id string) error { return t.inner.Close(id) }

func (t *livenessTx) Update(id string, opts beads.UpdateOpts) error {
	if len(opts.Metadata) > 0 {
		opts.Metadata = liveness.FallbackPlan(opts.Metadata, t.policy.lv.Now())
	}
	return t.inner.Update(id, opts)
}

func (t *livenessTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return t.inner.SetMetadataBatch(id, liveness.FallbackPlan(kvs, t.policy.lv.Now()))
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
		s.lv.noteOpError(err)
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
		if !beadMayCarryLiveness(b) {
			continue
		}
		ids = append(ids, b.ID)
	}
	if len(ids) == 0 {
		return list
	}
	ctx, cancel := livenessOpContext()
	snaps, err := store.GetMany(ctx, ids)
	cancel()
	if err != nil {
		noteLivenessFailure("read", err)
		s.lv.noteOpError(err)
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

// beadMayCarryLiveness reports whether a bead is worth a liveness lookup on a
// LIST path. Without it every List queried the liveness table for every bead it
// returned — thousands of ids on a whole-city list, against a store with
// documented complex-read stalls.
//
// The predicate is a deliberate SUPERSET of "is a session bead", because session
// beads are NOT the only writers: `gc bd heartbeat <issue-id>` stamps
// gc.last_heartbeat_at on an arbitrary WORK bead (cmd_bd.go), and that key is in
// the moved set. The second and third clauses cover it: the heartbeat path
// commits a one-time marker the first time it sees a bead, so any bead that has
// ever been heartbeated carries a committed liveness key forever after and stays
// in the candidate set from then on. The fence stamp does the same for any bead
// that ever took a degraded or transactional write.
//
// Get is deliberately NOT filtered — a single-bead read is one point query, and
// the fencing and lifecycle paths that read one bead must always be exact.
func beadMayCarryLiveness(b beads.Bead) bool {
	if b.Type == sessionBeadType {
		return true
	}
	for _, label := range b.Labels {
		if label == sessionBeadLabel {
			return true
		}
	}
	for k := range b.Metadata {
		if liveness.IsKey(k) || liveness.IsMarkerKey(k) {
			return true
		}
	}
	return false
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
