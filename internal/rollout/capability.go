package rollout

import "context"

// Capability reports whether the runtime can execute a gate's new path. It is
// supplied per-call by a consumer-owned adapter (beads CAS supplies a bd/store
// probe; a future non-beads gate supplies its own) and is NEVER stored on a Spec
// or in the registry — that is what keeps this package free of consumer imports
// and the capability model general. A nil Capability means "this gate has no
// runtime capability question" and is vacuously capable.
type Capability func(ctx context.Context) (capable bool, reason string)

// Decision is the four-way verdict of the enable-AND-capable product.
type Decision string

const (
	// UseLegacy runs the old path (Off, or ModeUnset defaulted to Off).
	UseLegacy Decision = "use_legacy"
	// UseNew runs the new path (Auto or Require, and capable).
	UseNew Decision = "use_new"
	// DegradeLoud runs the old path but obliges the caller to surface a
	// diagnostic (Auto and not capable) — never a silent fallback.
	DegradeLoud Decision = "degrade_loud"
	// RefuseClosed is a typed refusal that must not fall back to the old path
	// (Require and not capable).
	RefuseClosed Decision = "refuse_closed"
)

// ResolveCapability computes the enable-AND-capable product — here and nowhere
// else, for every rollout gate, generically. The cell contract:
//
//	ModeUnset          -> UseLegacy    ("mode unset; defaulted to off"); cap not consulted
//	Off                -> UseLegacy    ("mode off"); cap NOT consulted (Off is zero-cost)
//	Auto,    capable   -> UseNew
//	Auto,    !capable  -> DegradeLoud  (reason carries the predicate's reason)
//	Require, capable   -> UseNew
//	Require, !capable  -> RefuseClosed (reason carries the predicate's reason)
//
// A nil cap is vacuously capable, so Auto/Require with a nil predicate resolve to
// UseNew. The capability predicate's reason string propagates verbatim into the
// returned reason.
func ResolveCapability(ctx context.Context, mode Mode, pred Capability) (Decision, string) {
	switch mode {
	case ModeUnset:
		return UseLegacy, "mode unset; defaulted to off"
	case Off:
		return UseLegacy, "mode off"
	case Auto, Require:
		// fall through to the capability check below.
	default:
		// An unrecognized mode is treated as the safe legacy path; Resolve
		// rejects out-of-enum config before a value ever reaches here.
		return UseLegacy, "unrecognized mode " + string(mode) + "; defaulted to off"
	}

	capable, reason := true, "no capability predicate"
	if pred != nil {
		capable, reason = pred(ctx)
	}
	if capable {
		return UseNew, reason
	}
	if mode == Require {
		return RefuseClosed, reason
	}
	return DegradeLoud, reason
}
