package session

import (
	"errors"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Termination metadata keys. Flat dotted keys, matching the rest of the session
// bead's metadata (gc.session_name, cert.entered, ...).
//
// THESE MUST NOT MOVE TO THE dolt-ignored session_liveness TABLE. Liveness
// heartbeats were relocated there precisely because they mint a Dolt commit per
// write; termination records want the opposite treatment. They are RARE (once
// per session ending), and their whole value is that dolt_history_issues keeps
// them — that history is the source of record for the handoff ratio, and mail
// cannot substitute for it (wisps skip DOLT_COMMIT, so a consumed row leaves no
// forensics at all).
const (
	TerminationKindKey        = "termination.kind"
	TerminationActorKey       = "termination.actor"
	TerminationReasonKey      = "termination.reason"
	TerminationAtKey          = "termination.at"
	TerminationRequestedAtKey = "termination.requested_at"
	TerminationEventIDKey     = "termination.event_id"
)

// TerminationPatch builds the metadata patch for one ending.
//
// Empty optional fields are written as empty strings rather than omitted, so a
// re-terminated session bead cannot carry a stale actor or reason from a
// PREVIOUS ending beside a fresh kind. A half-updated record reads as a
// coherent one and is worse than an obviously missing field.
func TerminationPatch(t runtime.Termination) MetadataPatch {
	at := t.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	p := MetadataPatch{
		TerminationKindKey:   string(t.Kind),
		TerminationActorKey:  t.Actor,
		TerminationReasonKey: t.Reason,
		TerminationAtKey:     at.UTC().Format(time.RFC3339),
		// Written unconditionally, like the rest: a stale id beside a fresh
		// kind would join this bead to the wrong event row.
		TerminationEventIDKey: t.EventID,
	}
	if t.RequestedAt.IsZero() {
		p[TerminationRequestedAtKey] = ""
	} else {
		p[TerminationRequestedAtKey] = t.RequestedAt.UTC().Format(time.RFC3339)
	}
	return p
}

// ErrNoSessionID is returned when a termination record carries no session id.
// The sink does NOT fall back to resolving the name: that would put a store
// read on the stop path, which is the one place a slow store must not reach.
var ErrNoSessionID = errors.New("termination record carries no SessionID, so it cannot be written to a bead")

// BeadTerminationSink writes the termination record to the session bead through
// Store.ApplyPatch — the documented metadata write chokepoint.
//
// THIS IS THE AUTHORITATIVE SINK. Bead history (dolt_history_issues) is the
// source of record for the handoff ratio; the session.terminated event stream
// corroborates it. Where a bead record is missing but an event exists, the
// event FILLS the hole and the week is FLAGGED — never silently patched
// (katya, ga-ksac39 condition 2).
//
// It rides Dolt, so it is the sink that CAN fail during an incident, which is
// exactly why there are two. StopRecorded never lets its failure hold the stop.
type BeadTerminationSink struct{ store *Store }

// NewBeadTerminationSink returns a sink writing through store. A nil store
// yields a nil sink, which StopRecorded skips.
func NewBeadTerminationSink(store *Store) *BeadTerminationSink {
	if store == nil {
		return nil
	}
	return &BeadTerminationSink{store: store}
}

// RecordTermination writes the record for t.SessionID. The session NAME is
// accepted to satisfy the sink interface and for diagnostics; the bead is
// addressed by id, never by a name lookup.
func (s *BeadTerminationSink) RecordTermination(_ string, t runtime.Termination) error {
	if s == nil || s.store == nil {
		return nil
	}
	if t.SessionID == "" {
		return ErrNoSessionID
	}
	return s.store.ApplyPatch(t.SessionID, TerminationPatch(t))
}

// Compile-time proof that this satisfies the seam's sink contract. Without it a
// signature drift would only surface at the call site that wires them together.
var _ runtime.TerminationSink = (*BeadTerminationSink)(nil)

// TERMINATION INTENT: a kind STATED BY ONE PATH AND CONSUMED BY ANOTHER.
//
// Most endings are classified by the caller that performs the stop. `gc handoff`
// is not: the seat asks for a restart and then the RECONCILER stops it, on a
// later tick, in a different process. Without a handover the reconciler cannot
// tell a handoff from any other restart request, so it records the generic kind
// — which is why KindHandoff had no producer anywhere in the tree and the ratio's
// NUMERATOR was structurally zero (found by the Codex review of PR #106).
//
// The intent is written on the session bead at request time and read by whoever
// performs the stop. Its timestamp is what finally makes Timer B real for the
// good ending: At - RequestedAt, measured rather than assumed. On 2026-09-19 a
// handoff on this box took 56m32s from request to new pane and the only way to
// know that was to reconstruct it by hand from a stall warning.
//
// CONSUMED AT MOST ONCE, deliberately. The reader CLEARS the intent whether or
// not it used it, because a handoff whose restart never arrives — which happens,
// and is its own open bug (ga-cctcju) — would otherwise leave the marker armed
// for whatever stopped the seat next, and mislabel an unrelated ending as a
// handoff. A kind that over-claims the numerator is worse than one that misses:
// the ratio exists to be trusted when it says the factory is handing off.
const (
	TerminationIntentKey   = "termination.intent"
	TerminationIntentAtKey = "termination.intent_at"
)

// TerminationIntentPatch states the kind a later stop should record.
func TerminationIntentPatch(kind runtime.TerminationKind, at time.Time) MetadataPatch {
	return MetadataPatch{
		TerminationIntentKey:   string(kind),
		TerminationIntentAtKey: at.UTC().Format(time.RFC3339),
	}
}

// ClearTerminationIntentPatch retires a stated intent. See the consumed-at-most-
// once note above: the consumer clears even when it declines to use it.
func ClearTerminationIntentPatch() MetadataPatch {
	return MetadataPatch{TerminationIntentKey: "", TerminationIntentAtKey: ""}
}

// ReadTerminationIntent returns the stated kind and the instant it was stated.
// The second result is false when no intent is present or the kind is not in the
// closed set — an unknown string must not become a bucket.
func ReadTerminationIntent(rawKind, rawAt string) (runtime.TerminationKind, time.Time, bool) {
	kind := runtime.TerminationKind(strings.TrimSpace(rawKind))
	if !kind.Valid() {
		return "", time.Time{}, false
	}
	return kind, parseRFC3339OrZero(rawAt), true
}

// parseRFC3339OrZero degrades an unreadable timestamp to the zero time rather
// than to an error. A record whose clock cannot be parsed is still a record, and
// every caller here would otherwise have to decide the same thing again.
func parseRFC3339OrZero(raw string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return t
}
