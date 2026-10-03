package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tlTurnEnd is tlTurnDuration's timestamp. The prompt that started that turn
// was submitted before it (tlPromptBefore); a prompt submitted after it
// (tlPromptAfter) is a new turn whose user line Claude has not written yet.
var (
	tlTurnEnd      = time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	tlPromptBefore = tlTurnEnd.Add(-5 * time.Second)
)

// The seat's UserPromptSubmit hook (gc nudge drain --inject) records the
// prompt marker the gate compares turn_duration against, keyed by
// GC_SESSION_ID, even on the empty-queue fast path.
func TestNudgeDrainInjectRecordsPromptSubmitted(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	writeNamedSessionCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_SESSION_ID", "gc-megheo")

	before := time.Now()
	var stdout, stderr bytes.Buffer
	if code := cmdNudgeDrainWithFormat(nil, true, "claude", &stdout, &stderr); code != 0 {
		t.Fatalf("cmdNudgeDrainWithFormat = %d, want 0; stderr=%q", code, stderr.String())
	}
	at, ok := readClaudePromptSubmitted(cityDir, "gc-megheo")
	if !ok {
		t.Fatal("no prompt-submitted marker after gc nudge drain --inject")
	}
	if at.Before(before.Add(-time.Second)) || at.After(time.Now().Add(time.Second)) {
		t.Fatalf("marker time %v not within the drain call", at)
	}
}

// R3-3: a failed marker write must not leave the previous marker looking
// valid; with no marker the transcript path does not deliver.
func TestRecordClaudePromptSubmittedFailedWriteRemovesOldMarker(t *testing.T) {
	city := t.TempDir()
	if err := recordClaudePromptSubmitted(city, "gc-1", tlPromptBefore); err != nil {
		t.Fatal(err)
	}
	failWrite := func(string, []byte) error { return errors.New("disk full") }
	orig := claudePromptMarkerWrite
	claudePromptMarkerWrite = failWrite
	t.Cleanup(func() { claudePromptMarkerWrite = orig })
	if err := recordClaudePromptSubmitted(city, "gc-1", time.Now()); err != nil {
		t.Fatalf("recordClaudePromptSubmitted = %v, want nil once the stale marker is removed", err)
	}
	if _, ok := readClaudePromptSubmitted(city, "gc-1"); ok {
		t.Fatal("old marker still readable after a failed write")
	}
}

// R3-3: when the stale marker cannot be removed either, the caller gets an
// error to log.
func TestRecordClaudePromptSubmittedReportsUnremovableStaleMarker(t *testing.T) {
	city := t.TempDir()
	if err := recordClaudePromptSubmitted(city, "gc-1", tlPromptBefore); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(claudePromptMarkerPath(city, "gc-1"))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := recordClaudePromptSubmitted(city, "gc-1", time.Now()); err == nil {
		t.Fatal("recordClaudePromptSubmitted = nil, want an error: write failed and stale marker kept")
	}
}

// R3-6: markers older than 7 days are pruned (at most once a day), so the
// directory does not grow with every session ever started.
func TestRecordClaudePromptSubmittedPrunesOldMarkers(t *testing.T) {
	city := t.TempDir()
	for _, id := range []string{"gc-old", "gc-recent"} {
		if err := recordClaudePromptSubmitted(city, id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(claudePromptMarkerPath(city, "gc-old"), old, old); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(claudePromptMarkerPath(city, "gc-old"))
	_ = os.Chtimes(filepath.Join(dir, ".pruned"), old, old)
	if err := recordClaudePromptSubmitted(city, "gc-new", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := readClaudePromptSubmitted(city, "gc-old"); ok {
		t.Fatal("8-day-old marker not pruned")
	}
	if _, ok := readClaudePromptSubmitted(city, "gc-recent"); !ok {
		t.Fatal("recent marker pruned")
	}
}

// Marker names that start with "." are never session ids (".pruned" is the
// prune stamp).
func TestClaudePromptMarkerPathRejectsDotNames(t *testing.T) {
	for _, id := range []string{".pruned", ".x", "."} {
		if got := claudePromptMarkerPath(t.TempDir(), id); got != "" {
			t.Errorf("claudePromptMarkerPath(%q) = %q, want \"\"", id, got)
		}
	}
}
