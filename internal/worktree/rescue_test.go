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
