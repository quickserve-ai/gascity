package session

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TerminationSurfacedAtKey is the READER'S BOOKMARK, not part of the record.
//
// It is deliberately absent from TerminationPatch. TerminationPatch writes the
// whole record on every ending, empty fields included, so a re-terminated bead
// cannot carry half of a previous ending; this key must NOT join that set,
// because clearing it on each ending would be a second write on the stop path
// buying nothing. The comparison does the work instead: a fresh ending stamps a
// NEWER termination.at than the bookmark, so the notice is owed again without
// anyone having to reset anything. Monotonic beats cleared.
const TerminationSurfacedAtKey = "termination.surfaced_at"

// TerminationRecord is the inverse of TerminationPatch: the record as the NEXT
// boot reads it back off the session bead.
//
// SessionID and the parsed timestamps come back; an unparseable timestamp comes
// back ZERO rather than failing the whole read, because a notice with a missing
// clock is worth more to a booting seat than no notice at all. Present reports
// whether there is a record here in the first place — the kind being non-empty
// is what makes it one.
type TerminationRecord struct {
	runtime.Termination
	// SurfacedAt is when a previous boot already showed this record to the
	// seat. Zero when it never has been.
	SurfacedAt time.Time
}

// ReadTerminationRecord parses the termination.* keys out of a session bead's
// metadata. The second return is false when the bead carries no record.
//
// It does NOT judge whether the record is owed a notice — that is
// TerminationKind.NeedsFallbackNote, and keeping the two apart matters: a
// reader that wants the ratio wants every record, and a reader that wants the
// notice wants a subset.
func ReadTerminationRecord(meta map[string]string) (TerminationRecord, bool) {
	if meta == nil {
		return TerminationRecord{}, false
	}
	kind := runtime.TerminationKind(strings.TrimSpace(meta[TerminationKindKey]))
	if kind == "" {
		return TerminationRecord{}, false
	}
	rec := TerminationRecord{Termination: runtime.Termination{
		Kind:        kind,
		Actor:       strings.TrimSpace(meta[TerminationActorKey]),
		Reason:      strings.TrimSpace(meta[TerminationReasonKey]),
		At:          parseRFC3339OrZero(meta[TerminationAtKey]),
		RequestedAt: parseRFC3339OrZero(meta[TerminationRequestedAtKey]),
	}}
	rec.SurfacedAt = parseRFC3339OrZero(meta[TerminationSurfacedAtKey])
	return rec, true
}

// NoticeOwed reports whether this record should be shown to the seat that is
// booting now: the kind must be one nobody wrote a note for, and a previous
// boot must not have shown it already.
//
// A RECORD WITH NO PARSEABLE At IS SHOWN ONCE AND THEN NEVER AGAIN, because the
// bookmark comparison cannot order it. Showing it is the right side to err on
// (the ending really did happen); the stamp written afterwards is what stops it
// repeating, since the next comparison has a zero At against a real bookmark.
func (r TerminationRecord) NoticeOwed() bool {
	if !r.Kind.NeedsFallbackNote() {
		return false
	}
	if r.SurfacedAt.IsZero() {
		return true
	}
	return r.At.After(r.SurfacedAt)
}

// TerminationSurfacedPatch stamps the bookmark. It runs at BOOT, on the seat's
// own bead, nowhere near a stop path.
func TerminationSurfacedPatch(at time.Time) MetadataPatch {
	return MetadataPatch{TerminationSurfacedAtKey: at.UTC().Format(time.RFC3339)}
}

func parseRFC3339OrZero(raw string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return t
}
