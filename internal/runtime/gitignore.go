package runtime

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// HashSkillSourceContent returns a hex-encoded SHA-256 of the content at
// path, like HashPathContent, but additionally excludes files that are
// git-ignored so a skill running its own scripts (or a build tool writing
// artifacts into the source tree) can never mutate the skill's
// config-fingerprint entry and trigger a drift drain (ga-rpf2).
//
// Semantics, chosen for cross-process determinism:
//
//   - Only committed-shape .gitignore files are consulted: those in the
//     hashed directory and below, plus those on the path from the enclosing
//     repository root down to the hashed directory. $GIT_DIR/info/exclude
//     and core.excludesFile are deliberately NOT read — they are per-user
//     state, and two processes with different user config must never
//     compute different hashes for the same checkout.
//   - No git subprocess is spawned. A subprocess that fails transiently
//     under load would flip the hash to the unavailable sentinel and back —
//     the exact nondeterminism this hasher exists to remove.
//   - Ignored files are excluded; untracked-but-not-ignored files still
//     hash. A legitimately new, not-yet-committed skill file must keep
//     triggering the drift that delivers it.
//   - The hardcoded artifact blocklist (hashPathContentSkipEntry) remains
//     the floor everywhere, including directories outside any git repo.
//   - An embedded .git directory is never hashed.
//   - If the hashed directory itself sits in git-ignored territory (for
//     example a skills dir under an ignored runtime root), ancestor rules
//     are dropped — otherwise they would exclude every entry and freeze
//     the hash — and only .gitignore files inside the hashed directory
//     apply.
//
// For a directory containing no ignored entries and no embedded .git, the
// output is byte-identical to HashPathContent, so switching a call site to
// this function does not flip existing fingerprints.
func HashSkillSourceContent(root string) string {
	info, err := os.Stat(root)
	if err != nil {
		return ""
	}
	if !info.IsDir() {
		return HashPathContent(root)
	}
	m := newSkillIgnoreMatcher(root)

	var entries []string
	var walkErr bool
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			walkErr = true
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			m.enterDir(root, "")
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if hashPathContentSkipEntry(d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if m.ignored(filepath.ToSlash(rel), d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			m.enterDir(p, filepath.ToSlash(rel))
			return nil
		}
		entries = append(entries, rel)
		return nil
	})
	if walkErr || m.err != nil {
		return ""
	}
	sort.Strings(entries)
	h := sha256.New()
	for _, rel := range entries {
		h.Write([]byte(rel)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})   //nolint:errcheck // hash.Write never errors
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return ""
		}
		h.Write(data)      //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0}) //nolint:errcheck // hash.Write never errors
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// skillIgnoreMatcher evaluates gitignore rules for entries under a hashed
// root. All rule bases and query paths live in "eval space": slash-separated
// paths relative to the enclosing repository root, or relative to the hashed
// root when it is not inside a repository (or when the carve-out dropped the
// ancestor context).
type skillIgnoreMatcher struct {
	rules []ignoreRule
	// rootPrefix maps hashed-root-relative paths into eval space; "" when
	// the hashed root is itself the eval root.
	rootPrefix string
	// err records a .gitignore that exists but could not be read. The
	// caller must fail closed: guessed ignore semantics would make the
	// hash depend on transient I/O state.
	err error
}

func newSkillIgnoreMatcher(root string) *skillIgnoreMatcher {
	m := &skillIgnoreMatcher{}
	repoRoot := findRepoRoot(root)
	if repoRoot == "" || repoRoot == root {
		return m
	}
	relRoot, err := filepath.Rel(repoRoot, root)
	if err != nil || relRoot == "." || strings.HasPrefix(relRoot, "..") {
		return m
	}
	m.rootPrefix = filepath.ToSlash(relRoot)
	segs := strings.Split(m.rootPrefix, "/")
	dir := repoRoot
	base := ""
	m.loadFile(filepath.Join(dir, ".gitignore"), base)
	for _, s := range segs[:len(segs)-1] {
		dir = filepath.Join(dir, s)
		base = path.Join(base, s)
		m.loadFile(filepath.Join(dir, ".gitignore"), base)
	}
	if m.rootInIgnoredTerritory() {
		m.rules = nil
		m.rootPrefix = ""
	}
	return m
}

// rootInIgnoredTerritory reports whether any directory on the path from the
// repository root to the hashed root is itself ignored. In git, everything
// under an ignored directory is ignored with no possible re-inclusion, so
// keeping ancestor rules would exclude every entry and freeze the hash.
func (m *skillIgnoreMatcher) rootInIgnoredTerritory() bool {
	p := ""
	for _, s := range strings.Split(m.rootPrefix, "/") {
		p = path.Join(p, s)
		if m.ignoredEval(p, true) {
			return true
		}
	}
	return false
}

// enterDir loads dir/.gitignore, if present, scoped to the directory's
// eval-space path. rel is the hashed-root-relative slash path ("" for the
// hashed root itself). WalkDir visits a directory before its contents, so
// rules are always in place before the entries they govern.
func (m *skillIgnoreMatcher) enterDir(dir, rel string) {
	m.loadFile(filepath.Join(dir, ".gitignore"), path.Join(m.rootPrefix, rel))
}

func (m *skillIgnoreMatcher) loadFile(gitignorePath, base string) {
	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		if !os.IsNotExist(err) && m.err == nil {
			m.err = err
		}
		return
	}
	m.rules = append(m.rules, parseIgnoreLines(string(data), base)...)
}

// ignored reports whether the hashed-root-relative slash path rel is
// git-ignored. Later-loaded rules (deeper .gitignore files) and later lines
// within a file take precedence, matching git's last-match-wins evaluation.
func (m *skillIgnoreMatcher) ignored(rel string, isDir bool) bool {
	return m.ignoredEval(path.Join(m.rootPrefix, rel), isDir)
}

func (m *skillIgnoreMatcher) ignoredEval(evalRel string, isDir bool) bool {
	result := false
	for _, r := range m.rules {
		if r.matches(evalRel, isDir) {
			result = !r.negate
		}
	}
	return result
}

// ignoreRule is one parsed .gitignore pattern. Supported subset: comments,
// blank lines, `!` negation, trailing-slash directory-only patterns,
// slash-anchored patterns, `*` / `?` / `[...]` globs (via path.Match), and
// `**` spanning zero or more path segments (a trailing `/**` requires at
// least one). Re-inclusion under an excluded directory is impossible, as in
// git: the walk skips an ignored directory's entire subtree.
type ignoreRule struct {
	segs     []string
	base     string
	negate   bool
	dirOnly  bool
	anchored bool
}

func parseIgnoreLines(data, base string) []ignoreRule {
	var rules []ignoreRule
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := false
		if strings.HasPrefix(line, "!") {
			negate = true
			line = line[1:]
		}
		if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
			line = line[1:]
		}
		line = trimUnescapedTrailingSpaces(line)
		dirOnly := false
		if strings.HasSuffix(line, "/") {
			dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		anchored := strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			continue
		}
		rules = append(rules, ignoreRule{
			segs:     strings.Split(line, "/"),
			base:     base,
			negate:   negate,
			dirOnly:  dirOnly,
			anchored: anchored,
		})
	}
	return rules
}

func trimUnescapedTrailingSpaces(s string) string {
	for strings.HasSuffix(s, " ") && !strings.HasSuffix(s, `\ `) {
		s = s[:len(s)-1]
	}
	return s
}

func (r ignoreRule) matches(evalRel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	q := evalRel
	if r.base != "" {
		prefix := r.base + "/"
		if !strings.HasPrefix(q, prefix) {
			return false
		}
		q = q[len(prefix):]
	}
	if !r.anchored {
		// A pattern without a slash matches a name at any depth below the
		// .gitignore's directory. Directory entries deeper than a matching
		// directory are handled by the walk skipping that subtree.
		return matchSegments(r.segs, []string{path.Base(q)})
	}
	return matchSegments(r.segs, strings.Split(q, "/"))
}

func matchSegments(ps, qs []string) bool {
	if len(ps) == 0 {
		return len(qs) == 0
	}
	if ps[0] == "**" {
		if len(ps) == 1 {
			// Trailing /** matches everything inside, not the directory itself.
			return len(qs) >= 1
		}
		for i := 0; i <= len(qs); i++ {
			if matchSegments(ps[1:], qs[i:]) {
				return true
			}
		}
		return false
	}
	if len(qs) == 0 {
		return false
	}
	ok, err := path.Match(ps[0], qs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(ps[1:], qs[1:])
}

// findRepoRoot walks up from dir looking for a .git entry (a directory in a
// normal checkout, a file in a linked worktree). Returns "" when dir is not
// inside a git repository.
func findRepoRoot(dir string) string {
	d := dir
	for {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}
