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
