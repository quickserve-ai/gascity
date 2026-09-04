package main

import (
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// Publish-pointer gate (ga-krso22 / ga-mmvpq1 B5). A bead may not enter the
// published state without carrying a pointer to the PR that was published on its
// behalf.
//
// THE HOLE THIS CLOSES (qc-tdkydo.58). A bead reached
// merge_result=pr_published_awaiting_gate with no pr_url, so the PR existed and
// nothing on the bead led to it. Every downstream mechanism that could have
// noticed — the refinery patrol, the silence detector, a human reading the bead
// — starts from the bead and finds no way to reach the work. Three people
// rebuilt one such branch independently, each having correctly concluded that
// nothing existed.
//
// WHY THE INVARIANT IS ON THE RESULTING STATE, NOT "THE SAME UPDATE". The design
// phrases it as "setting the state without the pointers in the SAME update
// refuses". Implemented literally that breaks a legitimate live path: the
// refinery's find-work release (formulas/mol-refinery-patrol.toml) re-sets
// merge_result=pr_published_awaiting_gate on a bead that ALREADY carries pr_url,
// and deliberately only does so when pr_url is non-empty. Gating on the
// resulting bead state instead is both stricter and correct — at the moment the
// state is set, the bead must HAVE a pointer, whether this update supplied it or
// a previous one did. A two-step "set the state now, add the pointer later" is
// still refused, because the refusal fires on the state-setting update.
//
// WHY pr_url IS THE LOAD-BEARING POINTER (08:10Z). "A pointer that resolves to
// an action record is insufficient. It has to resolve to something
// RE-DERIVABLE." pr_number alone is an index into a repo the bead does not name;
// pr_url is fetchable as it stands. pr_number is accepted as derivable FROM
// pr_url rather than separately required — measured on the live store, 157 of
// 197 beads carrying merge_result have pr_url while only 143 have pr_number, so
// requiring both independently would refuse writes the factory legitimately
// makes today while adding no evidence that pr_url does not already carry.
//
// The gate SHAPE-CHECKS the URL rather than fetching it: a write must not depend
// on network reachability, and an unfetchable-but-well-formed URL is a different
// (and detectable) problem from no pointer at all. The silence detector fetches
// it every sweep, which is where liveness belongs.
//
// Unlike the work-record close gate this ships ENFORCING, with no warn-only
// default. The refusal fires only on a write that would CREATE the hole; a bead
// that cannot name its own PR has no business entering a state whose entire
// meaning is "a PR is waiting". Legacy beads already in the state are untouched
// by a write gate — they are reconciled by the silence detector, which alarms on
// a published bead with no resolvable pointer.

const (
	mergeResultMetadataKey = "merge_result"
	prNumberMetadataKey    = "pr_number"
	prURLMetadataKey       = "pr_url"

	// publishedAwaitingGate is the state whose meaning is "a PR exists and is
	// waiting on a gate". gate_clear_awaiting_merge is deliberately NOT gated
	// here: a bead can only reach it by passing through publication, so gating
	// the entry point covers it without a second refusal surface.
	publishedAwaitingGate = "pr_published_awaiting_gate"
)

// parsePRURL reports the PR number a pr_url names, and whether the URL is a
// fetchable GitHub pull-request URL at all.
//
// Parsed with net/url rather than matched with a regex: a hand-rolled pattern
// whose host and path segments merely exclude "/" accepts strings that are not
// PR URLs (query and fragment delimiters slip into what looks like a path, so
// "https://github.com/?/repo/pull/1" matches) while rejecting fetchable variants
// like an uppercase host. Host comparison is therefore case-folded, and the path
// is validated segment by segment on the PARSED path, after url.Parse has
// already split off any query or fragment.
func parsePRURL(raw string) (number string, ok bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" {
		return "", false
	}
	if !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", false
	}
	segments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	// owner / repo / "pull" / number
	if len(segments) < 4 || segments[2] != "pull" {
		return "", false
	}
	if segments[0] == "" || segments[1] == "" {
		return "", false
	}
	if _, err := strconv.Atoi(segments[3]); err != nil {
		return "", false
	}
	return segments[3], true
}

// publishPointerViolation describes why a write was refused, in terms the caller
// can act on.
type publishPointerViolation struct {
	beadID string
	reason string
}

// prospectivePublishState applies an update's metadata edits to a stored bead's
// metadata and reports the resulting merge_result plus the pointers that would
// accompany it. Reusing parseWorkRecordMetadataEdits keeps this gate's view of
// bd's flag semantics identical to the close gate's — one parser, one set of
// quirks, rather than two that drift.
func prospectivePublishState(stored beads.Bead, bdArgs []string) (mergeResult, prNumber, prURL string, err error) {
	metadata := make(beads.StringMap, len(stored.Metadata)+4)
	for key, value := range stored.Metadata {
		metadata[key] = value
	}
	edits, err := parseWorkRecordMetadataEdits(bdArgs)
	if err != nil {
		return "", "", "", err
	}
	if err := applyWorkRecordMetadataEdits(metadata, edits); err != nil {
		return "", "", "", err
	}
	return strings.TrimSpace(metadata[mergeResultMetadataKey]),
		strings.TrimSpace(metadata[prNumberMetadataKey]),
		strings.TrimSpace(metadata[prURLMetadataKey]), nil
}

// checkPublishPointer reports a violation when the resulting bead state would be
// the published state without a re-derivable PR pointer. A nil return means the
// write is allowed.
func checkPublishPointer(beadID, mergeResult, prNumber, prURL string) *publishPointerViolation {
	if mergeResult != publishedAwaitingGate {
		return nil
	}
	if prURL == "" {
		if prNumber == "" {
			return &publishPointerViolation{beadID: beadID, reason: fmt.Sprintf(
				"carries neither %s nor %s", prURLMetadataKey, prNumberMetadataKey)}
		}
		// A bare number is not re-derivable: it indexes a repository the bead
		// does not name, so nothing can fetch it without out-of-band knowledge.
		return &publishPointerViolation{beadID: beadID, reason: fmt.Sprintf(
			"carries %s=%s but no %s — a bare PR number names no repository and cannot be fetched",
			prNumberMetadataKey, prNumber, prURLMetadataKey)}
	}
	urlNumber, ok := parsePRURL(prURL)
	if !ok {
		return &publishPointerViolation{beadID: beadID, reason: fmt.Sprintf(
			"%s=%q is not a fetchable GitHub pull-request URL", prURLMetadataKey, prURL)}
	}
	if prNumber != "" && prNumber != urlNumber {
		return &publishPointerViolation{beadID: beadID, reason: fmt.Sprintf(
			"%s=%s disagrees with %s (which names PR #%s) — the two pointers must not name different PRs",
			prNumberMetadataKey, prNumber, prURLMetadataKey, urlNumber)}
	}
	return nil
}

// runPublishPointerGate refuses a `gc bd` write that would leave a bead in the
// published state with no re-derivable pointer to its PR. Returns true when the
// write was blocked.
//
// Scope note, stated rather than implied: this gates writes that pass through
// `gc bd`, which is the path every formula and script in the town actually uses.
// A direct `bd update` bypasses gc entirely and this cannot see it. Closing that
// truly at the storage boundary is a bd-side change (filed as follow-up); this
// closes it at the seam gc owns.
func runPublishPointerGate(bdArgs []string, store beads.Store, preFetched map[string]beads.Bead, stderr io.Writer) bool {
	targets, ok := publishPointerTargets(bdArgs)
	if !ok || len(targets) == 0 {
		return false
	}
	if store == nil {
		// Cannot verify. Never block a write on our own read failure — the same
		// rule the detector applies to reads: a failure is UNKNOWN, not a verdict.
		return false
	}
	blocked := false
	for _, id := range targets {
		stored, ok := preFetched[id]
		if !ok {
			fetched, err := store.Get(id)
			if err != nil {
				// Absent/ephemeral/unreadable: fall through rather than refuse.
				continue
			}
			stored = fetched
		}
		mergeResult, prNumber, prURL, err := prospectivePublishState(stored, bdArgs)
		if err != nil {
			continue
		}
		violation := checkPublishPointer(id, mergeResult, prNumber, prURL)
		if violation == nil {
			continue
		}
		fmt.Fprintf(stderr, "gc bd: refusing to set %s=%s on %s: %s\n",
			mergeResultMetadataKey, publishedAwaitingGate, violation.beadID, violation.reason) //nolint:errcheck // best-effort stderr
		fmt.Fprintf(stderr, "  A bead in this state means \"a PR exists and is waiting\". Without a fetchable\n"+
			"  %s nothing that starts from the bead can reach the work, which is how a\n"+
			"  published branch becomes invisible and gets rebuilt from scratch (qc-tdkydo.58).\n"+
			"  Set it in the same update:  --set-metadata %s=https://github.com/<owner>/<repo>/pull/<n>\n",
			prURLMetadataKey, prURLMetadataKey) //nolint:errcheck // best-effort stderr
		blocked = true
	}
	return blocked
}

// publishPointerTargets reports the beads a `bd update` would mutate, when that
// update touches merge_result at all. Scoped to `update` deliberately: `bd
// create` cannot land in the published state without an update (the factory
// publishes onto an existing work bead), and narrowing the trigger keeps the
// gate off the hot path of every create in the town.
//
// The ambiguity check is load-bearing and inherited from the close gate: if the
// write IDs cannot be resolved unambiguously, the gate declines rather than
// guessing which bead it is protecting.
func publishPointerTargets(bdArgs []string) ([]string, bool) {
	if len(bdArgs) == 0 || bdArgs[0] != "update" {
		return nil, false
	}
	if !bdUpdateTouchesPublishState(bdArgs) {
		return nil, false
	}
	ids, ok, ambiguous := bdMutationWriteIDs(bdArgs)
	if !ok || ambiguous || len(ids) == 0 {
		return nil, false
	}
	return ids, true
}

// bdUpdateTouchesMergeResult reports whether an update mentions merge_result in
// any metadata form. A cheap pre-filter so the gate parses and reads a bead only
// for the writes that could possibly trip it.
func bdUpdateTouchesPublishState(bdArgs []string) bool {
	for _, arg := range bdArgs {
		// merge_result: the state itself. pr_url / pr_number: REMOVING or
		// blanking the pointer on a bead already in the published state breaks
		// the same invariant as never setting it, and an update that mentions
		// only the pointer would otherwise sail past a merge_result-only
		// trigger.
		if strings.Contains(arg, mergeResultMetadataKey) ||
			strings.Contains(arg, prURLMetadataKey) ||
			strings.Contains(arg, prNumberMetadataKey) {
			return true
		}
	}
	return false
}
