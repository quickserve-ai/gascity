// Package liveness holds the non-versioned session-liveness telemetry store.
//
// Session-liveness fields (state, awake_started_at, generation, ...) used to
// live in the metadata JSON of session beads in the versioned Dolt `issues`
// table. Every such write minted a permanent Dolt commit (~840 KB, ~250/hr
// fleet-wide) for data that is pure node-local telemetry: last-write-wins,
// never merged, never useful in history. This package moves those fields into a
// `session_liveness` table registered in `dolt_ignore`, so the rows live only in
// the working set and never stage, commit, or replicate — the same mechanism the
// beads library itself uses for leases and wisps.
//
// The package deliberately depends on nothing else in gascity except
// internal/beadmeta — the stdlib-only key-vocabulary leaf that everything may
// import: the key set has to be referenceable from internal/session,
// internal/beads and cmd/gc without creating an import cycle.
package liveness

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// WrittenAtKey is a SYNTHETIC metadata key the read overlay stamps onto a bead
// whose liveness rows exist. Its value is the RFC3339Nano max(written_at) across
// that bead's rows — the "last liveness write" clock.
//
// It exists because moving liveness off the issues table makes a session bead's
// row-level UpdatedAt go quiet: nothing touches the row between genuine status
// transitions, so any prune/staleness rule keyed on UpdatedAt starts reading a
// live session as ancient. Consumers that need a freshness clock read this key
// and take the later of it and Bead.UpdatedAt (see session.EffectiveUpdatedAt).
//
// It is never accepted as an INPUT: SetBatch refuses it, so it can only ever be
// produced by the overlay from the table's own timestamps.
const WrittenAtKey = beadmeta.LivenessWrittenAtMetadataKey

// FencePrefix begins the VERSIONED marker keys that fence stale liveness rows
// out of the overlay. There is ONE marker per fenced liveness key —
// FenceKeyFor("state") is "gc.liveness_fence.state" — and its value is the
// moment that key's value was committed to versioned metadata instead of the
// table: a degraded write, a transactional write, or every write under
// ModeMetadata.
//
// The fence exists because the overlay would otherwise let ANY surviving
// liveness row win over committed metadata, across arbitrary time. Concretely:
// the liveness pool dies, a session transition falls back to a versioned write,
// the pool recovers — and now the PRE-outage rows for generation /
// instance_token / pending_create_claim / state shadow the POST-outage committed
// values. Wake fencing then compares against a stale instance_token and can tear
// down a live session, or re-enter a pending-create that was already rolled back.
//
// WHY PER KEY, not one stamp plus a key list. Metadata writes MERGE per key at
// the store layer, so a marker per key accumulates: a later fallback covering a
// different key set adds its own markers and leaves the earlier ones standing.
// The scalar pair this replaces (a single stamp plus a comma-separated list) was
// last-write-wins on BOTH halves, so the second fallback — or merely the second
// batch of a multi-batch Tx — rewrote the list and UN-FENCED every key the first
// one had fenced, resurrecting exactly the stale instance_token /
// pending_create_claim rows the fence exists to bury.
//
// Scoping to actual keys still matters, and per-key markers give it for free. A
// bead-wide fence would also drop rows for keys the fallback never wrote — and
// for a key that only ever lives in the table (state, say, after the create-time
// value went stale) the committed value is not newer, it is ancient. Fencing it
// would replace live telemetry with the value the bead was created with, which
// is worse than the shadow the fence exists to prevent.
//
// The rule is a strict inequality on the row's OWN timestamp: Overlay drops a
// row whose written_at is at or before that key's fence stamp, and keeps
// anything written after it. A later successful liveness write therefore takes
// over again on its own, with no reset step.
//
// Like WrittenAtKey these are infrastructure, never session state: they are
// refused as liveness keys on input and stripped from any inbound patch.
const FencePrefix = beadmeta.LivenessFencePrefix

// StampFormat is the wire format for every marker key. Nanosecond precision
// matters: the table's written_at is DATETIME(6), and a second-granularity
// stamp would fence out rows written in the same second as the fallback.
const StampFormat = time.RFC3339Nano

// FenceKeyFor returns the marker key that fences liveness key k.
func FenceKeyFor(k string) string {
	return FencePrefix + k
}

// IsMarkerKey reports whether key is one of the overlay's own infrastructure
// markers rather than session telemetry. Markers are never accepted as liveness
// input and never routed to the table.
func IsMarkerKey(key string) bool {
	return key == WrittenAtKey || strings.HasPrefix(key, FencePrefix)
}

// FenceStamp renders at as a fence-marker value.
func FenceStamp(at time.Time) string {
	return at.UTC().Format(StampFormat)
}

// ParseFence parses a fence-marker value. An absent or unparseable stamp yields
// the zero time, which fences nothing — the conservative direction: a corrupt
// marker must not silently discard live telemetry.
func ParseFence(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(StampFormat, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// keys is the exact set of bead-metadata keys that move to the liveness table.
// Defined ONCE here; every splitter, overlay and test references it. Anything
// not in this set stays in versioned metadata.
//
// Membership rule: the field is node-local runtime telemetry whose only correct
// merge is last-write-wins. Stable identity/config (agent_name, alias, command,
// provider, gc.session_name, gc.work_dir, configured_*) stays versioned, and so
// do the bead's own columns (status, assignee, close_reason) — those are genuine
// lifecycle history and SHOULD keep committing.
//
// BATCH COMPLETENESS is the second rule, and the reason this set grew after the
// first deploy. The splitter skips the versioned write only when EVERY key in a
// patch is a liveness key, so one straggler in a hot patch costs the whole
// commit: post-deploy churn stayed at ~244/hr because state moved but its
// same-batch companion state_reason did not, and every SleepPatch /
// ConfirmStartedPatch / RequestWakePatch still minted a Dolt commit for it.
// Before leaving a key versioned, check which patch builders in
// internal/session/lifecycle_transition.go carry it: a key that only ever
// appears beside moved keys must move too, or it re-mints every commit they
// avoid.
var keys = map[string]struct{}{
	// Lifecycle state and its same-batch companions. state_reason accompanies
	// state in every builder that writes one (Sleep/ConfirmStarted/BeginDrain/
	// Quarantine/Reactivate/RequestWake); leaving it behind made all of them
	// commit anyway.
	"state":                      {},
	"state_reason":               {},
	"awake_started_at":           {},
	"last_woke_at":               {},
	"slept_at":                   {},
	"sleep_reason":               {},
	"sleep_intent":               {},
	"synced_at":                  {},
	"generation":                 {},
	"held_until":                 {},
	"drain_at":                   {},
	"quarantined_until":          {},
	"quarantine_cycle":           {},
	"churn_count":                {},
	"wake_attempts":              {},
	"wait_hold":                  {},
	"wake_request":               {},
	"wake_requested_at":          {},
	"continuation_epoch":         {},
	"continuation_reset_pending": {},
	"pending_create_claim":       {},
	"pending_create_started_at":  {},
	"primed_at":                  {},
	"priming_attempted_at":       {},
	"instance_token":             {},
	"prior_session_key":          {},
	"creation_complete_at":       {},
	"detached_at":                {},

	// The work bead a session is currently processing. A secondary marker that
	// the reconciler re-derives from the live assignment every tick and that
	// build_desired_state explicitly refuses to treat as authoritative ("that
	// secondary marker can lag the live process"), so it carries no history.
	"currently_processing_bead_id": {},

	// Per-awake-interval accounting markers. Both are idempotency stamps keyed
	// on awake_started_at, never history; usage_model_swept_at is the declared
	// sibling of usage_compute_emitted_at (cmd/gc/usage_compute.go) and is
	// written by its own single-key SetMetadata, so leaving it behind kept one
	// commit per terminal interval.
	"usage_compute_emitted_at": {},
	"usage_model_swept_at":     {},

	// Nudge delivery. Stamped on EVERY successful delivery (cmd_nudge.go,
	// cmd_sling.go, the ACP dispatcher) by a single-key SetMarker, and read only
	// to render "last nudge N ago" in `gc session list` and the API. The single
	// biggest churn class measured after the first deploy.
	"last_nudge_delivered_at": {},

	// Stalled-claim backstop state machine (cmd/gc/idle_nudge.go). All three
	// keys are written by ONE SetMetadataBatch in writeIdleClaimMarker and
	// cleared by one in clearIdleClaimMarker, so the count key has to move with
	// the other two or the batch keeps committing.
	"idle_claim_nudge_trigger": {},
	"idle_claim_nudge_count":   {},
	"idle_claim_nudge_at":      {},

	// Post-step continuation-claim backstop — the same engine, the same
	// write-one-batch shape (writeContinuationClaimMarker), so the same
	// all-or-nothing rule applies to its six keys.
	"continuation_claim_nudge_work":       {},
	"continuation_claim_nudge_root":       {},
	"continuation_claim_nudge_store_ref":  {},
	"continuation_claim_nudge_generation": {},
	"continuation_claim_nudge_count":      {},
	"continuation_claim_nudge_at":         {},

	// Per-episode throttle for the session.stranded diagnostic. Its own writer
	// already documents the durable value as best-effort ("the in-memory marker
	// is the load-bearing single-emission guarantee"), and it is set and cleared
	// once per stranding episode by a single-key SetMarker.
	"stranded_event_emitted_at": {},

	beadmeta.LastHeartbeatAtMetadataKey: {},
}

// LEFT VERSIONED ON PURPOSE — gc.trigger_bead_id (beadmeta.TriggerBeadIDMetadataKey).
// It drives commits, but it fails the membership rule on two counts.
//
//  1. It is not telemetry. It is the pool slot's binding to the work it was
//     dispatched for: build_desired_state reads it to decide worktree reuse and
//     live-resume continuation, and the idle-claim backstop keys its whole state
//     machine on it. The reconciler never re-derives it — it IS the record.
//
//  2. Moving it would break a documented atomicity guarantee. It is written as
//     one member of the trigger/provenance cluster (trigger id, store ref, brain
//     parent sid, pack, workspace, work dir) through
//     session.Store.UpdateMetadataInfo, whose contract is one backend operation
//     so the cluster "commits atomically or not at all"
//     (internal/session/store.go). The splitter would send the trigger id to the
//     table and the rest through Update — exactly the split that contract
//     exists to forbid, leaving a bead bound to a new trigger with the old
//     store ref and work dir.
//
// The same reasoning keeps gc.trigger_bead_store_ref, gc.pack,
// gc.pack_workspace, gc.work_dir and gc.brain_parent_sid versioned.

// KNOWN LIMIT — do not filter a bead QUERY on a moved key. The read overlay
// merges liveness values onto beads AFTER the store has selected them, so a
// ListQuery.Metadata / ListByMetadata predicate on (say) state or
// pending_create_claim still matches the STALE committed value and would select
// the wrong beads. No such query exists in the tree today (verified across
// cmd/gc and internal/ when the split landed, and re-verified key by key when
// the sweep widened the set: every metadata filter keys on
// alias, named-session identity, kind, routed_to, root-bead id or idempotency
// key — all versioned). A new one must either query session_liveness directly or
// filter in memory after the overlay.

// IsKey reports whether key is a session-liveness key that must be routed to the
// liveness table instead of versioned bead metadata.
func IsKey(key string) bool {
	_, ok := keys[key]
	return ok
}

// Keys returns a copy of the moved key set, for tests and diagnostics.
func Keys() []string {
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	return out
}

// Split partitions a metadata patch into the liveness keys and the remainder.
// Both maps are freshly allocated; either may be empty. A nil patch yields two
// empty maps.
//
// An empty-string value is NOT filtered out here: empty means "cleared", and the
// clear has to reach the liveness table as a tombstone row (see SQLStore.SetBatch)
// so the read overlay does not fall back to a stale committed value.
//
// The overlay's own marker keys are dropped from BOTH halves. They are produced
// by the overlay and by the fallback stamper, never by a caller; letting an
// inbound WrittenAtKey through to the versioned remainder would commit a forged
// freshness clock that session.EffectiveUpdatedAt would then believe.
func Split(patch map[string]string) (live, rest map[string]string) {
	live = make(map[string]string, len(patch))
	rest = make(map[string]string, len(patch))
	for k, v := range patch {
		if IsMarkerKey(k) {
			continue
		}
		if IsKey(k) {
			live[k] = v
			continue
		}
		rest[k] = v
	}
	return live, rest
}
