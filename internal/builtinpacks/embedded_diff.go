package builtinpacks

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PackForSourceSubpath reports the bundled pack a source addresses BY SUBPATH
// ALONE, ignoring which clone URL it names.
//
// It deliberately differs from NameForSource, and the difference between them
// is the reason both exist. NameForSource decides RESOLUTION — whether gc
// serves an import from embedded content, and how its cache key is derived —
// so it must stay strict about the repository, and an import spelled against a
// FORK of the pack repository is correctly NOT a bundled import: it is fetched
// and pinned like any other remote, and editing its pin does exactly what it
// says. This predicate decides only COMPARABILITY: a fork's copy of
// internal/bootstrap/packs/core is still meant to BE the core pack, so a
// divergence report has to be able to recognize it and say how far the content
// that executes has drifted from the content this binary carries. Nothing here
// changes how anything resolves.
func PackForSourceSubpath(source string) (Pack, bool) {
	_, subpath := splitSource(source)
	if subpath == "" {
		return Pack{}, false
	}
	for _, layout := range syntheticPackLayouts() {
		if layout.Subpath != "" && layout.Subpath == subpath {
			return layout.Pack, true
		}
	}
	return Pack{}, false
}

// EmbeddedDivergence describes how a materialized pack directory differs from
// the running binary's embedded copy of the same pack.
type EmbeddedDivergence struct {
	// Differing are paths present in both whose content or executable bit
	// differs, relative to the pack root and slash-separated.
	Differing []string
	// Missing are paths this binary carries that the directory does not have.
	Missing []string
	// Extra are paths the directory has that this binary no longer carries.
	Extra []string
	// TestOnly counts *_test.go paths excluded from the three lists above.
	// Pack test files ship inside the pack but never execute as an order, a
	// formula or a script, so counting them as divergence trains readers to
	// ignore the report. They are counted, not hidden.
	TestOnly int
}

// Count returns the number of divergences on the pack's executing surface.
func (d EmbeddedDivergence) Count() int {
	return len(d.Differing) + len(d.Missing) + len(d.Extra)
}

// Summary renders the divergence as one line, listing at most limit paths from
// each category so a check message stays readable while still naming the files
// — a bare count sends the reader off to re-derive the list, which is how this
// class of staleness stayed invisible in the first place.
func (d EmbeddedDivergence) Summary(limit int) string {
	if d.Count() == 0 {
		return "no divergence"
	}
	var parts []string
	if len(d.Differing) > 0 {
		parts = append(parts, fmt.Sprintf("%d differ (%s)", len(d.Differing), joinCapped(d.Differing, limit)))
	}
	if len(d.Missing) > 0 {
		parts = append(parts, fmt.Sprintf("%d missing here (%s)", len(d.Missing), joinCapped(d.Missing, limit)))
	}
	if len(d.Extra) > 0 {
		parts = append(parts, fmt.Sprintf("%d no longer in the binary (%s)", len(d.Extra), joinCapped(d.Extra, limit)))
	}
	out := strings.Join(parts, "; ")
	if d.TestOnly > 0 {
		out += fmt.Sprintf("; %d test file(s) also differ, not counted", d.TestOnly)
	}
	return out
}

func joinCapped(paths []string, limit int) string {
	if limit <= 0 || len(paths) <= limit {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s, +%d more", strings.Join(paths[:limit], ", "), len(paths)-limit)
}

// DiffEmbeddedPack compares the pack tree materialized at dir against pack's
// embedded content, file by file.
//
// It reads this binary's own embedded FS as the reference, which is the point:
// every other way of answering "is the pack fix live?" reaches for something
// adjacent to the executing copy — the installed commit's tree, a merge-base,
// a file mtime, the pack cache's git head (there is no .git in a materialized
// pack, so git answers about the enclosing repository instead) — and all of
// those can be true while the copy that executes is weeks behind (ga-rvvji2).
//
// Content and the executable bit both count. A script that lost +x fails at
// exec time with no useful message, so it is divergence even when the bytes
// match.
func DiffEmbeddedPack(pack Pack, dir string) (EmbeddedDivergence, error) {
	var div EmbeddedDivergence
	if strings.TrimSpace(dir) == "" {
		return div, fmt.Errorf("pack directory is required")
	}
	manifest, err := manifestForFS(pack.FS)
	if err != nil {
		return div, fmt.Errorf("reading embedded %q pack: %w", pack.Name, err)
	}

	rels := make([]string, 0, len(manifest))
	for rel := range manifest {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	for _, rel := range rels {
		want := manifest[rel]
		path := filepath.Join(dir, filepath.FromSlash(rel))
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				div.add(rel, &div.Missing)
				continue
			}
			return div, fmt.Errorf("inspecting %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			div.add(rel, &div.Differing)
			continue
		}
		got, err := os.ReadFile(path) //nolint:gosec // path derives from the embedded manifest
		if err != nil {
			return div, fmt.Errorf("reading %s: %w", path, err)
		}
		if !bytes.Equal(got, want.data) || executable(info.Mode()) != executable(want.perm) {
			div.add(rel, &div.Differing)
		}
	}

	extras, err := extraFiles(dir, manifest)
	if err != nil {
		return div, err
	}
	for _, rel := range extras {
		div.add(rel, &div.Extra)
	}

	return div, nil
}

// add files rel into bucket, or counts it as test-only. Pack test files are
// tracked separately rather than dropped so a report can say that it saw them
// and chose not to count them.
func (d *EmbeddedDivergence) add(rel string, bucket *[]string) {
	if strings.HasSuffix(rel, "_test.go") {
		d.TestOnly++
		return
	}
	*bucket = append(*bucket, rel)
}

// extraFiles lists regular files under dir that the embedded manifest does not
// carry. A file the binary has deleted but the pinned copy still holds keeps
// executing, so it belongs in the report.
func extraFiles(dir string, manifest map[string]fileEntry) ([]string, error) {
	var extras []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := manifest[rel]; !ok {
			extras = append(extras, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", dir, err)
	}
	sort.Strings(extras)
	return extras, nil
}

func executable(mode os.FileMode) bool { return mode.Perm()&0o111 != 0 }
