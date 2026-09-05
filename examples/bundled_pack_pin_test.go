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

	seenSources := map[string]bool{}
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
			seenSources[pin.source] = true
			want := config.BundledSourcePinnedVersion(pin.source)
			if sameSha(pin.value, want) {
				continue
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			exampleDir := filepath.ToSlash(filepath.Dir(filepath.Join("examples", rel)))
			t.Errorf("examples/%s: %s pins bundled source %s at %s, but the binary's canonical pin is %s.\n"+
				"Only the canonical pin is served offline from embedded content, so this fixture fails "+
				"config.LoadWithIncludes on any machine without a pre-warmed cache (every CI runner).\n"+
				"Run \"gc doctor --fix\" in %s to re-pin it, or update the fixture in the same commit as the "+
				"config.BundledPackImportVersion bump.",
				filepath.ToSlash(rel), pin.field, pin.source, quoteSha(pin.value), quoteSha(want), exampleDir)
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
	bdSource, ok := builtinpacks.Source("bd")
	if !ok {
		t.Fatal("bundled bd pack not registered")
	}
	if !seenSources[bdSource] {
		t.Errorf("no example fixture pins %s; this guard exists to keep that import current, "+
			"so a walk that never sees it is not guarding anything", bdSource)
	}
}

// bundledPin is one checked-in pin of a bundled pack source.
type bundledPin struct {
	source string
	field  string // the pin's field name, e.g. `version` or `commit`
	value  string // as written, with or without the "sha:" prefix
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
		locked := lock.Packs[source]
		if v := strings.TrimSpace(locked.Version); v != "" {
			pins = append(pins, bundledPin{source: source, field: "version", value: v})
		}
		if c := strings.TrimSpace(locked.Commit); c != "" {
			pins = append(pins, bundledPin{source: source, field: "commit", value: c})
		}
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

// sameSha compares two pins written either bare or with the "sha:" prefix.
func sameSha(got, want string) bool {
	return strings.TrimPrefix(strings.TrimSpace(got), "sha:") ==
		strings.TrimPrefix(strings.TrimSpace(want), "sha:")
}

// quoteSha renders a pin in the "sha:<commit>" form the constants use, so the
// failure message reads the same whichever field carried it.
func quoteSha(pin string) string {
	pin = strings.TrimSpace(pin)
	if pin == "" {
		return `""`
	}
	if !strings.HasPrefix(pin, "sha:") {
		pin = "sha:" + pin
	}
	return pin
}
