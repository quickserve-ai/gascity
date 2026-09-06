package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func gaConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
	}
}

func TestExtractBeadIDFromWorktreeNameBareID(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "ga-n0oafq")
	if got != "ga-n0oafq" {
		t.Errorf("got %q, want %q", got, "ga-n0oafq")
	}
}

func TestExtractBeadIDFromWorktreeNameCompound(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "builder-ga-34q3ss")
	if got != "ga-34q3ss" {
		t.Errorf("got %q, want %q", got, "ga-34q3ss")
	}
}

func TestExtractBeadIDFromWorktreeNameNoMatch(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "builder-feature-branch")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractBeadIDFromWorktreeNameSingleSegment(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "builder")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractBeadIDFromWorktreeNameNilConfig(t *testing.T) {
	got := extractBeadIDFromWorktreeName(nil, "ga-n0oafq")
	if got != "" {
		t.Errorf("got %q, want empty for nil config", got)
	}
}

func TestExtractBeadIDFromWorktreeNameEmptyName(t *testing.T) {
	got := extractBeadIDFromWorktreeName(gaConfig(), "")
	if got != "" {
		t.Errorf("got %q, want empty for empty name", got)
	}
}

func TestIsStrictlyUnderDirSubpath(t *testing.T) {
	dir := filepath.Join("a", "b")
	path := filepath.Join("a", "b", "c")
	if !isStrictlyUnderDir(dir, path) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = false, want true", dir, path)
	}
}

func TestIsStrictlyUnderDirSameDir(t *testing.T) {
	dir := filepath.Join("a", "b")
	if isStrictlyUnderDir(dir, dir) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = true, want false (same dir)", dir, dir)
	}
}

func TestIsStrictlyUnderDirPathTraversal(t *testing.T) {
	dir := filepath.Join("a", "b")
	path := filepath.Join("a", "c") // sibling — relative path starts with ".."
	if isStrictlyUnderDir(dir, path) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = true, want false (path traversal)", dir, path)
	}
}

func TestIsStrictlyUnderDirDeepSubpath(t *testing.T) {
	dir := filepath.Join("root", "worktrees")
	path := filepath.Join("root", "worktrees", "gascity", "builder")
	if !isStrictlyUnderDir(dir, path) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = false, want true", dir, path)
	}
}

func TestNonSedimentStatusLinesFiltersProvisioningOnly(t *testing.T) {
	porcelain := "?? .claude/skills/cherub-law.codex/\n" +
		"?? .omp/hooks/gc-hook.ts\n" +
		" D .beads/config.yaml\n" +
		"D  .beads/formulas/brand-review.formula.toml\n" +
		"?? .beads/routes.jsonl\n" +
		"?? .worktree-stale\n" +
		"?? AGENTS-gc.md\n"
	if got := nonSedimentStatusLines(porcelain); len(got) != 0 {
		t.Fatalf("pure provisioning sediment classified as authored work: %v", got)
	}
}

func TestNonSedimentStatusLinesKeepsAuthoredWork(t *testing.T) {
	porcelain := "?? .claude/skills/x/\n" +
		" M internal/server/main.go\n" +
		"?? newfile.go\n" +
		" D .beads/config.yaml\n"
	got := nonSedimentStatusLines(porcelain)
	if len(got) != 2 {
		t.Fatalf("authored lines = %v, want the modified tracked file and the untracked source file", got)
	}
}

func TestNonSedimentStatusLinesDoesNotOvermatchLookalikes(t *testing.T) {
	// Paths that merely RESEMBLE sediment must stay authored: deletions
	// outside .beads/, files whose names embed the markers deeper, and
	// tracked modifications inside the dot-dirs.
	porcelain := " D src/config.yaml\n" +
		"?? docs/.worktree-stale.md\n" +
		" M .claude/settings.json\n" +
		"?? vendor/AGENTS-gc.md.bak\n"
	got := nonSedimentStatusLines(porcelain)
	if len(got) != 4 {
		t.Fatalf("lookalike lines = %d authored (%v), want all 4 kept", len(got), got)
	}
}
