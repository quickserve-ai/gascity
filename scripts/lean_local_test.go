package scripts_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	for _, testCase := range []struct {
		name   string
		delete func(*testing.T, prStaticScopeFixture)
	}{
		{
			name: "removed from disk",
			delete: func(t *testing.T, fixture prStaticScopeFixture) {
				if err := os.Remove(filepath.Join(fixture.repoRoot, "alpha", "dist", "sub", "only.js")); err != nil {
					t.Fatalf("delete the only embedded file in a subdirectory: %v", err)
				}
			},
		},
		{
			// go list still inventories a file left on disk, so the deleted
			// path must not count as its own surviving sibling.
			name: "removed from the index only",
			delete: func(t *testing.T, fixture prStaticScopeFixture) {
				runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(), "git rm -q --cached alpha/dist/sub/only.js")
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newDirectoryEmbedFixture(t, map[string]string{
				"index.html":    "<script type=\"module\" src=\"/assets/app.js\"></script>\n",
				"assets/app.js": hashedAssetBody,
				"sub/only.js":   "export const only = 1;\n",
			})
			testCase.delete(t, fixture)
			status := runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(), "git diff --name-status HEAD --")
			if status != "D\talpha/dist/sub/only.js\n" {
				t.Fatalf("fixture is not a single tracked deletion:\n%s", status)
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
		})
	}
}

// A deleted path is vouched for only by a sibling that its own pattern still
// embeds. Each deletion here is index-only, so the file stays on disk, go list
// keeps it in the inventory, and a local compile passes: only the selector
// can refuse before a clean checkout fails.
func TestLeanLocalJudgesDeletedEmbedPathBySiblingsOfItsOwnPattern(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		directive string
		data      []string
		refuse    bool
	}{
		{
			name:      "a sibling only another glob embeds does not vouch",
			directive: "data/*.txt data/*.json",
			data:      []string{"a.txt", "keep.json"},
			refuse:    true,
		},
		{
			name:      "a name the directory walk skips does not vouch",
			directive: "data data/_keep.txt",
			data:      []string{"a.txt", "_keep.txt"},
			refuse:    true,
		},
		{
			name:      "a sibling the same glob embeds vouches",
			directive: "data/*.txt data/*.json",
			data:      []string{"a.txt", "b.txt", "keep.json"},
		},
		{
			name:      "all: makes the directory walk embed a leading underscore",
			directive: "all:data data/_keep.txt",
			data:      []string{"a.txt", "_keep.txt"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			files := map[string]string{
				"alpha/alpha.go": "package alpha\n\nimport \"embed\"\n\n//go:embed " + testCase.directive + "\nvar Data embed.FS\n",
			}
			for _, name := range testCase.data {
				files["alpha/data/"+name] = name + "\n"
			}
			fixture := newPRStaticScopeFixture(t, files)
			runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(), "git rm -q --cached alpha/data/a.txt")
			status := runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(), "git diff --name-status HEAD --")
			if status != "D\talpha/data/a.txt\n" {
				t.Fatalf("fixture is not a single tracked deletion:\n%s", status)
			}
			if _, err := os.Stat(filepath.Join(fixture.repoRoot, "alpha", "data", "a.txt")); err != nil {
				t.Fatalf("index-only removal must leave the file on disk: %v", err)
			}

			fixture.resetCalls(t)
			if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
				t.Errorf("lint-affected failed: %v\n%s", err, output)
			}
			output, err := fixture.runMakeTargetWithGo("check-lean-local", fixture.realGo)
			if testCase.refuse {
				fixture.requireCalls(t, []string{"run", "./..."})
				fixture.requireGoCalls(t, []string{"vet", "./..."})
				if err == nil || !strings.Contains(output, "may be absent from the current embed inventory") {
					t.Fatalf("local profile accepted a sibling its own pattern does not embed: err=%v\n%s", err, output)
				}
				return
			}
			fixture.requireCalls(t, []string{"run", "./alpha"})
			fixture.requireGoCalls(t, []string{"vet", "./alpha"})
			if err != nil || !strings.Contains(output, "check-lean-local: testing ./alpha\n") {
				t.Fatalf("local profile did not select the embed owner: err=%v\n%s", err, output)
			}
		})
	}
}

// The sibling check must decide what cmd/go embeds, because an over-match
// forges the evidence that a pattern still resolves. Each fixture package
// declares one pattern over the same tree, so go list reports that pattern's
// own files, and the selector's matcher must agree on every file. The tree
// holds a symlink and go list runs with GODEBUG=embedfollowsymlinks=1, so a
// pattern naming the link embeds it while a directory walk still skips it.
func TestLeanLocalEmbedSiblingMatcherAgreesWithGoEmbed(t *testing.T) {
	patterns := []string{
		"data", "all:data", "data/*", "data/**", "data/*.txt", "all:data/*.txt",
		"data/*/*.txt", "data/?.txt", "data/[a-b].*", "data/[^a]*.txt", "data/[!a]*",
		`data/\a.txt`, "data/sub", "all:data/sub", "data/_skip", "data/.dotdir", "data/x.txt",
	}
	tree := []string{
		"a.txt", "b.json", "!bang.txt", ".hidden.txt", "_under.txt",
		"sub/c.txt", "sub/.dot.txt", "sub/_u.txt", "_skip/d.txt", ".dotdir/e.txt",
		"x.txt/inner.js", "x.txt/_inner.js",
	}
	files := map[string]string{}
	var links []string
	for index, pattern := range patterns {
		dir := fmt.Sprintf("p%02d", index)
		files[dir+"/p.go"] = fmt.Sprintf("package %s\n\nimport \"embed\"\n\n//go:embed %s\nvar Files embed.FS\n", dir, pattern)
		files[dir+"/target.txt"] = "target\n"
		for _, name := range tree {
			files[dir+"/data/"+name] = name + "\n"
		}
		links = append(links, dir+"/data/link.txt")
	}
	fixture := newPRStaticScopeFixture(t, files)
	for _, link := range links {
		if err := os.Symlink("../target.txt", filepath.Join(fixture.repoRoot, link)); err != nil {
			t.Fatalf("create embed oracle symlink: %v", err)
		}
	}

	list := testCommand(fixture.realGo, "list", "-json", "./...")
	list.Dir = fixture.repoRoot
	list.Env = append(fixture.commandEnv(), "GODEBUG=embedfollowsymlinks=1")
	var stderr bytes.Buffer
	list.Stderr = &stderr
	raw, err := list.Output()
	if err != nil {
		t.Fatalf("go list the embed oracle: %v\n%s", err, stderr.String())
	}
	type oraclePackage struct {
		Dir     string `json:"dir"`
		Pattern string `json:"pattern"`
	}
	var packages []oraclePackage
	want := map[string][]string{}
	for decoder := json.NewDecoder(bytes.NewReader(raw)); decoder.More(); {
		var listed struct {
			ImportPath    string
			EmbedPatterns []string
			EmbedFiles    []string
		}
		if err := decoder.Decode(&listed); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		dir := strings.TrimPrefix(listed.ImportPath, "example.com/static-scope/")
		if len(listed.EmbedPatterns) != 1 || len(listed.EmbedFiles) == 0 {
			t.Fatalf("package %s must embed through exactly one resolving pattern: %+v", dir, listed)
		}
		packages = append(packages, oraclePackage{Dir: dir, Pattern: listed.EmbedPatterns[0]})
		for _, embedded := range listed.EmbedFiles {
			want[dir] = append(want[dir], dir+"/"+embedded)
		}
		slices.Sort(want[dir])
	}
	if len(packages) != len(patterns) {
		t.Fatalf("go list reported %d oracle packages, want %d", len(packages), len(patterns))
	}

	candidates := append([]string{"go.mod"}, links...)
	for name := range files {
		candidates = append(candidates, name)
	}
	slices.Sort(candidates)
	request, err := json.Marshal(map[string]any{
		"root":     fixture.repoRoot,
		"packages": packages,
		"files":    candidates,
	})
	if err != nil {
		t.Fatalf("encode matcher request: %v", err)
	}
	code := `import json
import runpy
import sys

module = runpy.run_path(sys.argv[1], run_name="ci_static_select_contract")
embed_pattern = module["embed_pattern"]
embeds = module["embed_pattern_embeds_file"]
request = json.load(sys.stdin)
json.dump(
    {
        package["dir"]: sorted(
            path
            for path in request["files"]
            if embeds(
                embed_pattern("owner", package["dir"], package["pattern"]),
                path,
                request["root"],
            )
        )
        for package in request["packages"]
    },
    sys.stdout,
)
`
	match := testCommand("python3", "-c", code, filepath.Join(repoRoot(t), "scripts", "ci-static-select"))
	match.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	match.Stdin = bytes.NewReader(request)
	stderr.Reset()
	match.Stderr = &stderr
	matched, err := match.Output()
	if err != nil {
		t.Fatalf("run the selector's embed matcher: %v\n%s", err, stderr.String())
	}
	var got map[string][]string
	if err := json.Unmarshal(matched, &got); err != nil {
		t.Fatalf("decode matcher output: %v\n%s", err, matched)
	}
	for _, listed := range packages {
		if !slices.Equal(got[listed.Dir], want[listed.Dir]) {
			t.Errorf("pattern %q: selector embeds %v, go embeds %v", listed.Pattern, got[listed.Dir], want[listed.Dir])
		}
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
