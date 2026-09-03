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
	// metadata, restoring the pre-change commit volume.
	//
	// Versioned metadata really is AUTHORITATIVE in this mode, and the mechanism
	// is the fence, not the absence of a table. Reads overlay unconditionally in
	// BOTH modes — the overlay has no idea which mode wrote a row, and a process
	// running in table mode can be reading beads a metadata-mode process just
	// wrote — so "metadata mode" cannot mean "the overlay is off". Instead every
	// metadata-mode write carries a FallbackAtKey stamp, which fences out every
	// table row written at or before it. The committed value therefore wins, in
	// this process and in every other one, without anybody having to agree on a
	// mode.
	//
	// It still MIRRORS liveness keys into the table so that a flip BACK to table
	// mode finds them current instead of frozen at the instant the flag was set.
	// A mirror failure is not fatal here: the fence already made the committed
	// value authoritative.
	//
	// SCOPE OF THE FLAG: it is read once per scope, when that scope's binding is
	// first created, and cached for the life of the process. Changing it requires
	// restarting the processes that should observe it — for the fleet that means
	// the controller, not just the next CLI invocation.
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
