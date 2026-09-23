package events

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newestFirstFixture writes two archives, a stale twin of the active log's
// first window (as a rotation racing the walk leaves behind), and an active
// log, and returns the log path and the two archives' rotation stamps.
func newestFirstFixture(t *testing.T) (string, time.Time, time.Time) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	stampA := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	stampB := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	writeArchiveWithEvents(t, dir, stampA.Format("20060102T150405Z"), 1, 2,
		Event{Seq: 1, Type: OrderFailed, Ts: stampA.Add(-time.Hour)},
		Event{Seq: 2, Type: OrderFired, Ts: stampA.Add(-time.Hour)},
	)
	writeArchiveWithEvents(t, dir, stampB.Format("20060102T150405Z"), 3, 4,
		Event{Seq: 3, Type: OrderCompleted, Ts: stampB.Add(-time.Hour)},
		Event{Seq: 4, Type: OrderFailed, Ts: stampB.Add(-time.Hour)},
	)
	writeArchiveWithEvents(t, dir, "20260915T000000Z", 5, 6,
		Event{Seq: 5, Type: OrderFailed, Ts: stampB.Add(time.Hour)},
	)
	active := `{"seq":5,"type":"order.failed","ts":"2026-09-10T01:00:00Z","actor":"t"}
{"seq":6,"type":"order.fired","ts":"2026-09-10T01:00:00Z","actor":"t"}
{"seq":7,"type":"order.completed","ts":"2026-09-10T02:00:00Z","actor":"t"}
`
	if err := os.WriteFile(path, []byte(active), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, stampA, stampB
}

func TestWalkFilteredTypesNewestFirst(t *testing.T) {
	path, stampA, stampB := newestFirstFixture(t)
	types := []string{OrderCompleted, OrderFailed}

	type call struct {
		seqs    string
		through time.Time
	}
	walk := func(filter Filter, stopAfter int) []call {
		t.Helper()
		var calls []call
		err := WalkFilteredTypesNewestFirst(path, filter, types, func(evts []Event, through time.Time) bool {
			var seqs []uint64
			for _, e := range evts {
				seqs = append(seqs, e.Seq)
			}
			calls = append(calls, call{fmt.Sprint(seqs), through})
			return len(calls) < stopAfter
		})
		if err != nil {
			t.Fatal(err)
		}
		return calls
	}

	// Newest first, the twin of the active log skipped, and through one second
	// past the next older archive's rotation stamp, zero at the end.
	got := walk(Filter{}, 99)
	want := []call{
		{"[5 7]", stampB.Add(time.Second)},
		{"[3 4]", stampA.Add(time.Second)},
		{"[1]", time.Time{}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("walk = %v, want %v", got, want)
	}

	// BeforeSeq leaves the active log and later seqs unread.
	got = walk(Filter{BeforeSeq: 4}, 99)
	want = []call{
		{"[]", stampB.Add(time.Second)},
		{"[3]", stampA.Add(time.Second)},
		{"[1]", time.Time{}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("walk before seq 4 = %v, want %v", got, want)
	}

	// fn returning false stops before the next older source is opened.
	if got := walk(Filter{}, 2); len(got) != 2 {
		t.Fatalf("walk stopped after 2 = %v, want exactly 2 sources read", got)
	}

	if err := WalkFilteredTypesNewestFirst(path, Filter{Type: OrderFailed}, types, func([]Event, time.Time) bool { return true }); err == nil {
		t.Error("a filter.Type alongside types must be refused")
	}
	if err := WalkFilteredTypesNewestFirst(path, Filter{}, nil, func([]Event, time.Time) bool { return true }); err == nil {
		t.Error("no types must be refused")
	}
}

func TestWalkFilteredTypesNewestFirstMissingLog(t *testing.T) {
	calls := 0
	err := WalkFilteredTypesNewestFirst(filepath.Join(t.TempDir(), "events.jsonl"), Filter{}, []string{OrderFailed},
		func([]Event, time.Time) bool { calls++; return true })
	if err != nil || calls != 0 {
		t.Fatalf("missing log: err=%v calls=%d, want nil and 0", err, calls)
	}
}
