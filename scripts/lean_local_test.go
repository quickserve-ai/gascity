package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A local check must test unchanged consumers of a changed package without
// running unrelated tests on the shared host. The required CI suite remains
// authoritative for those unrelated packages.
func TestLeanLocalChecksAffectedBehavior(t *testing.T) {
	fixture := newPRStaticScopeFixture(t, map[string]string{
		"alpha/alpha.go": "package alpha\n\nfunc Value() int { return 1 }\n",
		"consumer/consumer_test.go": `package consumer

import (
    "testing"
    "example.com/static-scope/alpha"
)

func TestValue(t *testing.T) {
    if alpha.Value() != 1 { t.Fatal("consumer contract broken") }
}
`,
		"unrelated/unrelated_test.go": `package unrelated

import "testing"

func TestUnrelated(t *testing.T) { t.Fatal("unrelated test must stay in CI") }
`,
	})
	changed := filepath.Join(fixture.repoRoot, "alpha", "alpha.go")
	writeTestFile(t, changed, "package alpha\n\n// Value is stable.\nfunc Value() int { return 1 }\n")
	if output, err := fixture.runMakeTargetWithGo("check-lean-local", fixture.realGo); err != nil {
		t.Fatalf("local profile ran unrelated tests or rejected valid work: %v\n%s", err, output)
	}
	writeTestFile(t, changed, "package alpha\n\nfunc Value() int { return 2 }\n")
	output, err := fixture.runMakeTargetWithGo("check-lean-local", fixture.realGo)
	if err == nil || !strings.Contains(output, "consumer contract broken") {
		t.Fatalf("local profile missed broken unchanged consumer: err=%v\n%s", err, output)
	}
}

const hashedAssetBody = `export function render(root) {
  const items = ["alpha", "beta", "gamma", "delta"];
  for (const item of items) {
    root.append(item);
  }
  return items.length;
}
`

// newDirectoryEmbedFixture builds a package that embeds a whole build-output
// directory, the shape of a bundler's dist tree, plus a consumer whose tests
// must run and an unrelated package whose tests must stay in CI.
func newDirectoryEmbedFixture(t *testing.T, dist map[string]string) prStaticScopeFixture {
	t.Helper()
	files := map[string]string{
		"alpha/alpha.go": `package alpha

import "embed"

//go:embed all:dist
var Dist embed.FS
`,
		"consumer/consumer_test.go": `package consumer

import (
	"testing"

	"example.com/static-scope/alpha"
)

func TestDist(t *testing.T) {
	if _, err := alpha.Dist.ReadFile("dist/index.html"); err != nil {
		t.Fatal(err)
	}
}
`,
		"unrelated/unrelated_test.go": `package unrelated

import "testing"

func TestUnrelated(t *testing.T) { t.Fatal("unrelated test must stay in CI") }
`,
	}
	for name, content := range dist {
		files[filepath.Join("alpha", "dist", name)] = content
	}
	return newPRStaticScopeFixture(t, files)
}

// A content-hashed bundler rebuild renames assets beside files that survive.
// The deleted name is absent from the post-change embed inventory, but its
// directory pattern still embeds files there, so the owner and its reverse
// dependents are the complete scope.
func TestLeanLocalSelectsEmbedOwnerForHashedAssetRename(t *testing.T) {
	fixture := newDirectoryEmbedFixture(t, map[string]string{
		"index.html":               "<script type=\"module\" src=\"/assets/app-AAAA1111.js\"></script>\n",
		"assets/app-AAAA1111.js":   "// build AAAA1111\n" + hashedAssetBody,
		"assets/other-BBBB2222.js": "export const other = 2;\n",
	})
	oldPath := filepath.Join(fixture.repoRoot, "alpha", "dist", "assets", "app-AAAA1111.js")
	newPath := filepath.Join(fixture.repoRoot, "alpha", "dist", "assets", "app-CCCC3333.js")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatalf("rename hashed asset: %v", err)
	}
	writeTestFile(t, newPath, "// build CCCC3333\n"+hashedAssetBody)
	runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(), "git add -A")
	status := runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(), "git diff --name-status -M HEAD --")
	if !strings.Contains(status, "R") ||
		!strings.Contains(status, "alpha/dist/assets/app-AAAA1111.js") ||
		!strings.Contains(status, "alpha/dist/assets/app-CCCC3333.js") {
		t.Fatalf("fixture is not a Git rename of a hashed asset:\n%s", status)
	}

	fixture.resetCalls(t)
	if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
		t.Errorf("lint-affected failed for a hashed-asset rename: %v\n%s", err, output)
	}
	fixture.requireSingleRunCallWithUnorderedTail(t, "./alpha", "./consumer")
	fixture.requireGoCalls(t, []string{"vet", "./alpha", "./consumer"})

	output, err := fixture.runMakeTargetWithGo("check-lean-local", fixture.realGo)
	if err != nil || strings.Contains(output, "cannot safely select tests") {
		t.Fatalf("local profile refused a hashed-asset rename: err=%v\n%s", err, output)
	}
	if !strings.Contains(output, "check-lean-local: testing ./alpha ./consumer\n") {
		t.Fatalf("local profile did not test exactly the embed owner and its consumer:\n%s", output)
	}
}

// A deletion that leaves its directory without any file the pattern still
// embeds is not evidence the pattern resolves there any more; refuse it.
func TestLeanLocalRefusesDeletedEmbedPathInEmptiedDirectory(t *testing.T) {
	fixture := newDirectoryEmbedFixture(t, map[string]string{
		"index.html":    "<script type=\"module\" src=\"/assets/app.js\"></script>\n",
		"assets/app.js": hashedAssetBody,
		"sub/only.js":   "export const only = 1;\n",
	})
	if err := os.Remove(filepath.Join(fixture.repoRoot, "alpha", "dist", "sub", "only.js")); err != nil {
		t.Fatalf("delete the only embedded file in a subdirectory: %v", err)
	}

	fixture.resetCalls(t)
	if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
		t.Errorf("lint-affected did not fail closed for an emptied embedded directory: %v\n%s", err, output)
	}
	fixture.requireCalls(t, []string{"run", "./..."})
	fixture.requireGoCalls(t, []string{"vet", "./..."})

	output, err := fixture.runMakeTargetWithGo("check-lean-local", fixture.realGo)
	if err == nil || !strings.Contains(output, "may be absent from the current embed inventory") {
		t.Fatalf("local profile did not refuse an emptied embedded directory: err=%v\n%s", err, output)
	}
}

func TestLeanLocalRefusesUnknownScope(t *testing.T) {
	fixture := newPRStaticScopeFixture(t, map[string]string{
		"alpha/alpha.go": "package alpha\n",
	})
	output, err := fixture.runMakeTargetWithGo("check-lean-local", fixture.realGo)
	if err == nil || !strings.Contains(output, "no changes selected") {
		t.Fatalf("clean worktree with default HEAD must not report a tested change: err=%v\n%s", err, output)
	}
	output, err = fixture.runMakeTargetWithOptions("check-lean-local", "missing-base", fixture.realGo)
	if err == nil || !strings.Contains(output, "cannot safely select tests") {
		t.Fatalf("missing base must refuse rather than skip or expand tests: err=%v\n%s", err, output)
	}
	modulePath := filepath.Join(fixture.repoRoot, "go.mod")
	module, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, modulePath, string(module)+"\n// lean-local module edit\n")
	output, err = fixture.runMakeTargetWithGo("check-lean-local", fixture.realGo)
	if err == nil || !strings.Contains(output, "module/workspace changes require the full CI profile") {
		t.Fatalf("module change must refuse rather than under-select tests: err=%v\n%s", err, output)
	}
}
