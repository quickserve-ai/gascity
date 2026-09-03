package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// ga-gsfxag. `git stash list` is repo-global (refs/stash lives in the common
// repository), so the old stash veto in the git-safety gate let one stash
// anywhere in a repo protect EVERY worktree of that repo, permanently — while
// guarding against a loss that cannot happen, because `git worktree remove`
// never touches refs/stash. This test requires TWO worktrees of ONE repo so it
// can distinguish repo-global from per-worktree scope: a single-worktree test
// would pass under both behaviours.
func TestReapClosedBeadWorktrees_RepoStashDoesNotProtectCleanWorktree(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	cleanWT := addClosedWorktree(t, rigRoot, cityPath, "polecats", "ga-clean001")
	dirtyWT := addClosedWorktree(t, rigRoot, cityPath, "polecats", "ga-dirty002")

	// Leave a stash in the shared repository. Made in the rig root, but
	// resolved through the common dir, it is listable from every worktree.
	if err := os.WriteFile(filepath.Join(rigRoot, "README.md"), []byte("stashed change\n"), 0o644); err != nil {
		t.Fatalf("dirty the rig root: %v", err)
	}
	mustGit(t, rigRoot, "stash", "push", "-m", "repo-global stash")

	// Assert the scope premise the old veto keyed on: the stash is visible
	// FROM the clean linked worktree (resolved through the common dir), so a
	// repo-global veto would have protected it.
	preCmd := exec.Command("git", "stash", "list")
	preCmd.Dir = cleanWT
	if out, err := preCmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "repo-global stash") {
		t.Fatalf("premise broken: stash not visible from the clean linked worktree (err=%v):\n%s", err, out)
	}

	// Dirty exactly one worktree; the other stays clean and fully pushed.
	if err := os.WriteFile(filepath.Join(dirtyWT, "scratch.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatalf("dirty the worktree: %v", err)
	}

	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "ga-clean001", Status: "closed"},
		{ID: "ga-dirty002", Status: "closed"},
	}, nil)
	cfg := reapTestConfig(rigRoot)
	injectLiveness(t, liveWorktreeState{scanned: true}) // scanned, nothing live

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-clean001" {
		t.Fatalf("Reaped = %+v, want exactly the clean ga-clean001 despite the repo stash\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if _, err := os.Stat(cleanWT); !os.IsNotExist(err) {
		t.Fatalf("clean worktree %s still present after reap (stat err=%v)", cleanWT, err)
	}
	if len(report.Protected) != 1 || report.Protected[0].BeadID != "ga-dirty002" || !strings.Contains(report.Protected[0].Reason, "uncommitted=true") {
		t.Fatalf("Protected = %+v, want exactly the dirty ga-dirty002 via its own uncommitted state", report.Protected)
	}
	if _, err := os.Stat(dirtyWT); err != nil {
		t.Fatalf("dirty worktree %s was removed or unstattable: %v", dirtyWT, err)
	}

	// The premise the old veto claimed to defend: the reap cannot lose the
	// stash, because it lives in the repository, not the removed worktree.
	stashCmd := exec.Command("git", "stash", "list")
	stashCmd.Dir = rigRoot
	out, err := stashCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git stash list after reap: %s: %v", out, err)
	}
	if !strings.Contains(string(out), "repo-global stash") {
		t.Fatalf("stash missing after reap; `git worktree remove` must never touch refs/stash. stash list:\n%s", out)
	}
}
