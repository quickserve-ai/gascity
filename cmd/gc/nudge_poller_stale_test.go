package main

import (
	"os"
	"path/filepath"
	"testing"
)

func staleCheckFor(t *testing.T, path string) nudgePollerStaleCheck {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return nudgePollerStaleCheck{path: path, start: fi}
}

func TestNudgePollerStaleCheck(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gc")
	if err := os.WriteFile(bin, []byte("build-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	check := staleCheckFor(t, bin)

	if check.stale() {
		t.Error("untouched binary read as stale")
	}

	// The deploy recipe's atomic swap: stage a new file, rename over the
	// path. Same path, new inode — the exact event that must trip the check.
	staged := filepath.Join(dir, "gc.staged")
	if err := os.WriteFile(staged, []byte("build-2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, bin); err != nil {
		t.Fatal(err)
	}
	if !check.stale() {
		t.Error("rename-swapped binary not detected as stale")
	}

	check.disarm()
	if check.stale() {
		t.Error("disarmed check still reports stale")
	}
}

func TestNudgePollerStaleCheckProbeFailuresReadNotStale(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gc")
	if err := os.WriteFile(bin, []byte("build-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	check := staleCheckFor(t, bin)
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if check.stale() {
		t.Error("stat failure (missing path) must read not-stale, never kill delivery")
	}
	if (nudgePollerStaleCheck{}).stale() {
		t.Error("zero-value check (startup stat failed) must read not-stale")
	}
}
