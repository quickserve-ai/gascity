package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestCheckPublishPointer pins the invariant: a bead entering the published
// state must carry a pointer that can be FETCHED, not merely a pointer that
// exists. The 08:10Z constraint is explicit that a pointer resolving to an
// action record is insufficient — it has to resolve to something re-derivable.
func TestCheckPublishPointer(t *testing.T) {
	const url = "https://github.com/quickserve-ai/q-core/pull/3743"
	for _, tc := range []struct {
		name         string
		mergeResult  string
		prNumber     string
		prURL        string
		wantRefused  bool
		wantContains string
	}{
		{name: "unrelated state is not gated", mergeResult: "blocked"},
		{name: "empty state is not gated"},
		{name: "gate_clear_awaiting_merge is covered by the entry point", mergeResult: "gate_clear_awaiting_merge"},
		{name: "published with a fetchable url", mergeResult: publishedAwaitingGate, prURL: url},
		{name: "published with agreeing number and url", mergeResult: publishedAwaitingGate, prNumber: "3743", prURL: url},
		{
			name: "published with no pointer at all", mergeResult: publishedAwaitingGate,
			wantRefused: true, wantContains: "carries neither",
		},
		{
			// A bare number indexes a repo the bead never names, so nothing can
			// fetch it. This is the qc-tdkydo.58 shape.
			name: "published with only a bare number", mergeResult: publishedAwaitingGate, prNumber: "3743",
			wantRefused: true, wantContains: "names no repository",
		},
		{
			name: "published with a malformed url", mergeResult: publishedAwaitingGate, prURL: "see the refinery log",
			wantRefused: true, wantContains: "not a fetchable GitHub pull-request URL",
		},
		{
			// Two pointers naming different PRs is worse than one pointer: a
			// reader cannot tell which is the work.
			name: "published with disagreeing pointers", mergeResult: publishedAwaitingGate, prNumber: "9999", prURL: url,
			wantRefused: true, wantContains: "disagrees with",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkPublishPointer("qc-test", tc.mergeResult, tc.prNumber, tc.prURL)
			if tc.wantRefused && got == nil {
				t.Fatalf("checkPublishPointer(%q, %q, %q) allowed the write; want refusal",
					tc.mergeResult, tc.prNumber, tc.prURL)
			}
			if !tc.wantRefused && got != nil {
				t.Fatalf("checkPublishPointer(%q, %q, %q) refused with %q; want allowed",
					tc.mergeResult, tc.prNumber, tc.prURL, got.reason)
			}
			if got != nil && tc.wantContains != "" && !strings.Contains(got.reason, tc.wantContains) {
				t.Errorf("refusal reason = %q, want it to contain %q", got.reason, tc.wantContains)
			}
		})
	}
}

// TestProspectivePublishStateAllowsRefineryRelease is a REGRESSION GUARD for a
// live path, and the reason this gate keys on the resulting bead state rather
// than on "the pointers appear in the same update".
//
// formulas/mol-refinery-patrol.toml's find-work release re-sets
// merge_result=pr_published_awaiting_gate on a bead that ALREADY carries pr_url,
// and passes no pointer flags of its own (it guards on pr_url being non-empty
// first). A literal same-update rule would refuse that write and stall the
// refinery's release path on every published bead it touches.
func TestProspectivePublishStateAllowsRefineryRelease(t *testing.T) {
	stored := beads.Bead{
		ID: "qc-tdkydo",
		Metadata: beads.StringMap{
			prURLMetadataKey: "https://github.com/quickserve-ai/q-core/pull/3743",
		},
	}
	args := []string{
		"update", "qc-tdkydo", "--assignee", "", "--status", "open",
		"--set-metadata", mergeResultMetadataKey + "=" + publishedAwaitingGate,
		"--set-metadata", "gc.routed_to=human",
	}
	mergeResult, prNumber, prURL, err := prospectivePublishState(stored, args)
	if err != nil {
		t.Fatalf("prospectivePublishState: %v", err)
	}
	if mergeResult != publishedAwaitingGate {
		t.Fatalf("merge_result = %q, want %q", mergeResult, publishedAwaitingGate)
	}
	if violation := checkPublishPointer(stored.ID, mergeResult, prNumber, prURL); violation != nil {
		t.Fatalf("refinery release refused (%s); the gate must key on the RESULTING bead state, "+
			"not on the pointers appearing in the same update", violation.reason)
	}
}

// TestProspectivePublishStateCatchesTwoStep proves the two-step evasion is still
// closed: setting the state on a bead that has no pointer refuses, whether or
// not a later update would have supplied one.
func TestProspectivePublishStateCatchesTwoStep(t *testing.T) {
	stored := beads.Bead{ID: "qc-hole", Metadata: beads.StringMap{}}
	args := []string{"update", "qc-hole", "--set-metadata", mergeResultMetadataKey + "=" + publishedAwaitingGate}
	mergeResult, prNumber, prURL, err := prospectivePublishState(stored, args)
	if err != nil {
		t.Fatalf("prospectivePublishState: %v", err)
	}
	if violation := checkPublishPointer(stored.ID, mergeResult, prNumber, prURL); violation == nil {
		t.Fatal("setting the published state on a pointerless bead was allowed; that is the qc-tdkydo.58 hole")
	}
}

// TestPublishPointerTargets pins the trigger surface: only `bd update`, only
// when merge_result is mentioned, and never on an ambiguous write set.
func TestPublishPointerTargets(t *testing.T) {
	if _, ok := publishPointerTargets([]string{"close", "qc-1"}); ok {
		t.Error("close must not trigger the publish-pointer gate")
	}
	if _, ok := publishPointerTargets([]string{"update", "qc-1", "--status", "open"}); ok {
		t.Error("an update that does not touch merge_result must not trigger the gate")
	}
	ids, ok := publishPointerTargets([]string{
		"update", "qc-1", "--set-metadata", mergeResultMetadataKey + "=" + publishedAwaitingGate,
	})
	if !ok || len(ids) != 1 || ids[0] != "qc-1" {
		t.Errorf("publishPointerTargets = %v, %v; want [qc-1], true", ids, ok)
	}
}

// TestParsePRURL pins the URL validation against the two failure directions a
// hand-rolled regex had: rejecting fetchable variants, and admitting strings
// that are not PR URLs because query/fragment delimiters slipped into what
// looked like a path.
func TestParsePRURL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		{name: "canonical", raw: "https://github.com/quickserve-ai/q-core/pull/3743", want: "3743", wantOK: true},
		{name: "trailing slash", raw: "https://github.com/o/r/pull/12/", want: "12", wantOK: true},
		{name: "with fragment", raw: "https://github.com/o/r/pull/12#issuecomment-9", want: "12", wantOK: true},
		{name: "with query", raw: "https://github.com/o/r/pull/12?w=1", want: "12", wantOK: true},
		{name: "deeper path (files view)", raw: "https://github.com/o/r/pull/12/files", want: "12", wantOK: true},
		// A fetchable URL must not be refused merely for host casing.
		{name: "uppercase host", raw: "https://GitHub.com/o/r/pull/12", want: "12", wantOK: true},
		// The regex admitted this: "?" opened a query, so "/repo/pull/1" was
		// never really a path, yet the pattern matched the raw string.
		{name: "query masquerading as path", raw: "https://github.com/?/repo/pull/1", wantOK: false},
		{name: "wrong host", raw: "https://gitlab.com/o/r/pull/12", wantOK: false},
		{name: "not https", raw: "http://github.com/o/r/pull/12", wantOK: false},
		{name: "issue not pull", raw: "https://github.com/o/r/issues/12", wantOK: false},
		{name: "non-numeric", raw: "https://github.com/o/r/pull/abc", wantOK: false},
		{name: "prose", raw: "see the refinery log", wantOK: false},
		{name: "empty owner", raw: "https:///r/pull/12", wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parsePRURL(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("parsePRURL(%q) ok = %v, want %v (number %q)", tc.raw, ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Errorf("parsePRURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestPublishPointerTriggersOnPointerRemoval is a regression guard for the
// bypass that a merge_result-only trigger left open: on a bead ALREADY in the
// published state, an update that only touches the pointer would never have been
// inspected, so blanking or unsetting pr_url silently produced exactly the state
// the gate exists to forbid.
func TestPublishPointerTriggersOnPointerRemoval(t *testing.T) {
	for _, args := range [][]string{
		{"update", "qc-1", "--unset-metadata", prURLMetadataKey},
		{"update", "qc-1", "--set-metadata", prURLMetadataKey + "="},
		{"update", "qc-1", "--set-metadata", prNumberMetadataKey + "=7"},
	} {
		if _, ok := publishPointerTargets(args); !ok {
			t.Errorf("publishPointerTargets(%v) did not trigger; removing or blanking the pointer "+
				"breaks the same invariant as never setting it", args)
		}
	}
}

// TestPointerRemovalOnPublishedBeadRefuses proves the widened trigger actually
// refuses, not merely inspects: a bead in the published state whose pr_url is
// unset by this update ends in the forbidden state.
func TestPointerRemovalOnPublishedBeadRefuses(t *testing.T) {
	stored := beads.Bead{
		ID: "qc-1",
		Metadata: beads.StringMap{
			mergeResultMetadataKey: publishedAwaitingGate,
			prURLMetadataKey:       "https://github.com/o/r/pull/12",
		},
	}
	args := []string{"update", "qc-1", "--unset-metadata", prURLMetadataKey}
	mergeResult, prNumber, prURL, err := prospectivePublishState(stored, args)
	if err != nil {
		t.Fatalf("prospectivePublishState: %v", err)
	}
	if violation := checkPublishPointer(stored.ID, mergeResult, prNumber, prURL); violation == nil {
		t.Fatal("unsetting pr_url on a published bead was allowed; that leaves the bead in the exact state the gate forbids")
	}
}
