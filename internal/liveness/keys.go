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
// The package deliberately depends on nothing else in gascity: the key set has
// to be referenceable from internal/session, internal/beads and cmd/gc without
// creating an import cycle.
package liveness

import (
	"strings"
	"time"
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
const WrittenAtKey = "gc.liveness_written_at"

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
const FencePrefix = "gc.liveness_fence."

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
var keys = map[string]struct{}{
	"state":                      {},
	"awake_started_at":           {},
	"last_woke_at":               {},
	"slept_at":                   {},
	"sleep_reason":               {},
	"synced_at":                  {},
	"generation":                 {},
	"held_until":                 {},
	"drain_at":                   {},
	"quarantined_until":          {},
	"churn_count":                {},
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
	"usage_compute_emitted_at":   {},
	"gc.last_heartbeat_at":       {},
}

// KNOWN LIMIT — do not filter a bead QUERY on a moved key. The read overlay
// merges liveness values onto beads AFTER the store has selected them, so a
// ListQuery.Metadata / ListByMetadata predicate on (say) state or
// pending_create_claim still matches the STALE committed value and would select
// the wrong beads. No such query exists in the tree today (verified across
// cmd/gc and internal/ when the split landed: every metadata filter keys on
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
