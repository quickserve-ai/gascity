package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
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

// emptyCommitKeepingMtimes runs `git commit --allow-empty` in a clean tree and
// then puts the root's, HEAD's and the index's mtimes back to age ago, as git
// may leave them: a commit that changes no file needs no index rewrite, and a
// symbolic HEAD is not rewritten. Only the reflog records the commit.
func emptyCommitKeepingMtimes(t *testing.T, worktreePath string, age time.Duration) {
	t.Helper()
	mustGit(t, worktreePath, "commit", "--allow-empty", "-q", "-m", "heartbeat")
	gitDir := strings.TrimSpace(runGit(t, worktreePath, "rev-parse", "--absolute-git-dir"))
	old := time.Now().Add(-age)
	for _, p := range []string{worktreePath, filepath.Join(gitDir, "HEAD"), filepath.Join(gitDir, "index")} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("age %s: %v", p, err)
		}
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

// TestReapClosedBeadWorktrees_ProtectsEmptyCommitThatLeavesIndexUntouched: a
// commit is recorded in the HEAD reflog even when it rewrites neither HEAD
// nor the index, so the gate reads the newest reflog entry's own time.
func TestReapClosedBeadWorktrees_ProtectsEmptyCommitThatLeavesIndexUntouched(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet03")
	emptyCommitKeepingMtimes(t, wt, 24*time.Hour)
	requireProtected(t, runQuietReap(t, cityPath, rigRoot, "ga-quiet03", nil, nil), wt, "used within the quiet period")
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
// gitdir, a missing index, a missing HEAD reflog, or an entry that does not
// parse.
func TestReapClosedBeadWorktrees_ProtectsWhenLastActivityIndeterminate(t *testing.T) {
	for name, breakIt := range map[string]func(wt, gitDir string) error{
		"pointer": func(wt, _ string) error {
			return os.WriteFile(filepath.Join(wt, ".git"), []byte("not a gitdir pointer\n"), 0o644)
		},
		"index":  func(_, gitDir string) error { return os.Remove(filepath.Join(gitDir, "index")) },
		"reflog": func(_, gitDir string) error { return os.Remove(filepath.Join(gitDir, "logs", "HEAD")) },
		"entry": func(_, gitDir string) error {
			return os.WriteFile(filepath.Join(gitDir, "logs", "HEAD"), []byte("garbage\n"), 0o644)
		},
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

// TestReapClosedBeadWorktrees_ProtectsFutureActivityTimestamp: a reflog entry
// stamped in the future (clock skew) reads as use now, never as idle.
func TestReapClosedBeadWorktrees_ProtectsFutureActivityTimestamp(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet07")
	gitDir := strings.TrimSpace(runGit(t, wt, "rev-parse", "--absolute-git-dir"))
	backdateReflogEntries(t, filepath.Join(gitDir, "logs", "HEAD"), time.Now().Add(time.Hour))
	requireProtected(t, runQuietReap(t, cityPath, rigRoot, "ga-quiet07", nil, nil), wt, "last_activity_age=-")
}

// reapActivityDuringListStore runs a hook inside the borrow-veto List, the
// pass's blocking store query, as a seat working in the tree meanwhile would.
type reapActivityDuringListStore struct {
	beads.Store
	during func()
}

func (s *reapActivityDuringListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.during()
	return s.Store.List(q)
}

// TestReapClosedBeadWorktrees_RechecksActivityBeforeRemoval: the tree is quiet
// when the pass classifies it, then a seat commits in it while the borrow-veto
// query blocks. The gate runs again immediately before removal, on the path
// with no pre-removal re-verification too, and protects it.
func TestReapClosedBeadWorktrees_RechecksActivityBeforeRemoval(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet08")
	store := &reapActivityDuringListStore{
		Store:  beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-quiet08", Status: "closed"}}, nil),
		during: func() { emptyCommitKeepingMtimes(t, wt, 24*time.Hour) },
	}
	requireProtected(t, runQuietReap(t, cityPath, rigRoot, "ga-quiet08", nil, store), wt, "pre-removal re-check: worktree used within the quiet period")
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
// root, and that the newest signal wins: here the reflog entry's own time,
// newer than every mtime.
func TestWorktreeLastActivity_RelativeGitdirAndNewestSignal(t *testing.T) {
	base := t.TempDir()
	wt := filepath.Join(base, "wt")
	gitDir := filepath.Join(base, "repo", ".git", "worktrees", "wt")
	if err := os.MkdirAll(filepath.Join(gitDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	files := map[string]string{
		filepath.Join(wt, ".git"):             "gitdir: ../repo/.git/worktrees/wt\n",
		filepath.Join(gitDir, "HEAD"):         "ref: refs/heads/x\n",
		filepath.Join(gitDir, "index"):        "",
		filepath.Join(gitDir, "logs", "HEAD"): "0000 1111 A U Thor <a@example.com> 1 +0000\tclone\n1111 2222 A U Thor <a@example.com> " + strconv.FormatInt(entry.Unix(), 10) + " -0700\tcommit: x\n",
	}
	old := time.Now().Add(-48 * time.Hour)
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(wt, old, old); err != nil {
		t.Fatal(err)
	}

	got, ok := worktreeLastActivity(wt)
	if !ok || !got.Equal(entry) {
		t.Fatalf("worktreeLastActivity = %v, %v; want %v (the newest reflog entry)", got, ok, entry)
	}
}
