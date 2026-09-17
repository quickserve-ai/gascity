package packman

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	gitutil "github.com/gastownhall/gascity/internal/git"
)

// CheckSeverity classifies an import state validation issue.
type CheckSeverity string

const (
	// CheckSeverityError means the import state is not usable as-is.
	CheckSeverityError CheckSeverity = "error"
	// CheckSeverityWarning means a check could not reach a verdict. It exists
	// so "I could not tell" never renders identically to "I checked and it is
	// bad" -- an offline network probe must not read as a broken import.
	CheckSeverityWarning CheckSeverity = "warning"
	// CheckSeverityNotice means a check DID reach a verdict and the import is
	// still usable: a definite finding about state the operator did not choose
	// and would not otherwise see. It is neither of the other two -- reporting
	// it as an error would fail the command for a supported configuration,
	// which is how a check ends up ignored, and reporting it as a warning
	// would claim no verdict was reached when one was. It does not count
	// toward ErrorCount, so it never changes an exit code; it exists so
	// "Import state OK" stops being printed over a finding.
	CheckSeverityNotice CheckSeverity = "notice"
)

// CheckIssue describes one read-only import state validation finding.
type CheckIssue struct {
	Severity   CheckSeverity
	Code       string
	ImportName string
	Source     string
	Commit     string
	Path       string
	Message    string
	RepairHint string
}

// CheckReport summarizes the read-only validation of a city's import state.
type CheckReport struct {
	CheckedSources int
	Issues         []CheckIssue
	// CheckedPins maps each declared remote source to the commit it is pinned
	// to, as this run understood it. VerifySourceReachability consumes it so
	// the network probe interrogates exactly the pins the offline check
	// validated, instead of re-deriving them and drifting from this walk.
	CheckedPins map[string]string
}

// ErrorCount returns the number of error-severity issues in the report.
func (r *CheckReport) ErrorCount() int {
	if r == nil {
		return 0
	}
	count := 0
	for _, issue := range r.Issues {
		if issue.Severity == CheckSeverityError {
			count++
		}
	}
	return count
}

// HasIssues reports whether validation found any issue.
func (r *CheckReport) HasIssues() bool {
	return r != nil && len(r.Issues) > 0
}

// CheckInstalled validates that declared remote imports are represented by
// packs.lock and by already-materialized local cache entries. It does not
// resolve versions, clone repositories, fetch, or mutate disk state.
func CheckInstalled(cityRoot string, imports map[string]config.Import) (*CheckReport, error) {
	report := &CheckReport{}

	lockExists, err := lockfileExists(cityRoot)
	if err != nil {
		return nil, err
	}
	lock, err := ReadLockfile(fsys.OSFS{}, cityRoot)
	if err != nil {
		return nil, err
	}

	if !lockExists && countRemoteImports(imports) > 0 {
		report.addIssue(CheckIssue{
			Code:       "missing-lockfile",
			Path:       filepath.Join(cityRoot, LockfileName),
			Message:    fmt.Sprintf("%s is missing for declared remote imports", LockfileName),
			RepairHint: `run "gc import install"`,
		})
		// The closure walk is skipped here, so nothing downstream would learn
		// what this city is pinned to -- and a city with no lockfile at all is
		// the fresh-bootstrap case a source-reachability probe most needs to
		// answer for. Record what the declaration itself states. Nested
		// imports are genuinely unknowable at this point: they live inside
		// cached packs that have never been fetched.
		recordDeclaredShaPins(report, imports)
		return report, nil
	}

	if countRemoteImports(imports) > 0 || len(lock.Packs) > 0 {
		if err := withRepoCacheReadLock(func() error {
			checkLockedImports(report, lock, imports, cityRoot)
			return nil
		}); err != nil {
			return nil, err
		}
	} else {
		checkLockedImports(report, lock, imports, cityRoot)
	}
	return report, nil
}

func checkLockedImports(report *CheckReport, lock *Lockfile, imports map[string]config.Import, cityRoot string) {
	state := &importCheckState{
		lock:              lock,
		report:            report,
		cityRoot:          cityRoot,
		constraints:       make(map[string]string),
		reachable:         make(map[string]struct{}),
		seen:              make(map[string]bool),
		reportedIssueKeys: make(map[string]struct{}),
	}

	names := sortedImportNames(imports)
	for _, name := range names {
		state.walkImport(name, imports[name], cityRoot)
	}

	state.reportStaleLockEntries()
}

type importCheckState struct {
	lock              *Lockfile
	report            *CheckReport
	cityRoot          string
	constraints       map[string]string
	reachable         map[string]struct{}
	seen              map[string]bool
	reportedIssueKeys map[string]struct{}
	closureIncomplete bool
}

// walkImport walks one import. declDir is the directory a relative local-path
// source is resolved against: the city root for top-level imports, and the
// declaring pack's own directory for nested imports.
func (s *importCheckState) walkImport(name string, imp config.Import, declDir string) {
	if !isRemoteSource(imp.Source) {
		s.walkLocalImport(name, imp, declDir)
		return
	}

	mergedConstraint, err := mergeConstraints(s.constraints[imp.Source], imp.Version)
	if err != nil {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "conflicting-constraints",
			ImportName: name,
			Source:     imp.Source,
			Message:    fmt.Sprintf("import constraints cannot be merged: %v", err),
			RepairHint: "edit imports to use compatible version constraints",
		})
		return
	}
	s.constraints[imp.Source] = mergedConstraint
	if _, ok := s.reachable[imp.Source]; !ok {
		s.report.CheckedSources++
	}
	s.reachable[imp.Source] = struct{}{}
	if declared, ok := strings.CutPrefix(mergedConstraint, "sha:"); ok {
		s.report.recordPin(imp.Source, declared)
	}

	locked, ok := s.lock.Packs[imp.Source]
	if !ok {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "missing-lock-entry",
			ImportName: name,
			Source:     imp.Source,
			Message:    "declared remote import is not present in packs.lock",
			RepairHint: `run "gc import install"`,
		})
		return
	}
	if strings.TrimSpace(locked.Commit) == "" {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "missing-lock-commit",
			ImportName: name,
			Source:     imp.Source,
			Message:    "packs.lock entry is missing a commit",
			RepairHint: `run "gc import install"`,
		})
		return
	}
	if !matchesExisting(locked, mergedConstraint) {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "lock-constraint-mismatch",
			ImportName: name,
			Source:     imp.Source,
			Commit:     locked.Commit,
			Message:    fmt.Sprintf("packs.lock entry version %q does not satisfy constraint %q", locked.Version, mergedConstraint),
			RepairHint: `run "gc import install"`,
		})
		return
	}

	s.report.recordPin(imp.Source, locked.Commit)

	packDir, ok := s.validateCachedPack(name, imp.Source, locked.Commit)
	if !ok {
		return
	}
	s.checkEmbeddedPackDivergence(name, imp.Source, locked.Commit, packDir)
	nested, err := readPackImports(packDir)
	if err != nil {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "invalid-cached-pack",
			ImportName: name,
			Source:     imp.Source,
			Commit:     locked.Commit,
			Path:       filepath.Join(packDir, "pack.toml"),
			Message:    err.Error(),
			RepairHint: `run "gc import install"`,
		})
		return
	}
	if !imp.ImportIsTransitive() {
		return
	}
	if s.seen[imp.Source] {
		return
	}
	s.seen[imp.Source] = true
	// A remote pack's nested relative local import (if any) resolves under
	// the cached checkout, not the city root.
	for _, nestedName := range sortedImportNames(nested) {
		s.walkImport(name+"/"+nestedName, nested[nestedName], packDir)
	}
}

// walkLocalImport handles a local path-source import. Unlike a remote
// import, it is never locked or cached, so it can't produce a
// missing-lock-entry issue for itself — but its own declared imports still
// need walking so a missing transitive remote import is caught here rather
// than surfacing later as a load-time "not installed" error. A relative
// source resolves against declDir, not the process working directory.
func (s *importCheckState) walkLocalImport(name string, imp config.Import, declDir string) {
	if !imp.ImportIsTransitive() {
		return
	}
	srcDir := imp.Source
	if !filepath.IsAbs(srcDir) {
		srcDir = filepath.Join(declDir, srcDir)
	}
	if s.seen[srcDir] {
		return
	}
	s.seen[srcDir] = true
	nested, err := readPackImports(srcDir)
	if err != nil {
		// A local path source that isn't materialized on disk yet has no
		// transitive imports to discover -- not a hard error. Only a
		// pack.toml that exists but fails to parse is a genuine problem.
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "invalid-local-pack",
			ImportName: name,
			Source:     imp.Source,
			Path:       filepath.Join(srcDir, "pack.toml"),
			Message:    err.Error(),
		})
		return
	}
	for _, nestedName := range sortedImportNames(nested) {
		s.walkImport(name+"/"+nestedName, nested[nestedName], srcDir)
	}
}

func (s *importCheckState) validateCachedPack(name, source, commit string) (string, bool) {
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "invalid-cache-path",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Message:    err.Error(),
			RepairHint: `run "gc import install"`,
		})
		return "", false
	}

	if repository, known := builtinpacks.RepositoryForSource(source); known && config.IsBundledSourceAtCanonicalPin(source, commit) {
		if err := builtinpacks.ValidateSyntheticRepo(cachePath, repository, commit); err != nil {
			gitInfo, gitErr := os.Stat(filepath.Join(cachePath, ".git"))
			if gitErr == nil && !gitutil.MissingCheckoutMarker(gitInfo, gitErr) {
				if !s.validateCachedGitCheckout(name, source, commit, cachePath) {
					return "", false
				}
			} else {
				if gitErr != nil && !gitutil.MissingCheckoutMarker(gitInfo, gitErr) {
					s.closureIncomplete = true
					s.addIssue(CheckIssue{
						Code:       "unreadable-cache",
						ImportName: name,
						Source:     source,
						Commit:     commit,
						Path:       filepath.Join(cachePath, ".git"),
						Message:    fmt.Sprintf("cannot inspect cached repository: %v; synthetic cache is invalid: %v", gitErr, err),
						RepairHint: `run "gc import install"`,
					})
					return "", false
				}
				s.closureIncomplete = true
				s.addIssue(CheckIssue{
					Code:       "invalid-synthetic-cache",
					ImportName: name,
					Source:     source,
					Commit:     commit,
					Path:       cachePath,
					Message:    fmt.Sprintf("synthetic cache is invalid: %v", err),
					RepairHint: `run "gc import install"`,
				})
				return "", false
			}
		}
	} else if !s.validateCachedGitCheckout(name, source, commit, cachePath) {
		return "", false
	}

	packDir := cachedPackDir(source, cachePath)
	if st, err := os.Stat(filepath.Join(packDir, "pack.toml")); err != nil {
		if os.IsNotExist(err) {
			s.closureIncomplete = true
			s.addIssue(CheckIssue{
				Code:       "missing-cached-pack",
				ImportName: name,
				Source:     source,
				Commit:     commit,
				Path:       filepath.Join(packDir, "pack.toml"),
				Message:    "cached import is missing pack.toml",
				RepairHint: `run "gc import install"`,
			})
			return "", false
		}
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "unreadable-cached-pack",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       filepath.Join(packDir, "pack.toml"),
			Message:    fmt.Sprintf("cannot inspect cached pack.toml: %v", err),
			RepairHint: `run "gc import install"`,
		})
		return "", false
	} else if st.IsDir() {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "invalid-cached-pack",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       filepath.Join(packDir, "pack.toml"),
			Message:    "cached pack.toml is a directory",
			RepairHint: `run "gc import install"`,
		})
		return "", false
	}

	return packDir, true
}

// checkEmbeddedPackDivergence reports how far a materialized import has
// drifted from the running binary's embedded copy of the same pack.
//
// This is the only check here that can see a pack fix that never shipped. An
// import spelled against a fork of the bundled pack repository resolves as an
// ordinary remote import, frozen at its pin -- correct behaviour, and nothing
// in `make install` or `gc supervisor install` refreshes it -- so the content
// that EXECUTES can be weeks older than the binary serving everything else
// while every other check here passes and prints "Import state OK".
//
// That state was live on this fleet for 24 days and cost a real mis-action
// (ga-rvvji2 / ga-2essre): a pack fix was verified three separate ways against
// the installed binary -- file content at the installed commit, the order's
// timeout at that commit, and merge-base proof that the fix was an ancestor of
// it -- all three true, and none of them about the copy the orders exec. It
// also hid a polecat worktree teardown guard and two omp hook versions.
//
// The finding is a notice, not an error: pinning a fork of a bundled pack is
// supported, and failing this command for a supported configuration is how a
// check gets switched off. Deciding whether a divergence should page is the
// caller's job -- a city patrol can gate on this code while the command itself
// keeps exiting 0.
func (s *importCheckState) checkEmbeddedPackDivergence(name, source, commit, packDir string) {
	pack, ok := builtinpacks.PackForSourceSubpath(source)
	if !ok {
		return
	}
	div, err := builtinpacks.DiffEmbeddedPack(pack, packDir)
	if err != nil {
		s.addIssue(CheckIssue{
			Severity:   CheckSeverityWarning,
			Code:       "bundled-pack-content-unreadable",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       packDir,
			Message:    fmt.Sprintf("cannot compare this import against the embedded %q pack: %v", pack.Name, err),
		})
		return
	}
	if div.Count() == 0 {
		return
	}
	s.addIssue(CheckIssue{
		Severity:   CheckSeverityNotice,
		Code:       "bundled-pack-content-diverged",
		ImportName: name,
		Source:     source,
		Commit:     commit,
		Path:       packDir,
		Message: fmt.Sprintf(
			"content that executes differs from this binary's embedded %q pack: %s",
			pack.Name, div.Summary(8)),
		RepairHint: embeddedDivergenceHint(pack.Name, name),
	})
}

// embeddedDivergenceHint spells out both ways forward, because the two have
// different consequences and picking for the operator would be wrong: serving
// embedded content means every gc install ships pack changes with it, while
// bumping the pin keeps the city in charge of when pack content moves and
// leaves this exact staleness possible again.
func embeddedDivergenceHint(packName, importName string) string {
	canonical, ok := builtinpacks.Source(packName)
	if !ok {
		return fmt.Sprintf("bump this import's version and run %q; no gc install refreshes a pinned import", "gc import install")
	}
	pin := config.BundledSourcePinnedVersion(canonical)
	if strings.TrimSpace(pin) == "" {
		return fmt.Sprintf("bump this import's version and run %q; no gc install refreshes a pinned import", "gc import install")
	}
	return fmt.Sprintf(
		"a pinned import is frozen and no gc install refreshes it: either re-point imports.%s to %s at %s to serve this binary's embedded content, or bump its version and run \"gc import install\"",
		importName, canonical, pin)
}

func (s *importCheckState) validateCachedGitCheckout(name, source, commit, cachePath string) bool {
	gitPath := filepath.Join(cachePath, ".git")
	st, err := os.Stat(gitPath)
	if gitutil.MissingCheckoutMarker(st, err) {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "missing-cache",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       cachePath,
			Message:    "locked import is missing from the local repo cache",
			RepairHint: `run "gc import install"`,
		})
		return false
	}
	if err != nil {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "unreadable-cache",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       gitPath,
			Message:    fmt.Sprintf("cannot inspect cached repository: %v", err),
			RepairHint: `run "gc import install"`,
		})
		return false
	}

	head, err := runGit(cachePath, "rev-parse", "HEAD")
	if err != nil {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "unreadable-cache-git",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       cachePath,
			Message:    fmt.Sprintf("cannot read cached repository HEAD: %v", err),
			RepairHint: `run "gc import install"`,
		})
		return false
	}
	if !gitutil.SameCommit(head, commit) {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "cache-checkout-mismatch",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       cachePath,
			Message:    fmt.Sprintf("cached repository is checked out at %s, expected %s", strings.TrimSpace(head), commit),
			RepairHint: `run "gc import install"`,
		})
		return false
	}
	dirty, err := cachedRepoDirty(cachePath)
	if err != nil {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "unreadable-cache-git",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       cachePath,
			Message:    fmt.Sprintf("cannot read cached repository status: %v", err),
			RepairHint: `run "gc import install"`,
		})
		return false
	}
	if dirty {
		s.closureIncomplete = true
		s.addIssue(CheckIssue{
			Code:       "cache-worktree-dirty",
			ImportName: name,
			Source:     source,
			Commit:     commit,
			Path:       cachePath,
			Message:    "cached repository has local worktree changes",
			RepairHint: `run "gc import install"`,
		})
		return false
	}
	return true
}

func (s *importCheckState) reportStaleLockEntries() {
	if s.closureIncomplete {
		return
	}
	sources := make([]string, 0, len(s.lock.Packs))
	for source := range s.lock.Packs {
		if _, ok := s.reachable[source]; !ok {
			sources = append(sources, source)
		}
	}
	sort.Strings(sources)
	for _, source := range sources {
		pack := s.lock.Packs[source]
		s.addIssue(CheckIssue{
			Code:       "stale-lock-entry",
			Source:     source,
			Commit:     pack.Commit,
			Message:    "packs.lock contains a source that is not reachable from declared imports",
			RepairHint: `run "gc import install"`,
		})
	}
}

func (s *importCheckState) addIssue(issue CheckIssue) {
	key := issue.Code + "\x00" + issue.Source + "\x00" + issue.Commit + "\x00" + issue.Path
	if _, ok := s.reportedIssueKeys[key]; ok {
		return
	}
	s.reportedIssueKeys[key] = struct{}{}
	s.report.addIssue(issue)
}

// recordPin remembers the commit a source is pinned to. The declared "sha:"
// constraint is recorded first so a city with no packs.lock yet -- the fresh
// bootstrap case this whole check exists for -- still has something to probe;
// the locked commit overwrites it once validated, because that is what an
// install actually materializes.
func (r *CheckReport) recordPin(source, commit string) {
	source = strings.TrimSpace(source)
	commit = strings.TrimSpace(commit)
	if source == "" || commit == "" {
		return
	}
	if r.CheckedPins == nil {
		r.CheckedPins = make(map[string]string)
	}
	r.CheckedPins[source] = commit
}

func (r *CheckReport) addIssue(issue CheckIssue) {
	if issue.Severity == "" {
		issue.Severity = CheckSeverityError
	}
	r.Issues = append(r.Issues, issue)
}

func lockfileExists(cityRoot string) (bool, error) {
	_, err := os.Stat(filepath.Join(cityRoot, LockfileName))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("checking %s: %w", LockfileName, err)
}

// recordDeclaredShaPins records the pin every remote import declares outright.
// Only "sha:" constraints name a commit; a semver constraint has no pin until
// an install resolves one.
func recordDeclaredShaPins(report *CheckReport, imports map[string]config.Import) {
	for _, name := range sortedImportNames(imports) {
		imp := imports[name]
		if !isRemoteSource(imp.Source) {
			continue
		}
		if declared, ok := strings.CutPrefix(strings.TrimSpace(imp.Version), "sha:"); ok {
			report.recordPin(imp.Source, declared)
		}
	}
}

func countRemoteImports(imports map[string]config.Import) int {
	count := 0
	for _, imp := range imports {
		if isRemoteSource(imp.Source) {
			count++
		}
	}
	return count
}

func sortedImportNames(imports map[string]config.Import) []string {
	names := make([]string, 0, len(imports))
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cachedPackDir(source, cachePath string) string {
	if subpath := normalizeRemoteSource(source).Subpath; subpath != "" {
		return filepath.Join(cachePath, subpath)
	}
	return cachePath
}
