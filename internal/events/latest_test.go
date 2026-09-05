package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLatestTestLog(t *testing.T, path string, evts ...Event) {
	t.Helper()
	var b strings.Builder
	for _, e := range evts {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshaling %v: %v", e, err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestLatestPerSubjectInActiveLogFoldsNewestPerPair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	writeLatestTestLog(t, path,
		Event{Seq: 1, Type: OrderFired, Subject: "gate-sweep", Ts: base},
		Event{Seq: 2, Type: OrderFired, Subject: "reaper", Ts: base.Add(1 * time.Minute)},
		// Out of Ts order on purpose: the fold must take the newest Ts, not the
		// last line, so a log that interleaves writers still folds correctly.
		Event{Seq: 3, Type: OrderFired, Subject: "gate-sweep", Ts: base.Add(9 * time.Minute)},
		Event{Seq: 4, Type: OrderFired, Subject: "gate-sweep", Ts: base.Add(4 * time.Minute)},
		Event{Seq: 5, Type: ControllerStarted, Ts: base.Add(2 * time.Minute)},
		Event{Seq: 6, Type: ControllerStarted, Ts: base.Add(7 * time.Minute)},
		// A type nobody asked for must not appear in the result.
		Event{Seq: 7, Type: BeadCreated, Subject: "gate-sweep", Ts: base.Add(30 * time.Minute)},
	)

	latest, err := LatestPerSubjectInActiveLog(path, OrderFired, ControllerStarted)
	if err != nil {
		t.Fatalf("LatestPerSubjectInActiveLog: %v", err)
	}
	if len(latest) != 3 {
		t.Fatalf("got %d pairs (%v), want 3", len(latest), latest)
	}
	if got, want := latest[TypeSubject{Type: OrderFired, Subject: "gate-sweep"}].Ts, base.Add(9*time.Minute); !got.Equal(want) {
		t.Errorf("gate-sweep last fired = %v, want %v", got, want)
	}
	if got, want := latest[TypeSubject{Type: OrderFired, Subject: "reaper"}].Ts, base.Add(1*time.Minute); !got.Equal(want) {
		t.Errorf("reaper last fired = %v, want %v", got, want)
	}
	if got, want := LatestTsForType(latest, ControllerStarted), base.Add(7*time.Minute); !got.Equal(want) {
		t.Errorf("latest controller.started = %v, want %v", got, want)
	}
	if got := LatestTsForType(latest, BeadCreated); !got.IsZero() {
		t.Errorf("bead.created leaked into the fold: %v", got)
	}
}

// TestLatestPerSubjectInActiveLogIgnoresArchives pins the bound that makes this
// read cheap: the fold's cost must track rotation policy, not retained history.
// An archive that would win the fold on Ts must be invisible to it.
func TestLatestPerSubjectInActiveLogIgnoresArchives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	base := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	writeLatestTestLog(t, path, Event{Seq: 9, Type: OrderFired, Subject: "gate-sweep", Ts: base})

	archived, err := json.Marshal(Event{Seq: 1, Type: OrderFired, Subject: "archived-only", Ts: base.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	writeGzipFile(t, filepath.Join(dir, formatArchiveBasename(base.Add(-30*time.Minute), 1, 8)), string(archived)+"\n")

	// ReadFiltered walks the archive; the bounded fold must not.
	full, err := ReadFiltered(path, Filter{Type: OrderFired})
	if err != nil {
		t.Fatalf("ReadFiltered: %v", err)
	}
	if len(full) != 2 {
		t.Fatalf("ReadFiltered returned %d events, want 2 (the fixture archive is not being read)", len(full))
	}

	latest, err := LatestPerSubjectInActiveLog(path, OrderFired)
	if err != nil {
		t.Fatalf("LatestPerSubjectInActiveLog: %v", err)
	}
	if _, ok := latest[TypeSubject{Type: OrderFired, Subject: "archived-only"}]; ok {
		t.Error("archived event entered the fold; the read is no longer bounded by the active log")
	}
	if got, want := latest[TypeSubject{Type: OrderFired, Subject: "gate-sweep"}].Ts, base; !got.Equal(want) {
		t.Errorf("gate-sweep last fired = %v, want %v", got, want)
	}
}

func TestLatestPerSubjectInActiveLogEdgeCases(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	t.Run("missing log is not an error", func(t *testing.T) {
		latest, err := LatestPerSubjectInActiveLog(filepath.Join(dir, "absent.jsonl"), OrderFired)
		if err != nil {
			t.Fatalf("LatestPerSubjectInActiveLog: %v", err)
		}
		if len(latest) != 0 {
			t.Fatalf("got %v, want empty", latest)
		}
	})

	t.Run("no types requested reads nothing", func(t *testing.T) {
		path := filepath.Join(dir, "typeless.jsonl")
		writeLatestTestLog(t, path, Event{Seq: 1, Type: OrderFired, Subject: "gate-sweep", Ts: base})
		latest, err := LatestPerSubjectInActiveLog(path)
		if err != nil {
			t.Fatalf("LatestPerSubjectInActiveLog: %v", err)
		}
		if len(latest) != 0 {
			t.Fatalf("got %v, want empty", latest)
		}
	})

	t.Run("malformed lines are skipped", func(t *testing.T) {
		path := filepath.Join(dir, "malformed.jsonl")
		good, err := json.Marshal(Event{Seq: 2, Type: OrderFired, Subject: "gate-sweep", Ts: base})
		if err != nil {
			t.Fatal(err)
		}
		body := "{not json\n" + string(good) + "\n\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		latest, err := LatestPerSubjectInActiveLog(path, OrderFired)
		if err != nil {
			t.Fatalf("LatestPerSubjectInActiveLog: %v", err)
		}
		if got, want := latest[TypeSubject{Type: OrderFired, Subject: "gate-sweep"}].Ts, base; !got.Equal(want) {
			t.Errorf("gate-sweep last fired = %v, want %v", got, want)
		}
	})
}
