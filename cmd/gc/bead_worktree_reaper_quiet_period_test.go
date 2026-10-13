package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

// runQuietReap runs one reap pass over a single closed-bead worktree with the
// given quiet-period minutes (nil = default) and no live process.
func runQuietReap(t *testing.T, cityPath, rigRoot, beadID string, minutes *int, store beads.Store) reapReport {
	t.Helper()
	if store == nil {
		store = beads.NewMemStoreFrom(1, []beads.Bead{{ID: beadID, Status: "closed"}}, nil)
	}
	cfg := reapTestConfig(rigRoot)
	cfg.Daemon.AutoReapClosedBeadWorktreesQuietPeriodMinutes = minutes
	injectLiveness(t, liveWorktreeState{scanned: true})
	var stderr bytes.Buffer
	return reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)
}

func requireProtected(t *testing.T, report reapReport, wt, wantReason string) {
	t.Helper()
	if len(report.Reaped) != 0 || len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, wantReason) {
		t.Fatalf("Reaped = %+v Protected = %+v, want one protection naming %q", report.Reaped, report.Protected, wantReason)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("protected worktree %s was removed or unstattable: %v", wt, err)
	}
}

// TestReapClosedBeadWorktrees_ProtectsRecentlyUsedWorktreeUnderDefaultQuietPeriod
// reproduces pl-4fj (brett-qc-nmoxbup, 2026-09-14): a named seat's per-bead
// worktree — closed bead, clean and pushed, created days ago, no live process
// cwd inside it because the seat's resident process sits in its home — was
// reaped six minutes after the seat's last commit in it. A git index touched
// minutes ago is recent use; the default quiet period protects the tree and
// the reason names the period and the activity age.
func TestReapClosedBeadWorktrees_ProtectsRecentlyUsedWorktreeUnderDefaultQuietPeriod(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet01")
	backdateWorktreeActivity(t, wt, 72*time.Hour)
	touchWorktreeIndex(t, wt)
	report := runQuietReap(t, cityPath, rigRoot, "ga-quiet01", nil, nil)
	requireProtected(t, report, wt, "quiet_period=6h0m0s, last_activity_age=")
}

// TestReapClosedBeadWorktrees_ReapsWorktreeQuietLongerThanDefaultQuietPeriod is
// the control: every activity signal older than the default quiet period, and
// a logs/HEAD file just rewritten, as `git gc` rewrites every worktree's
// reflog at once, keeping the entries' own times. The tree is reaped.
func TestReapClosedBeadWorktrees_ReapsWorktreeQuietLongerThanDefaultQuietPeriod(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet02")
	backdateWorktreeActivity(t, wt, 72*time.Hour)
	mustGit(t, rigRoot, "reflog", "expire", "--expire=never", "--all") // as git gc does
	report := runQuietReap(t, cityPath, rigRoot, "ga-quiet02", nil, nil)
	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-quiet02" {
		t.Fatalf("Reaped = %+v Protected = %+v, want exactly ga-quiet02", report.Reaped, report.Protected)
	}
}

// TestReapClosedBeadWorktrees_QuietPeriodConfig proves the gate reads the
// configured period, that only an explicit zero turns it off, and that a
// negative or overflowing value protects and is named rather than disabling
// the gate.
func TestReapClosedBeadWorktrees_QuietPeriodConfig(t *testing.T) {
	for _, tc := range []struct {
		minutes    int
		lastUse    time.Duration
		wantReason string // "" = reaped
	}{
		{minutes: 0, lastUse: 15 * time.Minute},
		{minutes: 60, lastUse: 2 * time.Hour},
		{minutes: 180, lastUse: 2 * time.Hour, wantReason: "quiet_period=3h0m0s"},
		{minutes: -1, lastUse: 15 * time.Minute, wantReason: "quiet period misconfigured"},
		{minutes: 153722868, lastUse: 15 * time.Minute, wantReason: "quiet period misconfigured"},
	} {
		cityPath, rigRoot := initReapRig(t)
		wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet04")
		backdateWorktreeActivity(t, wt, tc.lastUse)
		minutes := tc.minutes
		report := runQuietReap(t, cityPath, rigRoot, "ga-quiet04", &minutes, nil)
		if tc.wantReason == "" {
			if len(report.Reaped) != 1 {
				t.Errorf("quiet_period=%dm, last use %s ago: Protected = %+v, want reaped", tc.minutes, tc.lastUse, report.Protected)
			}
			continue
		}
		requireProtected(t, report, wt, tc.wantReason)
	}
	if _, err := (&config.DaemonConfig{}).AutoReapClosedBeadWorktreesQuietPeriod(); err != nil {
		t.Errorf("default quiet period: %v", err)
	}
}

// TestReapClosedBeadWorktrees_ProtectsWhenLastActivityIndeterminate proves the
// gate fails closed when a signal cannot be read: a ".git" pointer naming no
// gitdir, or a missing index.
func TestReapClosedBeadWorktrees_ProtectsWhenLastActivityIndeterminate(t *testing.T) {
	for name, breakIt := range map[string]func(wt, gitDir string) error{
		"pointer": func(wt, _ string) error {
			return os.WriteFile(filepath.Join(wt, ".git"), []byte("not a gitdir pointer\n"), 0o644)
		},
		"index": func(_, gitDir string) error { return os.Remove(filepath.Join(gitDir, "index")) },
	} {
		cityPath, rigRoot := initReapRig(t)
		wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet05")
		gitDir := strings.TrimSpace(runGit(t, wt, "rev-parse", "--absolute-git-dir"))
		if err := breakIt(wt, gitDir); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		backdateWorktreeGitFile(t, wt, 24*time.Hour) // creation age stays determinate
		t.Run(name, func(t *testing.T) {
			requireProtected(t, runQuietReap(t, cityPath, rigRoot, "ga-quiet05", nil, nil), wt, "last activity indeterminate")
		})
	}
}

// TestReapSkipTracker_SuppressesQuietPeriodRepeats pins that the quiet-period
// protection is announced once across passes, although the reason's activity
// age changes every pass.
func TestReapSkipTracker_SuppressesQuietPeriodRepeats(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet06")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-quiet06", Status: "closed"}}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true})

	fake := events.NewFake()
	skips := newReapSkipTracker()
	var stderr bytes.Buffer
	touchWorktreeIndex(t, wt)
	for pass := 1; pass <= 3; pass++ {
		time.Sleep(1100 * time.Millisecond) // the activity age differs each pass
		reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, fake, skips, &stderr)
	}

	reasons := skipReasonsFor(t, fake, wt)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "last_activity_age=") {
		t.Fatalf("reap_skipped reasons = %q, want exactly one quiet-period event across 3 passes", reasons)
	}
}

// TestWorktreeLastActivity_RelativeGitdirAndNewestSignal covers a relative
// gitdir pointer (worktree.useRelativePaths), resolved against the worktree
// root, and that the newest signal wins.
func TestWorktreeLastActivity_RelativeGitdirAndNewestSignal(t *testing.T) {
	base := t.TempDir()
	wt := filepath.Join(base, "wt")
	gitDir := filepath.Join(base, "repo", ".git", "worktrees", "wt")
	for _, d := range []string{wt, gitDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	headTime := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	for path, body := range map[string]string{
		filepath.Join(wt, ".git"):      "gitdir: ../repo/.git/worktrees/wt\n",
		filepath.Join(gitDir, "HEAD"):  "ref: refs/heads/x\n",
		filepath.Join(gitDir, "index"): "",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for path, at := range map[string]time.Time{wt: old, filepath.Join(wt, ".git"): old, filepath.Join(gitDir, "index"): old, filepath.Join(gitDir, "HEAD"): headTime} {
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}

	got, ok := worktreeLastActivity(wt)
	if !ok || !got.Equal(headTime) {
		t.Fatalf("worktreeLastActivity = %v, %v; want %v (HEAD, the newest signal)", got, ok, headTime)
	}
}
