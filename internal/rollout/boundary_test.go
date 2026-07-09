package rollout

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestRolloutImportBoundary is the structural half of the general-Auto guarantee
// and the "no capability flags" line: internal/rollout non-test files may import
// ONLY the standard library, internal/config, and internal/deps. Importing any
// consumer package — internal/beads above all, but also beads-adjacent
// internal/beadmeta, internal/dispatch, internal/molecule, internal/events —
// fails this test naming the file and package. The capability model is general;
// beads CAS is merely its first consumer and lives on the OTHER side of this line.
func TestRolloutImportBoundary(t *testing.T) {
	t.Parallel()
	const self = "github.com/gastownhall/gascity/internal/rollout"
	allowedInternal := map[string]bool{
		"github.com/gastownhall/gascity/internal/config": true,
		"github.com/gastownhall/gascity/internal/deps":   true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if isStdlibImport(p) || allowedInternal[p] || p == self {
				continue
			}
			t.Errorf("%s imports disallowed package %q; internal/rollout must import only "+
				"stdlib + internal/config + internal/deps (it must never reach a consumer like internal/beads)", name, p)
		}
	}
	if checked == 0 {
		t.Fatal("import-boundary test scanned zero non-test package files")
	}
}

// isStdlibImport reports whether an import path is a standard-library package:
// its first path segment carries no dot, i.e. no module domain.
func isStdlibImport(p string) bool {
	seg := p
	if i := strings.IndexByte(p, '/'); i >= 0 {
		seg = p[:i]
	}
	return !strings.Contains(seg, ".")
}
