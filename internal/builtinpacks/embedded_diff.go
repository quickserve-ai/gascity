package builtinpacks

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
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
	// Differing are paths present in both whose CONTENT differs, relative to
	// the pack root and slash-separated. File mode is deliberately not
	// compared — see DiffEmbeddedPack.
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
	// SourceOnly counts on-disk paths that are part of the pack's SOURCE tree
	// but deliberately not part of what gc embeds: Go sources (a pack's
	// embed.go is what selects the embedded set, so it is never inside it) and
	// the pack's own README. A faithful checkout of the pack at exactly the
	// binary's revision still has these, so counting them as divergence would
	// make this report cry wolf on every correctly-pinned fork — which is the
	// one thing it cannot afford to do.
	SourceOnly int
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
	if out == "" {
		out = "no divergence"
	}
	if d.TestOnly > 0 {
		out += fmt.Sprintf("; %d test file(s) also differ, not counted", d.TestOnly)
	}
	if d.SourceOnly > 0 {
		out += fmt.Sprintf("; %d source-only file(s) present but never embedded, not counted", d.SourceOnly)
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
// CONTENT ONLY — file mode is deliberately NOT compared, and the first version
// of this function got that wrong. MaterializedFileMode assigns 0755 to every
// .sh/.py/.bash path and 0644 to everything else, so the embedded "mode" is
// derived entirely from the file's NAME and carries no information about
// whether its content is current. A git checkout meanwhile has git's tracked
// modes, which legitimately disagree: core's kimi session-start hook is tracked
// 100644 and materialized 0755. Comparing modes reported divergence on a
// faithful checkout of the pack at this binary's own revision — a false
// positive on exactly the configuration this check exists to reassure about. A
// mode difference cannot indicate stale content; it indicates a different
// materialization source.
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
		target := filepath.Join(dir, filepath.FromSlash(rel))
		info, err := os.Lstat(target)
		if err != nil {
			if os.IsNotExist(err) {
				div.add(rel, &div.Missing)
				continue
			}
			return div, fmt.Errorf("inspecting %s: %w", target, err)
		}
		if !info.Mode().IsRegular() {
			// A directory or symlink where the binary carries a file: the
			// content cannot be what we ship, whatever it resolves to.
			div.add(rel, &div.Differing)
			continue
		}
		got, err := os.ReadFile(target) //nolint:gosec // target derives from the embedded manifest
		if err != nil {
			return div, fmt.Errorf("reading %s: %w", target, err)
		}
		if !bytes.Equal(got, want.data) {
			div.add(rel, &div.Differing)
		}
	}

	extras, err := extraFiles(dir, manifest)
	if err != nil {
		return div, err
	}
	for _, rel := range extras {
		if isSourceOnly(rel) {
			div.SourceOnly++
			continue
		}
		div.add(rel, &div.Extra)
	}

	return div, nil
}

// isSourceOnly reports whether an on-disk path that the embedded set does not
// contain belongs to the pack's source tree rather than its executing surface.
//
// This exists because the pack directory in a real git checkout is a SUPERSET
// of what gets embedded: embed.go is the file that selects the embedded set, so
// it can never be in it, and the pack's README is documentation. Without this,
// a fork checked out at exactly the binary's own revision reports divergence
// forever, naming files that were never embedded — and a report that is always
// non-empty is a report nobody reads.
func isSourceOnly(rel string) bool {
	if strings.HasSuffix(rel, "_test.go") {
		// Pack tests have their own bucket; leave them to it.
		return false
	}
	if strings.HasSuffix(rel, ".go") {
		return true
	}
	return path.Base(rel) == "README.md"
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

// extraFiles lists paths under dir that the embedded manifest does not carry.
// A file the binary has deleted but the pinned copy still holds keeps
// executing, so it belongs in the report.
//
// Symlinks are REPORTED, not skipped. A symlinked script executes exactly like
// a regular one, and an omission here would be a silent clean result — the
// failure mode this whole comparison exists to remove. filepath.WalkDir does
// not descend through a symlinked directory, so its contents are not
// enumerated; the link itself is named instead, which is enough for a reader
// to go look. That asymmetry is the one gap left: a matching embedded file
// reached through a symlinked ANCESTOR still compares fine (os.Lstat on the
// full path follows intermediate links), while extra files beside it are not
// walked. Packs are materialized by gc itself and contain no symlinks, so this
// is a boundary note rather than a live hazard.
func extraFiles(dir string, manifest map[string]fileEntry) ([]string, error) {
	var extras []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
			// Sockets, devices, pipes: not pack content in any sense.
			return nil
		}
		rel, err := filepath.Rel(dir, p)
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

