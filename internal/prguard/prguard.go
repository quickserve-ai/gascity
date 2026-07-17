// Package prguard enforces the close-time PR invariant from qc-p45lz:
// a bead whose metadata records a pull-request URL must not be closed
// while that PR is confirmed unmerged. Closing such a bead orphans the
// PR — no open bead drives the merge gate, so green PRs rot silently.
//
// The guard is deliberately fail-open: it blocks only on a CONFIRMED
// non-merged PR state. Missing metadata, an unparseable URL, a missing
// gh binary, network errors, and timeouts all Allow (with a Note for
// the caller to surface), so bead closes never hinge on GitHub being
// reachable.
package prguard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Verdict is the outcome of a pre-close PR check.
type Verdict int

const (
	// Allow means the close may proceed: no PR metadata, the PR is
	// merged, or the state could not be confirmed (fail-open).
	Allow Verdict = iota
	// Block means the PR is confirmed to exist and to be unmerged.
	Block
)

// Result carries the verdict plus context for user-facing messages.
type Result struct {
	Verdict Verdict
	PRURL   string
	// State is the confirmed PR state (MERGED, OPEN, CLOSED) when the
	// lookup succeeded, empty otherwise.
	State string
	// Note explains a fail-open Allow (e.g. network error) so callers
	// can warn without blocking. Empty on clean Allows and on Block.
	Note string
}

// Runner executes an external command and returns its stdout. It exists
// so tests can substitute the gh invocation.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// prURLPattern matches GitHub pull-request URLs. Dispatch and agents
// record these under the bead metadata keys checked in MetadataPRURL.
var prURLPattern = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/pull/(\d+)$`)

// metadataPRURLKeys are the bead metadata keys, in precedence order,
// that may carry the PR URL. "pr_url" is what the qcore lanes write
// today; "gc.pr_url" is the reserved beadmeta key.
var metadataPRURLKeys = []string{"pr_url", "gc.pr_url"}

// disableEnv turns the guard off entirely (emergency kill switch).
const disableEnv = "GC_PR_CLOSE_GUARD"

// MetadataPRURL returns the PR URL recorded on the bead, if any.
func MetadataPRURL(metadata map[string]string) string {
	for _, key := range metadataPRURLKeys {
		if url := strings.TrimSpace(metadata[key]); url != "" {
			return url
		}
	}
	return ""
}

// Enabled reports whether the guard is active. GC_PR_CLOSE_GUARD=off
// disables it globally.
func Enabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv(disableEnv)), "off")
}

// CheckBeadClosable applies the invariant to one bead's metadata using
// the given runner (nil for the real gh binary) and per-lookup timeout.
func CheckBeadClosable(metadata map[string]string, runner Runner, timeout time.Duration) Result {
	url := MetadataPRURL(metadata)
	if url == "" {
		return Result{Verdict: Allow}
	}
	return CheckPRURL(url, runner, timeout)
}

// CheckPRURL resolves the PR's merge state via gh. Only a confirmed
// non-merged state Blocks; every failure mode Allows with a Note.
func CheckPRURL(url string, runner Runner, timeout time.Duration) Result {
	m := prURLPattern.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return Result{Verdict: Allow, PRURL: url, Note: "unrecognized PR URL shape; skipping merge check"}
	}
	if runner == nil {
		if _, err := exec.LookPath("gh"); err != nil {
			return Result{Verdict: Allow, PRURL: url, Note: "gh not found in PATH; cannot verify PR merge state"}
		}
		runner = execRunner
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// gh api is used instead of gh pr view so the answer comes from the
	// canonical PR resource regardless of local checkout state.
	path := fmt.Sprintf("repos/%s/%s/pulls/%s", m[1], m[2], m[3])
	out, err := runner(ctx, "gh", "api", path, "--jq", "{state: .state, merged: .merged}")
	if err != nil {
		return Result{Verdict: Allow, PRURL: url, Note: fmt.Sprintf("PR merge check failed (%v); allowing close", err)}
	}
	var pr struct {
		State  string `json:"state"`
		Merged bool   `json:"merged"`
	}
	if err := json.Unmarshal(out, &pr); err != nil {
		return Result{Verdict: Allow, PRURL: url, Note: "PR merge check returned unparseable output; allowing close"}
	}
	if pr.Merged {
		return Result{Verdict: Allow, PRURL: url, State: "MERGED"}
	}
	state := strings.ToUpper(strings.TrimSpace(pr.State))
	if state == "" {
		return Result{Verdict: Allow, PRURL: url, Note: "PR merge check returned no state; allowing close"}
	}
	// Confirmed unmerged (OPEN, or CLOSED without merge).
	return Result{Verdict: Block, PRURL: url, State: state}
}
