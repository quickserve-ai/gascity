package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

// Closed-root guard (qc-z0fmn0n). A molecule is a ROOT bead plus STEP beads
// stamped gc.root_bead_id=<root>. Closing the root — deliberately, through a
// crash, or by a torn mint — used to leave its steps open and routed, and
// gc hook --claim kept serving them: on 2026-09-23 a seat was handed a six-bead
// continuation chain off closed root qc-xqc0b83, and closed root qc-4al798r's 27
// steps stayed routable for an hour. Each seat that took one burned three
// worktree attempts on a base branch that no longer existed.
//
// The guard sits on the two tiers that MINT a claim (the ready-assignment
// promotion and the fresh routed claim). Adoption of work this seat already has
// in_progress is not gated — a started step is left to its seat.
//
// Three rules keep it from skipping work that must run:
//
//   - Only an OBSERVED status=closed skips. A root read that errors — including
//     not-found, which on a federated or split city means "not in the ledger
//     this leg reads", not "does not exist" (anotherWorkLegHolds, cross-store
//     roots via gc.root_store_ref, bd's text-matched not-found) — proves nothing,
//     so the step is served exactly as before and the unreadable root is named.
//   - The teardown tail of a closed root is SERVED. The finalizer and run-cancel
//     close the root and deliberately leave teardown work open, because it runs
//     after the root settles (molecule.TeardownTailExclusion). A candidate that
//     predicate excludes from the terminal sweep is served here too — the same
//     rule dispatch's isTeardownTailControl applies — so worktree cleanup still
//     runs.
//   - Verdicts are resolved BEFORE a tier opens its claim-mutation context, so a
//     slow root read cannot spend the budget the claim CAS needs and turn live
//     work into a false no_work drain.
//
// One gate serves a whole federated invocation: each root is read once, its
// skip is reported once, and a step skipped by two tiers is counted once.

// closedRootSkipSampleLimit caps the step ids listed on one skip line.
const closedRootSkipSampleLimit = 5

// withClosedRootGuard arms the guard for the production claim path by reading
// roots through the claim's own ReadWorkMeta seam — the one
// classRoutedHookClaimOps wraps — so a root that lives in a relocated class
// store is resolved where the claim itself would resolve it. The teardown tail
// is built through the TeardownTail seam, which the class route wraps the same
// way. An explicitly supplied ReadRoot is kept.
func withClosedRootGuard(ops hookClaimOps) hookClaimOps {
	if ops.ReadRoot != nil {
		return ops
	}
	ops.applyDefaults()
	ops.ReadRoot = ops.ReadWorkMeta
	return ops
}

// hookClaimTeardownTailWithBdStore is the production TeardownTail seam: the
// molecule's teardown-tail predicate built over the leg's bd context, with every
// bd child bound to ctx.
func hookClaimTeardownTailWithBdStore(ctx context.Context, dir string, env []string, rootID, assignee string) (func(beads.Bead) bool, error) {
	return molecule.TeardownTailExclusion(hookClaimBdStoreContext(ctx, dir, env, assignee), rootID)
}

// hookRootVerdict is what one root lookup observed.
type hookRootVerdict struct {
	// closed is set only when the read returned the root with status=closed.
	closed bool
	// tail is the closed root's teardown-tail predicate, built on first need.
	// tailBuilt distinguishes "not needed yet" from "built".
	tail      func(beads.Bead) bool
	tailBuilt bool
}

// hookClosedRootGate caches root verdicts for one claim invocation and records
// the steps it skipped so each closed root is reported once.
type hookClosedRootGate struct {
	read     func(context.Context, string, []string, string, string) (beads.Bead, error)
	tailFor  func(context.Context, string, []string, string, string) (func(beads.Bead) bool, error)
	assignee string
	stderr   io.Writer

	verdicts map[string]*hookRootVerdict
	order    []string
	skipped  map[string][]string
	seen     map[string]map[string]struct{}
}

func newHookClosedRootGate(ops hookClaimOps, opts hookClaimOptions, stderr io.Writer) *hookClosedRootGate {
	if ops.ReadRoot == nil {
		return nil
	}
	tailFor := ops.TeardownTail
	if tailFor == nil {
		tailFor = hookClaimTeardownTailWithBdStore
	}
	return &hookClosedRootGate{
		read:     ops.ReadRoot,
		tailFor:  tailFor,
		assignee: opts.Assignee,
		stderr:   stderr,
		verdicts: map[string]*hookRootVerdict{},
		skipped:  map[string][]string{},
		seen:     map[string]map[string]struct{}{},
	}
}

// candidateRoot returns the root a candidate must be checked against, or "" when
// the candidate is exempt without any read: no root id, its own root, or a
// teardown-scoped step (served whatever the root's state).
func candidateRoot(candidate beads.Bead) string {
	rootID := strings.TrimSpace(candidate.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" || rootID == strings.TrimSpace(candidate.ID) {
		return ""
	}
	if candidate.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleTeardown {
		return ""
	}
	return rootID
}

// resolve reads, through the leg (dir, env), the root of every candidate that
// mayServe admits and whose root has no verdict yet, and builds the teardown
// tail of any closed root one of them needs. It runs before either claim tier
// opens its mutation context, so the tiers' skip checks cost no store read.
func (g *hookClosedRootGate) resolve(candidates []beads.Bead, dir string, env []string, mayServe func(beads.Bead) bool) {
	if g == nil {
		return
	}
	for _, candidate := range candidates {
		rootID := candidateRoot(candidate)
		if rootID == "" || !mayServe(candidate) {
			continue
		}
		verdict := g.verdict(rootID, dir, env)
		if verdict.closed && strings.TrimSpace(candidate.Metadata[beadmeta.StepIDMetadataKey]) != "" {
			g.ensureTail(rootID, verdict, dir, env)
		}
	}
}

func (g *hookClosedRootGate) verdict(rootID, dir string, env []string) *hookRootVerdict {
	if v, ok := g.verdicts[rootID]; ok {
		return v
	}
	v := g.lookup(rootID, dir, env)
	g.verdicts[rootID] = v
	return v
}

func (g *hookClosedRootGate) lookup(rootID, dir string, env []string) *hookRootVerdict {
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	root, err := g.read(ctx, dir, env, rootID, g.assignee)
	if err != nil {
		fmt.Fprintf(g.stderr, "gc hook --claim: root %s not readable here (%v); serving its steps unguarded\n", rootID, err) //nolint:errcheck
		return &hookRootVerdict{}
	}
	return &hookRootVerdict{closed: strings.EqualFold(strings.TrimSpace(root.Status), "closed")}
}

// ensureTail builds a closed root's teardown-tail predicate once. When the
// subtree cannot be read, it falls back to the candidate-local half of the rule
// (gc.scope_role=teardown, which candidateRoot already exempts), so only a retry
// attempt linked by gc.step_id alone waits for a later invocation — and says so.
func (g *hookClosedRootGate) ensureTail(rootID string, v *hookRootVerdict, dir string, env []string) {
	if v.tailBuilt {
		return
	}
	v.tailBuilt = true
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	tail, err := g.tailFor(ctx, dir, env, rootID, g.assignee)
	if err != nil {
		fmt.Fprintf(g.stderr, "gc hook --claim: root %s closed but its teardown tail is not readable (%v); serving only gc.scope_role=teardown steps of it\n", rootID, err) //nolint:errcheck
		return
	}
	v.tail = tail
}

// skip reports whether candidate must not be served because its molecule root
// was observed closed and the candidate is not in that root's teardown tail.
// It reads nothing for a root resolve already answered.
func (g *hookClosedRootGate) skip(candidate beads.Bead, dir string, env []string) bool {
	rootID, closed := g.closedRootOf(candidate, dir, env)
	if !closed {
		return false
	}
	seen := g.seen[rootID]
	if seen == nil {
		seen = map[string]struct{}{}
		g.seen[rootID] = seen
		g.order = append(g.order, rootID)
	}
	if _, dup := seen[candidate.ID]; !dup {
		seen[candidate.ID] = struct{}{}
		g.skipped[rootID] = append(g.skipped[rootID], candidate.ID)
	}
	return true
}

// closedRootOf reports the root a candidate is blocked by, if any. A root with
// no verdict yet is read (a fallback — resolve normally answered it already).
func (g *hookClosedRootGate) closedRootOf(candidate beads.Bead, dir string, env []string) (string, bool) {
	if g == nil {
		return "", false
	}
	rootID := candidateRoot(candidate)
	if rootID == "" {
		return "", false
	}
	v := g.verdict(rootID, dir, env)
	if !v.closed {
		return "", false
	}
	if strings.TrimSpace(candidate.Metadata[beadmeta.StepIDMetadataKey]) != "" {
		g.ensureTail(rootID, v, dir, env)
		if v.tail != nil && v.tail(candidate) {
			return "", false
		}
	}
	return rootID, true
}

// observedClosedRootOf answers from cached verdicts only, never reading: the
// root this invocation observed closed for bead, if bead is not in its teardown
// tail. Used by the drain's divergence classifier.
func (g *hookClosedRootGate) observedClosedRootOf(bead beads.Bead) (string, bool) {
	if g == nil {
		return "", false
	}
	rootID := candidateRoot(bead)
	v, ok := g.verdicts[rootID]
	if rootID == "" || !ok || !v.closed {
		return "", false
	}
	if v.tail != nil && v.tail(bead) {
		return "", false
	}
	return rootID, true
}

// report writes one line per closed root that had steps skipped.
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
		fmt.Fprintf(g.stderr, "gc hook --claim: skipped: root %s closed (status=closed); %d step(s) not served (qc-z0fmn0n): %s%s\n", //nolint:errcheck
			rootID, len(steps), strings.Join(sample, ", "), suffix)
	}
}

// hookCandidateMayBeServed is the union of the two claim tiers' cheap admission
// checks: a candidate either tier could go on to claim. resolve reads roots only
// for these, so rows this seat could never take cost no store read.
func hookCandidateMayBeServed(candidate beads.Bead, opts hookClaimOptions, now time.Time) bool {
	if strings.TrimSpace(candidate.ID) == "" || hookCandidateBudgetDeferred(candidate, now) {
		return false
	}
	ready := !hookClaimCandidateIsMessage(candidate) &&
		strings.EqualFold(strings.TrimSpace(candidate.Status), "open") &&
		hookClaimHasIdentity(candidate.Assignee, opts.IdentityCandidates)
	if ready || hookCandidateClaimable(candidate, opts.RouteTargets, now) {
		return true
	}
	return opts.AutoReclaimStaleClaims && hookCandidateReclaimEligible(candidate, opts.RouteTargets, now)
}
