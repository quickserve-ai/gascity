package liveness

import "time"

// Plan is the routing decision for one metadata patch: which keys go to the
// non-versioned liveness table and which go to versioned bead metadata.
//
// The decisive field is Versioned. When it is empty the caller must SKIP the
// underlying store write altogether — a skipped write is a Dolt commit that
// never happens, and that skip is the entire mechanism of this change.
type Plan struct {
	// Liveness is written to the session_liveness table. An empty-string value
	// is a clear and is written as a tombstone row, never deleted.
	Liveness map[string]string
	// Versioned is written to bead metadata through the normal store path.
	Versioned map[string]string
}

// PlanWrite routes patch according to mode.
//
//   - ModeTable splits: liveness keys to the table, everything else versioned.
//   - ModeMetadata sends the FULL patch versioned, FENCED — the patch carries a
//     fence marker for every liveness key in it, so the always-on read overlay
//     cannot let a table row shadow the committed values this write just made
//     authoritative. It still mirrors the liveness keys into the table so a flip
//     back to ModeTable finds them current instead of frozen at the moment the
//     flag was set.
//
// now supplies the fence stamps and must come from the liveness store's clock
// (Store.Now), not the local one: written_at is minted server-side, so a stamp
// from a client running behind the Dolt host would fence nothing.
//
// Both returned maps are freshly allocated and may be empty; neither is nil.
func PlanWrite(mode Mode, patch map[string]string, now time.Time) Plan {
	live, rest := Split(patch)
	if mode == ModeMetadata {
		full := make(map[string]string, len(patch)+len(live))
		for k, v := range patch {
			if IsMarkerKey(k) {
				continue
			}
			full[k] = v
		}
		stampFences(full, live, now)
		return Plan{Liveness: live, Versioned: full}
	}
	return Plan{Liveness: live, Versioned: rest}
}

// FallbackPlan is the routing decision for a write that could NOT reach the
// liveness table — a degraded write, or one inside a transaction where the
// liveness half must not be applied separately from the versioned half.
//
// Everything goes versioned, fenced with one marker PER liveness key so no
// surviving table row for those keys can shadow it afterwards. Nothing is routed
// to the table: a caller reaching for this either could not write it, or must not.
func FallbackPlan(patch map[string]string, now time.Time) map[string]string {
	live, rest := Split(patch)
	out := make(map[string]string, len(patch)+len(live))
	for k, v := range rest {
		out[k] = v
	}
	for k, v := range live {
		out[k] = v
	}
	stampFences(out, live, now)
	return out
}

// stampFences adds one fence marker per key in live.
//
// PER KEY is the whole mechanism. Metadata writes merge key-by-key at the store
// layer, so these markers ACCUMULATE across writes: a second fallback covering a
// different key set — or the second batch of a multi-batch Tx — adds its own
// markers and leaves every earlier one standing. The single stamp + key-list
// this replaced was last-write-wins, so the later write un-fenced the earlier
// one's keys and let their pre-outage rows win again.
func stampFences(out, live map[string]string, now time.Time) {
	if len(live) == 0 {
		return
	}
	stamp := FenceStamp(now)
	for k := range live {
		out[FenceKeyFor(k)] = stamp
	}
}
