//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ga-bq84cj. THE test: on macOS the scanner must actually produce liveness data.
// Against the old /proc-only implementation this fails, because os.ReadDir("/proc")
// errors on Darwin and scanned is false — which is how the reaper accumulated
// 157k skip events and zero reaps.
//
// It asserts on the REAL production path (no stub), and on a cwd it knows must be
// present: this test process's own. Asserting merely that scanned==true would pass
// on an empty set; asserting a known member is what makes it non-vacuous.
func TestCollectLiveWorktreeState_DarwinProducesRealLiveness(t *testing.T) {
	state := collectLiveWorktreeState()

	if !state.scanned {
		t.Fatal("scanned=false on darwin: the reaper will fail closed and reap nothing (this is the ga-bq84cj regression)")
	}
	if len(state.cwds) == 0 {
		t.Fatal("scanned=true with zero cwds: this process is live and has a cwd, so the set cannot be empty")
	}

	self, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	want := normalizeLiveCWDs([]string{self})
	if len(want) != 1 {
		t.Fatalf("could not normalize own cwd %q", self)
	}
	for _, got := range state.cwds {
		if got == want[0] {
			return // found ourselves: the scan sees real, current processes
		}
	}
	t.Fatalf("scan did not include this test process's own cwd %q; it reported %d cwds", want[0], len(state.cwds))
}

// The protection the reaper depends on must hold end to end: a worktree that
// contains a live process's cwd is reported live. Uses the real scan plus this
// process's own cwd as the "agent working in the tree".
func TestWorktreeIsLive_ProtectsTreeContainingThisProcess(t *testing.T) {
	state := collectLiveWorktreeState()
	if !state.scanned {
		t.Skip("scan unavailable; covered by the fail-closed tests")
	}
	self, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// The parent of our cwd stands in for a worktree root we are working inside.
	parent := filepath.Dir(self)

	live, reason := worktreeIsLive(parent, state, nil)
	if !live {
		t.Fatalf("worktree %q contains this live process (cwd %q) but was reported reapable", parent, self)
	}
	if !strings.Contains(reason, "live process cwd") {
		t.Fatalf("reason should name the live process cwd, got %q", reason)
	}
}

// --- fail-closed paths. Each must yield scanned=false, never a partial set. ---

func TestCollectLiveWorktreeState_FailsClosedWhenLsofMissing(t *testing.T) {
	orig := lsofPath
	t.Cleanup(func() { lsofPath = orig })
	lsofPath = filepath.Join(t.TempDir(), "definitely-not-lsof")

	state := collectLiveWorktreeState()
	if state.scanned {
		t.Fatal("missing lsof must be indeterminate (scanned=false), not a clean empty scan")
	}
	if len(state.cwds) != 0 {
		t.Fatalf("fail-closed must carry no cwds, got %v", state.cwds)
	}
}

// A clean exit with no parseable output means lsof changed format or was
// blocked. It cannot be true — this process has a cwd — so it must fail closed
// rather than report "nothing is live", which would authorize reaping the fleet.
func TestCollectLiveWorktreeState_FailsClosedOnEmptyOutput(t *testing.T) {
	orig := lsofPath
	t.Cleanup(func() { lsofPath = orig })
	lsofPath = writeStub(t, "#!/bin/sh\nexit 0\n")

	state := collectLiveWorktreeState()
	if state.scanned {
		t.Fatal("empty output must fail closed: an empty live set would make every worktree look reapable")
	}
}

func TestCollectLiveWorktreeState_FailsClosedOnTimeout(t *testing.T) {
	origPath, origTimeout := lsofPath, lsofCWDTimeout
	t.Cleanup(func() { lsofPath, lsofCWDTimeout = origPath, origTimeout })
	lsofPath = writeStub(t, "#!/bin/sh\nsleep 30\n")
	lsofCWDTimeout = 300 * time.Millisecond

	start := time.Now()
	state := collectLiveWorktreeState()
	if state.scanned {
		t.Fatal("a timed-out scan is truncated; acting on partial liveness could reap a protected tree")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout was not enforced: took %s", elapsed)
	}
}

// TRUNCATED OUTPUT MUST BE REJECTED, and this is the dangerous case.
//
// The other timeout test passes for the wrong reason: its stub emits nothing, so
// the empty-output guard catches it and the ctx.Err() check is never exercised.
// Mutation proved it — deleting the ctx.Err() check left that test green. Here the
// stub emits a VALID cwd line and then hangs past the deadline, so the only thing
// standing between us and "scanned=true with a half-read process table" is the
// timeout check. A partial live set is worse than none: every worktree whose
// protecting process sat in the unread remainder looks reapable.
func TestCollectLiveWorktreeState_FailsClosedOnTimeoutWithPartialOutput(t *testing.T) {
	origPath, origTimeout := lsofPath, lsofCWDTimeout
	t.Cleanup(func() { lsofPath, lsofCWDTimeout = origPath, origTimeout })

	dir := t.TempDir()
	lsofPath = writeStub(t, "#!/bin/sh\nprintf 'p111\\nfcwd\\nn"+dir+"\\n'\nsleep 30\n")
	lsofCWDTimeout = 400 * time.Millisecond

	state := collectLiveWorktreeState()
	if state.scanned {
		t.Fatalf("truncated scan accepted as authoritative (cwds=%v); a partial live set makes protected worktrees look reapable", state.cwds)
	}
	if len(state.cwds) != 0 {
		t.Fatalf("fail-closed must carry no cwds, got %v", state.cwds)
	}
}

// A NON-ZERO exit with good output is lsof's normal partial-success behavior and
// must be ACCEPTED. Rejecting it would silently restore never-reaps.
func TestCollectLiveWorktreeState_AcceptsNonZeroExitWithOutput(t *testing.T) {
	orig := lsofPath
	t.Cleanup(func() { lsofPath = orig })
	dir := t.TempDir()
	lsofPath = writeStub(t, "#!/bin/sh\nprintf 'p123\\nfcwd\\nn"+dir+"\\n'\nexit 1\n")

	state := collectLiveWorktreeState()
	if !state.scanned {
		t.Fatal("lsof exits 1 on partial success; a usable answer must not be discarded on exit code alone")
	}
	want := normalizeLiveCWDs([]string{dir})
	if len(state.cwds) != 1 || state.cwds[0] != want[0] {
		t.Fatalf("cwds = %v, want %v", state.cwds, want)
	}
}

// --- parser ---

func TestParseLsofCWDs_OnlyTakesCWDDescriptors(t *testing.T) {
	out := strings.Join([]string{
		"p100",
		"fcwd",
		"n/tmp/alpha",
		"p200",
		"ftxt", // NOT a cwd: an open file. Must be ignored.
		"n/tmp/should-be-ignored",
		"p300",
		"fcwd",
		"n/tmp/beta",
	}, "\n")

	got := parseLsofCWDs([]byte(out))
	if len(got) != 2 || got[0] != "/tmp/alpha" || got[1] != "/tmp/beta" {
		t.Fatalf("parsed %v; a non-cwd descriptor's path leaked in or a cwd was dropped", got)
	}
}

func TestParseLsofCWDs_DescriptorStateResetsPerProcess(t *testing.T) {
	// p200 has no f line at all. Without a reset at the p record it would
	// inherit p100's cwd state and wrongly contribute its name.
	out := strings.Join([]string{
		"p100", "fcwd", "n/tmp/alpha",
		"p200", "n/tmp/leaked",
	}, "\n")

	got := parseLsofCWDs([]byte(out))
	if len(got) != 1 || got[0] != "/tmp/alpha" {
		t.Fatalf("parsed %v; descriptor state leaked across process records", got)
	}
}

func TestParseLsofCWDs_EmptyInput(t *testing.T) {
	if got := parseLsofCWDs(nil); len(got) != 0 {
		t.Fatalf("expected no paths, got %v", got)
	}
}

// --- shared normalization ---

func TestNormalizeLiveCWDs_DedupesAndDropsDeleted(t *testing.T) {
	dir := t.TempDir()
	got := normalizeLiveCWDs([]string{
		dir,
		dir, // duplicate
		"",  // empty
		"/nonexistent/path (deleted)",
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 normalized cwd, got %v", got)
	}
	for _, g := range got {
		if strings.HasSuffix(g, " (deleted)") {
			t.Fatalf("unlinked cwd marker survived normalization: %q", g)
		}
	}
}

func writeStub(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stub-lsof")
	if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return p
}
