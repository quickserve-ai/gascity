package packman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
)

// forkPinnedCoreSource spells the core pack against a FORK of the pack
// repository. That spelling is the whole regression: it resolves as an ordinary
// remote import frozen at its pin, so no gc install refreshes it, while every
// other check in this file passes (ga-rvvji2).
const forkPinnedCoreSource = "https://github.com/quickserve-ai/gascity.git//internal/bootstrap/packs/core"

// stageForkPinnedCorePack materializes this binary's embedded packs into the
// cache slot a fork-pinned core import resolves to, and returns the core pack
// directory inside it. Starting from an exact copy of the embedded content is
// deliberate: every assertion below is then about the ONE thing the test
// changes, and a clean run proves the comparison does not fire on noise.
func stageForkPinnedCorePack(t *testing.T, commit string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	stubCachedPackGit(t)

	cachePath, err := RepoCachePath(forkPinnedCoreSource, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := builtinpacks.MaterializeSyntheticRepo(cachePath, builtinpacks.Repository, commit); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cachePath, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git): %v", err)
	}
	writeCachedPackCommit(t, cachePath, commit)
	return filepath.Join(cachePath, "internal", "bootstrap", "packs", "core")
}

func checkForkPinnedCore(t *testing.T, commit string) *CheckReport {
	t.Helper()
	city := t.TempDir()
	writeTestLockfile(t, city, map[string]LockedPack{
		forkPinnedCoreSource: {Version: "sha:" + commit, Commit: commit},
	})
	report, err := CheckInstalled(city, map[string]config.Import{
		"core": {Source: forkPinnedCoreSource, Version: "sha:" + commit},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	return report
}

// TestCheckInstalledAcceptsForkPinnedPackMatchingTheBinary is the control. A
// fork-pinned import whose content matches this binary is a perfectly ordinary
// state and must stay silent, or the notice below means nothing.
func TestCheckInstalledAcceptsForkPinnedPackMatchingTheBinary(t *testing.T) {
	const commit = "1111111111111111111111111111111111111111"
	stageForkPinnedCorePack(t, commit)
	report := checkForkPinnedCore(t, commit)
	if report.HasIssues() {
		t.Fatalf("clean fork-pinned pack reported issues: %#v", report.Issues)
	}
}

// TestCheckInstalledReportsForkPinnedPackContentDivergence is the check this
// bead exists for: the pack content that EXECUTES differs from the binary's,
// and nothing else in the report can see it.
func TestCheckInstalledReportsForkPinnedPackContentDivergence(t *testing.T) {
	const commit = "2222222222222222222222222222222222222222"
	packDir := stageForkPinnedCorePack(t, commit)

	// Stand in for the real 2026-08-24 pin: an order that still carries the
	// pre-fix timeout, plus a script the binary has since added.
	order := filepath.Join(packDir, "orders", "renudge-stale-human-gates.toml")
	if _, err := os.Stat(order); err != nil {
		t.Skipf("embedded core pack no longer carries %s: %v", order, err)
	}
	if err := os.WriteFile(order, []byte("timeout = \"120s\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	added := filepath.Join(packDir, "assets", "scripts", "detect-silent-published-work.sh")
	if err := os.Remove(added); err != nil && !os.IsNotExist(err) {
		t.Fatalf("Remove: %v", err)
	}

	report := checkForkPinnedCore(t, commit)

	var found *CheckIssue
	for i := range report.Issues {
		if report.Issues[i].Code == "bundled-pack-content-diverged" {
			found = &report.Issues[i]
		}
	}
	if found == nil {
		t.Fatalf("no bundled-pack-content-diverged issue: %#v", report.Issues)
	}
	if found.Severity != CheckSeverityNotice {
		t.Fatalf("severity = %q, want notice", found.Severity)
	}
	// A notice must never change an exit code: pinning a fork is supported,
	// and a check that fails a supported configuration gets switched off.
	if report.ErrorCount() != 0 {
		t.Fatalf("ErrorCount = %d, want 0: %#v", report.ErrorCount(), report.Issues)
	}
	if !report.HasIssues() {
		t.Fatal("HasIssues = false; the finding must stop \"Import state OK\" from printing")
	}
	if found.ImportName != "core" {
		t.Fatalf("ImportName = %q, want core", found.ImportName)
	}
	if found.Path != packDir {
		t.Fatalf("Path = %q, want the executing pack dir %q", found.Path, packDir)
	}
	// The message has to name files, not just a count: a count sends the
	// reader off to re-derive the list, which is how this stayed invisible.
	if !strings.Contains(found.Message, "renudge-stale-human-gates.toml") {
		t.Fatalf("Message does not name the changed file: %q", found.Message)
	}
	// The hint must say that an install cannot fix this, because reaching for
	// an install is the intuitive and wrong move.
	if !strings.Contains(found.RepairHint, "no gc install refreshes") {
		t.Fatalf("RepairHint does not say an install will not help: %q", found.RepairHint)
	}
}

// TestCheckInstalledDoesNotReportDivergenceForANonBundledPack keeps the leg
// scoped: an import that is not a bundled pack under another name has nothing
// to compare against and must not be second-guessed.
func TestCheckInstalledDoesNotReportDivergenceForANonBundledPack(t *testing.T) {
	const commit = "3333333333333333333333333333333333333333"
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	stubCachedPackGit(t)

	source := "https://github.com/quickserve-ai/qc-bridge.git//packs/bridge"
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	packDir := filepath.Join(cachePath, "packs", "bridge")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte("[pack]\nname = \"bridge\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cachePath, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git): %v", err)
	}
	writeCachedPackCommit(t, cachePath, commit)

	city := t.TempDir()
	writeTestLockfile(t, city, map[string]LockedPack{
		source: {Version: "sha:" + commit, Commit: commit},
	})
	report, err := CheckInstalled(city, map[string]config.Import{
		"bridge": {Source: source, Version: "sha:" + commit},
	})
	if err != nil {
		t.Fatalf("CheckInstalled: %v", err)
	}
	for _, issue := range report.Issues {
		if strings.HasPrefix(issue.Code, "bundled-pack-content-") {
			t.Fatalf("non-bundled import produced %q: %#v", issue.Code, issue)
		}
	}
}
