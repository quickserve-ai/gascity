package session

import (
	"errors"
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
