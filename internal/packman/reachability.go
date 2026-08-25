package packman

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/gitcred"
)

// Issue codes emitted by VerifySourceReachability.
const (
	// CodePinUnreachableAtSource means the declared source advertises refs and
	// none of them reach the pinned commit. The pin therefore resolves today
	// only from a warm local cache: a fresh city, a new machine, or the first
	// run after "gc import prune" fails to install this pack.
	CodePinUnreachableAtSource = "pin-unreachable-at-source"
	// CodeSourceProbeFailed means the probe could not reach a verdict at all
	// (offline, auth failure, unreachable host). It is deliberately a warning:
	// "I could not tell" must not read the same as "I checked and it is bad".
	CodeSourceProbeFailed = "source-probe-failed"
)

// reachabilityRefNamespace is where the probe parks the source's advertised
// refs inside its scratch repository. It is deliberately not refs/heads or
// refs/remotes: the containment query is scoped to this namespace so nothing
// the scratch repo acquired by another route can be mistaken for a ref the
// source actually advertises.
const reachabilityRefNamespace = "refs/gc-reach"

// SourceReachability is one source's verdict.
type SourceReachability struct {
	Source string
	Commit string
	// Reachable reports whether some ref advertised by the source reaches the
	// pinned commit.
	Reachable bool
	// Ref is the containing ref that proved reachability, empty when unreachable.
	Ref string
	// Tips is how many refs the source advertised.
	Tips int
}

// VerifySourceReachability probes each pinned import against its DECLARED
// source and reports every pin that no ref at that source reaches.
//
// This is the check CheckInstalled cannot be: CheckInstalled is offline by
// contract and only validates that declared == locked == installed, which is
// true and useless when the declared source cannot produce the pin at all.
// Reaching a verdict here REQUIRES the network, so this lives behind an
// explicit opt-in and never runs on the default check or doctor path.
//
// pins maps a declared remote source to the commit that source is pinned to;
// CheckReport.CheckedPins from an offline CheckInstalled run supplies it.
func VerifySourceReachability(cityRoot string, pins map[string]string) (*CheckReport, error) {
	report := &CheckReport{}
	for _, source := range sortedPinSources(pins) {
		commit := strings.TrimSpace(pins[source])
		if commit == "" {
			continue
		}
		// A bundled source at its canonical pin is materialized from the binary
		// itself, not fetched, so it has no remote to interrogate. Probing it
		// would report a failure for a pack that is by construction present.
		if config.IsBundledSourceAtCanonicalPin(source, commit) {
			continue
		}
		report.CheckedSources++
		verdict, err := probeSourceReachability(cityRoot, source, commit)
		if err != nil {
			report.addIssue(CheckIssue{
				Severity: CheckSeverityWarning,
				Code:     CodeSourceProbeFailed,
				Source:   source,
				Commit:   commit,
				Message:  fmt.Sprintf("could not determine whether the pin is reachable at its source: %v", err),
			})
			continue
		}
		if verdict.Reachable {
			continue
		}
		report.addIssue(CheckIssue{
			Code:    CodePinUnreachableAtSource,
			Source:  source,
			Commit:  commit,
			Message: unreachableMessage(verdict),
			RepairHint: "point this import at a source that can produce the pin, or publish the commit " +
				"to a ref there; the pin resolves today only from the local repo cache",
		})
	}
	return report, nil
}

func unreachableMessage(v SourceReachability) string {
	if v.Tips == 0 {
		return "declared source advertises no refs at all"
	}
	return fmt.Sprintf("no ref at the declared source reaches the pinned commit (%d ref(s) advertised)", v.Tips)
}

// probeSourceReachability answers "does some ref at this source reach this
// commit". It deliberately does NOT ask "does this source serve this object":
// GitHub fork networks share an object store, so a fork-only commit is served
// from the parent repository's URL and any existence probe -- the REST commits
// endpoint, or a direct `git fetch <url> <sha>`, both measured returning
// success for exactly such a commit on 2026-08-24 -- reports a healthy import
// for the one input this check exists to catch. Only ref containment
// discriminates.
func probeSourceReachability(cityRoot, source, commit string) (SourceReachability, error) {
	verdict := SourceReachability{Source: source, Commit: commit}
	cloneURL := normalizeRemoteSource(source).CloneURL
	if strings.TrimSpace(cloneURL) == "" {
		return verdict, fmt.Errorf("source %q has no clone URL", gitcred.RedactUserinfo(source))
	}

	out, err := runNetworkGit(cityRoot, cloneURL, "", "ls-remote", "--heads", "--tags", cloneURL)
	if err != nil {
		return verdict, fmt.Errorf("listing refs for %q: %w", gitcred.RedactUserinfo(source), err)
	}
	tips := parseRemoteRefTips(out)
	verdict.Tips = len(tips)
	if verdict.Tips == 0 {
		// No refs advertised: nothing can contain the pin. A definitive miss,
		// not a probe failure.
		return verdict, nil
	}
	// A pin that IS a ref tip needs no object transfer to prove.
	if ref, ok := tips[strings.ToLower(strings.TrimSpace(commit))]; ok {
		verdict.Reachable = true
		verdict.Ref = ref
		return verdict, nil
	}

	scratch, err := os.MkdirTemp("", "gc-import-reach-")
	if err != nil {
		return verdict, fmt.Errorf("creating probe workspace: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck

	if _, err := runGit("", "init", "--quiet", "--bare", scratch); err != nil {
		return verdict, fmt.Errorf("preparing probe workspace: %w", err)
	}
	attachCacheAlternate(scratch, source, commit)

	if err := fetchReachabilityGraph(cityRoot, cloneURL, scratch); err != nil {
		return verdict, err
	}

	hit, err := runGit(scratch, "for-each-ref", "--contains", commit, "--count=1",
		"--format=%(refname:short)", reachabilityRefNamespace)
	if err != nil {
		// for-each-ref --contains fails on an object it does not have. When the
		// commit is genuinely absent from the graph the source advertises, that
		// IS the answer -- absent from every advertised ref's history.
		if !objectPresent(scratch, commit) {
			return verdict, nil
		}
		return verdict, fmt.Errorf("testing ref containment for %s: %w", commit, err)
	}
	if hit = strings.TrimSpace(hit); hit != "" {
		verdict.Reachable = true
		verdict.Ref = hit
	}
	return verdict, nil
}

// fetchReachabilityGraph pulls the source's advertised refs and the commit
// graph behind them into scratch. --filter=tree:0 is what makes this
// affordable: ancestry needs commits only, so trees and blobs never move.
// Servers that refuse partial clone get a plain fetch rather than a failed
// check.
func fetchReachabilityGraph(cityRoot, cloneURL, scratch string) error {
	refspecs := []string{
		"+refs/heads/*:" + reachabilityRefNamespace + "/heads/*",
		"+refs/tags/*:" + reachabilityRefNamespace + "/tags/*",
	}
	filtered := append([]string{"fetch", "--quiet", "--no-tags", "--filter=tree:0", cloneURL}, refspecs...)
	if _, err := runNetworkGit(cityRoot, cloneURL, scratch, filtered...); err == nil {
		return nil
	}
	plain := append([]string{"fetch", "--quiet", "--no-tags", cloneURL}, refspecs...)
	if _, err := runNetworkGit(cityRoot, cloneURL, scratch, plain...); err != nil {
		return fmt.Errorf("fetching refs from %q: %w", gitcred.RedactUserinfo(cloneURL), err)
	}
	return nil
}

// attachCacheAlternate points the scratch repo at the already-installed cache
// clone's object store. It is purely an accelerator -- the fork network's
// history is usually already on disk, so only the delta moves (measured
// 2026-08-24: 40s cold, 4.7s with the alternate on the same probe, same
// verdict). It CANNOT change a verdict: refs come only from the source, and
// containment is scoped to the namespace those refs land in. Best effort by
// design; a missing or unreadable cache just costs time.
func attachCacheAlternate(scratch, source, commit string) {
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		return
	}
	objects := filepath.Join(cachePath, ".git", "objects")
	if st, err := os.Stat(objects); err != nil || !st.IsDir() {
		return
	}
	altDir := filepath.Join(scratch, "objects", "info")
	if err := os.MkdirAll(altDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(altDir, "alternates"), []byte(objects+"\n"), 0o600) //nolint:errcheck
}

func objectPresent(repo, commit string) bool {
	_, err := runGit(repo, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// parseRemoteRefTips maps each advertised tip commit to one ref name. Peeled
// tag lines (refs/tags/x^{}) are kept under the tag's name so an annotated
// tag's target counts as a tip.
func parseRemoteRefTips(out string) map[string]string {
	tips := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		sha := strings.ToLower(fields[0])
		ref := strings.TrimSuffix(fields[1], "^{}")
		if sha == "" || ref == "" {
			continue
		}
		if _, seen := tips[sha]; !seen {
			tips[sha] = ref
		}
	}
	return tips
}

func sortedPinSources(pins map[string]string) []string {
	sources := make([]string, 0, len(pins))
	for source := range pins {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	return sources
}
