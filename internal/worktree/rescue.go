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
	common, err := validateRescueTarget(spec)
	if err != nil {
		return RescueReport{}, err
	}
	lock, err := lockPath(spec.Path, spec.Path)
	if err != nil {
		return RescueReport{}, err
	}
	defer lock.unlock()
	return rescueLocked(spec, common)
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
	if err := removeLinkedWorktree(common, spec.Path); err != nil {
		return report, err
	}
	report.Removed = true
	return report, nil
}

// validateRescueTarget checks the spec and proves spec.Path is the root of a
// registered linked worktree, returning the repository's common git dir.
func validateRescueTarget(spec RescueSpec) (string, error) {
	if !filepath.IsAbs(spec.Path) {
		return "", fmt.Errorf("worktree path %q must be absolute", spec.Path)
	}
	if !validRescueBeadID.MatchString(spec.BeadID) || strings.Contains(spec.BeadID, "..") ||
		strings.HasSuffix(spec.BeadID, ".lock") || strings.HasSuffix(spec.BeadID, ".") {
		return "", fmt.Errorf("bead id %q cannot name a rescue ref", spec.BeadID)
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

	sediment, err := classifySediment(spec)
	if err != nil {
		return report, fmt.Errorf("classifying gc sediment (failing closed): %w", err)
	}
	report.Sediment = sediment

	tree, err := snapshotWorkTree(wt, head, sediment)
	if err != nil {
		return report, err
	}
	baseTree := emptyTreeFor(wt)
	if head != "" {
		baseTree, err = gitTrimmed(wt, nil, "rev-parse", "--verify", head+"^{tree}")
		if err != nil {
			return report, err
		}
	}
	if tree != baseTree {
		taint, err := taintedPaths(wt, baseTree, tree)
		if err != nil {
			return report, err
		}
		report.Taint = taint
	}

	base := RescueRefPrefix + spec.BeadID
	var candidate string
	switch {
	case head != "" && tree == baseTree:
		candidate = head
	default:
		report.WIP = true
		reused, reuseRef, err := findReusableRescue(wt, base, head, tree)
		if err != nil {
			return report, err
		}
		if reused != "" {
			report.RescueSHA, report.RescueRef = reused, reuseRef
			return report, verifyFromCommonDir(common, report.RescueRef, report.RescueSHA)
		}
		candidate, err = commitSnapshot(wt, spec.BeadID, head, tree, report.Taint)
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
			if _, owned := targets[rest]; owned {
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

// snapshotWorkTree writes the worktree's full state, minus sediment, as a tree
// object. It stages into a temporary COPY of the worktree's index, beside the
// real one so a split index still resolves, and leaves the real index alone.
// Starting from the index git itself uses keeps its skip-worktree bits and
// staged state. (A sparse checkout's absent paths are not recorded as
// deletions either way on current git, whose `add` refuses paths outside the
// sparse definition; a test pins that outcome, not this mechanism.)
func snapshotWorkTree(wt, head string, sediment []string) (string, error) {
	indexPath, err := gitTrimmed(wt, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(indexPath), "gc-rescue-index-*")
	if err != nil {
		return "", fmt.Errorf("creating temporary index: %w", err)
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
			return "", fmt.Errorf("copying index: %w", copyErr)
		}
	case errors.Is(err, os.ErrNotExist):
		_ = tmp.Close()
		// An empty file is not a valid index; git creates a fresh one only
		// when the path does not exist.
		if err := os.Remove(tmpPath); err != nil {
			return "", fmt.Errorf("preparing temporary index: %w", err)
		}
		if head != "" {
			if _, err := gitOutput(wt, env, "read-tree", head); err != nil {
				return "", err
			}
		}
	default:
		_ = tmp.Close()
		return "", fmt.Errorf("opening index: %w", err)
	}

	if _, err := gitOutput(wt, env, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	if len(sediment) > 0 {
		unstage := append([]string{"GIT_LITERAL_PATHSPECS=1"}, env...)
		list := strings.Join(sediment, "\x00") + "\x00"
		if _, err := gitOutputStdin(wt, unstage, list,
			"rm", "--cached", "-q", "--ignore-unmatch", "-r", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return "", err
		}
	}
	return gitTrimmed(wt, env, "write-tree")
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

// findReusableRescue returns an existing rescue of exactly this state (same
// tree, same parent) so a retry does not mint a second commit that differs
// only in its timestamp.
func findReusableRescue(wt, base, head, tree string) (sha, ref string, err error) {
	out, err := gitOutput(wt, nil, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(tree)%00%(parent)", base, base+"-*")
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, "\x00")
		if len(fields) != 4 {
			continue
		}
		if fields[2] == tree && fields[3] == head {
			return fields[1], fields[0], nil
		}
	}
	return "", "", nil
}

func commitSnapshot(wt, beadID, head, tree string, taint []string) (string, error) {
	msg := fmt.Sprintf("rescue(%s): work secured by gc worktree rescue before teardown\n\nRescue-Bead: %s\n", beadID, beadID)
	if len(taint) > 0 {
		msg += "Rescue-Taint: " + strings.Join(taint, ", ") + "\n"
	}
	args := []string{"commit-tree", tree, "-F", "-"}
	if head != "" {
		args = append(args, "-p", head)
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + rescueIdentityName, "GIT_AUTHOR_EMAIL=" + rescueIdentityEmail,
		"GIT_COMMITTER_NAME=" + rescueIdentityName, "GIT_COMMITTER_EMAIL=" + rescueIdentityEmail,
	}
	out, err := gitOutputStdin(wt, env, msg, append([]string{"-c", "commit.gpgSign=false"}, args...)...)
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
// registered linked worktree. The recursive delete is a fallback for trees git
// refuses to remove (one containing submodules), and it re-proves the target
// immediately before deleting.
func removeLinkedWorktree(common, worktreePath string) error {
	_, removeErr := gitOutput(common, nil, "worktree", "remove", "--force", "--force", worktreePath)
	if removeErr != nil {
		if err := requireLinkedWorktree(worktreePath); err != nil {
			return fmt.Errorf("git worktree remove failed (%w) and the recursive fallback is refused: %w", removeErr, err)
		}
		if err := os.RemoveAll(worktreePath); err != nil {
			return fmt.Errorf("git worktree remove failed (%w) and so did the recursive fallback: %w", removeErr, err)
		}
	}
	if _, err := gitOutput(common, nil, "worktree", "prune"); err != nil {
		return err
	}
	if _, err := os.Lstat(worktreePath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("worktree %q still exists after removal", worktreePath)
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
