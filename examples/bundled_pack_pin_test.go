package examples_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/packman"
)

// TestShippedExamplesPinBundledPacksAtCanonicalVersion walks every checked-in
// example fixture and fails when a bundled pack (gascity.git core/bd/dolt, or a
// public gascity-packs pack) is pinned at anything other than the running
// binary's canonical pin.
//
// Only the canonical pin is served offline from the binary's embedded content:
// config.IsBundledSourceAtCanonicalPin compares the full sha exactly, so the
// offline self-heal in rematerializeAbsentBundledCache refuses to hydrate a
// cache for a superseded pin. A fixture left on the old sha therefore fails
// every config.LoadWithIncludes on a machine with no network and no pre-warmed
// cache — which is every CI runner. That is exactly how bumping
// BundledPackImportVersion once stranded the five example cities: the constant
// moved, the fixtures did not, and three packages-core shards went red on
// "locked but not cached".
//
// This guard makes the next bump fail here, in one obvious place, instead of
// scattered across the example suites.
func TestShippedExamplesPinBundledPacksAtCanonicalVersion(t *testing.T) {
	root := examplesRoot(t)

	seenPacks := map[string]bool{}
	pins := 0

	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".gc", "node_modules", "worktrees":
				return filepath.SkipDir
			}
			return nil
		}

		var found []bundledPin
		switch entry.Name() {
		case packman.LockfileName:
			found, err = lockfileBundledPins(path)
		case "pack.toml", "city.toml":
			found, err = manifestBundledPins(path)
		default:
			return nil
		}
		if err != nil {
			return err
		}

		for _, pin := range found {
			pins++
			if name, ok := builtinpacks.NameForSource(pin.source); ok {
				seenPacks[name] = true
			}
			// The comparison is deliberately exact per field, in the spelling
			// the resolver consumes: version fields carry the "sha:<commit>"
			// constraint form (packman treats anything else as a ref to fetch
			// over the network), and the lockfile commit field is the bare
			// commit (it keys the repo cache and IsBundledSourceAtCanonicalPin
			// verbatim). A pin that is "the right sha in the wrong spelling"
			// still strands an offline runner, so it must fail here too.
			want := config.BundledSourcePinnedVersion(pin.source)
			if pin.field == "commit" {
				want = strings.TrimPrefix(want, "sha:")
			}
			if pin.value == want {
				continue
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			exampleDir := filepath.ToSlash(filepath.Dir(filepath.Join("examples", rel)))
			t.Errorf("examples/%s: %s pins bundled source %s at %q, but the binary's canonical pin is %q.\n"+
				"Only the canonical pin, in exactly that spelling, is served offline from embedded content, so "+
				"this fixture fails config.LoadWithIncludes on any machine without a pre-warmed cache (every CI "+
				"runner).\nRun \"gc doctor --fix\" in %s to re-pin it, or update the fixture in the same commit "+
				"as the config.BundledPackImportVersion bump.",
				filepath.ToSlash(rel), pin.field, pin.source, pin.value, want, exampleDir)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking %s: %v", root, walkErr)
	}

	// A walk that matches nothing would report a clean bill of health for
	// fixtures it never opened. Assert the guard actually measured the pin
	// that stranded CI rather than trusting a silent zero.
	if pins == 0 {
		t.Fatal("found no bundled pack pins under examples/ — the fixture walk stopped measuring anything; " +
			"fix the walk before trusting this guard")
	}
	if !seenPacks["bd"] {
		t.Error("no example fixture pins the bundled bd pack (under any source spelling); this guard " +
			"exists to keep that import current, so a walk that never sees it is not guarding anything")
	}
}

// bundledPin is one checked-in pin of a bundled pack source.
type bundledPin struct {
	source string
	field  string // the pin's field name, e.g. `version` or `commit`
	value  string // exactly as written; the comparison is spelling-exact per field
}

// lockfileBundledPins reports the bundled-source pins recorded in a packs.lock.
// The lock is what config actually resolves against (resolveInstalledRemoteImport
// keys the repo cache off the locked commit), so both fields it stores are pins
// that can strand a fixture.
func lockfileBundledPins(path string) ([]bundledPin, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lock, err := packman.ParseLockfile(data)
	if err != nil {
		return nil, err
	}
	sources := make([]string, 0, len(lock.Packs))
	for source := range lock.Packs {
		sources = append(sources, source)
	}
	sort.Strings(sources)

	var pins []bundledPin
	for _, source := range sources {
		if !builtinpacks.IsSource(source) {
			continue
		}
		// Emit both fields unconditionally: a builtin lock entry missing its
		// version or commit is itself drift (install always writes both), and
		// an empty value fails the exact comparison with a message naming the
		// field instead of being silently skipped.
		locked := lock.Packs[source]
		pins = append(pins,
			bundledPin{source: source, field: "version", value: strings.TrimSpace(locked.Version)},
			bundledPin{source: source, field: "commit", value: strings.TrimSpace(locked.Commit)},
		)
	}
	return pins, nil
}

// manifestBundledPins reports the bundled-source pins declared in a pack.toml or
// city.toml. Imports appear at several depths ([imports.*],
// [defaults.rig.imports.*], per-rig imports), so this scans the decoded document
// for any table carrying a `source` rather than modeling each container — a new
// import site is covered the day it is added.
func manifestBundledPins(path string) ([]bundledPin, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return nil, err
	}
	var pins []bundledPin
	collectImportPins(doc, &pins)
	return pins, nil
}

func collectImportPins(node any, pins *[]bundledPin) {
	switch v := node.(type) {
	case map[string]any:
		if source, ok := v["source"].(string); ok {
			source = strings.TrimSpace(source)
			version, _ := v["version"].(string)
			version = strings.TrimSpace(version)
			// An import with no declared version resolves to the canonical
			// pin by construction, so it can never go stale.
			if version != "" && builtinpacks.IsSource(source) {
				*pins = append(*pins, bundledPin{source: source, field: "version", value: version})
			}
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectImportPins(v[key], pins)
		}
	case []map[string]any:
		for _, entry := range v {
			collectImportPins(entry, pins)
		}
	case []any:
		for _, entry := range v {
			collectImportPins(entry, pins)
		}
	}
}
