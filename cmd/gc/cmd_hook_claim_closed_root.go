package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
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
// The rules that keep it from skipping work that must run, or starving it:
//
//   - Only an OBSERVED status=closed skips. A root read that errors — including
//     not-found, which on a federated or split city means "not in the ledger
//     this leg reads" — proves nothing, so the step is served exactly as before
//     and the unreadable root is named.
//   - Every verdict belongs to the STORE that produced it. Legs are different
//     ledgers: one leg's not-found must not unguard the leg that holds the
//     closed root, and one leg's closed root must not suppress a same-named
//     root another leg holds open. Only the report is invocation-wide.
//   - The teardown tail of a closed root is SERVED: the finalizer and run-cancel
//     close the root and deliberately leave teardown work open. The tail is the
//     teardown-scoped steps plus every attempt sharing their gc.step_id (retry
//     expansion strips gc.scope_role from the first attempt) — the rule of
//     molecule.TeardownTailExclusion — built from ONE narrow query per root.
//   - Roots are resolved lazily in tier order, before either tier opens its
//     claim-mutation context, and resolution stops at the first candidate that
//     can be served. Dead molecules behind a live row cost nothing; dead
//     molecules ahead of it cost O(1) store reads each.

// closedRootSkipSampleLimit caps the step ids listed on one skip line.
const closedRootSkipSampleLimit = 5

// withClosedRootGuard arms the guard for the production claim path by reading
// roots through the claim's own ReadWorkMeta seam — the one
// classRoutedHookClaimOps wraps — so a root that lives in a relocated class
// store is resolved where the claim itself would resolve it. An explicitly
// supplied ReadRoot is kept.
func withClosedRootGuard(ops hookClaimOps) hookClaimOps {
	if ops.ReadRoot != nil {
		return ops
	}
	ops.applyDefaults()
	ops.ReadRoot = ops.ReadWorkMeta
	return ops
}

// hookClaimTeardownTail builds a closed root's teardown-tail predicate from ONE
// narrow query: the members of rootID tagged gc.scope_role=teardown (closed ones
// included — a settled logical step still names its live retry attempts). A
// candidate is in the tail when it carries the tag or shares one of their
// gc.step_id values.
func hookClaimTeardownTail(store beads.Store, rootID string) (func(beads.Bead) bool, error) {
	members, err := store.ListByMetadata(map[string]string{
		beadmeta.RootBeadIDMetadataKey: rootID,
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleTeardown,
	}, 0, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		return nil, err
	}
	stepIDs := make(map[string]struct{}, len(members))
	for _, member := range members {
		if stepID := strings.TrimSpace(member.Metadata[beadmeta.StepIDMetadataKey]); stepID != "" {
			stepIDs[stepID] = struct{}{}
		}
	}
	return func(b beads.Bead) bool {
		if b.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleTeardown {
			return true
		}
		stepID := strings.TrimSpace(b.Metadata[beadmeta.StepIDMetadataKey])
		if stepID == "" {
			return false
		}
		_, ok := stepIDs[stepID]
		return ok
	}, nil
}

// hookClaimTeardownTailWithBdStore is the production TeardownTail seam: the
// narrow teardown query over the leg's bd context, bound to ctx.
func hookClaimTeardownTailWithBdStore(ctx context.Context, dir string, env []string, rootID, assignee string) (func(beads.Bead) bool, error) {
	return hookClaimTeardownTail(hookClaimBdStoreContext(ctx, dir, env, assignee), rootID)
}

// runWithDeadline runs fn and returns its result, or ctx's error once ctx is
// done — for store calls that take no context of their own (the relocated class
// binding). fn keeps running to completion in the background; its late result
// is discarded.
func runWithDeadline[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// hookRootVerdict is what one store's root lookup observed.
type hookRootVerdict struct {
	// closed is set only when the read returned the root with status=closed.
	closed bool
	// tailTried/tail: the closed root's teardown-tail predicate, built on first
	// need; tail stays nil when the tail query failed.
	tailTried bool
	tail      func(beads.Bead) bool
}

// hookClosedRootGate caches root verdicts per (store, root) for one claim
// invocation and records, per root, the steps it skipped so each closed root is
// reported once.
type hookClosedRootGate struct {
	read     func(context.Context, string, []string, string, string) (beads.Bead, error)
	tailFor  func(context.Context, string, []string, string, string) (func(beads.Bead) bool, error)
	assignee string
	stderr   io.Writer

	verdicts map[string]*hookRootVerdict // keyed by hookRootStoreKey
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

// hookRootStoreKey identifies a root within the store a leg reads: the leg's
// directory and its BEADS_DIR selector, which together are what hookClaimEnvMap
// hands the bd child.
func hookRootStoreKey(dir string, env []string, rootID string) string {
	return strings.TrimSpace(dir) + "\x00" + hookClaimEnvValue(env, "BEADS_DIR") + "\x00" + rootID
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

// resolveUntilServable walks ordered (the candidates in the order the claim
// tiers will try them) and resolves each one's root through the leg until it
// reaches a candidate the guard would serve. It runs before either tier opens
// its claim-mutation context; it reads nothing past the first servable row.
func (g *hookClosedRootGate) resolveUntilServable(ordered []beads.Bead, dir string, env []string) {
	if g == nil {
		return
	}
	for _, candidate := range ordered {
		if _, blocked := g.closedRootOf(candidate, dir, env); !blocked {
			return
		}
	}
}

func (g *hookClosedRootGate) verdict(rootID, dir string, env []string) *hookRootVerdict {
	key := hookRootStoreKey(dir, env, rootID)
	if v, ok := g.verdicts[key]; ok {
		return v
	}
	v := g.lookup(rootID, dir, env)
	g.verdicts[key] = v
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

// ensureTail builds a closed root's teardown-tail predicate once per store. When
// the query fails, only the candidate-local half of the rule
// (gc.scope_role=teardown, which candidateRoot already exempts) applies, so a
// retry attempt linked by gc.step_id alone waits for a later invocation — and
// the line says so.
func (g *hookClosedRootGate) ensureTail(rootID string, v *hookRootVerdict, dir string, env []string) {
	if v.tailTried {
		return
	}
	v.tailTried = true
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
// was observed closed in this leg's store and the candidate is not in that
// root's teardown tail, and records the skip.
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

// closedRootOf reports the root a candidate is blocked by in the leg's store,
// if any. A root with no verdict for this store yet is read (resolve normally
// answered it already, before the tier's claim budget opened).
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

// observedClosedRootOf answers from this store's cached verdicts only, never
// reading: the root this invocation observed closed for bead, when an
// ESTABLISHED teardown tail says bead is outside it. With no tail built for that
// root, the answer is "no" — a retry attempt the query never returned could be
// in the tail. Used by the drain's divergence classifier.
func (g *hookClosedRootGate) observedClosedRootOf(bead beads.Bead, dir string, env []string) (string, bool) {
	if g == nil {
		return "", false
	}
	rootID := candidateRoot(bead)
	if rootID == "" {
		return "", false
	}
	v, ok := g.verdicts[hookRootStoreKey(dir, env, rootID)]
	if !ok || !v.closed || v.tail == nil || v.tail(bead) {
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

// hookCandidateReadyForPromotion mirrors the ready-assignment tier's cheap
// admission checks.
func hookCandidateReadyForPromotion(candidate beads.Bead, opts hookClaimOptions, now time.Time) bool {
	return strings.TrimSpace(candidate.ID) != "" &&
		!hookClaimCandidateIsMessage(candidate) &&
		strings.EqualFold(strings.TrimSpace(candidate.Status), "open") &&
		hookClaimHasIdentity(candidate.Assignee, opts.IdentityCandidates) &&
		!hookCandidateBudgetDeferred(candidate, now)
}

// hookCandidateEligibleForClaim mirrors the routed tier's cheap admission
// checks (a fresh claim, or an opted-in stale-lease reclaim).
func hookCandidateEligibleForClaim(candidate beads.Bead, opts hookClaimOptions, now time.Time) bool {
	if hookCandidateClaimable(candidate, opts.RouteTargets, now) {
		return true
	}
	return opts.AutoReclaimStaleClaims && hookCandidateReclaimEligible(candidate, opts.RouteTargets, now)
}

// hookClaimTierOrder lists candidates in the order the claim tiers try them:
// every ready assignment first, then every routed-tier candidate.
func hookClaimTierOrder(candidates []beads.Bead, opts hookClaimOptions, now time.Time) []beads.Bead {
	ordered := make([]beads.Bead, 0, len(candidates))
	for _, candidate := range candidates {
		if hookCandidateReadyForPromotion(candidate, opts, now) {
			ordered = append(ordered, candidate)
		}
	}
	for _, candidate := range candidates {
		if hookCandidateEligibleForClaim(candidate, opts, now) {
			ordered = append(ordered, candidate)
		}
	}
	return ordered
}
