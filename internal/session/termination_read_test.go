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

// TestUnparseableTimestampDoesNotDiscardTheRecord. A notice with a missing clock
// is worth more to a booting seat than no notice at all, so a bad At degrades to
// zero rather than failing the read.
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
		t.Error("a record with no readable clock is still an ending the seat should hear about")
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
		{"forced ending, never surfaced", runtime.KindOperatorKill, time.Time{}, true},
		{"forced ending, surfaced before it happened", runtime.KindOperatorKill, at.Add(-time.Hour), true},
		{"forced ending, already surfaced after it happened", runtime.KindOperatorKill, at.Add(time.Minute), false},
		{"the seat handed off itself", runtime.KindHandoff, time.Time{}, false},
		{"the sender wrote the note", runtime.KindHandoffTarget, time.Time{}, false},
		{"context survived the restart", runtime.KindInterruptRestart, time.Time{}, false},
		{"runtime was already dead — successor is still amnesiac", runtime.KindObservedDead, time.Time{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := TerminationRecord{
				Termination: runtime.Termination{Kind: tc.kind, At: at},
				SurfacedAt:  tc.surfaced,
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

// TestSurfacedBookmarkIsNotPartOfTheRecord guards the placement decision: if
// TerminationPatch ever started writing the bookmark, each new ending would
// stamp "already surfaced" at the moment it happened and the notice would never
// fire again.
func TestSurfacedBookmarkIsNotPartOfTheRecord(t *testing.T) {
	patch := TerminationPatch(runtime.Termination{Kind: runtime.KindOperatorKill, SessionID: "ga-1"})
	if _, present := patch[TerminationSurfacedAtKey]; present {
		t.Error("TerminationPatch must NOT write termination.surfaced_at: the record is the writer's, the bookmark is the reader's")
	}
	stamp := TerminationSurfacedPatch(time.Date(2026, 9, 19, 22, 0, 0, 0, time.UTC))
	if stamp[TerminationSurfacedAtKey] != "2026-09-19T22:00:00Z" {
		t.Errorf("stamp = %q", stamp[TerminationSurfacedAtKey])
	}
	if len(stamp) != 1 {
		t.Errorf("the stamp must touch exactly one key, got %d", len(stamp))
	}
}
