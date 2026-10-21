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

// emptyCommitKeepingMtimes runs `git commit --allow-empty` in a clean tree with
// a committer date of 2020, so the reflog entry it appends records a time long
// past, and then puts the root's, HEAD's and the index's mtimes back to age
// ago, as git may leave them: a commit that changes no file needs no index
// rewrite, and a symbolic HEAD is not rewritten. Only logs/HEAD's mtime moves.
func emptyCommitKeepingMtimes(t *testing.T, worktreePath string, age time.Duration) {
	t.Helper()
	t.Setenv("GIT_COMMITTER_DATE", "2020-01-01T00:00:00Z")
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
// commit appends to the HEAD reflog even when it rewrites neither HEAD nor the
// index, and the gate reads that file's mtime, not the date the entry records
// (backdated here, as GIT_COMMITTER_DATE allows).
func TestReapClosedBeadWorktrees_ProtectsEmptyCommitThatLeavesIndexUntouched(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet03")
	emptyCommitKeepingMtimes(t, wt, 24*time.Hour)
	requireProtected(t, runQuietReap(t, cityPath, rigRoot, "ga-quiet03", nil, nil), wt, "used within the quiet period")
}

// TestReapClosedBeadWorktrees_ReapsWorktreeQuietLongerThanDefaultQuietPeriod is
// the control: a tree whose every activity signal is older than the default
// quiet period is reaped.
func TestReapClosedBeadWorktrees_ReapsWorktreeQuietLongerThanDefaultQuietPeriod(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet02")
	report := runQuietReap(t, cityPath, rigRoot, "ga-quiet02", nil, nil)
	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-quiet02" {
		t.Fatalf("Reaped = %+v Protected = %+v, want exactly %s reaped", report.Reaped, report.Protected, wt)
	}
}

// TestReapClosedBeadWorktrees_ProtectsFreshReflogWhateverItsContent: a
// logs/HEAD written within the quiet period protects although every other
// signal is old, whether it is emptied, holds garbage, or was rewritten by
// `git reflog expire` as `git gc` does (the accepted cost: one more quiet
// period before that tree is reaped). Its content is never read.
func TestReapClosedBeadWorktrees_ProtectsFreshReflogWhateverItsContent(t *testing.T) {
	writeReflog := func(t *testing.T, reflog string, content []byte) {
		t.Helper()
		if err := os.WriteFile(reflog, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, write := range map[string]func(t *testing.T, rigRoot, reflog string){
		"empty": func(t *testing.T, _, reflog string) { writeReflog(t, reflog, nil) },
		"garbage": func(t *testing.T, _, reflog string) {
			writeReflog(t, reflog, []byte("garbage garbage garbage 1 garbage\n"))
		},
		"gc rewrite": func(t *testing.T, rigRoot, _ string) {
			mustGit(t, rigRoot, "reflog", "expire", "--expire=never", "--all")
		},
	} {
		t.Run(name, func(t *testing.T) {
			cityPath, rigRoot := initReapRig(t)
			wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet09")
			gitDir := strings.TrimSpace(runGit(t, wt, "rev-parse", "--absolute-git-dir"))
			write(t, rigRoot, filepath.Join(gitDir, "logs", "HEAD"))
			requireProtected(t, runQuietReap(t, cityPath, rigRoot, "ga-quiet09", nil, nil), wt, "used within the quiet period")
		})
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
// gitdir, a missing index, or a logs/HEAD that is missing or fails to stat (an
// unreadable or non-directory logs). A missing logs/HEAD cannot tell a HEAD
// that never moved from a reflog that held recent use and was then removed,
// so it protects like any other unreadable signal.
func TestReapClosedBeadWorktrees_ProtectsWhenLastActivityIndeterminate(t *testing.T) {
	for name, breakIt := range map[string]func(t *testing.T, wt, gitDir string) error{
		"pointer": func(_ *testing.T, wt, _ string) error {
			return os.WriteFile(filepath.Join(wt, ".git"), []byte("not a gitdir pointer\n"), 0o644)
		},
		"index": func(_ *testing.T, _, gitDir string) error { return os.Remove(filepath.Join(gitDir, "index")) },
		"logs/HEAD missing": func(_ *testing.T, _, gitDir string) error {
			return os.Remove(filepath.Join(gitDir, "logs", "HEAD"))
		},
		"logs unreadable": func(t *testing.T, _, gitDir string) error {
			if os.Geteuid() == 0 {
				t.Skip("root stats through a mode-000 directory")
			}
			logs := filepath.Join(gitDir, "logs")
			t.Cleanup(func() { _ = os.Chmod(logs, 0o755) })
			return os.Chmod(logs, 0)
		},
		"logs not a directory": func(_ *testing.T, _, gitDir string) error {
			logs := filepath.Join(gitDir, "logs")
			if err := os.RemoveAll(logs); err != nil {
				return err
			}
			return os.WriteFile(logs, nil, 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			cityPath, rigRoot := initReapRig(t)
			wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet05")
			gitDir := strings.TrimSpace(runGit(t, wt, "rev-parse", "--absolute-git-dir"))
			if err := breakIt(t, wt, gitDir); err != nil {
				t.Fatal(err)
			}
			backdateWorktreeGitFile(t, wt, 24*time.Hour) // creation age stays determinate
			requireProtected(t, runQuietReap(t, cityPath, rigRoot, "ga-quiet05", nil, nil), wt, "last activity indeterminate")
		})
	}
}

// TestReapClosedBeadWorktrees_ProtectsFutureActivityTimestamp: an activity
// mtime in the future (clock skew) reads as use now, never as idle.
func TestReapClosedBeadWorktrees_ProtectsFutureActivityTimestamp(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet07")
	gitDir := strings.TrimSpace(runGit(t, wt, "rev-parse", "--absolute-git-dir"))
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(gitDir, "logs", "HEAD"), future, future); err != nil {
		t.Fatal(err)
	}
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

// TestReapClosedBeadWorktrees_DryRunRechecksActivityLikeRemoval: the
// activity-during-List case under dry-run. Dry-run runs the same final quiet
// check a real removal runs, so it reports the tree Protected with the same
// reason a real pass would, never "would reap".
func TestReapClosedBeadWorktrees_DryRunRechecksActivityLikeRemoval(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addClosedWorktree(t, rigRoot, cityPath, "seat", "ga-quiet10")
	store := &reapActivityDuringListStore{
		Store:  beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-quiet10", Status: "closed"}}, nil),
		during: func() { emptyCommitKeepingMtimes(t, wt, 24*time.Hour) },
	}
	injectLiveness(t, liveWorktreeState{scanned: true})
	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, true, events.Discard, nil, &stderr)
	requireProtected(t, report, wt, "pre-removal re-check: worktree used within the quiet period")
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
	// Every other activity signal is old, so the index alone sets the age; each
	// pass stamps it a different number of minutes ago (all inside the 6h quiet
	// period), so the reason's age differs per pass without a wall-clock wait.
	backdateWorktreeActivity(t, wt, 72*time.Hour)
	gitDir := strings.TrimSpace(runGit(t, wt, "rev-parse", "--absolute-git-dir"))
	for pass := 1; pass <= 3; pass++ {
		at := time.Now().Add(-time.Duration(pass) * time.Minute)
		if err := os.Chtimes(filepath.Join(gitDir, "index"), at, at); err != nil {
			t.Fatalf("stamp index for pass %d: %v", pass, err)
		}
		reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, fake, skips, &stderr)
	}

	reasons := skipReasonsFor(t, fake, wt)
	if len(reasons) != 1 || !strings.Contains(reasons[0], "last_activity_age=") {
		t.Fatalf("reap_skipped reasons = %q, want exactly one quiet-period event across 3 passes", reasons)
	}
}

// TestWorktreeLastActivity_RelativeGitdirAndNewestSignal covers a relative
// gitdir pointer (worktree.useRelativePaths), resolved against the worktree
// root, and that the newest signal wins: here logs/HEAD's mtime, newer than
// every other, whatever its entries say.
func TestWorktreeLastActivity_RelativeGitdirAndNewestSignal(t *testing.T) {
	base := t.TempDir()
	wt := filepath.Join(base, "wt")
	gitDir := filepath.Join(base, "repo", ".git", "worktrees", "wt")
	for _, d := range []string{wt, filepath.Join(gitDir, "logs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	reflogTime := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	for path, body := range map[string]string{
		filepath.Join(wt, ".git"):             "gitdir: ../repo/.git/worktrees/wt\n",
		filepath.Join(gitDir, "HEAD"):         "ref: refs/heads/x\n",
		filepath.Join(gitDir, "index"):        "",
		filepath.Join(gitDir, "logs", "HEAD"): "0000 1111 A U Thor <a@example.com> 1 +0000\tcommit: x\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for path, at := range map[string]time.Time{wt: old, filepath.Join(wt, ".git"): old, filepath.Join(gitDir, "HEAD"): old, filepath.Join(gitDir, "index"): old, filepath.Join(gitDir, "logs", "HEAD"): reflogTime} {
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}

	got, ok := worktreeLastActivity(wt)
	if !ok || !got.Equal(reflogTime) {
		t.Fatalf("worktreeLastActivity = %v, %v; want %v (logs/HEAD's mtime, the newest signal)", got, ok, reflogTime)
	}
}
