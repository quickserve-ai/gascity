package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Closed-root guard (qc-z0fmn0n). A molecule is a ROOT bead plus STEP beads
// stamped gc.root_bead_id=<root>. Closing the root — deliberately, through a
// crash, or by a torn mint that never wrote it — used to leave its steps open
// and routed, and gc hook --claim kept serving them: on 2026-09-23 a seat was
// handed a six-bead continuation chain off closed root qc-xqc0b83, and closed
// root qc-4al798r's 27 steps stayed routable for an hour. Each seat that took
// one burned three worktree attempts on a base branch that no longer existed.
//
// The guard sits on the two tiers that MINT a claim (the ready-assignment
// promotion and the fresh routed claim): a candidate whose root reads closed,
// or whose root the store proves absent, is skipped rather than served.
// Adoption of work this seat already has in_progress is deliberately not
// gated — a started step is left to its seat.
//
// Every verdict is written by the lookup that observed it. A read that could
// not complete (a transport error, or beads.ErrVerifyIndeterminate — "absence
// unproven") is NOT a closed root: the step is served exactly as before and the
// failed read is named, the same fail-open trade the drain-pending probe makes.

// closedRootSkipSampleLimit caps the step ids listed on one skip line.
const closedRootSkipSampleLimit = 5

// withClosedRootGuard arms the guard for the production claim path by reading
// roots through the claim's own ReadWorkMeta seam — the one classRoutedHookClaimOps
// wraps — so a root that lives in a relocated class store is resolved where
// the claim itself would resolve it, not reported missing by the wrong ledger.
// An explicitly supplied ReadRoot is kept.
func withClosedRootGuard(ops hookClaimOps) hookClaimOps {
	if ops.ReadRoot != nil {
		return ops
	}
	ops.applyDefaults()
	ops.ReadRoot = ops.ReadWorkMeta
	return ops
}

// hookRootVerdict is what one root lookup observed.
type hookRootVerdict struct {
	closed   bool
	observed string // e.g. "status=closed", "root bead not found"
}

// hookClosedRootGate caches root verdicts for one claim attempt, so a root
// shared by many candidate steps costs one store read, and records the steps it
// skipped so the skip is reported once per root.
type hookClosedRootGate struct {
	read     func(context.Context, string, []string, string, string) (beads.Bead, error)
	dir      string
	env      []string
	assignee string
	stderr   io.Writer

	verdicts map[string]hookRootVerdict
	order    []string
	skipped  map[string][]string
}

func newHookClosedRootGate(ops hookClaimOps, opts hookClaimOptions, dir string, stderr io.Writer) *hookClosedRootGate {
	if ops.ReadRoot == nil {
		return nil
	}
	return &hookClosedRootGate{
		read:     ops.ReadRoot,
		dir:      dir,
		env:      opts.Env,
		assignee: opts.Assignee,
		stderr:   stderr,
		verdicts: map[string]hookRootVerdict{},
		skipped:  map[string][]string{},
	}
}

// skip reports whether candidate must not be served because its molecule root
// is closed or absent. A nil gate, a candidate with no root id, and a
// candidate that IS its own root never skip and never read.
func (g *hookClosedRootGate) skip(candidate beads.Bead) bool {
	if g == nil {
		return false
	}
	rootID := strings.TrimSpace(candidate.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" || rootID == strings.TrimSpace(candidate.ID) {
		return false
	}
	verdict, cached := g.verdicts[rootID]
	if !cached {
		verdict = g.lookup(rootID)
		g.verdicts[rootID] = verdict
	}
	if !verdict.closed {
		return false
	}
	if _, seen := g.skipped[rootID]; !seen {
		g.order = append(g.order, rootID)
	}
	g.skipped[rootID] = append(g.skipped[rootID], candidate.ID)
	return true
}

func (g *hookClosedRootGate) lookup(rootID string) hookRootVerdict {
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	root, err := g.read(ctx, g.dir, g.env, rootID, g.assignee)
	switch {
	case err == nil:
		status := strings.TrimSpace(root.Status)
		if strings.EqualFold(status, "closed") {
			return hookRootVerdict{closed: true, observed: "status=closed"}
		}
		return hookRootVerdict{observed: "status=" + status}
	case errors.Is(err, beads.ErrNotFound) && !errors.Is(err, beads.ErrVerifyIndeterminate):
		return hookRootVerdict{closed: true, observed: "root bead not found, treated as closed"}
	default:
		fmt.Fprintf(g.stderr, "gc hook --claim: root %s lookup failed: %v; serving its steps unguarded\n", rootID, err) //nolint:errcheck
		return hookRootVerdict{observed: "lookup failed"}
	}
}

// report writes one line per closed root naming the skip. Called once, when the
// claim attempt ends, whatever its outcome.
func (g *hookClosedRootGate) report() {
	if g == nil {
		return
	}
	for _, rootID := range g.order {
		steps := g.skipped[rootID]
		sample := steps
		suffix := ""
		if len(sample) > closedRootSkipSampleLimit {
			suffix = fmt.Sprintf(" (+%d more)", len(sample)-closedRootSkipSampleLimit)
			sample = sample[:closedRootSkipSampleLimit]
		}
		fmt.Fprintf(g.stderr, "gc hook --claim: skipped: root %s closed (%s); %d step(s) not served (qc-z0fmn0n): %s%s\n", //nolint:errcheck
			rootID, g.verdicts[rootID].observed, len(steps), strings.Join(sample, ", "), suffix)
	}
}
