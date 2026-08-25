package packman

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
)

// gitFixture runs git for test setup only. It never goes through runGit, so a
// bug in the runner under test cannot also build the fixture and hide itself.
func gitFixture(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v in %q: %s: %v", args, dir, strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out))
}

func fixtureCommit(t *testing.T, repo, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	gitFixture(t, repo, "add", name)
	gitFixture(t, repo, "commit", "--quiet", "-m", "add "+name)
	return gitFixture(t, repo, "rev-parse", "HEAD")
}

// newFixtureRepo returns a repository with one commit on main, plus its URL.
func newFixtureRepo(t *testing.T) (dir, url, head string) {
	t.Helper()
	dir = t.TempDir()
	gitFixture(t, "", "init", "--quiet", "-b", "main", dir)
	head = fixtureCommit(t, dir, "pack.toml", "[pack]\nname = \"fixture\"\nschema = 1\n")
	return dir, "file://" + dir, head
}

func reachTestEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
}

// TestVerifySourceReachabilityReportsPinNoRefReachesAtItsSource is the
// regression for ga-dyb5na, reproduced in the shape that actually bit: the
// commit is PRESENT in local objects (GitHub fork networks share an object
// store across a fork and its parent, and here the installed cache clone
// supplies the same visibility) while NO ref at the declared source reaches
// it. Any existence-flavored probe calls this healthy. Only ref containment
// catches it.
func TestVerifySourceReachabilityReportsPinNoRefReachesAtItsSource(t *testing.T) {
	reachTestEnv(t)
	sourceDir, sourceURL, _ := newFixtureRepo(t)

	// The pin lives only in a sibling lineage, exactly like a fork-only commit.
	sibling := t.TempDir()
	gitFixture(t, "", "clone", "--quiet", sourceURL, sibling)
	gitFixture(t, sibling, "checkout", "--quiet", "-b", "carry/operational")
	pin := fixtureCommit(t, sibling, "guard.sh", "#!/bin/sh\nexit 0\n")

	// Install that sibling as the local repo cache for (source, pin), which is
	// how the pin resolves today without the source ever being able to produce
	// it.
	cachePath, err := RepoCachePath(sourceURL, pin)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir cache parent: %v", err)
	}
	gitFixture(t, "", "clone", "--quiet", "file://"+sibling, cachePath)
	gitFixture(t, cachePath, "checkout", "--quiet", pin)

	// Non-vacuity: the fixture really is the fork-network shape. If the source
	// held the object, or the cache did not, this test would pass for the
	// wrong reason.
	if objectPresentFixture(t, sourceDir, pin) {
		t.Fatalf("fixture is wrong: declared source already holds %s", pin)
	}
	if !objectPresentFixture(t, cachePath, pin) {
		t.Fatalf("fixture is wrong: cache clone does not hold %s", pin)
	}

	report, err := VerifySourceReachability(t.TempDir(), map[string]string{sourceURL: pin})
	if err != nil {
		t.Fatalf("VerifySourceReachability: %v", err)
	}
	if report.CheckedSources != 1 {
		t.Fatalf("CheckedSources = %d, want 1", report.CheckedSources)
	}
	assertSingleIssue(t, report, CodePinUnreachableAtSource)
	if got := report.Issues[0].Severity; got != CheckSeverityError {
		t.Fatalf("severity = %q, want %q", got, CheckSeverityError)
	}
	if got := report.Issues[0].Commit; got != pin {
		t.Fatalf("issue commit = %q, want %q", got, pin)
	}
}

// TestVerifySourceReachabilityAcceptsAPinAnAdvertisedBranchReaches is the
// control for the test above: same machinery, a pin the source's history
// really does contain, and it must come back clean. Without it, a probe that
// always reported "unreachable" would pass the regression test.
func TestVerifySourceReachabilityAcceptsAPinAnAdvertisedBranchReaches(t *testing.T) {
	reachTestEnv(t)
	dir, url, first := newFixtureRepo(t)
	fixtureCommit(t, dir, "later.txt", "later\n") // pin is now an ancestor, not a tip

	report, err := VerifySourceReachability(t.TempDir(), map[string]string{url: first})
	if err != nil {
		t.Fatalf("VerifySourceReachability: %v", err)
	}
	if report.HasIssues() {
		t.Fatalf("issues = %#v, want none", report.Issues)
	}
	if report.CheckedSources != 1 {
		t.Fatalf("CheckedSources = %d, want 1", report.CheckedSources)
	}
}

// TestVerifySourceReachabilityAcceptsARefTipWithoutTransferringObjects pins
// the cheap path: when the pin IS an advertised tip, ls-remote alone settles
// it. The fetch runner is rigged to fail, so a verdict can only come from the
// shortcut.
func TestVerifySourceReachabilityAcceptsARefTipWithoutTransferringObjects(t *testing.T) {
	reachTestEnv(t)
	_, url, head := newFixtureRepo(t)

	realRunNetworkGit := runNetworkGit
	t.Cleanup(func() { runNetworkGit = realRunNetworkGit })
	fetches := 0
	runNetworkGit = func(cityRoot, remoteURL, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "fetch" {
			fetches++
			t.Errorf("probe fetched for a pin that is an advertised tip: %v", args)
		}
		return realRunNetworkGit(cityRoot, remoteURL, dir, args...)
	}

	report, err := VerifySourceReachability(t.TempDir(), map[string]string{url: head})
	if err != nil {
		t.Fatalf("VerifySourceReachability: %v", err)
	}
	if report.HasIssues() {
		t.Fatalf("issues = %#v, want none", report.Issues)
	}
	if fetches != 0 {
		t.Fatalf("fetches = %d, want 0", fetches)
	}
}

// TestVerifySourceReachabilityAcceptsAnAnnotatedTagPin covers the peeled
// ls-remote line: an annotated tag advertises both the tag object and, as
// "<ref>^{}", the commit it points at. Matching only the unpeeled line would
// call a correctly tagged pin unreachable.
func TestVerifySourceReachabilityAcceptsAnAnnotatedTagPin(t *testing.T) {
	reachTestEnv(t)
	dir, url, head := newFixtureRepo(t)
	gitFixture(t, dir, "tag", "-a", "v1.0.0", "-m", "release")
	// Take the pin off every branch line, so the annotated tag is the only ref
	// that reaches it. An orphan branch shares no history with main, so
	// deleting main afterwards leaves nothing but the tag.
	gitFixture(t, dir, "checkout", "--quiet", "--orphan", "unrelated")
	gitFixture(t, dir, "rm", "-rq", "--cached", ".")
	fixtureCommit(t, dir, "unrelated.txt", "unrelated\n")
	gitFixture(t, dir, "branch", "-D", "main")

	report, err := VerifySourceReachability(t.TempDir(), map[string]string{url: head})
	if err != nil {
		t.Fatalf("VerifySourceReachability: %v", err)
	}
	if report.HasIssues() {
		t.Fatalf("issues = %#v, want none", report.Issues)
	}
}

// TestVerifySourceReachabilityWarnsRatherThanFailsWhenItCannotReachAVerdict:
// an offline machine has learned nothing about its imports. Reporting that as
// a broken import trains readers to ignore the check, so it must be a warning
// and must not contribute to ErrorCount.
func TestVerifySourceReachabilityWarnsRatherThanFailsWhenItCannotReachAVerdict(t *testing.T) {
	reachTestEnv(t)
	missing := "file://" + filepath.Join(t.TempDir(), "no-such-repo")

	report, err := VerifySourceReachability(t.TempDir(), map[string]string{
		missing: "0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatalf("VerifySourceReachability: %v", err)
	}
	if len(report.Issues) != 1 {
		t.Fatalf("len(Issues) = %d, want 1: %#v", len(report.Issues), report.Issues)
	}
	if got := report.Issues[0].Code; got != CodeSourceProbeFailed {
		t.Fatalf("issue code = %q, want %q", got, CodeSourceProbeFailed)
	}
	if got := report.Issues[0].Severity; got != CheckSeverityWarning {
		t.Fatalf("severity = %q, want %q", got, CheckSeverityWarning)
	}
	if report.ErrorCount() != 0 {
		t.Fatalf("ErrorCount = %d, want 0 for an undetermined probe", report.ErrorCount())
	}
}

// TestVerifySourceReachabilitySkipsBundledSourceAtItsCanonicalPin: a bundled
// pack at the pin the binary carries is materialized from the binary, never
// fetched. Probing it would report a network failure for a pack that is
// present by construction.
func TestVerifySourceReachabilitySkipsBundledSourceAtItsCanonicalPin(t *testing.T) {
	reachTestEnv(t)
	source := builtinpacks.MustSource("core")
	commit := strings.TrimPrefix(config.BundledSourcePinnedVersion(source), "sha:")
	if commit == "" {
		t.Fatalf("no canonical pin for bundled source %q", source)
	}
	// Non-vacuity: the skip must be earned by the canonical-pin predicate, not
	// by the pair simply failing to look bundled.
	if !config.IsBundledSourceAtCanonicalPin(source, commit) {
		t.Fatalf("fixture is wrong: %q@%s is not a bundled source at its canonical pin", source, commit)
	}

	realRunNetworkGit := runNetworkGit
	t.Cleanup(func() { runNetworkGit = realRunNetworkGit })
	runNetworkGit = func(_, _, _ string, args ...string) (string, error) {
		t.Errorf("bundled source at its canonical pin went to the network: %v", args)
		return "", nil
	}

	report, err := VerifySourceReachability(t.TempDir(), map[string]string{source: commit})
	if err != nil {
		t.Fatalf("VerifySourceReachability: %v", err)
	}
	if report.HasIssues() {
		t.Fatalf("issues = %#v, want none", report.Issues)
	}
	if report.CheckedSources != 0 {
		t.Fatalf("CheckedSources = %d, want 0", report.CheckedSources)
	}
}

// TestAttachCacheAlternateMakesCachedObjectsVisible pins the accelerator
// itself. Without this the fork-network regression test above could pass
// because the alternate silently did nothing, which is the vacuous version of
// the same assertion.
func TestAttachCacheAlternateMakesCachedObjectsVisible(t *testing.T) {
	reachTestEnv(t)
	_, url, head := newFixtureRepo(t)
	cachePath, err := RepoCachePath(url, head)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir cache parent: %v", err)
	}
	gitFixture(t, "", "clone", "--quiet", url, cachePath)

	scratch := t.TempDir()
	gitFixture(t, "", "init", "--quiet", "--bare", scratch)
	if objectPresent(scratch, head) {
		t.Fatalf("fixture is wrong: empty scratch already sees %s", head)
	}
	attachCacheAlternate(scratch, url, head)
	if !objectPresent(scratch, head) {
		t.Fatalf("attachCacheAlternate did not make cached objects visible")
	}
}

// TestParseRemoteRefTipsIgnoresMalformedLines keeps a short or blank
// ls-remote line from being read as a tip.
func TestParseRemoteRefTipsIgnoresMalformedLines(t *testing.T) {
	tips := parseRemoteRefTips("aaa\trefs/heads/main\n\nnotwofields\nbbb\trefs/tags/v1^{}\n")
	if len(tips) != 2 {
		t.Fatalf("tips = %#v, want 2 entries", tips)
	}
	if got := tips["bbb"]; got != "refs/tags/v1" {
		t.Fatalf("peeled tag ref = %q, want refs/tags/v1", got)
	}
}

func objectPresentFixture(t *testing.T, repo, commit string) bool {
	t.Helper()
	cmd := exec.Command("git", "cat-file", "-e", commit+"^{commit}")
	cmd.Dir = repo
	return cmd.Run() == nil
}

// TestCheckInstalledRecordsTheLockedPinForProbing: the network phase asks the
// offline walk which pins it validated instead of re-deriving them, so the two
// can never disagree about what is installed. The locked commit is what an
// install materializes, so that is what must be recorded.
func TestCheckInstalledRecordsTheLockedPinForProbing(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	source := builtinpacks.MustSource("gastown")
	commit := canonicalBundledCommit(source)
	writeTestLockfile(t, city, map[string]LockedPack{
		source: {Version: "sha:" + commit, Commit: commit},
	})
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	repository, ok := builtinpacks.RepositoryForSource(source)
	if !ok {
		t.Fatalf("RepositoryForSource(%s): not a bundled source", source)
	}
	if err := builtinpacks.MaterializeSyntheticRepo(cachePath, repository, commit); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:gastown": {Source: source, Version: "sha:" + commit},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	if got := report.CheckedPins[source]; got != commit {
		t.Fatalf("CheckedPins[%q] = %q, want %q", source, got, commit)
	}
}

// TestCheckInstalledRecordsTheDeclaredPinWithNoLockfileYet covers the case the
// whole check exists for: a city bootstrapping from a pack.toml it has never
// installed. There is no locked commit to read, so the declared sha has to be
// what gets probed -- otherwise the one configuration that most needs the
// check is the one it silently skips.
func TestCheckInstalledRecordsTheDeclaredPinWithNoLockfileOnDisk(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	const source = "https://example.com/tools.git"
	const declared = "0123456789abcdef0123456789abcdef01234567"
	// NO lockfile on disk at all -- not an empty one. CheckInstalled returns
	// early on a missing lockfile without walking the closure, so an empty-but-
	// present lockfile exercises a different branch and would let this test
	// pass while the real bootstrap case recorded nothing. Found in the field,
	// not by the first version of this test.
	if _, err := os.Stat(filepath.Join(city, LockfileName)); !os.IsNotExist(err) {
		t.Fatalf("fixture is wrong: %s must not exist (stat err = %v)", LockfileName, err)
	}

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: source, Version: "sha:" + declared},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	// The offline pass still reports the missing lockfile; the point here is
	// that it does not ALSO lose the pin.
	assertSingleIssue(t, report, "missing-lockfile")
	if got := report.CheckedPins[source]; got != declared {
		t.Fatalf("CheckedPins[%q] = %q, want the declared pin %q", source, got, declared)
	}
}

// TestVerifySourceReachabilityRejectsASourceAdvertisingNoRefs: a source with
// no refs cannot reach anything, so a pin against it is unreachable by
// definition. Reading "nothing advertised" as "fine" would make an emptied or
// wrong repository the quietest possible failure.
func TestVerifySourceReachabilityRejectsASourceAdvertisingNoRefs(t *testing.T) {
	reachTestEnv(t)
	empty := t.TempDir()
	gitFixture(t, "", "init", "--quiet", "--bare", empty)

	report, err := VerifySourceReachability(t.TempDir(), map[string]string{
		"file://" + empty: "0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatalf("VerifySourceReachability: %v", err)
	}
	assertSingleIssue(t, report, CodePinUnreachableAtSource)
	if !strings.Contains(report.Issues[0].Message, "no refs at all") {
		t.Fatalf("message = %q, want it to name the empty advertisement", report.Issues[0].Message)
	}
}

// TestCheckInstalledRecordsTheLockedPinUnderASemverConstraint: with a semver
// constraint there is no declared sha to fall back on, so the locked commit is
// the only thing that can be probed. This is what separates the two recording
// paths -- a sha-pinned import would record the same value either way and
// could not tell them apart.
func TestCheckInstalledRecordsTheLockedPinUnderASemverConstraint(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	stubCachedPackGit(t)
	const source = "https://example.com/tools.git"
	const commit = "aaaa"
	writeTestLockfile(t, city, map[string]LockedPack{
		source: {Version: "1.0.0", Commit: commit},
	})
	stageCachedPack(t, source, commit, `
[pack]
name = "tools"
schema = 1
`)

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: source, Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	if report.HasIssues() {
		t.Fatalf("issues = %#v, want none", report.Issues)
	}
	if got := report.CheckedPins[source]; got != commit {
		t.Fatalf("CheckedPins[%q] = %q, want the locked commit %q", source, got, commit)
	}
}

// TestCheckInstalledRecordsTheDeclaredPinWhenTheLockHasNoEntryForIt: the
// lockfile exists but says nothing about this source, so walkImport bails
// before any locked commit is available. Without the declaration-side record
// the pin would be lost in the one state where "is this source even right?"
// is the question worth asking.
func TestCheckInstalledRecordsTheDeclaredPinWhenTheLockHasNoEntryForIt(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	const source = "https://example.com/tools.git"
	const declared = "0123456789abcdef0123456789abcdef01234567"
	// A lockfile that EXISTS but is empty: this is a different branch from a
	// missing lockfile, and only this one reaches walkImport.
	writeTestLockfile(t, city, map[string]LockedPack{})

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: source, Version: "sha:" + declared},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "missing-lock-entry")
	if got := report.CheckedPins[source]; got != declared {
		t.Fatalf("CheckedPins[%q] = %q, want the declared pin %q", source, got, declared)
	}
}

// TestCheckInstalledRecordsNoPinForASemverConstraintWithNothingLocked: "^1.0"
// is not a commit. Recording it would hand the source probe a version range to
// look up as a sha, which produces a confident, wrong "unreachable" verdict on
// a perfectly healthy import.
func TestCheckInstalledRecordsNoPinForASemverConstraintWithNothingLocked(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	const source = "https://example.com/tools.git"
	writeTestLockfile(t, city, map[string]LockedPack{})

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: source, Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "missing-lock-entry")
	if got, ok := report.CheckedPins[source]; ok {
		t.Fatalf("CheckedPins[%q] = %q, want no pin recorded for a version range", source, got)
	}
}

// TestCheckInstalledRecordsNoPinForASemverConstraintWithNoLockfile is the same
// property on the missing-lockfile branch, which records pins through a
// separate path and could drift from the walk.
func TestCheckInstalledRecordsNoPinForASemverConstraintWithNoLockfile(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	const source = "https://example.com/tools.git"
	if _, err := os.Stat(filepath.Join(city, LockfileName)); !os.IsNotExist(err) {
		t.Fatalf("fixture is wrong: %s must not exist (stat err = %v)", LockfileName, err)
	}

	report, err := CheckInstalled(city, map[string]config.Import{
		"pack:tools": {Source: source, Version: "^1.0"},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	assertSingleIssue(t, report, "missing-lockfile")
	if got, ok := report.CheckedPins[source]; ok {
		t.Fatalf("CheckedPins[%q] = %q, want no pin recorded for a version range", source, got)
	}
}
