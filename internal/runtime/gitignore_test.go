package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTree creates files under dir; keys are slash-relative paths, values
// file contents. Parent directories are created as needed.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// newRepo creates a fake git repository root (a .git directory is enough
// for repo-root discovery) and returns its path.
func newRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestHashSkillSourceContentMatchesHashPathContentWithoutIgnores(t *testing.T) {
	// Rollout safety: for a skill dir with no ignored entries, the new
	// hasher must produce byte-identical output to HashPathContent so the
	// call-site switch does not flip existing fingerprints.
	repo := newRepo(t)
	skill := filepath.Join(repo, "packs", "demo", "skills", "example")
	writeTree(t, skill, map[string]string{
		"SKILL.md":           "# demo",
		"scripts/run.py":     "print('hi')",
		"tests/test_run.py":  "assert True",
		"evals/evals.json":   "{}",
		"nested/deep/f.toml": "x = 1",
	})
	if got, want := HashSkillSourceContent(skill), HashPathContent(skill); got != want || got == "" {
		t.Errorf("HashSkillSourceContent = %q, HashPathContent = %q; want equal and non-empty", got, want)
	}
}

func TestHashSkillSourceContentStableWhenIgnoredArtifactAppears(t *testing.T) {
	// The ga-rpf2 property: a skill running its own script drops a
	// git-ignored artifact into its source dir; the hash must not move.
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{".gitignore": "*.log\nbuild/\n"})
	skill := filepath.Join(repo, "skills", "example")
	writeTree(t, skill, map[string]string{"SKILL.md": "# demo"})

	before := HashSkillSourceContent(skill)
	if before == "" {
		t.Fatal("expected non-empty hash")
	}
	writeTree(t, skill, map[string]string{
		"run.log":          "generated at runtime",
		"build/out.tar.gz": "artifact",
	})
	if after := HashSkillSourceContent(skill); after != before {
		t.Errorf("hash moved when ignored artifacts appeared: %q -> %q", before, after)
	}
}

func TestHashSkillSourceContentUntrackedFileStillHashes(t *testing.T) {
	// Exclude IGNORED files, not untracked ones: a new not-yet-committed
	// skill file must still change the hash, or skill edits stop
	// triggering the drift that delivers them.
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{".gitignore": "*.log\n"})
	skill := filepath.Join(repo, "skills", "example")
	writeTree(t, skill, map[string]string{"SKILL.md": "# demo"})

	before := HashSkillSourceContent(skill)
	writeTree(t, skill, map[string]string{"NEW_FILE.md": "brand new"})
	if after := HashSkillSourceContent(skill); after == before {
		t.Error("untracked (not ignored) file did not change the hash")
	}
}

func TestHashSkillSourceContentNestedGitignore(t *testing.T) {
	repo := newRepo(t)
	skill := filepath.Join(repo, "skills", "example")
	writeTree(t, skill, map[string]string{
		"SKILL.md":              "# demo",
		"scripts/.gitignore":    "out/\n",
		"scripts/run.py":        "print('hi')",
		"scripts/out/result.js": "ignored",
	})
	withArtifact := HashSkillSourceContent(skill)

	if err := os.RemoveAll(filepath.Join(skill, "scripts", "out")); err != nil {
		t.Fatal(err)
	}
	if without := HashSkillSourceContent(skill); without != withArtifact {
		t.Errorf("nested .gitignore not honored: %q vs %q", withArtifact, without)
	}
}

func TestHashSkillSourceContentNegation(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{".gitignore": "*.log\n!keep.log\n"})
	skill := filepath.Join(repo, "skills", "example")
	writeTree(t, skill, map[string]string{"SKILL.md": "# demo", "keep.log": "v1"})

	before := HashSkillSourceContent(skill)
	writeTree(t, skill, map[string]string{"keep.log": "v2"})
	if after := HashSkillSourceContent(skill); after == before {
		t.Error("negated (re-included) file did not affect the hash")
	}
}

func TestHashSkillSourceContentDeeperGitignoreWins(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{".gitignore": "*.gen\n"})
	skill := filepath.Join(repo, "skills", "example")
	writeTree(t, skill, map[string]string{
		"SKILL.md":    "# demo",
		".gitignore":  "!special.gen\n",
		"special.gen": "v1",
	})

	before := HashSkillSourceContent(skill)
	writeTree(t, skill, map[string]string{"special.gen": "v2"})
	if after := HashSkillSourceContent(skill); after == before {
		t.Error("deeper .gitignore negation did not override repo-root rule")
	}
}

func TestHashSkillSourceContentCarveOutForIgnoredRoot(t *testing.T) {
	// A skills dir living under an ignored parent (e.g. a runtime root)
	// must not have every entry excluded by the ancestor rule; only
	// .gitignore files inside the hashed dir apply.
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{".gitignore": ".runtime/\n"})
	skill := filepath.Join(repo, ".runtime", "skills", "example")
	writeTree(t, skill, map[string]string{"SKILL.md": "# demo"})

	before := HashSkillSourceContent(skill)
	if before == "" {
		t.Fatal("expected non-empty hash for skill under ignored parent")
	}
	writeTree(t, skill, map[string]string{"extra.md": "content"})
	if after := HashSkillSourceContent(skill); after == before {
		t.Error("carve-out failed: content change under ignored parent did not move the hash")
	}
}

func TestHashSkillSourceContentOutsideRepoUsesBlocklistOnly(t *testing.T) {
	dir := t.TempDir()
	skill := filepath.Join(dir, "example")
	writeTree(t, skill, map[string]string{"SKILL.md": "# demo"})

	before := HashSkillSourceContent(skill)
	writeTree(t, skill, map[string]string{
		"__pycache__/run.cpython-314.pyc": "cache",
		".DS_Store":                       "finder turd",
	})
	if after := HashSkillSourceContent(skill); after != before {
		t.Error("blocklist floor not applied outside a git repo")
	}
	if got, want := HashSkillSourceContent(skill), HashPathContent(skill); got != want {
		t.Errorf("outside a repo the hashers must agree: %q vs %q", got, want)
	}
}

func TestHashSkillSourceContentSkipsEmbeddedGitDir(t *testing.T) {
	repo := newRepo(t)
	skill := filepath.Join(repo, "skills", "example")
	writeTree(t, skill, map[string]string{"SKILL.md": "# demo"})

	before := HashSkillSourceContent(skill)
	writeTree(t, skill, map[string]string{".git/index": "volatile"})
	if after := HashSkillSourceContent(skill); after != before {
		t.Error("embedded .git contents leaked into the hash")
	}
}

func TestHashSkillSourceContentFileDelegates(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := HashSkillSourceContent(f), HashPathContent(f); got != want || got == "" {
		t.Errorf("file hashing must match HashPathContent: %q vs %q", got, want)
	}
}

func TestIgnoreRuleMatching(t *testing.T) {
	tests := []struct {
		name    string
		lines   string
		base    string
		path    string
		isDir   bool
		ignored bool
	}{
		{"basename anywhere", "*.log\n", "", "a/b/c.log", false, true},
		{"anchored to base", "/top.log\n", "", "top.log", false, true},
		{"anchored not nested", "/top.log\n", "", "sub/top.log", false, false},
		{"dir only matches dir", "build/\n", "", "build", true, true},
		{"dir only skips file", "build/\n", "", "build", false, false},
		{"slash pattern anchored", "docs/*.md\n", "", "docs/x.md", false, true},
		{"slash pattern not deeper", "docs/*.md\n", "", "docs/sub/x.md", false, false},
		{"double star middle", "a/**/z.txt\n", "", "a/z.txt", false, true},
		{"double star middle deep", "a/**/z.txt\n", "", "a/b/c/z.txt", false, true},
		{"trailing double star", "cache/**\n", "", "cache/x", false, true},
		{"trailing double star not self", "cache/**\n", "", "cache", true, false},
		{"leading double star", "**/vendor\n", "", "a/b/vendor", true, true},
		{"negation wins later", "*.gen\n!keep.gen\n", "", "keep.gen", false, false},
		{"base scoping applies", "*.tmp\n", "sub", "sub/x.tmp", false, true},
		{"base scoping excludes outside", "*.tmp\n", "sub", "other/x.tmp", false, false},
		{"comment ignored", "# *.md\n", "", "x.md", false, false},
		{"escaped bang literal", "\\!important\n", "", "!important", false, true},
		{"question mark", "?.md\n", "", "a.md", false, true},
		{"char class", "[ab].md\n", "", "b.md", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &skillIgnoreMatcher{rules: parseIgnoreLines(tt.lines, tt.base)}
			if got := m.ignoredEval(tt.path, tt.isDir); got != tt.ignored {
				t.Errorf("ignoredEval(%q, dir=%v) with rules %q base %q = %v, want %v",
					tt.path, tt.isDir, tt.lines, tt.base, got, tt.ignored)
			}
		})
	}
}
