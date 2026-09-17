package builtinpacks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const forkCoreSource = "https://github.com/quickserve-ai/gascity.git//internal/bootstrap/packs/core"

// TestPackForSourceSubpathRecognizesAForkNameForSourceRejects locks the one
// property this predicate exists for. A city that spells a bundled pack import
// against a FORK of the pack repository gets an ordinary remote import -- which
// is correct, and NameForSource must keep saying so, because it derives cache
// keys -- but the content is still meant to be that pack. If both predicates
// agreed, a fork-pinned pack could drift for weeks with nothing able to notice
// (ga-rvvji2).
func TestPackForSourceSubpathRecognizesAForkNameForSourceRejects(t *testing.T) {
	if _, ok := NameForSource(forkCoreSource); ok {
		t.Fatalf("NameForSource(%q) = ok; a fork URL must NOT resolve as a bundled import", forkCoreSource)
	}
	pack, ok := PackForSourceSubpath(forkCoreSource)
	if !ok {
		t.Fatalf("PackForSourceSubpath(%q) = !ok; want the core pack", forkCoreSource)
	}
	if pack.Name != "core" {
		t.Fatalf("PackForSourceSubpath(%q) = %q; want core", forkCoreSource, pack.Name)
	}
}

func TestPackForSourceSubpath(t *testing.T) {
	canonical, ok := Source("core")
	if !ok {
		t.Fatal("Source(core) = !ok")
	}
	for _, tc := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "canonical source", source: canonical, want: "core"},
		{name: "fork clone url", source: forkCoreSource, want: "core"},
		{name: "unrelated host", source: "https://example.invalid/mirror.git//internal/bootstrap/packs/core", want: "core"},
		{name: "no subpath", source: "https://github.com/quickserve-ai/gascity.git", want: ""},
		{name: "unknown subpath", source: "https://github.com/quickserve-ai/gascity.git//packs/nope", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pack, ok := PackForSourceSubpath(tc.source)
			if tc.want == "" {
				if ok {
					t.Fatalf("PackForSourceSubpath(%q) = %q, want no match", tc.source, pack.Name)
				}
				return
			}
			if !ok {
				t.Fatalf("PackForSourceSubpath(%q) = !ok, want %q", tc.source, tc.want)
			}
			if pack.Name != tc.want {
				t.Fatalf("PackForSourceSubpath(%q) = %q, want %q", tc.source, pack.Name, tc.want)
			}
		})
	}
}

// materializeCorePack writes the embedded core pack to a temp dir through the
// same writer the synthetic cache uses, so a clean comparison really does mean
// "identical to what this binary carries" rather than "identical to whatever my
// test happened to write".
func materializeCorePack(t *testing.T) (Pack, string) {
	t.Helper()
	pack, ok := ByName("core")
	if !ok {
		t.Fatal("ByName(core) = !ok")
	}
	dir := t.TempDir()
	if err := materializeFS(pack.FS, dir); err != nil {
		t.Fatalf("materializeFS: %v", err)
	}
	return pack, dir
}

func TestDiffEmbeddedPackCleanCopyHasNoDivergence(t *testing.T) {
	pack, dir := materializeCorePack(t)
	div, err := DiffEmbeddedPack(pack, dir)
	if err != nil {
		t.Fatalf("DiffEmbeddedPack: %v", err)
	}
	if div.Count() != 0 {
		t.Fatalf("clean materialization diverges: %s", div.Summary(10))
	}
	if got := div.Summary(10); got != "no divergence" {
		t.Fatalf("Summary = %q, want %q", got, "no divergence")
	}
}

func TestDiffEmbeddedPackDetectsChangedContent(t *testing.T) {
	pack, dir := materializeCorePack(t)
	target := filepath.Join(dir, "pack.toml")
	if err := os.WriteFile(target, []byte("# not the shipped pack\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	div, err := DiffEmbeddedPack(pack, dir)
	if err != nil {
		t.Fatalf("DiffEmbeddedPack: %v", err)
	}
	if len(div.Differing) != 1 || div.Differing[0] != "pack.toml" {
		t.Fatalf("Differing = %v, want [pack.toml]", div.Differing)
	}
	if len(div.Missing) != 0 || len(div.Extra) != 0 {
		t.Fatalf("unexpected other divergence: %s", div.Summary(10))
	}
}

// TestDiffEmbeddedPackDetectsLostExecutableBit covers the failure that reads as
// nothing at all: bytes match, the order execs the script, and the shell
// reports a permission error with no reference to the pack.
func TestDiffEmbeddedPackDetectsLostExecutableBit(t *testing.T) {
	pack, dir := materializeCorePack(t)
	var script string
	manifest, err := manifestForFS(pack.FS)
	if err != nil {
		t.Fatalf("manifestForFS: %v", err)
	}
	for rel, entry := range manifest {
		if entry.perm.Perm()&0o111 != 0 {
			script = rel
			break
		}
	}
	if script == "" {
		t.Skip("embedded core pack carries no executable file")
	}
	if err := os.Chmod(filepath.Join(dir, filepath.FromSlash(script)), 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	div, err := DiffEmbeddedPack(pack, dir)
	if err != nil {
		t.Fatalf("DiffEmbeddedPack: %v", err)
	}
	if len(div.Differing) != 1 || div.Differing[0] != script {
		t.Fatalf("Differing = %v, want [%s]", div.Differing, script)
	}
}

func TestDiffEmbeddedPackDetectsMissingAndExtra(t *testing.T) {
	pack, dir := materializeCorePack(t)
	if err := os.Remove(filepath.Join(dir, "pack.toml")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "left-behind.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	div, err := DiffEmbeddedPack(pack, dir)
	if err != nil {
		t.Fatalf("DiffEmbeddedPack: %v", err)
	}
	if len(div.Missing) != 1 || div.Missing[0] != "pack.toml" {
		t.Fatalf("Missing = %v, want [pack.toml]", div.Missing)
	}
	if len(div.Extra) != 1 || div.Extra[0] != "left-behind.sh" {
		t.Fatalf("Extra = %v, want [left-behind.sh]", div.Extra)
	}
	summary := div.Summary(10)
	for _, want := range []string{"missing here", "no longer in the binary", "pack.toml", "left-behind.sh"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("Summary = %q, want it to mention %q", summary, want)
		}
	}
}

// TestDiffEmbeddedPackCountsTestFilesSeparately keeps the report aimed at the
// executing surface. Pack test files ship inside the pack and never run as an
// order, a formula or a script; counting them as divergence is how a report
// earns a reputation for crying wolf.
func TestDiffEmbeddedPackCountsTestFilesSeparately(t *testing.T) {
	pack, dir := materializeCorePack(t)
	if err := os.WriteFile(filepath.Join(dir, "stray_test.go"), []byte("package core\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	div, err := DiffEmbeddedPack(pack, dir)
	if err != nil {
		t.Fatalf("DiffEmbeddedPack: %v", err)
	}
	if div.Count() != 0 {
		t.Fatalf("test-only divergence counted on the executing surface: %s", div.Summary(10))
	}
	if div.TestOnly != 1 {
		t.Fatalf("TestOnly = %d, want 1", div.TestOnly)
	}
}

func TestDiffEmbeddedPackRequiresADirectory(t *testing.T) {
	pack, ok := ByName("core")
	if !ok {
		t.Fatal("ByName(core) = !ok")
	}
	if _, err := DiffEmbeddedPack(pack, "  "); err == nil {
		t.Fatal("DiffEmbeddedPack with a blank directory returned no error")
	}
}

func TestEmbeddedDivergenceSummaryCapsPaths(t *testing.T) {
	div := EmbeddedDivergence{Differing: []string{"a", "b", "c", "d"}}
	got := div.Summary(2)
	if !strings.Contains(got, "a, b, +2 more") {
		t.Fatalf("Summary = %q, want it capped at 2 with a remainder", got)
	}
	if strings.Contains(got, "c") {
		t.Fatalf("Summary = %q, want the capped paths omitted", got)
	}
}
