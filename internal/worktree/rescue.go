package worktree

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// Rescue and Teardown are the secure-before-destroy path for worktrees that
// formulas create ad hoc (`git worktree add --detach`) and therefore carry no
// provenance for Cleanup to verify (ga-w805wc).
//
// The hazard teardown creates is LOCAL: removing a worktree destroys its
// uncommitted work and the reachability of a detached HEAD's commits. So the
// work is secured locally, in a ref the removal cannot touch, and never by
// asking whether a remote already has it. The design this replaces tested
// remote containment with `git branch -r --contains HEAD`, which reads the last
// fetch's cache: a stale remote-tracking ref "contained" HEAD, the push was
// skipped, and the tree was deleted anyway. It also pushed whatever
// `git add -A` swept up to a branch that could be a live PR head. Rescue does
// neither: it contacts no remote and moves no branch.
//
// refs/rescue/* is shared by every worktree of a repository (only
// refs/worktree/*, refs/bisect/* and refs/rewritten/* are per-worktree), so a
// rescue ref written from inside a linked worktree lands in the common git dir
// and survives `git worktree remove`, `fetch --prune`, and `gc --prune=now`.
// Getting rescued work off the box is a separate job, not teardown's.

const (
	// RescueRefPrefix namespaces the local refs Rescue writes.
	RescueRefPrefix = "refs/rescue/"

	// SkillOwnershipManifest is the per-sink file in which skill
	// materialization records the symlinks it owns. It must equal
	// materialize.OwnershipManifestFile; a cmd/gc test pins the two together.
	SkillOwnershipManifest = ".gc-skill-ownership.json"

	rescueIdentityName  = "gc worktree rescue"
	rescueIdentityEmail = "gc-worktree-rescue@localhost"
)

// validRescueBeadID admits bead IDs that are also safe as one ref-name
// component and as a literal for-each-ref pattern: no slash (a nested name
// would collide with the base ref), no glob metacharacters, no "..".
var validRescueBeadID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// RescueSpec names one linked worktree whose work must be secured before it
// is removed.
type RescueSpec struct {
	// Path is the worktree root.
	Path string
	// BeadID names the rescue ref: refs/rescue/<BeadID>.
	BeadID string
	// CityPath, when set, lets a regular file under a skill sink count as gc
	// sediment if it is byte-identical to the same path under the city (the
	// city-synced skill copies). Unset, such files are secured as work.
	CityPath string
	// SkillSinks lists sink directories (".claude/skills") whose
	// manifest-recorded symlinks are gc's own and are left out of the rescue.
	SkillSinks []string
	// AbsentOK makes a Path that does not exist (ENOENT, and nothing else) a
	// success reported as Absent, so a caller can tell "already gone" from
	// "could not look" without a shell test that conflates the two.
	AbsentOK bool
}

// RescueReport describes where a worktree's work was secured. RescueSHA is the
// commit that restores the worktree's state; RescueRef is a local ref that
// reaches it.
type RescueReport struct {
	Path      string `json:"path"`
	Repo      string `json:"repo"`
	BeadID    string `json:"bead_id"`
	Head      string `json:"head,omitempty"`
	RescueRef string `json:"rescue_ref"`
	RescueSHA string `json:"rescue_sha"`
	// WIP reports whether the worktree held changes beyond HEAD, so that
	// RescueSHA is a snapshot commit rather than HEAD itself.
	WIP bool `json:"wip"`
	// Sediment lists untracked paths left out because gc provably put them
	// there.
	Sediment []string `json:"sediment,omitempty"`
	// Taint lists secured paths whose names look like credentials. The
	// plaintext was already on this disk; whatever later moves rescues off
	// the box must not push a tainted one.
	Taint []string `json:"taint,omitempty"`
	// IndexUnmerged reports an index with conflicts. Its stages are not
	// recorded; the conflicted working content is, and the in-progress
	// operation's heads are kept as parents.
	IndexUnmerged bool `json:"index_unmerged,omitempty"`
	// PrivateCommits lists the commits only the worktree's private state
	// reached (reflog, in-progress operations, per-worktree refs, submodule
	// HEADs), kept reachable through an anchor parent of the rescue commit.
	PrivateCommits []string `json:"private_commits,omitempty"`
	// Absent reports, under AbsentOK, that Path does not exist. Nothing
	// else in the report is set.
	Absent bool `json:"absent,omitempty"`
}

// TeardownSpec names a worktree to remove once its current state is secured
// exactly as recorded.
type TeardownSpec struct {
	RescueSpec
	// RescueSHA is the rescue the caller recorded (normally on the bead)
	// before asking for removal.
	RescueSHA string
}

// TeardownReport describes a teardown.
type TeardownReport struct {
	Rescue        *RescueReport `json:"rescue,omitempty"`
	Removed       bool          `json:"removed"`
	AlreadyAbsent bool          `json:"already_absent,omitempty"`
	// PreservedModules is where the worktree's submodule repositories were
	// moved, verbatim, before removal (empty when it had none).
	PreservedModules string `json:"preserved_modules,omitempty"`
}

// PreservedModulesDir is where Teardown keeps a removed worktree's submodule
// repositories: <common>/gc-rescued-modules/<bead>-<rescue sha[:12]>.
func PreservedModulesDir(common, beadID, rescueSHA string) string {
	short := rescueSHA
	if len(short) > 12 {
		short = short[:12]
	}
	return filepath.Join(common, "gc-rescued-modules", beadID+"-"+short)
}

// Rescue secures a linked worktree's work in a local rescue ref and changes
// nothing else: HEAD, the index, the working tree, and every branch are left
// as they were, and no remote is contacted.
//
// The snapshot is built with plumbing against a temporary copy of the index,
// so no hook runs (a repository's pre-commit can fail, mutate, or hang) and no
// author identity has to be configured. When the worktree holds nothing beyond
// HEAD, the rescue is HEAD itself.
//
// Rescue is idempotent: rerunning it on an unchanged worktree returns the same
// commit and ref. It never overwrites a rescue it does not descend from; a
// divergent rescue is written beside the existing one.
func Rescue(spec RescueSpec) (RescueReport, error) {
	if spec.AbsentOK {
		if err := validateRescueSpec(spec); err != nil {
			return RescueReport{}, err
		}
		if _, err := os.Lstat(spec.Path); errors.Is(err, os.ErrNotExist) {
			return RescueReport{Path: spec.Path, BeadID: spec.BeadID, Absent: true}, nil
		}
	}
	common, err := validateRescueTarget(spec)
	if err != nil {
		return RescueReport{}, err
	}
	lock, err := lockPath(spec.Path, spec.Path)
	if err != nil {
		return RescueReport{}, err
	}
	defer lock.unlock()
	if spec.AbsentOK {
		if _, err := os.Lstat(spec.Path); errors.Is(err, os.ErrNotExist) {
			return RescueReport{Path: spec.Path, BeadID: spec.BeadID, Absent: true}, nil
		}
	}
	if common, err = revalidateLocked(spec, common); err != nil {
		return RescueReport{}, err
	}
	return rescueLocked(spec, common)
}

// afterPathLock, when set by a test, runs as soon as a Rescue or Teardown
// holds its path lock, standing in for another gc operation that changed the
// path while this one waited.
var afterPathLock func()

// revalidateLocked repeats validateRescueTarget once the path lock is held.
// The unlocked pass only finds the repository whose lock guards the path.
// Another gc operation can replace the tree while this one waits for that
// lock, so the tree that is rescued and removed must be the one validated
// under it.
func revalidateLocked(spec RescueSpec, common string) (string, error) {
	if afterPathLock != nil {
		afterPathLock()
	}
	again, err := validateRescueTarget(spec)
	if err != nil {
		return "", fmt.Errorf("%q changed while waiting for its lock: %w", spec.Path, err)
	}
	if !samePathCanonical(again, common) {
		return "", fmt.Errorf("%q changed while waiting for its lock: its repository was %s and is now %s", spec.Path, common, again)
	}
	return again, nil
}

// Teardown removes a linked worktree, but only after securing its current
// state again and finding that the result is exactly spec.RescueSHA. A
// worktree that changed since the caller recorded its rescue is secured anew
// and left in place, and the error names the new rescue so the caller can
// record it and retry. A path that no longer exists is an idempotent success.
//
// Removal refuses anything that is not a registered linked worktree: a main
// checkout, a submodule, and an unregistered directory all fail before any
// deletion.
//
// RESIDUAL, stated so nobody reads more into the check than it gives: the
// final rescue and the removal are separate steps, and the path lock excludes
// only other gc worktree operations, not editors, builds or background
// processes. A write that lands after the final snapshot is removed with the
// tree. Formula teardown runs after the body scope is terminal, from the
// session that owned the work, which keeps that window small but does not
// close it (ga-w805wc follow-up).
//
// The recorded sha binds the worktree's STATE, not its instance. Every check
// runs again under the path lock, so a tree replaced while this call waited
// is judged as it now is. But a replacement whose state secures to exactly
// the recorded sha (a fresh, clean worktree at the same commit) passes and is
// removed; it holds nothing the rescue does not.
func Teardown(spec TeardownSpec) (TeardownReport, error) {
	var report TeardownReport
	if strings.TrimSpace(spec.RescueSHA) == "" {
		return report, errors.New("teardown requires the rescue sha recorded before removal")
	}
	if _, err := os.Lstat(spec.Path); errors.Is(err, os.ErrNotExist) {
		report.AlreadyAbsent = true
		return report, nil
	}
	common, err := validateRescueTarget(spec.RescueSpec)
	if err != nil {
		return report, err
	}
	lock, err := lockPath(spec.Path, spec.Path)
	if err != nil {
		return report, err
	}
	defer lock.unlock()
	if _, err := os.Lstat(spec.Path); errors.Is(err, os.ErrNotExist) {
		report.AlreadyAbsent = true // removed by the operation this one waited for
		return report, nil
	}
	if common, err = revalidateLocked(spec.RescueSpec, common); err != nil {
		return report, err
	}

	rescue, err := rescueLocked(spec.RescueSpec, common)
	if err != nil {
		return report, err
	}
	report.Rescue = &rescue
	if !git.SameCommit(rescue.RescueSHA, spec.RescueSHA) {
		return report, fmt.Errorf(
			"worktree %q changed since its rescue was recorded: the current state is secured as %s at %s, "+
				"but the caller recorded %s; record the new rescue and retry, the worktree was left in place",
			spec.Path, rescue.RescueSHA, rescue.RescueRef, spec.RescueSHA)
	}
	preserved, err := preserveModules(spec.Path, PreservedModulesDir(common, spec.BeadID, rescue.RescueSHA))
	report.PreservedModules = preserved // set even on error: the repositories may already have moved
	if err != nil {
		return report, err
	}
	if err := removeLinkedWorktree(common, spec.Path); err != nil {
		return report, err
	}
	report.Removed = true
	return report, nil
}

// preserveModules moves the worktree's submodule repositories (its admin
// dir's modules/, which removal deletes) to dest in one rename, keeping every
// ref, stash, reflog and object verbatim. Nothing in them is analyzed or
// trusted: a submodule's branches, stash and reflog are its own state, and
// only moving the repository keeps all of it.
func preserveModules(worktreePath, dest string) (string, error) {
	adminDir, err := gitTrimmed(worktreePath, nil, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return "", err
	}
	src := filepath.Join(adminDir, "modules")
	_, srcErr := os.Lstat(src)
	_, destErr := os.Lstat(dest)
	switch {
	case errors.Is(srcErr, os.ErrNotExist) && errors.Is(destErr, os.ErrNotExist):
		return "", nil // no submodules
	case errors.Is(srcErr, os.ErrNotExist) && destErr == nil:
		return dest, nil // moved by an earlier attempt that did not finish
	case srcErr != nil:
		return "", fmt.Errorf("inspecting %s: %w", src, srcErr)
	case destErr == nil:
		return "", fmt.Errorf("both %s and %s exist; refusing to merge submodule repositories", src, dest)
	case !errors.Is(destErr, os.ErrNotExist):
		return "", fmt.Errorf("inspecting %s: %w", dest, destErr)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("preparing %s: %w", filepath.Dir(dest), err)
	}
	if err := os.Rename(src, dest); err != nil {
		return "", fmt.Errorf("preserving submodule repositories: %w", err)
	}
	return dest, detachPreservedRepos(dest)
}

// detachPreservedRepos clears core.worktree in every repository under dir.
// A submodule repository records its checkout's path there; once the
// worktree is removed that path is gone, and git refuses to open the
// repository at all ("cannot chdir"), which would make the preserved work
// unreadable to exactly the person trying to recover it.
func detachPreservedRepos(dir string) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "config" {
			return nil
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(p), "HEAD")); err != nil {
			return nil // not a git dir's config
		}
		// Run from "/": inside the git dir, git's own discovery would trip on
		// the very core.worktree being removed.
		_, err = gitOutput("/", nil, "config", "--file", p, "--unset-all", "core.worktree")
		var exitErr *exec.ExitError
		if err != nil && errors.As(err, &exitErr) && exitErr.ExitCode() == 5 {
			return nil // was not set
		}
		return err
	})
}

func validateRescueSpec(spec RescueSpec) error {
	if !filepath.IsAbs(spec.Path) {
		return fmt.Errorf("worktree path %q must be absolute", spec.Path)
	}
	if !validRescueBeadID.MatchString(spec.BeadID) || strings.Contains(spec.BeadID, "..") ||
		strings.HasSuffix(spec.BeadID, ".lock") || strings.HasSuffix(spec.BeadID, ".") {
		return fmt.Errorf("bead id %q cannot name a rescue ref", spec.BeadID)
	}
	return nil
}

// validateRescueTarget checks the spec and proves spec.Path is the root of a
// registered linked worktree, returning the repository's common git dir.
func validateRescueTarget(spec RescueSpec) (string, error) {
	if err := validateRescueSpec(spec); err != nil {
		return "", err
	}
	if info, err := os.Lstat(spec.Path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%q is a symlink; pass the worktree's real path", spec.Path)
	}
	if err := requireLinkedWorktree(spec.Path); err != nil {
		return "", err
	}
	common, err := git.New(spec.Path).CommonDir()
	if err != nil {
		return "", err
	}
	return common, nil
}

// requireLinkedWorktree proves path is the root of a linked worktree that its
// repository has registered. A main checkout's .git is a directory, but a
// submodule checkout's .git is a FILE, exactly like a linked worktree's, and
// inside the submodule `git worktree list` reports the checkout as that
// repository's main worktree. The discriminator is the git dir: a linked
// worktree's (.git/worktrees/<name>) differs from the common dir, while a main
// checkout's, submodule or not, is the common dir itself.
func requireLinkedWorktree(worktreePath string) error {
	info, err := os.Lstat(filepath.Join(worktreePath, ".git"))
	if err != nil {
		return fmt.Errorf("%q is not a linked worktree: %w", worktreePath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a linked worktree: its .git is not a regular file (a main checkout, or something else)", worktreePath)
	}
	// No separate is-this-the-root check: git resolves the worktree root to
	// the directory holding the .git file, so the Lstat above already refuses
	// a subdirectory.
	out, err := gitOutput(worktreePath, nil, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if err != nil {
		return err
	}
	fields := strings.Split(strings.TrimSpace(out), "\n")
	if len(fields) != 2 {
		return fmt.Errorf("unexpected rev-parse output for %q: %q", worktreePath, out)
	}
	gitDir, common := fields[0], fields[1]
	if samePathCanonical(gitDir, common) {
		return fmt.Errorf("%q is a main checkout of %q (a submodule, or a repository's own tree), not a linked worktree", worktreePath, common)
	}
	// The admin dir must point back at THIS tree. A directory whose .git names
	// another worktree's admin dir (a copied or recreated tree) would have that
	// other worktree's state rescued and its admin dir removed.
	back, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return fmt.Errorf("reading the admin dir back-pointer of %q: %w", worktreePath, err)
	}
	if !samePathCanonical(strings.TrimSpace(string(back)), filepath.Join(worktreePath, ".git")) {
		return fmt.Errorf("%q's .git names admin dir %q, which belongs to %q", worktreePath, gitDir, strings.TrimSpace(string(back)))
	}
	entries, err := git.New(worktreePath).WorktreeList()
	if err != nil {
		return fmt.Errorf("listing registered worktrees: %w", err)
	}
	matches := 0
	for i, entry := range entries {
		if samePathCanonical(entry.Path, worktreePath) {
			if i == 0 {
				return fmt.Errorf("%q is registered as its repository's main worktree", worktreePath)
			}
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("%q has %d worktree registrations, want exactly 1", worktreePath, matches)
	}
	return nil
}

// pathUnder reports whether p lies strictly inside dir, comparing canonical
// forms so /tmp and /private/tmp agree.
func pathUnder(p, dir string) bool {
	cp, errP := canonicalPathAllowMissing(p)
	cd, errD := canonicalPathAllowMissing(dir)
	if errP != nil || errD != nil {
		return false
	}
	return strings.HasPrefix(cp, cd+string(filepath.Separator))
}

func samePathCanonical(a, b string) bool {
	ca, errA := canonicalPathAllowMissing(a)
	cb, errB := canonicalPathAllowMissing(b)
	if errA != nil || errB != nil {
		return false
	}
	return pathutil.SamePath(ca, cb)
}

func rescueLocked(spec RescueSpec, common string) (RescueReport, error) {
	report := RescueReport{Path: spec.Path, Repo: common, BeadID: spec.BeadID}
	wt := spec.Path

	head, err := resolveHead(wt)
	if err != nil {
		return report, err
	}
	report.Head = head

	// A submodule's repository lives in this worktree's PRIVATE admin dir
	// (.git/worktrees/<name>/modules/). Teardown moves those repositories
	// aside verbatim; a submodule's uncommitted working-tree changes are in
	// neither place, so refuse them.
	adminDir, err := gitTrimmed(wt, nil, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return report, err
	}
	if err := requireSubmodulesSecured(wt, "", filepath.Join(adminDir, "modules")); err != nil {
		return report, err
	}
	if err := requireNoNestedWorktrees(wt); err != nil {
		return report, err
	}

	sediment, err := classifySediment(spec)
	if err != nil {
		return report, fmt.Errorf("classifying gc sediment (failing closed): %w", err)
	}
	report.Sediment = sediment

	snap, err := snapshotWorkTree(wt, head, sediment)
	if err != nil {
		return report, err
	}
	if err := requireNoNestedRepos(wt, snap.tree); err != nil {
		return report, err
	}
	baseTree := emptyTreeFor(wt)
	if head != "" {
		baseTree, err = gitTrimmed(wt, nil, "rev-parse", "--verify", head+"^{tree}")
		if err != nil {
			return report, err
		}
	}
	tips, err := privateRootTips(wt, head, RescueRefPrefix+spec.BeadID)
	if err != nil {
		return report, fmt.Errorf("collecting commits private to the worktree (failing closed): %w", err)
	}

	// Taint covers everything the rescue keeps: the working state, the index
	// parent (where a staged-then-deleted secret survives alone), and the
	// commits it makes durable that no remote has.
	taint := map[string]bool{}
	for _, tree := range []string{snap.tree, snap.indexTree} {
		if tree == "" || tree == baseTree {
			continue
		}
		paths, err := taintedPaths(wt, baseTree, tree)
		if err != nil {
			return report, err
		}
		for _, p := range paths {
			taint[p] = true
		}
	}
	kept := tips
	if head != "" {
		kept = append([]string{head}, tips...)
	}
	committed, err := committedTaint(wt, kept)
	if err != nil {
		return report, fmt.Errorf("scanning the kept commits for credential paths (failing closed): %w", err)
	}
	for _, p := range committed {
		taint[p] = true
	}
	for p := range taint {
		report.Taint = append(report.Taint, p)
	}
	sort.Strings(report.Taint)

	// Parents beyond HEAD keep reachable what the worktree holds nowhere
	// else: the index when it differs from both HEAD and the working state
	// (staged-only content), and an anchor over every commit that only this
	// worktree's private state reaches.
	var extra []string
	if snap.indexTree != "" && snap.indexTree != baseTree && snap.indexTree != snap.tree {
		indexCommit, err := commitIndex(wt, spec.BeadID, head, snap.indexTree)
		if err != nil {
			return report, err
		}
		extra = append(extra, indexCommit)
	}
	if len(tips) > 0 {
		anchor, err := commitAnchor(wt, spec.BeadID, head, tips)
		if err != nil {
			return report, err
		}
		extra = append(extra, anchor)
		report.PrivateCommits = tips
	}
	report.IndexUnmerged = snap.unmerged

	base := RescueRefPrefix + spec.BeadID
	var candidate string
	if head != "" && snap.tree == baseTree && len(extra) == 0 {
		candidate = head
	} else {
		report.WIP = true
		parents := extra
		if head != "" {
			parents = append([]string{head}, extra...)
		}
		reused, reuseRef, err := findReusableRescue(wt, base, parents, snap.tree)
		if err != nil {
			return report, err
		}
		if reused != "" {
			report.RescueSHA, report.RescueRef = reused, reuseRef
			return report, verifyFromCommonDir(common, report.RescueRef, report.RescueSHA)
		}
		candidate, err = commitSnapshot(wt, spec.BeadID, parents, snap.tree, report.Taint)
		if err != nil {
			return report, err
		}
	}

	ref, err := storeRescueRef(wt, base, candidate)
	if err != nil {
		return report, err
	}
	report.RescueSHA, report.RescueRef = candidate, ref
	return report, verifyFromCommonDir(common, ref, candidate)
}

// requireSubmodulesSecured refuses a worktree holding a populated submodule
// with uncommitted or untracked changes, which neither the rescue nor the
// preserved submodule repository carries. It recurses into nested submodules.
func requireSubmodulesSecured(dir, prefix, modulesRoot string) error {
	out, err := gitOutput(dir, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	for _, rec := range strings.Split(out, "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok || !strings.HasPrefix(meta, "160000 ") {
			continue
		}
		sub := filepath.Join(dir, filepath.FromSlash(p))
		name := prefix + p
		if _, err := os.Lstat(filepath.Join(sub, ".git")); errors.Is(err, os.ErrNotExist) {
			continue // not populated: nothing of it is on disk
		} else if err != nil {
			return fmt.Errorf("inspecting submodule %s: %w", name, err)
		}
		// Only a repository under this worktree's modules/ is preserved by
		// teardown. A gitlink whose repository lives anywhere else (an
		// embedded clone someone `git add`ed, whose .git is inside the tree)
		// would be deleted with the tree while the rescue keeps a pointer.
		subGitDir, err := gitTrimmed(sub, nil, "rev-parse", "--path-format=absolute", "--git-dir")
		if err != nil {
			return fmt.Errorf("locating the repository of %s: %w", name, err)
		}
		if !pathUnder(subGitDir, modulesRoot) {
			return fmt.Errorf("%s is a git repository stored at %s, not a submodule of this worktree; removal would delete it and a rescue records only a pointer, so move or push it first", name, subGitDir)
		}
		status, err := gitOutput(sub, nil, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
		if err != nil {
			return fmt.Errorf("checking submodule %s: %w", name, err)
		}
		if strings.TrimSpace(status) != "" {
			return fmt.Errorf("submodule %s has uncommitted or untracked changes; a rescue records only its gitlink and its repository is removed with the worktree, so commit and push them first", name)
		}
		if err := requireSubmodulesSecured(sub, name+"/", modulesRoot); err != nil {
			return err
		}
	}
	return nil
}

// todoCommitToken matches an object name in a sequencer or rebase todo line.
var todoCommitToken = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// perWorktreeRefDirs are the ref namespaces private to one worktree.
var perWorktreeRefDirs = []string{"refs/worktree/", "refs/bisect/", "refs/rewritten/"}

// privateRootTips returns the minimal set of commits that keep reachable
// everything ONLY this worktree's private state reaches: its HEAD reflog, the
// pseudo-refs and state files of an in-progress merge, cherry-pick, revert,
// rebase or bisect, and its per-worktree refs and their reflogs. Removal
// deletes all of it, and git counts none of it as reachable once the
// registration is gone. (Submodule repositories are not analyzed: Teardown
// preserves them verbatim, see preserveModules.)
//
// A commit already reachable from a DURABLE ref (a tag, another rescue) is
// left out; HEAD is left out because the rescue commit's first parent is HEAD.
// Remote-tracking refs do not count (they are the fetch cache, and
// `fetch --prune` drops them), and neither do local branches: merged-branch
// cleanup deletes them, and a branch deleted between rescue and teardown
// would change the anchor and force a retry. A commit also reachable
// elsewhere is merely anchored twice, which costs nothing.
func privateRootTips(wt, head, base string) ([]string, error) {
	gitDir, err := gitTrimmed(wt, nil, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return nil, err
	}
	var cands []string
	add := func(rev string) error {
		sha, err := resolveRef(wt, rev)
		if err != nil {
			return err
		}
		if sha != "" {
			cands = append(cands, sha)
		}
		return nil
	}
	for _, name := range []string{"ORIG_HEAD", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "BISECT_HEAD"} {
		if err := add(name); err != nil {
			return nil, err
		}
	}
	// A multi-commit cherry-pick, revert or interactive rebase that stopped
	// keeps the commits it has not applied yet ONLY in its todo list, not in
	// any pseudo-ref (Codex review of #97).
	for _, file := range []string{"sequencer/todo", "sequencer/head", "rebase-merge/git-rebase-todo", "rebase-merge/done"} {
		data, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(file)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			for _, tok := range strings.Fields(line) {
				if todoCommitToken.MatchString(tok) {
					if err := add(tok); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	for _, file := range []string{"rebase-merge/orig-head", "rebase-merge/stopped-sha", "rebase-apply/orig-head"} {
		data, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(file)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if rev := strings.TrimSpace(string(data)); rev != "" {
			if err := add(rev); err != nil {
				return nil, err
			}
		}
	}
	// Any other state git keeps for this worktree can name a commit only it
	// reaches: a conflicted rebase's autostash, MERGE_AUTOSTASH, whatever a
	// later git adds. Enumerating those files is how each earlier gap
	// happened (Codex review of #97, rounds 4 and 6), so every full object
	// name written in any file of the admin dir is a candidate; names that
	// are not commits of this repository drop out, and the rest are only
	// anchored, which costs nothing when they are reachable anyway.
	named, err := adminDirObjectNames(gitDir)
	if err != nil {
		return nil, fmt.Errorf("scanning %s for commits: %w", gitDir, err)
	}
	commits, err := existingCommits(wt, named)
	if err != nil {
		return nil, err
	}
	cands = append(cands, commits...)
	refs, err := gitOutput(wt, nil, append([]string{"for-each-ref", "--format=%(refname)"}, perWorktreeRefDirs...)...)
	if err != nil {
		return nil, err
	}
	logged := []string{"HEAD"}
	for _, ref := range strings.Fields(refs) {
		if err := add(ref); err != nil {
			return nil, err
		}
		logged = append(logged, ref)
	}
	for _, ref := range logged {
		if _, err := gitOutput(wt, nil, "reflog", "exists", ref); err != nil {
			continue // no reflog for this ref
		}
		out, err := gitOutput(wt, nil, "reflog", "show", "--format=%H", ref, "--")
		if err != nil {
			return nil, err
		}
		cands = append(cands, strings.Fields(out)...)
	}
	return privateTipsOf(wt, head, base, cands)
}

// adminScanMaxFile bounds one text file the admin-dir scan reads. A larger one
// fails the rescue rather than being skipped, since skipping could drop the
// only record of a commit.
const adminScanMaxFile = 64 << 20

// adminDirObjectNames returns every full object name (40 or 64 lowercase hex)
// written in a text file of a worktree's admin dir. It skips the submodule
// repositories under modules/, which teardown preserves whole, and binary
// files (the index family), whose content the snapshot records.
func adminDirObjectNames(gitDir string) ([]string, error) {
	modules := filepath.Join(gitDir, "modules")
	seen := map[string]bool{}
	var names []string
	err := filepath.WalkDir(gitDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == modules {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, binary, err := readAdminTextFile(p)
		if err != nil || binary {
			return err
		}
		for _, tok := range strings.FieldsFunc(data, func(r rune) bool {
			return (r < '0' || r > '9') && (r < 'a' || r > 'f')
		}) {
			if (len(tok) == 40 || len(tok) == 64) && !seen[tok] {
				seen[tok] = true
				names = append(names, tok)
			}
		}
		return nil
	})
	return names, err
}

// readAdminTextFile reads p unless its first 8000 bytes hold a NUL (binary,
// git's own test), refusing a text file over adminScanMaxFile.
func readAdminTextFile(p string) (data string, binary bool, err error) {
	f, err := os.Open(p)
	if err != nil {
		return "", false, err
	}
	defer f.Close() //nolint:errcheck // read-only
	head := make([]byte, 8000)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", false, err
	}
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return "", true, nil
	}
	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if info.Size() > adminScanMaxFile {
		return "", false, fmt.Errorf("%s is %d bytes, too large to scan for commits; refusing rather than skipping it", p, info.Size())
	}
	all, err := os.ReadFile(p)
	return string(all), false, err
}

// existingCommits returns the names that are commits in wt's repository. Lazy
// fetching is off: a partial clone must not turn a rescue into a remote call.
func existingCommits(wt string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out, err := gitOutputStdin(wt, []string{"GIT_NO_LAZY_FETCH=1"}, strings.Join(names, "\n")+"\n",
		"cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	var commits []string
	for _, line := range strings.Split(out, "\n") {
		if name, typ, ok := strings.Cut(line, " "); ok && typ == "commit" {
			commits = append(commits, name)
		}
	}
	return commits, nil
}

// privateTipsOf reduces candidates to the independent tips of those not
// reachable from HEAD (the rescue's first parent) or a durable local ref,
// sorted so the anchor built on them is stable.
//
// This bead's OWN rescue refs do not count as durable. They exist because an
// earlier rescue anchored these very commits; counting them would make the
// next rescue of an unchanged worktree drop the anchor, mint a different
// commit, and fail teardown's equality check forever.
func privateTipsOf(wt, head, base string, cands []string) ([]string, error) {
	seen := map[string]bool{}
	var uniq []string
	for _, c := range cands {
		if c != "" && c != head && !seen[c] {
			seen[c] = true
			uniq = append(uniq, c)
		}
	}
	if len(uniq) == 0 {
		return nil, nil
	}
	// --stdin FIRST: a --not given before it would negate the stdin revisions.
	args := []string{"rev-list", "--stdin", "--not", "--tags"}
	if head != "" {
		args = append(args, head)
	}
	args = append(args, "--exclude="+base, "--exclude="+base+"-*", "--glob=refs/rescue/*")
	out, err := gitOutputStdin(wt, nil, strings.Join(uniq, "\n")+"\n", args...)
	if err != nil {
		return nil, err
	}
	private := map[string]bool{}
	for _, sha := range strings.Fields(out) {
		private[sha] = true
	}
	var roots []string
	for _, c := range uniq {
		if private[c] {
			roots = append(roots, c)
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}
	// merge-base takes revisions as arguments only; past a few thousand the
	// command line hits the OS limit. Redundant parents are harmless, so skip
	// the reduction for very large sets rather than fail.
	if len(roots) > 1 && len(roots) <= 2000 {
		out, err = gitOutput(wt, nil, append([]string{"merge-base", "--independent"}, roots...)...)
		if err != nil {
			return nil, err
		}
		roots = strings.Fields(out)
	}
	sort.Strings(roots)
	return roots, nil
}

// anchorBatch caps the parents one anchor commit takes on its command line.
var anchorBatch = 256

// commitAnchor records the private tips as the parents of empty-tree commits
// dated like HEAD, so the same tips always yield the same anchor. Parents go
// on the command line, so large sets are folded in batches.
func commitAnchor(wt, beadID, head string, tips []string) (string, error) {
	date, err := headDate(wt, head)
	if err != nil {
		return "", err
	}
	env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	msg := fmt.Sprintf("rescue(%s): commits only this worktree's private state reached\n", beadID)
	batch := anchorBatch
	for len(tips) > batch {
		var folded []string
		for i := 0; i < len(tips); i += batch {
			end := min(i+batch, len(tips))
			a, err := commitTree(wt, emptyTreeFor(wt), tips[i:end], msg, env)
			if err != nil {
				return "", err
			}
			folded = append(folded, a)
		}
		tips = folded
	}
	return commitTree(wt, emptyTreeFor(wt), tips, msg, env)
}

func headDate(wt, head string) (string, error) {
	if head == "" {
		return "1 +0000", nil
	}
	ts, err := gitTrimmed(wt, nil, "log", "-1", "--format=%ct", head)
	if err != nil {
		return "", err
	}
	return ts + " +0000", nil
}

// requireNoNestedWorktrees refuses when another registered worktree lives
// beneath this one: removing this tree would delete it.
func requireNoNestedWorktrees(wt string) error {
	entries, err := git.New(wt).WorktreeList()
	if err != nil {
		return fmt.Errorf("listing registered worktrees: %w", err)
	}
	self, err := canonicalPathAllowMissing(wt)
	if err != nil {
		return err
	}
	for _, e := range entries {
		other, err := canonicalPathAllowMissing(e.Path)
		if err != nil {
			return fmt.Errorf("canonicalizing registered worktree %q: %w", e.Path, err)
		}
		if !strings.HasPrefix(other, self+string(filepath.Separator)) {
			continue
		}
		if _, err := os.Lstat(e.Path); errors.Is(err, os.ErrNotExist) {
			continue // a stale registration: removing this tree cannot touch it
		} else if err != nil {
			return fmt.Errorf("inspecting nested worktree %s: %w", e.Path, err)
		}
		return fmt.Errorf("registered worktree %s is nested inside %s; removing this worktree would delete it", e.Path, wt)
	}
	return nil
}

// requireNoNestedRepos refuses when the snapshot holds a gitlink the index
// does not: an untracked directory with its own .git (a nested clone or
// worktree), whose contents `git add` records only as a pointer.
func requireNoNestedRepos(wt, tree string) error {
	idx, err := gitOutput(wt, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, rec := range strings.Split(idx, "\x00") {
		if meta, p, ok := strings.Cut(rec, "\t"); ok && strings.HasPrefix(meta, "160000 ") {
			known[p] = true
		}
	}
	out, err := gitOutput(wt, nil, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return err
	}
	for _, rec := range strings.Split(out, "\x00") {
		if meta, p, ok := strings.Cut(rec, "\t"); ok && strings.HasPrefix(meta, "160000 ") && !known[p] {
			return fmt.Errorf("%s holds its own git repository; a rescue records only a pointer to it and removal would delete it, so move or push it first", p)
		}
	}
	// An IGNORED directory never reaches the snapshot, so a repository inside
	// one shows no gitlink above, yet removal deletes it with everything it
	// holds (Codex review of #97). Walk the tree itself, ignored paths
	// included. Index gitlinks are submodule checkouts, secured by
	// requireSubmodulesSecured, so the walk does not descend into them.
	return filepath.WalkDir(wt, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == wt {
			return nil
		}
		rel, err := filepath.Rel(wt, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() && known[rel] {
			return filepath.SkipDir
		}
		if d.Name() != ".git" {
			return nil
		}
		if rel == ".git" {
			return nil // this worktree's own .git file
		}
		if d.IsDir() {
			return fmt.Errorf("%s holds its own git repository (ignored or untracked); a rescue does not carry it and removal would delete it, so move or push it first", path.Dir(rel))
		}
		target, live := gitPointerTarget(p)
		if !live {
			// A pointer to nothing holds no git data. Packages ship these:
			// temporalio's wheel carries its source tree's submodule .git file.
			return nil
		}
		return fmt.Errorf("%s is a checkout of the repository at %s (ignored or untracked); removal would delete its working files, so move it or remove it first", path.Dir(rel), target)
	})
}

// gitPointerTarget reports where a .git FILE (or symlink) points, and whether
// that git dir exists. A file that is not a "gitdir:" pointer points nowhere,
// as it does for git itself.
func gitPointerTarget(p string) (string, bool) {
	info, err := os.Lstat(p)
	if err != nil {
		return "", false
	}
	var target string
	if info.Mode()&os.ModeSymlink != 0 {
		if target, err = filepath.EvalSymlinks(p); err != nil {
			return "", false
		}
	} else {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", false
		}
		rest, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
		if !ok {
			return "", false
		}
		target = strings.TrimSpace(rest)
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
	}
	if st, err := os.Stat(target); err != nil || !st.IsDir() {
		return target, false
	}
	return target, true
}

// resolveHead returns HEAD's commit, or "" for an unborn HEAD.
func resolveHead(wt string) (string, error) {
	out, err := gitOutput(wt, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err == nil {
		return strings.TrimSpace(out), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		// Distinguish an unborn HEAD from a broken one: an unborn HEAD still
		// names a branch symbolically.
		if _, symErr := gitOutput(wt, nil, "symbolic-ref", "--quiet", "HEAD"); symErr == nil {
			return "", nil
		}
	}
	return "", fmt.Errorf("resolving HEAD in %q: %w", wt, err)
}

func emptyTreeFor(wt string) string {
	out, err := gitOutputStdin(wt, nil, "", "hash-object", "-t", "tree", "--stdin")
	if err != nil {
		return "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	}
	return strings.TrimSpace(out)
}

// classifySediment returns the untracked paths gc provably put in the
// worktree. Only UNTRACKED entries qualify, and each must be proven rather
// than guessed from its path: a tracked edit under .claude/ or .beads/ is real
// work (repositories track skills and formulas there), and so is an authored
// untracked file or symlink. An untracked entry is sediment only when it is:
//
//   - a skill symlink <sink>/<name> recorded in that sink's ownership
//     manifest, or the manifest itself;
//   - a regular file under a sink that is byte-identical to the same path
//     under the city (city-synced copies);
//   - AGENTS-gc.md or .worktree-stale at the worktree root.
//
// Any failure is returned, never an empty list: an empty list would secure
// sediment as work, which is harmless locally, but a failure to read the
// status must not be mistaken for a clean tree either.
func classifySediment(spec RescueSpec) ([]string, error) {
	out, err := gitOutput(spec.Path, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	sinks := make(map[string]bool, len(spec.SkillSinks))
	for _, s := range spec.SkillSinks {
		sinks[path.Clean(filepath.ToSlash(s))] = true
	}
	manifests := map[string]map[string]string{}
	loadManifest := func(sink string) (map[string]string, error) {
		if m, ok := manifests[sink]; ok {
			return m, nil
		}
		data, err := os.ReadFile(filepath.Join(spec.Path, filepath.FromSlash(sink), SkillOwnershipManifest))
		if errors.Is(err, os.ErrNotExist) {
			manifests[sink] = nil
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s ownership manifest: %w", sink, err)
		}
		var m struct {
			Targets map[string]string `json:"targets"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("parsing %s ownership manifest: %w", sink, err)
		}
		manifests[sink] = m.Targets
		return m.Targets, nil
	}

	var sediment []string
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		code, p := entry[:2], entry[3:]
		if code[0] == 'R' || code[0] == 'C' || code[1] == 'R' || code[1] == 'C' {
			i++ // a rename or copy carries its source as the next record
			continue
		}
		if code != "??" {
			continue
		}
		switch p {
		case "AGENTS-gc.md", ".worktree-stale":
			sediment = append(sediment, p)
			continue
		}
		sink, rest, ok := splitSinkPath(p, sinks)
		if !ok {
			continue
		}
		if rest == SkillOwnershipManifest {
			sediment = append(sediment, p)
			continue
		}
		full := filepath.Join(spec.Path, filepath.FromSlash(p))
		info, err := os.Lstat(full)
		if err != nil {
			return nil, fmt.Errorf("inspecting %s: %w", p, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if strings.Contains(rest, "/") {
				continue
			}
			targets, err := loadManifest(sink)
			if err != nil {
				return nil, err
			}
			recorded, named := targets[rest]
			if !named {
				continue
			}
			// The name alone is not proof: a link someone repointed after
			// the materializer recorded it is theirs. Compare the target the
			// way the materializer records it (absolute, normalized).
			link, err := os.Readlink(full)
			if err != nil {
				return nil, fmt.Errorf("reading symlink %s: %w", p, err)
			}
			if filepath.IsAbs(link) && pathutil.NormalizePathForCompare(link) == recorded {
				sediment = append(sediment, p)
			}
			continue
		}
		if info.Mode().IsRegular() && spec.CityPath != "" {
			same, err := sameFileContent(full, filepath.Join(spec.CityPath, filepath.FromSlash(p)))
			if err != nil {
				return nil, err
			}
			if same {
				sediment = append(sediment, p)
			}
		}
	}
	sort.Strings(sediment)
	return sediment, nil
}

// splitSinkPath reports whether p lies under one of the sink directories and
// returns that sink and the remainder of the path.
func splitSinkPath(p string, sinks map[string]bool) (sink, rest string, ok bool) {
	for s := range sinks {
		if strings.HasPrefix(p, s+"/") {
			return s, strings.TrimPrefix(p, s+"/"), true
		}
	}
	return "", "", false
}

// sameFileContent reports whether two paths hold identical bytes. A missing
// city copy is simply "not the same"; any other read failure is an error.
func sameFileContent(a, b string) (bool, error) {
	bInfo, err := os.Stat(b)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspecting city copy %s: %w", b, err)
	}
	if !bInfo.Mode().IsRegular() {
		return false, nil
	}
	aData, err := os.ReadFile(a)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", a, err)
	}
	bData, err := os.ReadFile(b)
	if err != nil {
		return false, fmt.Errorf("reading city copy %s: %w", b, err)
	}
	return bytes.Equal(aData, bData), nil
}

// workSnapshot is a worktree's state as tree objects.
type workSnapshot struct {
	// tree is the full working state, minus sediment.
	tree string
	// indexTree is the index exactly as git held it, or "" when it has
	// unmerged entries (the conflicted content is in tree, and the operation's
	// heads are kept as parents).
	indexTree string
	unmerged  bool
}

// snapshotWorkTree writes the worktree's state as tree objects. It stages into
// a temporary COPY of the worktree's index, beside the real one so a split
// index still resolves, and leaves the real index alone.
//
// Before staging it records the index as-is: content that is staged but no
// longer in the working tree exists nowhere else. It then clears the two
// index bits that make `git add` skip real edits (assume-unchanged on any
// path; skip-worktree on a path that is present on disk) and stages with
// --sparse, so an edited file outside a sparse-checkout definition is kept
// while an absent one is not recorded as deleted. fsmonitor is bypassed so a
// stale daemon cannot hide a change.
func snapshotWorkTree(wt, head string, sediment []string) (workSnapshot, error) {
	var snap workSnapshot
	indexPath, err := gitTrimmed(wt, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return snap, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(indexPath), "gc-rescue-index-*")
	if err != nil {
		return snap, fmt.Errorf("creating temporary index: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort temp cleanup
	env := []string{"GIT_INDEX_FILE=" + tmpPath}

	src, err := os.Open(indexPath)
	switch {
	case err == nil:
		_, copyErr := io.Copy(tmp, src)
		_ = src.Close()
		if closeErr := tmp.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return snap, fmt.Errorf("copying index: %w", copyErr)
		}
	case errors.Is(err, os.ErrNotExist):
		_ = tmp.Close()
		// An empty file is not a valid index; git creates a fresh one only
		// when the path does not exist.
		if err := os.Remove(tmpPath); err != nil {
			return snap, fmt.Errorf("preparing temporary index: %w", err)
		}
		if head != "" {
			if _, err := gitOutput(wt, env, "read-tree", head); err != nil {
				return snap, err
			}
		}
	default:
		_ = tmp.Close()
		return snap, fmt.Errorf("opening index: %w", err)
	}

	unmerged, err := gitOutput(wt, env, "ls-files", "--unmerged")
	if err != nil {
		return snap, err
	}
	if strings.TrimSpace(unmerged) != "" {
		snap.unmerged = true
	} else if snap.indexTree, err = gitTrimmed(wt, env, "write-tree"); err != nil {
		return snap, err
	}

	if err := clearSkipBits(wt, env); err != nil {
		return snap, err
	}
	if _, err := gitOutput(wt, env, "-c", "core.fsmonitor=false", "add", "-A", "--sparse", "--", "."); err != nil {
		return snap, err
	}
	if len(sediment) > 0 {
		unstage := append([]string{"GIT_LITERAL_PATHSPECS=1"}, env...)
		list := strings.Join(sediment, "\x00") + "\x00"
		if _, err := gitOutputStdin(wt, unstage, list,
			"rm", "--cached", "-q", "--ignore-unmatch", "-r", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return snap, err
		}
	}
	snap.tree, err = gitTrimmed(wt, env, "write-tree")
	return snap, err
}

// clearSkipBits clears, in the index env names, the assume-unchanged bit on
// every entry and the skip-worktree bit on every entry whose file is present,
// so `git add` compares their content instead of trusting the index.
func clearSkipBits(wt string, env []string) error {
	out, err := gitOutput(wt, env, "ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	var assumed, skipped []string
	for _, rec := range strings.Split(out, "\x00") {
		if len(rec) < 3 {
			continue
		}
		tag, p := rec[0], rec[2:]
		if tag >= 'a' && tag <= 'z' {
			assumed = append(assumed, p)
		}
		if tag == 'S' || tag == 's' {
			if _, err := os.Lstat(filepath.Join(wt, filepath.FromSlash(p))); err == nil {
				skipped = append(skipped, p)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspecting %s: %w", p, err)
			}
		}
	}
	for flag, paths := range map[string][]string{"--no-assume-unchanged": assumed, "--no-skip-worktree": skipped} {
		if len(paths) == 0 {
			continue
		}
		list := strings.Join(paths, "\x00") + "\x00"
		if _, err := gitOutputStdin(wt, env, list, "update-index", "-z", flag, "--stdin"); err != nil {
			return err
		}
	}
	return nil
}

// credentialNames matches file names that usually hold secrets.
var credentialNames = regexp.MustCompile(
	`^(\.env(\..*)?|.*\.(pem|key|p12|pfx|keystore|jks)|id_(rsa|dsa|ecdsa|ed25519)(\.pub)?|\.netrc|\.pgpass|credentials(\..*)?)$`)

func taintedPaths(wt, fromTree, toTree string) ([]string, error) {
	out, err := gitOutput(wt, nil, "diff-tree", "-r", "--no-commit-id", "--name-only", "--diff-filter=AM", "-z", fromTree, toTree)
	if err != nil {
		return nil, err
	}
	var taint []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" && credentialNames.MatchString(path.Base(p)) {
			taint = append(taint, p)
		}
	}
	sort.Strings(taint)
	return taint, nil
}

// committedTaint lists credential-shaped paths that the commits reachable from
// tips add or modify, over the commits no remote-tracking ref reaches. A clean
// worktree on a local-only commit has nothing beyond HEAD, yet the rescue makes
// that commit durable, and whatever later pushes the rescue would publish it.
// A tracking ref that lags its remote only widens the scan. One the remote has
// since dropped excludes commits that were published once, which a later push
// does not newly expose. With no remote-tracking refs at all, every reachable
// commit is scanned.
func committedTaint(wt string, tips []string) ([]string, error) {
	if len(tips) == 0 {
		return nil, nil
	}
	revs, err := gitOutput(wt, nil, append(append([]string{"rev-list"}, tips...), "--not", "--remotes")...)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(revs) == "" {
		return nil, nil
	}
	// -m shows a merge against each parent and --root a root commit as a
	// creation; each commit's id is printed among the names and never matches.
	out, err := gitOutputStdin(wt, nil, revs, "diff-tree", "--stdin", "-r", "-m", "--root", "--no-renames", "--name-only", "-z", "--diff-filter=AM")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var taint []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" && !seen[p] && credentialNames.MatchString(path.Base(p)) {
			seen[p] = true
			taint = append(taint, p)
		}
	}
	sort.Strings(taint)
	return taint, nil
}

// findReusableRescue returns an existing rescue of exactly this state (same
// tree, same parents) so a retry does not mint a second commit that differs
// only in its timestamp.
func findReusableRescue(wt, base string, parents []string, tree string) (sha, ref string, err error) {
	out, err := gitOutput(wt, nil, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(tree)%00%(parent)", base, base+"-*")
	if err != nil {
		return "", "", err
	}
	want := strings.Join(parents, " ")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, "\x00")
		if len(fields) != 4 || !isOwnRescueRef(base, fields[0]) {
			continue // the glob also matches bead <base>-x's refs
		}
		if fields[2] == tree && fields[3] == want {
			return fields[1], fields[0], nil
		}
	}
	return "", "", nil
}

// siblingSuffix matches the "-<sha12>" a divergent rescue ref carries.
var siblingSuffix = regexp.MustCompile(`^-[0-9a-f]{12}$`)

func isOwnRescueRef(base, ref string) bool {
	return ref == base || (strings.HasPrefix(ref, base) && siblingSuffix.MatchString(ref[len(base):]))
}

func commitSnapshot(wt, beadID string, parents []string, tree string, taint []string) (string, error) {
	msg := fmt.Sprintf("rescue(%s): work secured by gc worktree rescue before teardown\n\nRescue-Bead: %s\n", beadID, beadID)
	if len(taint) > 0 {
		msg += "Rescue-Taint: " + strings.Join(taint, ", ") + "\n"
	}
	return commitTree(wt, tree, parents, msg, nil)
}

// commitIndex records the index as a commit on HEAD. Its date is HEAD's, so
// the same index always yields the same commit and a retry can reuse the
// rescue that has it as a parent.
func commitIndex(wt, beadID, head, tree string) (string, error) {
	date, err := headDate(wt, head)
	if err != nil {
		return "", err
	}
	var parents []string
	if head != "" {
		parents = []string{head}
	}
	msg := fmt.Sprintf("rescue(%s): index as staged at teardown\n", beadID)
	return commitTree(wt, tree, parents, msg, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date})
}

func commitTree(wt, tree string, parents []string, msg string, extraEnv []string) (string, error) {
	args := []string{"-c", "commit.gpgSign=false", "commit-tree", tree, "-F", "-"}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	env := append([]string{
		"GIT_AUTHOR_NAME=" + rescueIdentityName, "GIT_AUTHOR_EMAIL=" + rescueIdentityEmail,
		"GIT_COMMITTER_NAME=" + rescueIdentityName, "GIT_COMMITTER_EMAIL=" + rescueIdentityEmail,
	}, extraEnv...)
	out, err := gitOutputStdin(wt, env, msg, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// storeRescueRef makes candidate reachable from a rescue ref without ever
// moving a ref to a commit that does not descend from its current value.
func storeRescueRef(wt, base, candidate string) (string, error) {
	existing, err := resolveRef(wt, base)
	if err != nil {
		return "", err
	}
	switch existing {
	case "":
		return base, createRef(wt, base, candidate)
	case candidate:
		return base, nil
	}
	contained, err := isAncestor(wt, candidate, existing)
	if err != nil {
		return "", err
	}
	if contained {
		return base, nil
	}
	fastForward, err := isAncestor(wt, existing, candidate)
	if err != nil {
		return "", err
	}
	if fastForward {
		msg := "gc worktree rescue: advance"
		_, err := gitOutput(wt, nil, "update-ref", "-m", msg, base, candidate, existing)
		return base, err
	}
	sibling := base + "-" + candidate[:12]
	current, err := resolveRef(wt, sibling)
	if err != nil {
		return "", err
	}
	if current == candidate {
		return sibling, nil
	}
	if current != "" {
		return "", fmt.Errorf("rescue ref %s already holds %s; refusing to overwrite it with %s", sibling, current, candidate)
	}
	return sibling, createRef(wt, sibling, candidate)
}

func resolveRef(wt, ref string) (string, error) {
	out, err := gitOutput(wt, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err == nil {
		return strings.TrimSpace(out), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return "", nil
	}
	return "", err
}

// createRef creates ref only if it does not already exist (an empty old value
// is git's create-only compare-and-swap).
func createRef(wt, ref, sha string) error {
	_, err := gitOutput(wt, nil, "update-ref", "-m", "gc worktree rescue", ref, sha, "")
	return err
}

// verifyFromCommonDir re-reads the rescue ref through the common git dir,
// proving it landed in the shared ref store rather than in state private to
// the worktree about to be removed.
func verifyFromCommonDir(common, ref, sha string) error {
	cmd := exec.Command("git", "--git-dir="+common, "merge-base", "--is-ancestor", sha, ref)
	cmd.Env = git.SanitizedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rescue %s is not reachable from %s in %s: %s: %w", sha, ref, common, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// removeLinkedWorktree removes a worktree its caller has already proven is a
// registered linked worktree, and nothing else. It never runs a repo-wide
// `git worktree prune`: another worktree's stale registration is still a
// reachability root for git, and pruning it would finish off work lost to an
// older teardown. The recursive delete is a fallback for trees git refuses to
// remove (one containing submodules); it re-proves the target first and then
// removes only THIS worktree's own admin directory.
func removeLinkedWorktree(common, worktreePath string) error {
	adminDir, err := gitTrimmed(worktreePath, nil, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return err
	}
	worktreesDir := filepath.Join(common, "worktrees") + string(filepath.Separator)
	if !strings.HasPrefix(adminDir, worktreesDir) {
		if c, cerr := canonicalPathAllowMissing(adminDir); cerr == nil {
			if w, werr := canonicalPathAllowMissing(filepath.Join(common, "worktrees")); werr == nil {
				adminDir, worktreesDir = c, w+string(filepath.Separator)
			}
		}
	}
	if !strings.HasPrefix(adminDir, worktreesDir) || strings.Contains(strings.TrimPrefix(adminDir, worktreesDir), string(filepath.Separator)) {
		return fmt.Errorf("admin dir %q of %q is not directly under %q; refusing to remove", adminDir, worktreePath, worktreesDir)
	}
	_, removeErr := gitOutput(common, nil, "worktree", "remove", "--force", "--force", worktreePath)
	if removeErr != nil {
		if err := requireLinkedWorktree(worktreePath); err != nil {
			return fmt.Errorf("git worktree remove failed (%w) and the recursive fallback is refused: %w", removeErr, err)
		}
		if err := os.RemoveAll(worktreePath); err != nil {
			return fmt.Errorf("git worktree remove failed (%w) and so did the recursive fallback: %w", removeErr, err)
		}
		if err := os.RemoveAll(adminDir); err != nil {
			return fmt.Errorf("removed %q but not its admin dir %q: %w", worktreePath, adminDir, err)
		}
	}
	if _, err := os.Lstat(worktreePath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("worktree %q still exists after removal", worktreePath)
	}
	if _, err := os.Lstat(adminDir); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("admin dir %q of removed worktree %q still exists", adminDir, worktreePath)
	}
	return nil
}

func gitTrimmed(dir string, env []string, args ...string) (string, error) {
	out, err := gitOutput(dir, env, args...)
	return strings.TrimSpace(out), err
}

func gitOutput(dir string, env []string, args ...string) (string, error) {
	return gitOutputStdin(dir, env, "", args...)
}

// gitOutputStdin runs git in dir with a sanitized environment plus env,
// feeding stdin, and returns stdout. The error carries stderr.
func gitOutputStdin(dir string, env []string, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(git.SanitizedEnv(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &gitError{args: args, stderr: strings.TrimSpace(stderr.String()), err: err}
	}
	return stdout.String(), nil
}

type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %s: %v", strings.Join(e.args, " "), e.stderr, e.err)
}

func (e *gitError) Unwrap() error { return e.err }
