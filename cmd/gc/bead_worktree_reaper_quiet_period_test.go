package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// touchWorktreeIndex stamps a worktree's private git index with the current
// time, as a commit or `git add` from a short-lived shell call would.
func touchWorktreeIndex(t *testing.T, worktreePath string) {
	t.Helper()
	gitDir := strings.TrimSpace(runGit(t, worktreePath, "rev-parse", "--absolute-git-dir"))
	now := time.Now()
	if err := os.Chtimes(filepath.Join(gitDir, "index"), now, now); err != nil {
		t.Fatalf("touch index: %v", err)
	}
}

// TestReapClosedBeadWorktrees_ProtectsRecentlyUsedWorktreeUnderDefaultQuietPeriod
// reproduces pl-4fj (brett-qc-nmoxbup, 2026-09-14): a named seat's per-bead
// worktree — closed bead, clean and pushed, created days ago, no live process
// cwd inside it because the seat's resident process sits in its home — was
// reaped six minutes after the seat's last commit in it. A git index touched
// minutes ago is recent use; the default quiet period must protect the tree
// and say why.
func TestReapClosedBeadWorktrees_ProtectsRecentlyUsedWorktreeUnderDefaultQuietPeriod(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet01")
	backdateWorktreeActivity(t, wt, 72*time.Hour)
	touchWorktreeIndex(t, wt)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-quiet01", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot) // quiet period unset -> default
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 for a worktree used minutes ago\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if len(report.Protected) != 1 || report.Protected[0].BeadID != "ga-quiet01" {
		t.Fatalf("Protected = %+v, want exactly ga-quiet01", report.Protected)
	}
	if reason := report.Protected[0].Reason; !strings.Contains(reason, "quiet period") || !strings.Contains(reason, "quiet_period=6h0m0s") {
		t.Errorf("Reason = %q, want it to name the quiet period and its configured length", reason)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("recently used worktree %s was removed or unstattable: %v", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ReapsWorktreeQuietLongerThanDefaultQuietPeriod is
// the control: the same tree with every activity signal older than the
// default quiet period is reaped.
func TestReapClosedBeadWorktrees_ReapsWorktreeQuietLongerThanDefaultQuietPeriod(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet02")
	backdateWorktreeActivity(t, wt, 72*time.Hour)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-quiet02", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-quiet02" {
		t.Fatalf("Reaped = %+v, want exactly ga-quiet02\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after reap (stat err=%v)", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ZeroQuietPeriodDisablesGate proves an explicit
// zero turns the quiet-period gate off: a tree used minutes ago is reaped when
// every other gate passes.
func TestReapClosedBeadWorktrees_ZeroQuietPeriodDisablesGate(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet03")
	touchWorktreeIndex(t, wt)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-quiet03", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	zero := 0
	cfg.Daemon.AutoReapClosedBeadWorktreesQuietPeriodMinutes = &zero
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-quiet03" {
		t.Fatalf("Reaped = %+v, want exactly ga-quiet03 with the quiet period disabled\nstderr:\n%s", report.Reaped, stderr.String())
	}
}

// TestReapClosedBeadWorktrees_QuietPeriodHonorsConfiguredValue proves the gate
// reads the configured period: activity 2h ago is quiet under a 1h period and
// recent under a 3h one.
func TestReapClosedBeadWorktrees_QuietPeriodHonorsConfiguredValue(t *testing.T) {
	for _, tc := range []struct {
		minutes  int
		wantReap bool
	}{
		{minutes: 60, wantReap: true},
		{minutes: 180, wantReap: false},
	} {
		cityPath, rigRoot := initReapRig(t)
		wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet04")
		backdateWorktreeActivity(t, wt, 2*time.Hour)
		store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-quiet04", Status: "closed"}}, nil)
		cfg := reapTestConfig(rigRoot)
		minutes := tc.minutes
		cfg.Daemon.AutoReapClosedBeadWorktreesQuietPeriodMinutes = &minutes
		injectLiveness(t, liveWorktreeState{scanned: true})

		var stderr bytes.Buffer
		report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

		if gotReap := len(report.Reaped) == 1; gotReap != tc.wantReap {
			t.Errorf("quiet_period=%dm, activity 2h ago: Reaped = %+v Protected = %+v, want reaped=%v", tc.minutes, report.Reaped, report.Protected, tc.wantReap)
		}
	}
}

// TestReapClosedBeadWorktrees_ProtectsWhenLastActivityIndeterminate proves the
// gate fails closed: a ".git" pointer that names no gitdir leaves no last-use
// signal to read, and the tree is protected rather than treated as idle.
func TestReapClosedBeadWorktrees_ProtectsWhenLastActivityIndeterminate(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet05")
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("not a gitdir pointer\n"), 0o644); err != nil {
		t.Fatalf("corrupt .git pointer: %v", err)
	}
	backdateWorktreeGitFile(t, wt, 24*time.Hour) // creation age stays determinate
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-quiet05", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 when last activity is indeterminate", report.Reaped)
	}
	if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, "last activity indeterminate") {
		t.Fatalf("Protected = %+v, want one entry naming the indeterminate last activity", report.Protected)
	}
}

// TestReapSkipTracker_SuppressesQuietPeriodRepeats pins that the quiet-period
// reason is stable across passes, including while the tree keeps being used,
// so the skip tracker announces it once rather than on every tick.
func TestReapSkipTracker_SuppressesQuietPeriodRepeats(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet06")
	touchWorktreeIndex(t, wt)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-quiet06", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	fake := events.NewFake()
	skips := newReapSkipTracker()
	var stderr bytes.Buffer
	for pass := 1; pass <= 3; pass++ {
		reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, fake, skips, &stderr)
		touchWorktreeIndex(t, wt) // the seat keeps working in the tree between passes
	}

	reasons := skipReasonsFor(t, fake, wt)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "quiet period") {
		t.Fatalf("reap_skipped reasons = %q, want exactly one quiet-period event across 3 passes", reasons)
	}
}

// TestWorktreeLastActivity_RelativeGitdirAndMissingOptionalSignals covers the
// pointer forms git writes: a relative gitdir (worktree.useRelativePaths)
// resolves against the worktree root, and an absent index or logs/HEAD is
// skipped rather than making the reading indeterminate. The newest signal wins.
func TestWorktreeLastActivity_RelativeGitdirAndMissingOptionalSignals(t *testing.T) {
	base := t.TempDir()
	wt := filepath.Join(base, "wt")
	gitDir := filepath.Join(base, "repo", ".git", "worktrees", "wt")
	for _, d := range []string{wt, gitDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: ../repo/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	headTime := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(wt, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(gitDir, "HEAD"), headTime, headTime); err != nil {
		t.Fatal(err)
	}

	got, ok := worktreeLastActivity(wt)
	if !ok {
		t.Fatalf("worktreeLastActivity: indeterminate, want HEAD's mtime")
	}
	if !got.Equal(headTime) {
		t.Errorf("worktreeLastActivity = %v, want %v (HEAD, the newest present signal)", got, headTime)
	}

	if err := os.Remove(filepath.Join(gitDir, "HEAD")); err != nil {
		t.Fatal(err)
	}
	if _, ok := worktreeLastActivity(wt); ok {
		t.Errorf("worktreeLastActivity with no HEAD: determinate, want indeterminate")
	}
}
