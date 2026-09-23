package session

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TerminationNoticeCheckedAtKey is the READER'S mark, not part of the record,
// and it does two jobs with one key: it is the bookmark that stops a notice
// repeating, AND it is the seat's LAST-BOOT marker.
//
// THE SECOND JOB IS WHY IT IS STAMPED ON EVERY SessionStart CHECK, not only when
// a notice renders (katya, S2 review). The bead sink is allowed to FAIL at death
// by design — five-second budget, sick store — so the newest record on a bead is
// not necessarily the newest ENDING. Without a boot marker, a seat whose last
// ending failed to record would be served the PREVIOUS ending's record as though
// it described the session that just died. Stamping every check turns the
// comparison into "did this ending happen since I last looked", which is the
// window that makes the claim true.
//
// It is deliberately absent from TerminationPatch. TerminationPatch writes the
// whole record on every ending, empty fields included, so a re-terminated bead
// cannot carry half of a previous ending; this key must NOT join that set,
// because clearing it on each ending would be a second write on the stop path
// buying nothing. Monotonic beats cleared.
const TerminationNoticeCheckedAtKey = "termination.notice_checked_at"

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
	// CheckedAt is when this seat last ran the SessionStart notice check —
	// in practice, when it last booted. Zero when it never has.
	CheckedAt time.Time
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
	rec.CheckedAt = parseRFC3339OrZero(meta[TerminationNoticeCheckedAtKey])
	return rec, true
}

// NoticeOwed reports whether this record should be shown to the seat booting
// now. Two conditions: the kind must be one nobody wrote a note for, and the
// ending must have happened SINCE THIS SEAT LAST LOOKED.
//
// THE SECOND CONDITION IS A FRESHNESS GUARD, NOT JUST DE-DUPLICATION (katya, S2
// review). The bead sink may fail at death, so the newest record on a bead can
// be older than the newest ending. Serving it anyway would tell a seat "your
// previous session did not hand off" on the strength of an ending two or ten
// restarts ago — a claim the record cannot support, in a notice whose entire
// value is that the seat can trust it. Bounding it to the window since the last
// check makes the claim exactly as strong as the evidence.
//
// A RECORD WITH NO PARSEABLE At CANNOT BE ORDERED, so it is shown only to a seat
// that has never checked before. After the first check there is a real mark to
// compare against and a zero At loses — deliberately: an unorderable record is
// not evidence about THIS boot, and "I cannot place this in time" is a reason to
// stay quiet rather than to assert.
func (r TerminationRecord) NoticeOwed() bool {
	if !r.Kind.NeedsFallbackNote() {
		return false
	}
	if r.CheckedAt.IsZero() {
		return true
	}
	return r.At.After(r.CheckedAt)
}

// TerminationNoticeCheckedPatch stamps the mark. It runs at BOOT on every
// SessionStart check, whether or not a notice rendered, on the seat's own bead
// and nowhere near a stop path. Stamping only on render would leave the window
// frozen at the last notice, which is the stale-record hole this closes.
func TerminationNoticeCheckedPatch(at time.Time) MetadataPatch {
	return MetadataPatch{TerminationNoticeCheckedAtKey: at.UTC().Format(time.RFC3339)}
}

func parseRFC3339OrZero(raw string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return t
}
