package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestWorkerFactoryRuntimeHandleRecordsItsEnding — Codex, PR #106 r8 (P1). The
// CLI factory never set FactoryConfig.Recorder, so a runtime-only handle's
// ending, which has no bead sink, went to events.Discard: its only record was
// lost and the ending was missing from the ratio's denominator.
func TestWorkerFactoryRuntimeHandleRecordsItsEnding(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "orphan-x", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	f, err := workerFactoryWithConfig(city, beads.NewMemStore(), sp, &config.City{})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	h, err := f.RuntimeHandle("orphan-x", "", "", nil)
	if err != nil {
		t.Fatalf("RuntimeHandle: %v", err)
	}
	if err := h.Kill(context.Background()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(city, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("no events file: the ending was not recorded: %v", err)
	}
	if !strings.Contains(string(data), `"session.terminated"`) || !strings.Contains(string(data), "orphan-x") {
		t.Fatalf("events.jsonl has no session.terminated for orphan-x:\n%s", data)
	}
	if again := sharedWorkerEventsRecorder(city); again != sharedWorkerEventsRecorder(city) {
		t.Fatal("the per-city recorder is not shared: each factory build would open a new file handle")
	}
}

func TestSharedWorkerEventsRecorderNeedsACity(t *testing.T) {
	if rec := sharedWorkerEventsRecorder("  "); rec != nil {
		t.Fatalf("empty city path returned %T, want nil (no recorder, no stray .gc/events.jsonl in cwd)", rec)
	}
}
