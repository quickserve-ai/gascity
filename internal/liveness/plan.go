package liveness

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
//   - ModeMetadata sends the FULL patch versioned (the rollback behaviour) and
//     additionally mirrors the liveness keys into the table so the read overlay
//     never shadows fresh committed metadata with frozen rows.
//
// Both returned maps are freshly allocated and may be empty; neither is nil.
func PlanWrite(mode Mode, patch map[string]string) Plan {
	live, rest := Split(patch)
	if mode == ModeMetadata {
		full := make(map[string]string, len(patch))
		for k, v := range patch {
			full[k] = v
		}
		return Plan{Liveness: live, Versioned: full}
	}
	return Plan{Liveness: live, Versioned: rest}
}
