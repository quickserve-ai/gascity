package liveness

import (
	"os"
	"strings"
)

// ModeEnv is the environment variable that selects where session-liveness
// telemetry is written. See Mode.
const ModeEnv = "GC_SESSION_LIVENESS_STORE"

// Mode selects the liveness write discipline.
type Mode string

const (
	// ModeTable (the default) SPLITS every metadata write: liveness keys go to
	// the non-versioned session_liveness table and the remainder goes to
	// versioned bead metadata. When nothing but liveness keys were in the
	// patch, the versioned write is skipped entirely — that skip is what
	// removes the Dolt commit.
	ModeTable Mode = "table"

	// ModeMetadata is the rollback path: the FULL patch goes to versioned bead
	// metadata exactly as it did before this change, restoring the pre-change
	// commit behavior byte for byte.
	//
	// It still MIRRORS liveness keys into the table. That is deliberate and is
	// the one deviation from "no split at all": reads overlay the table in both
	// modes, so a mode flip that stopped updating the table would leave the
	// overlay shadowing fresh committed metadata with frozen table rows. The
	// mirror keeps both stores in agreement, which makes the flag genuinely
	// reversible in BOTH directions instead of one-way. A mirror failure is not
	// fatal in this mode — versioned metadata is authoritative here.
	ModeMetadata Mode = "metadata"
)

// ModeFromEnv resolves the mode from the process environment. An unset, empty,
// or unrecognized value resolves to ModeTable: the flag exists to turn the new
// behavior OFF deliberately, never to have it disabled by a typo silently
// re-enabling ~250 Dolt commits an hour.
func ModeFromEnv() Mode {
	return ParseMode(os.Getenv(ModeEnv))
}

// ParseMode resolves a raw flag value. Only an exact (case-insensitive) match on
// "metadata" selects the legacy path.
func ParseMode(raw string) Mode {
	if strings.EqualFold(strings.TrimSpace(raw), string(ModeMetadata)) {
		return ModeMetadata
	}
	return ModeTable
}
