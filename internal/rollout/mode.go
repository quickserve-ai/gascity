package rollout

import (
	"fmt"
	"strings"
)

// Mode is the tri-state value kind for a correctness/migration rollout gate.
type Mode string

const (
	// ModeUnset is the zero value: "nobody threaded a mode." It resolves AS Off
	// but carries a diagnostic reason so an unwired call site is visible rather
	// than silently defaulting.
	ModeUnset Mode = ""
	// Off runs the legacy path, byte-identical to pre-flag behavior. Off is
	// zero-cost: a capability predicate is never consulted.
	Off Mode = "off"
	// Auto runs the new path where the runtime is capable and loud-degrades to
	// the legacy path otherwise — never a silent unconditional fallback.
	Auto Mode = "auto"
	// Require runs the new path or refuses closed; a silent fallback is
	// inexpressible.
	Require Mode = "require"
)

// ParseMode parses a user-supplied spelling into a Mode. It is case- and
// space-tolerant ("Require", " AUTO " are accepted) and recognizes ONLY the
// three mode names — bool/truthy spellings and the empty string are errors that
// name the off|auto|require grammar. (A tri-state gate has no meaningful bool
// spelling; ModeUnset is produced by absence, never by parsing a value.)
func ParseMode(s string) (Mode, error) {
	switch normalizeToken(s) {
	case "off":
		return Off, nil
	case "auto":
		return Auto, nil
	case "require":
		return Require, nil
	default:
		return ModeUnset, fmt.Errorf("invalid mode %q: want one of off, auto, require", s)
	}
}

// normalizeToken lowercases and trims surrounding whitespace for the mode
// grammar (case/space tolerant break-glass values).
func normalizeToken(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
