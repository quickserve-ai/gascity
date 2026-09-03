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
func Split(patch map[string]string) (live, rest map[string]string) {
	live = make(map[string]string, len(patch))
	rest = make(map[string]string, len(patch))
	for k, v := range patch {
		if IsKey(k) {
			live[k] = v
			continue
		}
		rest[k] = v
	}
	return live, rest
}
