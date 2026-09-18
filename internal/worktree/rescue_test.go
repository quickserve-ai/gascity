package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// linkedWorktree creates a repository with one tracked file and a detached
// linked worktree at its HEAD, the shape formula teardown operates on.
func linkedWorktree(t *testing.T) (repo, wt, head string) {
	t.Helper()
	repo, _ = initTestRepo(t)
	writeFile(t, filepath.Join(repo, "a.txt"), "base\n")
	runGit(t, repo, "add", "a.txt")
	runGit(t, repo, "commit", "-m", "a")
	wt = filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", "--detach", wt, "HEAD")
	return repo, wt, runGit(t, wt, "rev-parse", "HEAD")
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rescueSpec(wt string) RescueSpec {
	return RescueSpec{Path: wt, BeadID: "qc-test.1", SkillSinks: []string{".claude/skills", ".omp/skills"}}
}

func rescueRefs(t *testing.T, repo string) []string {
	t.Helper()
	out := runGit(t, repo, "for-each-ref", "--format=%(refname) %(objectname)", "refs/rescue/")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func treePaths(t *testing.T, dir, commit string) []string {
	t.Helper()
	return strings.Split(runGit(t, dir, "ls-tree", "-r", "--name-only", commit), "\n")
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestRescueSecuresWIPWithoutTouchingTheWorktree(t *testing.T) {
	repo, wt, head := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, "a.txt"), "edited\n")
	writeFile(t, filepath.Join(wt, "staged.txt"), "staged\n")
	runGit(t, wt, "add", "staged.txt")
	writeFile(t, filepath.Join(wt, "untracked.txt"), "untracked\n")
	statusBefore := runGit(t, wt, "status", "--porcelain")

	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if !rep.WIP || rep.RescueSHA == head || rep.RescueRef != "refs/rescue/qc-test.1" {
		t.Fatalf("report = %+v, want a WIP snapshot under refs/rescue/qc-test.1", rep)
	}
	if got := runGit(t, repo, "rev-parse", rep.RescueSHA+"^"); got != head {
		t.Errorf("rescue parent = %s, want HEAD %s", got, head)
	}
	if got := runGit(t, repo, "show", rep.RescueSHA+":a.txt"); got != "edited" {
		t.Errorf("rescued a.txt = %q, want the edited content", got)
	}
	paths := treePaths(t, repo, rep.RescueSHA)
	for _, want := range []string{"staged.txt", "untracked.txt"} {
		if !containsString(paths, want) {
			t.Errorf("rescue tree %v is missing %s", paths, want)
		}
	}
	if got := runGit(t, wt, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s", got)
	}
	if got := runGit(t, wt, "status", "--porcelain"); got != statusBefore {
		t.Errorf("rescue changed the index or tree:\nbefore:\n%s\nafter:\n%s", statusBefore, got)
	}
	if got := runGit(t, repo, "for-each-ref", "refs/heads/"); strings.Contains(got, rep.RescueSHA) {
		t.Errorf("rescue moved a branch: %s", got)
	}
}

func TestRescueOfACleanTreeIsHead(t *testing.T) {
	repo, wt, head := linkedWorktree(t)
	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if rep.WIP || rep.RescueSHA != head {
		t.Fatalf("report = %+v, want HEAD %s with no WIP", rep, head)
	}
	if got := rescueRefs(t, repo); len(got) != 1 || got[0] != "refs/rescue/qc-test.1 "+head {
		t.Fatalf("rescue refs = %v", got)
	}
}

func TestRescueIsIdempotent(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, "wip.txt"), "wip\n")
	first, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("first Rescue: %v", err)
	}
	second, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("second Rescue: %v", err)
	}
	if first.RescueSHA != second.RescueSHA || first.RescueRef != second.RescueRef {
		t.Fatalf("retry minted a new rescue: %+v then %+v", first, second)
	}
	if got := rescueRefs(t, repo); len(got) != 1 {
		t.Fatalf("rescue refs after retry = %v, want exactly one", got)
	}
}

func TestRescueNeverOverwritesADivergentRescue(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	runGit(t, repo, "commit", "--allow-empty", "-m", "unrelated")
	unrelated := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "update-ref", "refs/rescue/qc-test.1", unrelated)
	writeFile(t, filepath.Join(wt, "wip.txt"), "wip\n")

	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if got := runGit(t, repo, "rev-parse", "refs/rescue/qc-test.1"); got != unrelated {
		t.Fatalf("existing rescue was overwritten: now %s, was %s", got, unrelated)
	}
	if want := "refs/rescue/qc-test.1-" + rep.RescueSHA[:12]; rep.RescueRef != want {
		t.Fatalf("rescue ref = %s, want the sibling %s", rep.RescueRef, want)
	}
	if got := runGit(t, repo, "rev-parse", rep.RescueRef); got != rep.RescueSHA {
		t.Fatalf("%s = %s, want %s", rep.RescueRef, got, rep.RescueSHA)
	}
}

func TestRescueAdvancesItsOwnEarlierRescue(t *testing.T) {
	repo, wt, head := linkedWorktree(t)
	if _, err := Rescue(rescueSpec(wt)); err != nil {
		t.Fatalf("first Rescue: %v", err)
	}
	writeFile(t, filepath.Join(wt, "more.txt"), "more\n")
	runGit(t, wt, "add", "more.txt")
	runGit(t, wt, "commit", "-m", "more")
	next := runGit(t, wt, "rev-parse", "HEAD")

	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("second Rescue: %v", err)
	}
	if rep.RescueSHA != next || rep.RescueRef != "refs/rescue/qc-test.1" {
		t.Fatalf("report = %+v, want the base ref advanced from %s to %s", rep, head, next)
	}
	if got := rescueRefs(t, repo); len(got) != 1 {
		t.Fatalf("rescue refs = %v, want the one ref advanced", got)
	}
}

func TestRescueLeavesOutOnlyProvenSediment(t *testing.T) {
	repo, wt, head := linkedWorktree(t)
	city := t.TempDir()
	skills := filepath.Join(wt, ".claude", "skills")
	writeFile(t, filepath.Join(skills, SkillOwnershipManifest), `{"targets":{"owned":"/pack/skills/owned"}}`)
	for _, name := range []string{"owned", "authored"} {
		if err := os.Symlink("/pack/skills/"+name, filepath.Join(skills, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(skills, "synced", "SKILL.md"), "same\n")
	writeFile(t, filepath.Join(city, ".claude", "skills", "synced", "SKILL.md"), "same\n")
	writeFile(t, filepath.Join(skills, "edited", "SKILL.md"), "mine\n")
	writeFile(t, filepath.Join(city, ".claude", "skills", "edited", "SKILL.md"), "theirs\n")
	writeFile(t, filepath.Join(wt, "AGENTS-gc.md"), "gc\n")
	writeFile(t, filepath.Join(wt, ".worktree-stale"), "")

	spec := rescueSpec(wt)
	spec.CityPath = city
	rep, err := Rescue(spec)
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	wantSediment := []string{
		".claude/skills/.gc-skill-ownership.json",
		".claude/skills/owned",
		".claude/skills/synced/SKILL.md",
		".worktree-stale",
		"AGENTS-gc.md",
	}
	if strings.Join(rep.Sediment, ",") != strings.Join(wantSediment, ",") {
		t.Errorf("sediment = %v, want %v", rep.Sediment, wantSediment)
	}
	paths := treePaths(t, repo, rep.RescueSHA)
	for _, want := range []string{".claude/skills/authored", ".claude/skills/edited/SKILL.md"} {
		if !containsString(paths, want) {
			t.Errorf("authored %s was not secured; tree = %v", want, paths)
		}
	}
	for _, unwanted := range wantSediment {
		if containsString(paths, unwanted) {
			t.Errorf("sediment %s was secured as work", unwanted)
		}
	}
	if rep.Head != head {
		t.Errorf("head = %s, want %s", rep.Head, head)
	}
}

func TestRescueOfSedimentOnlyTreeIsHead(t *testing.T) {
	_, wt, head := linkedWorktree(t)
	skills := filepath.Join(wt, ".claude", "skills")
	writeFile(t, filepath.Join(skills, SkillOwnershipManifest), `{"targets":{"owned":"/x"}}`)
	if err := os.Symlink("/x", filepath.Join(skills, "owned")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "AGENTS-gc.md"), "gc\n")

	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if rep.WIP || rep.RescueSHA != head {
		t.Fatalf("report = %+v, want HEAD with no WIP", rep)
	}
}

func TestRescueTracksEditsUnderSinksAsWork(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, ".claude", "skills", "tracked", "SKILL.md"), "v1\n")
	runGit(t, wt, "add", ".")
	runGit(t, wt, "commit", "-m", "track a skill")
	writeFile(t, filepath.Join(wt, ".claude", "skills", "tracked", "SKILL.md"), "v2\n")

	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if got := runGit(t, repo, "show", rep.RescueSHA+":.claude/skills/tracked/SKILL.md"); got != "v2" {
		t.Fatalf("tracked edit under a sink was not secured: %q", got)
	}
}

func TestRescueFailsClosedOnAnUnreadableManifest(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	skills := filepath.Join(wt, ".claude", "skills")
	writeFile(t, filepath.Join(skills, SkillOwnershipManifest), `{not json`)
	if err := os.Symlink("/x", filepath.Join(skills, "owned")); err != nil {
		t.Fatal(err)
	}
	if _, err := Rescue(rescueSpec(wt)); err == nil || !strings.Contains(err.Error(), "failing closed") {
		t.Fatalf("Rescue err = %v, want a fail-closed sediment error", err)
	}
	if got := rescueRefs(t, repo); len(got) != 0 {
		t.Fatalf("a failed classification still wrote rescue refs: %v", got)
	}
}

func TestRescueTaintsCredentialShapedFiles(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, ".env"), "TOKEN=x\n")
	writeFile(t, filepath.Join(wt, "keys", "id_ed25519"), "k\n")
	writeFile(t, filepath.Join(wt, "notes.txt"), "n\n")

	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if want := ".env,keys/id_ed25519"; strings.Join(rep.Taint, ",") != want {
		t.Fatalf("taint = %v, want %s", rep.Taint, want)
	}
	msg := runGit(t, repo, "log", "-1", "--format=%B", rep.RescueSHA)
	if !strings.Contains(msg, "Rescue-Taint: .env, keys/id_ed25519") || !strings.Contains(msg, "Rescue-Bead: qc-test.1") {
		t.Fatalf("rescue message lacks its trailers:\n%s", msg)
	}
}

func TestRescueKeepsSparseCheckoutPathsInTheSnapshot(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, "keep", "k.txt"), "k\n")
	writeFile(t, filepath.Join(wt, "drop", "d.txt"), "d\n")
	runGit(t, wt, "add", ".")
	runGit(t, wt, "commit", "-m", "two dirs")
	runGit(t, wt, "sparse-checkout", "set", "--no-cone", "/keep/", "/a.txt")
	writeFile(t, filepath.Join(wt, "keep", "k.txt"), "edited\n")

	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if !containsString(treePaths(t, repo, rep.RescueSHA), "drop/d.txt") {
		t.Fatal("a sparse-excluded path was recorded as deleted in the rescue")
	}
}

func TestRescueRejectsUnsafeBeadIDs(t *testing.T) {
	_, wt, _ := linkedWorktree(t)
	for _, id := range []string{"", "a/b", "../x", "x.lock", "x..y", "x*", "-x"} {
		spec := rescueSpec(wt)
		spec.BeadID = id
		if _, err := Rescue(spec); err == nil {
			t.Errorf("bead id %q was accepted", id)
		}
	}
}

func TestRescuedWorkSurvivesTeardownAndGarbageCollection(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, "only-copy.txt"), "precious\n")
	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	td, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: rep.RescueSHA})
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if !td.Removed {
		t.Fatalf("teardown report = %+v, want removed", td)
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
	runGit(t, repo, "reflog", "expire", "--expire=now", "--all")
	runGit(t, repo, "gc", "--prune=now", "--quiet")
	if got := runGit(t, repo, "show", "refs/rescue/qc-test.1:only-copy.txt"); got != "precious" {
		t.Fatalf("rescued content after gc = %q", got)
	}
	if got := runGit(t, repo, "worktree", "list", "--porcelain"); strings.Contains(got, "wt") {
		t.Fatalf("worktree still registered:\n%s", got)
	}
}

func TestTeardownRefusesAWorktreeThatChangedSinceItsRescue(t *testing.T) {
	_, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, "wip.txt"), "v1\n")
	recorded, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	writeFile(t, filepath.Join(wt, "wip.txt"), "v2\n")

	td, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: recorded.RescueSHA})
	if err == nil || !strings.Contains(err.Error(), "changed since its rescue was recorded") {
		t.Fatalf("Teardown err = %v, want a changed-worktree refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(wt, "wip.txt")); statErr != nil {
		t.Fatalf("worktree was touched: %v", statErr)
	}
	if td.Rescue == nil || td.Rescue.RescueSHA == recorded.RescueSHA {
		t.Fatalf("the changed state was not secured anew: %+v", td.Rescue)
	}

	td, err = Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: td.Rescue.RescueSHA})
	if err != nil || !td.Removed {
		t.Fatalf("retry with the new rescue: report %+v err %v", td, err)
	}
}

func TestTeardownRefusesAMainCheckout(t *testing.T) {
	repo, _, head := linkedWorktree(t)
	spec := rescueSpec(repo)
	if _, err := Teardown(TeardownSpec{RescueSpec: spec, RescueSHA: head}); err == nil {
		t.Fatal("teardown accepted a main checkout")
	}
	if _, err := os.Stat(filepath.Join(repo, "a.txt")); err != nil {
		t.Fatalf("main checkout was damaged: %v", err)
	}
}

func TestTeardownRefusesASubmoduleCheckout(t *testing.T) {
	sub, _ := initTestRepo(t)
	writeFile(t, filepath.Join(sub, "s.txt"), "s\n")
	runGit(t, sub, "add", "s.txt")
	runGit(t, sub, "commit", "-m", "s")
	super, _ := initTestRepo(t)
	runGit(t, super, "-c", "protocol.file.allow=always", "submodule", "add", sub, "sub")
	checkout := filepath.Join(super, "sub")
	if info, err := os.Lstat(filepath.Join(checkout, ".git")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("control: a submodule checkout's .git should be a file: %v", err)
	}
	head := runGit(t, checkout, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(checkout, "wip.txt"), "wip\n")

	// A submodule passes the .git-is-a-file test and its own worktree list
	// registers it; only the git-dir check tells it apart.
	if _, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(checkout), RescueSHA: head}); err == nil ||
		!strings.Contains(err.Error(), "main checkout") {
		t.Fatalf("teardown err = %v, want a main-checkout refusal", err)
	}
	if _, err := os.Stat(filepath.Join(checkout, "wip.txt")); err != nil {
		t.Fatalf("submodule checkout was deleted: %v", err)
	}
}

func TestTeardownRefusesASubdirectoryOfAWorktree(t *testing.T) {
	_, wt, head := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, "dir", "f.txt"), "f\n")
	if _, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(filepath.Join(wt, "dir")), RescueSHA: head}); err == nil {
		t.Fatal("teardown accepted a subdirectory of a worktree")
	}
	if _, err := os.Stat(filepath.Join(wt, "dir", "f.txt")); err != nil {
		t.Fatalf("subdirectory was deleted: %v", err)
	}
}

func TestTeardownOfAnAbsentPathIsIdempotent(t *testing.T) {
	td, err := Teardown(TeardownSpec{
		RescueSpec: rescueSpec(filepath.Join(t.TempDir(), "gone")),
		RescueSHA:  "0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil || !td.AlreadyAbsent || td.Removed {
		t.Fatalf("report %+v err %v, want an already-absent success", td, err)
	}
}

func TestTeardownRemovesALockedWorktree(t *testing.T) {
	_, wt, head := linkedWorktree(t)
	runGit(t, wt, "worktree", "lock", wt)
	td, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: head})
	if err != nil || !td.Removed {
		t.Fatalf("report %+v err %v, want a locked worktree removed after its rescue", td, err)
	}
}

// Codex review of ga-w805wc, finding 2: content that is staged but no longer
// in the working tree lives only in the index, and `add -A` overwrites it.
func TestRescueKeepsStagedOnlyContent(t *testing.T) {
	repo, wt, head := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, "a.txt"), "staged-only\n")
	runGit(t, wt, "add", "a.txt")
	writeFile(t, filepath.Join(wt, "a.txt"), "base\n") // working tree back to HEAD

	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if rep.RescueSHA == head {
		t.Fatal("rescue collapsed to HEAD and dropped the staged content")
	}
	parents := strings.Fields(runGit(t, repo, "log", "-1", "--format=%P", rep.RescueSHA))
	if len(parents) != 2 || parents[0] != head {
		t.Fatalf("rescue parents = %v, want HEAD plus an index commit", parents)
	}
	if got := runGit(t, repo, "show", parents[1]+":a.txt"); got != "staged-only" {
		t.Fatalf("index commit a.txt = %q", got)
	}
	again, err := Rescue(rescueSpec(wt))
	if err != nil || again.RescueSHA != rep.RescueSHA {
		t.Fatalf("retry with the same index minted %s (was %s), err %v", again.RescueSHA, rep.RescueSHA, err)
	}
}

// Codex finding 3: an assume-unchanged bit makes `git add` trust the index.
func TestRescueSeesEditsBehindAssumeUnchanged(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	runGit(t, wt, "update-index", "--assume-unchanged", "a.txt")
	writeFile(t, filepath.Join(wt, "a.txt"), "hidden edit\n")

	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if got := runGit(t, repo, "show", rep.RescueSHA+":a.txt"); got != "hidden edit" {
		t.Fatalf("rescued a.txt = %q, want the edit behind assume-unchanged", got)
	}
	if got := runGit(t, wt, "ls-files", "-v", "a.txt"); !strings.HasPrefix(got, "h ") {
		t.Fatalf("rescue changed the real index's flag: %q", got)
	}
}

// Codex finding 3b: an edited file outside the sparse definition.
func TestRescueKeepsEditsOutsideTheSparseDefinition(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, "keep", "k.txt"), "k\n")
	writeFile(t, filepath.Join(wt, "drop", "d.txt"), "d\n")
	runGit(t, wt, "add", ".")
	runGit(t, wt, "commit", "-m", "two dirs")
	runGit(t, wt, "sparse-checkout", "set", "--no-cone", "/keep/", "/a.txt")
	writeFile(t, filepath.Join(wt, "drop", "d.txt"), "written outside the cone\n")

	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if got := runGit(t, repo, "show", rep.RescueSHA+":drop/d.txt"); got != "written outside the cone" {
		t.Fatalf("rescued drop/d.txt = %q", got)
	}
}

func submoduleWorktree(t *testing.T) (wt, sub string) {
	t.Helper()
	upstream, _ := initTestRepo(t)
	writeFile(t, filepath.Join(upstream, "s.txt"), "s\n")
	runGit(t, upstream, "add", "s.txt")
	runGit(t, upstream, "commit", "-m", "s")
	repo, _, _ := linkedWorktree(t)
	runGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", upstream, "sub")
	runGit(t, repo, "commit", "-m", "add sub")
	wt = filepath.Join(t.TempDir(), "wt-sub")
	runGit(t, repo, "worktree", "add", "--detach", wt, "HEAD")
	runGit(t, wt, "-c", "protocol.file.allow=always", "submodule", "update", "--init")
	return wt, filepath.Join(wt, "sub")
}

// Codex finding 1: a submodule's repository lives in the worktree's private
// admin dir, so its uncommitted work and local-only commits die with it.
func TestRescueRefusesASubmoduleWithWorkItCannotCarry(t *testing.T) {
	t.Run("clean and pushed is fine", func(t *testing.T) {
		wt, _ := submoduleWorktree(t)
		if _, err := Rescue(rescueSpec(wt)); err != nil {
			t.Fatalf("Rescue of a clean submodule: %v", err)
		}
	})
	t.Run("uncommitted", func(t *testing.T) {
		wt, sub := submoduleWorktree(t)
		writeFile(t, filepath.Join(sub, "wip.txt"), "wip\n")
		if _, err := Rescue(rescueSpec(wt)); err == nil || !strings.Contains(err.Error(), "submodule sub") {
			t.Fatalf("Rescue err = %v, want a submodule refusal", err)
		}
		head := runGit(t, wt, "rev-parse", "HEAD")
		if _, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: head}); err == nil {
			t.Fatal("teardown removed a worktree whose submodule held uncommitted work")
		}
		if _, err := os.Stat(filepath.Join(sub, "wip.txt")); err != nil {
			t.Fatalf("submodule work lost: %v", err)
		}
	})
	t.Run("its private state is preserved verbatim by teardown", func(t *testing.T) {
		wt, sub := submoduleWorktree(t)
		git := func(args ...string) string {
			return runGit(t, sub, append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		}
		git("checkout", "-q", "-b", "feat")
		writeFile(t, filepath.Join(sub, "feat.txt"), "feat\n")
		git("add", "feat.txt")
		git("commit", "-qm", "feat")
		feat := git("rev-parse", "HEAD")
		writeFile(t, filepath.Join(sub, "s.txt"), "stashed\n")
		git("stash", "-q")
		stash := git("rev-parse", "refs/stash")
		writeFile(t, filepath.Join(sub, "away.txt"), "away\n")
		git("add", "away.txt")
		git("commit", "-qm", "away")
		away := git("rev-parse", "HEAD")
		git("reset", "-q", "--hard", "HEAD~1")
		rep, err := Rescue(rescueSpec(wt))
		if err != nil {
			t.Fatalf("Rescue: %v", err)
		}
		td, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: rep.RescueSHA})
		if err != nil || !td.Removed {
			t.Fatalf("Teardown: %+v %v", td, err)
		}
		if td.PreservedModules != PreservedModulesDir(rep.Repo, "qc-test.1", rep.RescueSHA) {
			t.Fatalf("preserved at %q", td.PreservedModules)
		}
		kept := filepath.Join(td.PreservedModules, "sub")
		for name, sha := range map[string]string{"branch feat": feat, "stash": stash, "reflog-only commit": away} {
			if _, err := gitOutput("/", nil, "--git-dir="+kept, "cat-file", "-e", sha+"^{commit}"); err != nil {
				t.Errorf("submodule %s (%s) not preserved: %v", name, sha, err)
			}
		}
		if got := strings.TrimSpace(runGitDir(t, kept, "rev-parse", "refs/heads/feat")); got != feat {
			t.Errorf("preserved branch feat = %q, want %s", got, feat)
		}
	})
	t.Run("a shallow submodule does not wedge teardown", func(t *testing.T) {
		upstream, _ := initTestRepo(t)
		for _, c := range []string{"one", "two"} {
			writeFile(t, filepath.Join(upstream, "s.txt"), c+"\n")
			runGit(t, upstream, "add", "s.txt")
			runGit(t, upstream, "commit", "-qm", c)
		}
		repo, _, _ := linkedWorktree(t)
		runGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "--depth", "1", "file://"+upstream, "sub")
		runGit(t, repo, "commit", "-qm", "add shallow sub")
		wt := filepath.Join(t.TempDir(), "wt-shallow")
		runGit(t, repo, "worktree", "add", "--detach", wt, "HEAD")
		runGit(t, wt, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--depth", "1")
		rep, err := Rescue(rescueSpec(wt))
		if err != nil {
			t.Fatalf("Rescue: %v", err)
		}
		if td, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: rep.RescueSHA}); err != nil || !td.Removed {
			t.Fatalf("Teardown: %+v %v", td, err)
		}
	})
}

// Codex finding 6: a manifest entry proves ownership only while the link
// still points where the materializer recorded.
func TestRescueSecuresARepointedSkillSymlink(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	skills := filepath.Join(wt, ".claude", "skills")
	writeFile(t, filepath.Join(skills, SkillOwnershipManifest), `{"targets":{"mine":"/original/gc/skill"}}`)
	if err := os.Symlink("/authored/replacement/skill", filepath.Join(skills, "mine")); err != nil {
		t.Fatal(err)
	}
	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if containsString(rep.Sediment, ".claude/skills/mine") ||
		!containsString(treePaths(t, repo, rep.RescueSHA), ".claude/skills/mine") {
		t.Fatalf("a repointed symlink was treated as gc sediment: sediment=%v", rep.Sediment)
	}
}

// Codex finding 5: absence is ENOENT only; an unreadable path is an error.
func TestRescueAbsentOKDistinguishesGoneFromUnreadable(t *testing.T) {
	spec := rescueSpec(filepath.Join(t.TempDir(), "gone"))
	spec.AbsentOK = true
	rep, err := Rescue(spec)
	if err != nil || !rep.Absent {
		t.Fatalf("absent path: report %+v err %v, want Absent", rep, err)
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the unreadable case cannot be built")
	}
	_, wt, _ := linkedWorktree(t)
	parent := filepath.Dir(wt)
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	spec = rescueSpec(wt)
	spec.AbsentOK = true
	if rep, err := Rescue(spec); err == nil || rep.Absent {
		t.Fatalf("unreadable path: report %+v err %v, want an error, not Absent", rep, err)
	}
}

func TestRescueKeepsAnInProgressMergeHeadReachable(t *testing.T) {
	repo, wt, head := linkedWorktree(t)
	runGit(t, wt, "checkout", "-q", "--detach", head)
	writeFile(t, filepath.Join(wt, "a.txt"), "theirs\n")
	runGit(t, wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "theirs")
	theirs := runGit(t, wt, "rev-parse", "HEAD")
	runGit(t, wt, "checkout", "-q", "--detach", head)
	writeFile(t, filepath.Join(wt, "a.txt"), "ours\n")
	runGit(t, wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "ours")
	cmdErr := runGitAllowFail(wt, "-c", "user.name=t", "-c", "user.email=t@t", "merge", theirs)
	if cmdErr == nil {
		t.Fatal("control: the merge should conflict")
	}
	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if !rep.IndexUnmerged {
		t.Error("an unmerged index was not reported")
	}
	assertReachableFromRescue(t, repo, rep.RescueRef, theirs)
}

// assertReachableFromRescue proves commit survives the worktree: reachable
// from the rescue ref after the worktree's private state is irrelevant.
func assertReachableFromRescue(t *testing.T, repo, ref, commit string) {
	t.Helper()
	ok, err := isAncestor(repo, commit, ref)
	if err != nil || !ok {
		t.Fatalf("%s is not reachable from %s (err %v)", commit, ref, err)
	}
}

// Opus review finding 1: a rebase stopped on a conflict holds the commits not
// yet replayed only in the worktree's private rebase state.
func TestRescueKeepsTheUnreplayedCommitsOfAStoppedRebase(t *testing.T) {
	repo, wt, base := linkedWorktree(t)
	var mine []string
	for i, content := range []string{"c1\n", "c2\n", "c3\n"} {
		writeFile(t, filepath.Join(wt, "f.txt"), content)
		if i == 0 {
			runGit(t, wt, "add", "f.txt")
		}
		runGit(t, wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", content)
		mine = append(mine, runGit(t, wt, "rev-parse", "HEAD"))
	}
	runGit(t, repo, "checkout", "-q", "-b", "upstream", base)
	writeFile(t, filepath.Join(repo, "f.txt"), "upstream\n")
	runGit(t, repo, "add", "f.txt")
	runGit(t, repo, "commit", "-qm", "upstream")
	if runGitAllowFail(wt, "-c", "user.name=t", "-c", "user.email=t@t", "rebase", "upstream") == nil {
		t.Fatal("control: the rebase should stop on a conflict")
	}
	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	for _, c := range mine {
		assertReachableFromRescue(t, repo, rep.RescueRef, c)
	}
}

// Opus finding 5: a commit reset away on a detached HEAD is reachable only
// from the worktree's own HEAD reflog.
func TestRescueKeepsReflogOnlyCommits(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, "gone.txt"), "reset away\n")
	runGit(t, wt, "add", "gone.txt")
	runGit(t, wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "reset away")
	resetAway := runGit(t, wt, "rev-parse", "HEAD")
	runGit(t, wt, "reset", "-q", "--hard", "HEAD~1")
	runGit(t, wt, "update-ref", "refs/worktree/mark", resetAway)

	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if len(rep.PrivateCommits) == 0 {
		t.Fatal("no private commits reported")
	}
	assertReachableFromRescue(t, repo, rep.RescueRef, resetAway)
}

// Opus finding 2a: an untracked directory with its own .git is recorded by
// `git add` as a bare pointer.
func TestRescueRefusesANestedRepository(t *testing.T) {
	_, wt, _ := linkedWorktree(t)
	runGit(t, wt, "init", "-q", "lib")
	writeFile(t, filepath.Join(wt, "lib", "x.txt"), "x\n")
	runGit(t, filepath.Join(wt, "lib"), "add", "x.txt")
	runGit(t, filepath.Join(wt, "lib"), "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x")
	if _, err := Rescue(rescueSpec(wt)); err == nil || !strings.Contains(err.Error(), "lib holds its own git repository") {
		t.Fatalf("Rescue err = %v, want a nested-repository refusal", err)
	}
}

// Opus finding 2b: another bead's worktree created beneath this one.
func TestTeardownRefusesWhenAnotherWorktreeIsNestedInside(t *testing.T) {
	repo, wt, head := linkedWorktree(t)
	inner := filepath.Join(wt, "worktrees", "qc-other")
	runGit(t, repo, "worktree", "add", "--detach", inner, "HEAD")
	writeFile(t, filepath.Join(inner, "theirs.txt"), "another bead's work\n")
	if _, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: head}); err == nil ||
		!strings.Contains(err.Error(), "nested inside") {
		t.Fatalf("Teardown err = %v, want a nested-worktree refusal", err)
	}
	if _, err := os.Stat(filepath.Join(inner, "theirs.txt")); err != nil {
		t.Fatalf("the nested worktree's work was deleted: %v", err)
	}
}

// Opus finding 3: a repo-wide prune would drop another worktree's stale
// registration, which git still treats as a reachability root.
func TestTeardownLeavesOtherRegistrationsAlone(t *testing.T) {
	repo, wt, head := linkedWorktree(t)
	stale := filepath.Join(t.TempDir(), "stale")
	runGit(t, repo, "worktree", "add", "--detach", stale, "HEAD")
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}
	if _, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: head}); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if list := runGit(t, repo, "worktree", "list", "--porcelain"); !strings.Contains(list, "stale") {
		t.Fatalf("teardown pruned another worktree's registration:\n%s", list)
	}
}

// The recursive fallback: git refuses to remove a worktree holding a
// submodule. Only this worktree's admin dir may go with it.
func TestTeardownFallbackRemovesOnlyItsOwnAdminDir(t *testing.T) {
	wt, _ := submoduleWorktree(t)
	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	repo := strings.TrimSuffix(rep.Repo, "/.git")
	stale := filepath.Join(t.TempDir(), "stale")
	runGit(t, repo, "worktree", "add", "--detach", stale, "HEAD")
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}
	adminDir := runGit(t, wt, "rev-parse", "--path-format=absolute", "--git-dir")
	td, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: rep.RescueSHA})
	if err != nil || !td.Removed {
		t.Fatalf("Teardown: %+v %v", td, err)
	}
	if _, err := os.Lstat(adminDir); !os.IsNotExist(err) {
		t.Fatalf("admin dir %s survived: %v", adminDir, err)
	}
	if list := runGit(t, repo, "worktree", "list", "--porcelain"); !strings.Contains(list, "stale") {
		t.Fatalf("the fallback pruned another worktree's registration:\n%s", list)
	}
}

// Opus finding 8: bead ga-1's reuse lookup must not adopt bead ga-1-2's ref.
func TestRescueReuseIgnoresAnotherBeadsRefs(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, "wip.txt"), "wip\n")
	other := rescueSpec(wt)
	other.BeadID = "ga-1-2"
	if _, err := Rescue(other); err != nil {
		t.Fatalf("Rescue ga-1-2: %v", err)
	}
	mine := rescueSpec(wt)
	mine.BeadID = "ga-1"
	rep, err := Rescue(mine)
	if err != nil {
		t.Fatalf("Rescue ga-1: %v", err)
	}
	if rep.RescueRef != "refs/rescue/ga-1" {
		t.Fatalf("ga-1's rescue landed at %s", rep.RescueRef)
	}
	if got := runGit(t, repo, "rev-parse", "refs/rescue/ga-1"); got == "" {
		t.Fatal("refs/rescue/ga-1 was not written")
	}
}

// Opus finding 9: a symlink to a worktree is refused rather than half-removed.
func TestTeardownRefusesASymlinkedPath(t *testing.T) {
	_, wt, head := linkedWorktree(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(wt, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(link), RescueSHA: head}); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Teardown err = %v, want a symlink refusal", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "a.txt")); err != nil {
		t.Fatalf("worktree behind the symlink was removed: %v", err)
	}
}

// runGitAllowFail runs git through the package's own runner (no new test
// subprocess sites for the resource census) and returns its error.
func runGitAllowFail(dir string, args ...string) error {
	_, err := gitOutput(dir, nil, args...)
	return err
}

// A detached worktree with local commits: its reflog holds those commits,
// and a second rescue must still produce the SAME commit, or teardown's
// equality check can never pass.
func TestRescueOfDetachedLocalCommitsIsStableAcrossRetries(t *testing.T) {
	_, wt, _ := linkedWorktree(t)
	for _, c := range []string{"one\n", "two\n"} {
		writeFile(t, filepath.Join(wt, "f.txt"), c)
		runGit(t, wt, "add", "f.txt")
		runGit(t, wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", c)
	}
	// And one commit that is NOT an ancestor of HEAD: reset away, so only the
	// reflog (then the anchor, then this bead's own rescue ref) reaches it.
	writeFile(t, filepath.Join(wt, "f.txt"), "three\n")
	runGit(t, wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "three")
	runGit(t, wt, "reset", "-q", "--hard", "HEAD~1")
	writeFile(t, filepath.Join(wt, "wip.txt"), "wip\n")
	first, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("first Rescue: %v", err)
	}
	if len(first.PrivateCommits) == 0 {
		t.Fatal("control: the reset-away commit should be anchored")
	}
	second, err := Rescue(rescueSpec(wt))
	if err != nil || second.RescueSHA != first.RescueSHA {
		t.Fatalf("second rescue %s != first %s (err %v)", second.RescueSHA, first.RescueSHA, err)
	}
	td, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: first.RescueSHA})
	if err != nil || !td.Removed {
		t.Fatalf("Teardown: %+v %v", td, err)
	}
}

// A commit reachable only from a remote-tracking ref is NOT durable: the next
// `fetch --prune` drops that ref. It is anchored like any private commit.
func TestRescueDoesNotTrustRemoteTrackingRefs(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, "pushed.txt"), "pushed once\n")
	runGit(t, wt, "add", "pushed.txt")
	runGit(t, wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "pushed once")
	pushed := runGit(t, wt, "rev-parse", "HEAD")
	runGit(t, repo, "update-ref", "refs/remotes/origin/feature", pushed)
	runGit(t, wt, "reset", "-q", "--hard", "HEAD~1")

	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	runGit(t, repo, "update-ref", "-d", "refs/remotes/origin/feature") // the remote branch went away
	assertReachableFromRescue(t, repo, rep.RescueRef, pushed)
}

func runGitDir(t *testing.T, gitDir string, args ...string) string {
	t.Helper()
	out, err := gitOutput("/", nil, append([]string{"--git-dir=" + gitDir}, args...)...)
	if err != nil {
		t.Fatalf("git --git-dir=%s %v: %v", gitDir, args, err)
	}
	return out
}

// Opus round 3, finding 2: a directory whose .git names ANOTHER worktree's
// admin dir must be refused, or that worktree's admin dir is removed.
func TestTeardownRefusesATreeWhoseGitNamesAnotherAdminDir(t *testing.T) {
	repo, wt, head := linkedWorktree(t)
	other := filepath.Join(t.TempDir(), "other")
	runGit(t, repo, "worktree", "add", "--detach", other, "HEAD")
	otherAdmin := runGit(t, other, "rev-parse", "--path-format=absolute", "--git-dir")
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(wt, os.DirFS(other)); err != nil {
		t.Fatal(err)
	}
	if _, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: head}); err == nil ||
		!strings.Contains(err.Error(), "belongs to") {
		t.Fatalf("Teardown err = %v, want a back-pointer refusal", err)
	}
	if _, err := os.Stat(otherAdmin); err != nil {
		t.Fatalf("the other worktree's admin dir was removed: %v", err)
	}
}

// Opus round 3, finding 4: a stale registration nested under the target (its
// directory already gone) must not block teardown forever.
func TestTeardownIgnoresAStaleNestedRegistration(t *testing.T) {
	repo, wt, head := linkedWorktree(t)
	inner := filepath.Join(wt, "worktrees", "qc-gone")
	runGit(t, repo, "worktree", "add", "--detach", inner, "HEAD")
	if err := os.RemoveAll(filepath.Join(wt, "worktrees")); err != nil {
		t.Fatal(err)
	}
	if td, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: head}); err != nil || !td.Removed {
		t.Fatalf("Teardown: %+v %v", td, err)
	}
}

// Opus round 3, finding 5: a local branch deleted between rescue and teardown
// (merged-branch cleanup) must not change the rescue.
func TestTeardownIsStableAcrossLocalBranchDeletion(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, "side.txt"), "side\n")
	runGit(t, wt, "add", "side.txt")
	runGit(t, wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "side")
	runGit(t, repo, "branch", "side", runGit(t, wt, "rev-parse", "HEAD"))
	runGit(t, wt, "reset", "-q", "--hard", "HEAD~1")
	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	runGit(t, repo, "branch", "-D", "side")
	if td, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: rep.RescueSHA}); err != nil || !td.Removed {
		t.Fatalf("Teardown after branch deletion: %+v %v", td, err)
	}
}

// Parents go on the commit-tree command line, so a large private set is folded
// into layers of anchors. Every tip must stay reachable, and the fold must be
// deterministic or teardown's equality check never passes.
func TestRescueFoldsManyPrivateCommitsDeterministically(t *testing.T) {
	old := anchorBatch
	anchorBatch = 3
	t.Cleanup(func() { anchorBatch = old })
	repo, wt, head := linkedWorktree(t)
	var tips []string
	for i := 0; i < 10; i++ {
		out, err := gitOutputStdin(wt, []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"},
			"private "+string(rune('a'+i))+"\n", "commit-tree", head+"^{tree}", "-p", head, "-F", "-")
		if err != nil {
			t.Fatal(err)
		}
		sha := strings.TrimSpace(out)
		runGit(t, wt, "update-ref", "refs/worktree/p"+string(rune('a'+i)), sha)
		tips = append(tips, sha)
	}
	first, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	for _, tip := range tips {
		assertReachableFromRescue(t, repo, first.RescueRef, tip)
	}
	// The rescue's parents are HEAD and the top anchor; no commit in the fold
	// may carry more than anchorBatch parents.
	parents := strings.Fields(runGit(t, repo, "log", "-1", "--format=%P", first.RescueSHA))
	if len(parents) != 2 {
		t.Fatalf("rescue parents = %v, want HEAD and one anchor", parents)
	}
	for _, line := range strings.Split(runGit(t, repo, "rev-list", "--parents", parents[1], "--not", head), "\n") {
		if n := len(strings.Fields(line)) - 1; n > anchorBatch {
			t.Fatalf("anchor commit %s has %d parents, want at most %d", strings.Fields(line)[0], n, anchorBatch)
		}
	}
	if td, err := Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: first.RescueSHA}); err != nil || !td.Removed {
		t.Fatalf("Teardown after a folded anchor: %+v %v", td, err)
	}
}

// Codex review of #97, P1: `git cherry-pick A B C` stopping on A keeps B and C
// ONLY in the worktree's sequencer/todo.
func TestRescueKeepsTheUnappliedCommitsOfAStoppedCherryPick(t *testing.T) {
	repo, wt, base := linkedWorktree(t)
	id := []string{"-c", "user.name=t", "-c", "user.email=t@t"}
	runGit(t, repo, "checkout", "-q", "-b", "side", base)
	var picks []string
	for _, c := range []string{"side-a", "side-b", "side-c"} {
		writeFile(t, filepath.Join(repo, "a.txt"), c+"\n")
		runGit(t, repo, append(id, "commit", "-qam", c)...)
		picks = append(picks, runGit(t, repo, "rev-parse", "HEAD"))
	}
	writeFile(t, filepath.Join(wt, "a.txt"), "conflicting\n")
	runGit(t, wt, append(id, "commit", "-qam", "conflicting")...)
	if runGitAllowFail(wt, append(id, "cherry-pick", picks[0], picks[1], picks[2])...) == nil {
		t.Fatal("control: the cherry-pick should stop on a conflict")
	}
	runGit(t, repo, "checkout", "-q", "--detach", base)
	runGit(t, repo, "branch", "-D", "side") // only the sequencer still names B and C
	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	for _, c := range picks {
		assertReachableFromRescue(t, repo, rep.RescueRef, c)
	}
}

// Codex review of #97, P1: an embedded repository that was `git add`ed is an
// index gitlink, but its .git lives inside the tree, not under modules/.
func TestRescueRefusesAStagedEmbeddedRepository(t *testing.T) {
	_, wt, _ := linkedWorktree(t)
	lib := filepath.Join(wt, "lib")
	runGit(t, wt, "init", "-q", "lib")
	writeFile(t, filepath.Join(lib, "x.txt"), "x\n")
	runGit(t, lib, "add", "x.txt")
	runGit(t, lib, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x")
	runGit(t, wt, "add", "lib")
	if _, err := Rescue(rescueSpec(wt)); err == nil || !strings.Contains(err.Error(), "not a submodule of this worktree") {
		t.Fatalf("Rescue err = %v, want an embedded-repository refusal", err)
	}
}

// Codex review of #97, P2: a secret staged and then deleted from the tree
// survives only in the index parent; taint must still name it.
func TestRescueTaintCoversTheIndexParent(t *testing.T) {
	_, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, ".env"), "TOKEN=x\n")
	runGit(t, wt, "add", ".env")
	if err := os.Remove(filepath.Join(wt, ".env")); err != nil {
		t.Fatal(err)
	}
	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if !containsString(rep.Taint, ".env") {
		t.Fatalf("taint = %v, want the staged-only .env", rep.Taint)
	}
}

// Codex review of #97, round 5, P1: validation ran before the path lock, so a
// tree replaced while teardown waited was rescued and removed unchecked.
func TestTeardownRevalidatesThePathUnderItsLock(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	// While teardown waits for the lock, the worktree is replaced by an
	// independent clone at the same path.
	afterPathLock = func() {
		afterPathLock = nil
		if err := os.RemoveAll(wt); err != nil {
			t.Fatal(err)
		}
		runGit(t, filepath.Dir(wt), "clone", "-q", repo, wt)
	}
	t.Cleanup(func() { afterPathLock = nil })
	_, err = Teardown(TeardownSpec{RescueSpec: rescueSpec(wt), RescueSHA: rep.RescueSHA})
	if err == nil || !strings.Contains(err.Error(), "changed while waiting for its lock") {
		t.Fatalf("Teardown err = %v, want the replacement refused under the lock", err)
	}
	if refs := rescueRefs(t, wt); len(refs) != 0 {
		t.Fatalf("the replacement clone was rescued before being refused: %v", refs)
	}
	if _, err := os.Stat(filepath.Join(wt, "a.txt")); err != nil {
		t.Fatalf("the replacement clone was damaged: %v", err)
	}
}

// Codex review of #97, round 5, P2: a clean worktree on a local-only commit
// that added a credential file has nothing beyond HEAD, but the rescue makes
// that commit durable, so taint must name the file.
func TestRescueTaintCoversALocalOnlyCommit(t *testing.T) {
	repo, wt, base := linkedWorktree(t)
	runGit(t, repo, "update-ref", "refs/remotes/origin/main", base) // the base is published
	writeFile(t, filepath.Join(wt, ".env"), "TOKEN=x\n")
	runGit(t, wt, "add", ".env")
	runGit(t, wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "local secret")
	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if rep.WIP {
		t.Fatalf("control: a clean worktree needs no snapshot, got %+v", rep)
	}
	if !containsString(rep.Taint, ".env") {
		t.Fatalf("taint = %v, want the .env the local-only HEAD added", rep.Taint)
	}
}

// A credential path that a remote-tracking ref already reaches was published
// before the rescue; the rescue does not newly expose it.
func TestRescueTaintSkipsCommitsARemoteAlreadyHas(t *testing.T) {
	repo, wt, _ := linkedWorktree(t)
	writeFile(t, filepath.Join(wt, ".env"), "TOKEN=x\n")
	runGit(t, wt, "add", ".env")
	runGit(t, wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "published secret")
	runGit(t, repo, "update-ref", "refs/remotes/origin/main", runGit(t, wt, "rev-parse", "HEAD"))
	rep, err := Rescue(rescueSpec(wt))
	if err != nil {
		t.Fatalf("Rescue: %v", err)
	}
	if len(rep.Taint) != 0 {
		t.Fatalf("taint = %v, want none: the remote already has that commit", rep.Taint)
	}
}
