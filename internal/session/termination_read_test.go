package session

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestReadTerminationRecordRoundTripsThePatch(t *testing.T) {
	at := time.Date(2026, 9, 19, 21, 34, 45, 0, time.UTC)
	req := time.Date(2026, 9, 19, 21, 5, 15, 0, time.UTC)
	patch := TerminationPatch(runtime.Termination{
		Kind:        runtime.KindDrainTimeout,
		Actor:       "reconciler",
		Reason:      "config-drift",
		At:          at,
		RequestedAt: req,
		SessionID:   "ga-yalk76",
	})
	rec, ok := ReadTerminationRecord(patch)
	if !ok {
		t.Fatal("a record written by TerminationPatch must read back")
	}
	if rec.Kind != runtime.KindDrainTimeout || rec.Actor != "reconciler" || rec.Reason != "config-drift" {
		t.Errorf("fields did not round-trip: %+v", rec)
	}
	if !rec.At.Equal(at) {
		t.Errorf("At = %v, want %v", rec.At, at)
	}
	if !rec.RequestedAt.Equal(req) {
		t.Errorf("RequestedAt = %v, want %v — Timer B is unreadable without it", rec.RequestedAt, req)
	}
}

// TestNoRecordIsNotAnEmptyRecord. A session bead that has never terminated
// carries no termination.* keys at all, and that must be distinguishable from an
// ending whose fields happen to be blank.
func TestNoRecordIsNotAnEmptyRecord(t *testing.T) {
	if _, ok := ReadTerminationRecord(nil); ok {
		t.Error("nil metadata is not a record")
	}
	if _, ok := ReadTerminationRecord(map[string]string{"gc.session_name": "woodhouse"}); ok {
		t.Error("a bead with no termination keys is not a record")
	}
	if _, ok := ReadTerminationRecord(map[string]string{TerminationKindKey: "  "}); ok {
		t.Error("a blank kind is not a record")
	}
}

// TestUnparseableTimestampDoesNotDiscardTheRecord. A bad At degrades to zero
// rather than failing the read — but an unorderable record is only shown to a
// seat that has never checked before, because after that there is a real mark to
// compare against and "I cannot place this in time" is a reason to stay quiet.
func TestUnparseableTimestampDoesNotDiscardTheRecord(t *testing.T) {
	rec, ok := ReadTerminationRecord(map[string]string{
		TerminationKindKey: string(runtime.KindOperatorKill),
		TerminationAtKey:   "last tuesday",
	})
	if !ok {
		t.Fatal("an unparseable timestamp must not discard the whole record")
	}
	if !rec.At.IsZero() {
		t.Error("an unparseable At must come back zero, not guessed")
	}
	if !rec.NoticeOwed() {
		t.Error("a first-ever check with no readable clock should still surface the ending")
	}
	rec.CheckedAt = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	if rec.NoticeOwed() {
		t.Error("once the seat has checked before, an unorderable record must not be claimed as THIS boot's ending")
	}
}

func TestNoticeOwedGatesOnKindAndOnTheBookmark(t *testing.T) {
	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		kind     runtime.TerminationKind
		surfaced time.Time
		want     bool
	}{
		{"forced ending, seat has never checked", runtime.KindOperatorKill, time.Time{}, true},
		{"forced ending, happened since the last check", runtime.KindOperatorKill, at.Add(-time.Hour), true},
		{"forced ending, already seen at the last check", runtime.KindOperatorKill, at.Add(time.Minute), false},
		{"the seat handed off itself", runtime.KindHandoff, time.Time{}, false},
		{"the sender wrote the note", runtime.KindHandoffTarget, time.Time{}, false},
		{"context survived the restart", runtime.KindInterruptRestart, time.Time{}, false},
		{"runtime was already dead — successor is still amnesiac", runtime.KindObservedDead, time.Time{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := TerminationRecord{
				Termination: runtime.Termination{Kind: tc.kind, At: at},
				CheckedAt:   tc.surfaced,
			}
			if got := rec.NoticeOwed(); got != tc.want {
				t.Errorf("NoticeOwed() = %v, want %v", got, tc.want)
			}
		})
	}
	// go test -run prints "ok" for a regex that matches nothing, and a table
	// that lost its rows would pass silently. Assert the count.
	if len(cases) != 7 {
		t.Fatalf("table has %d cases, want 7", len(cases))
	}
}

// TestCheckedMarkIsNotPartOfTheRecord guards the placement decision: if
// TerminationPatch ever started writing the mark, each new ending would stamp
// "already checked" at the moment it happened and the notice would never fire
// again.
func TestCheckedMarkIsNotPartOfTheRecord(t *testing.T) {
	patch := TerminationPatch(runtime.Termination{Kind: runtime.KindOperatorKill, SessionID: "ga-1"})
	if _, present := patch[TerminationNoticeCheckedAtKey]; present {
		t.Error("TerminationPatch must NOT write termination.surfaced_at: the record is the writer's, the bookmark is the reader's")
	}
	stamp := TerminationNoticeCheckedPatch(time.Date(2026, 9, 19, 22, 0, 0, 0, time.UTC))
	if stamp[TerminationNoticeCheckedAtKey] != "2026-09-19T22:00:00Z" {
		t.Errorf("stamp = %q", stamp[TerminationNoticeCheckedAtKey])
	}
	if len(stamp) != 1 {
		t.Errorf("the stamp must touch exactly one key, got %d", len(stamp))
	}
}

// TestAStaleRecordIsNotServedAsThisBootsEnding is katya's S2 pre-merge condition.
// The bead sink may FAIL at death by design (five-second budget, sick store), so
// the newest record on a bead is not necessarily the newest ENDING. Without the
// boot marker, a seat whose last ending failed to record would be told "your
// previous session did not hand off" on the strength of an ending several
// restarts ago — a claim the record cannot support, in a notice whose whole value
// is that the seat can trust it.
func TestAStaleRecordIsNotServedAsThisBootsEnding(t *testing.T) {
	lastWeek := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	lastBoot := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)

	stale := TerminationRecord{
		Termination: runtime.Termination{Kind: runtime.KindOperatorKill, At: lastWeek},
		CheckedAt:   lastBoot,
	}
	if stale.NoticeOwed() {
		t.Error("a record predating the last boot describes an older session and must not be claimed as this boot's ending")
	}

	// And the guard must not swallow a REAL one: an ending after the last check
	// is exactly what the notice is for.
	fresh := stale
	fresh.At = lastBoot.Add(3 * time.Hour)
	if !fresh.NoticeOwed() {
		t.Error("an ending since the last check is the case this whole slice exists to report")
	}
}
