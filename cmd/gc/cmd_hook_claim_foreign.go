package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/gchome"
)

// Cross-town shared-pool invariant (ga-h4iqzr, agreed cross-town as R2a on
// qc-e8os51): a seat executes only molecules instantiated for its OWN host.
// Pool routing targets are unqualified on the shared store, so a molecule the
// other town poured is claimable here and vice versa — on 8/29 and again on
// 8/30 that produced duplicate cross-town execution, one town's failure racing
// the other's PR. A candidate is accepted iff its gc.formula_source was
// instantiated under one of this host's roots OR its formula positively
// declares path_agnostic. The refusal is LOUD and COUNTABLE ("declined-
// foreign"): a silent skip is how the box's 26-hour outage hid, and the drain
// reason lets the feeder detector consume the fact. The guard is symmetric —
// the box's seats carry the same check with the same signature (syl's half).

// declinedForeignSampleLimit caps how many declined ids the summary line names
// so a large pass stays readable (mirrors the sweeper's
// protectedForeignAssigneeIDSampleLimit).
const declinedForeignSampleLimit = 5

// hookClaimHostRoots returns the filesystem roots this host instantiates
// formulas under, in order: the city path, the gc home, and the invoking
// user's home. Molecules whose gc.formula_source lies outside every root were
// poured by another host.
//
// The gc home is the root that matters most (gc-m61x): a formula-instantiated
// molecule root is stamped with a gc.formula_source under the gc home's cache
// (`<gc home>/cache/repos/<hash>/.../formulas/<f>.toml`), and gc resolves that
// home through internal/gchome — GC_HOME first, then $HOME/.gc. On a host
// whose GC_HOME lies outside $HOME (westeros: GC_HOME=/data/gc-home) the user
// home does not cover it, and without this root every locally poured molecule
// was declined as foreign, bricking the pool (2026-09-23 17:02Z). The
// read-only resolver is used so computing the roots creates nothing on disk,
// and only a STABLE provenance (explicit GC_HOME or the user-home default) is
// a root: the process-unique temp fallback (`<tmp>/gc-home-<pid>`) never is,
// because nothing was instantiated under it and another host's source could
// match the string by coincidence. Roots are cleaned (`/data/gc-home/.` is
// `/data/gc-home`), and empty and duplicate entries are dropped.
func hookClaimHostRoots() []string {
	var roots []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		if seen[p] {
			return
		}
		seen[p] = true
		roots = append(roots, p)
	}
	add(os.Getenv("GC_CITY_PATH"))
	if h := gchome.ResolveReadOnly(); h.Provenance().Stable() {
		add(h.Path())
	}
	if h, err := os.UserHomeDir(); err == nil {
		add(h)
	}
	return roots
}

// formulaSourceForeignToHost reports whether an absolute formula_source path
// lies outside every host root. Empty and relative sources are NOT foreign
// (not formula-instantiated, or resolved against the local city). With no
// known roots the guard fails CLOSED — declining loudly is diagnosable, while
// claiming reproduces the duplicate-execution incident this guard exists for.
func formulaSourceForeignToHost(src string, hostRoots []string) bool {
	src = strings.TrimSpace(src)
	if src == "" || !filepath.IsAbs(src) {
		return false
	}
	for _, root := range hostRoots {
		root = strings.TrimRight(strings.TrimSpace(root), string(filepath.Separator))
		if root == "" {
			continue
		}
		if src == root || strings.HasPrefix(src, root+string(filepath.Separator)) {
			return false
		}
	}
	return true
}

// hookCandidateForeignSource returns the foreign formula_source and true when
// the candidate is a formula-instantiated molecule poured on another host and
// its formula does not declare path_agnostic. The declaration must be
// POSITIVE: an undeclared formula poured elsewhere is declined — defaulting to
// "claim unless proven foreign" is what produced the duplicate execution.
func hookCandidateForeignSource(candidate beads.Bead, hostRoots []string) (string, bool) {
	if strings.EqualFold(strings.TrimSpace(candidate.Metadata[beadmeta.FormulaPathAgnosticMetadataKey]), "true") {
		return "", false
	}
	src := strings.TrimSpace(candidate.Metadata[beadmeta.FormulaSourceMetadataKey])
	if formulaSourceForeignToHost(src, hostRoots) {
		return src, true
	}
	return "", false
}

// reportDeclinedForeign writes the never-silent summary for a pass that
// declined foreign-instantiated candidates, naming up to
// declinedForeignSampleLimit of them.
func reportDeclinedForeign(stderr io.Writer, declined []string) {
	if len(declined) == 0 {
		return
	}
	sample := declined
	suffix := ""
	if len(sample) > declinedForeignSampleLimit {
		suffix = fmt.Sprintf(" (+%d more)", len(sample)-declinedForeignSampleLimit)
		sample = sample[:declinedForeignSampleLimit]
	}
	fmt.Fprintf(stderr, "gc hook --claim: declined-foreign: %d routed candidate(s) instantiated for a foreign host were not claimed (R2a invariant, ga-h4iqzr): %s%s\n", //nolint:errcheck
		len(declined), strings.Join(sample, ", "), suffix)
}
