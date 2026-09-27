package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// Pins the closed-root router skip (qc-z0fmn0n). On 2026-09-23 two molecule
// roots were closed while their step beads stayed open and routed: qc-xqc0b83,
// whose seat was then handed a six-bead continuation chain off it, and
// qc-4al798r, whose 27 steps stayed routable for an hour. Every seat that
// claimed one burned three worktree attempts on a deleted base branch.
// gc hook --claim must not serve a step whose gc.root_bead_id names a root it
// OBSERVED closed — except the root's teardown tail, which runs after the root
// closes by design — and must say so on its diagnostic stream.
//
// Roots here live in a real beads.MemStore (or the real SQLite class store, or
// a real BdStore over a scripted runner), never a stub that answers "open" or
// "closed" by fiat: each verdict is one a store actually produced.

type closedRootClaimSpy struct{ ids []string }

func (s *closedRootClaimSpy) fn(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
	s.ids = append(s.ids, id)
	return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{}}, true, nil
}

// closedRootStoreReader counts root reads so the cache is observable, and
// reads through a real MemStore. It honors the leg: byDir maps a leg's dir to
// the store that leg reads, and any other dir reads store.
type closedRootStoreReader struct {
	mu      sync.Mutex
	store   *beads.MemStore
	byDir   map[string]*beads.MemStore
	reads   []string
	readsAt []string
	tails   int
	// lastRead is the wall time of the latest root read. It is only read,
	// never waited on: tests compare it with when a claim context opened.
	lastRead time.Time
}

func (r *closedRootStoreReader) storeFor(dir string) *beads.MemStore {
	if s, ok := r.byDir[dir]; ok {
		return s
	}
	return r.store
}

func (r *closedRootStoreReader) fn(_ context.Context, dir string, _ []string, id, _ string) (beads.Bead, error) {
	r.mu.Lock()
	r.reads = append(r.reads, id)
	r.readsAt = append(r.readsAt, dir+"|"+id)
	r.lastRead = time.Now()
	r.mu.Unlock()
	return r.storeFor(dir).Get(id)
}

// tail builds the teardown tail with the PRODUCTION function over the leg's
// store, and counts the builds.
func (r *closedRootStoreReader) tail(_ context.Context, dir string, _ []string, rootID, _ string) (func(beads.Bead) bool, error) {
	r.mu.Lock()
	r.tails++
	r.mu.Unlock()
	return hookClaimTeardownTail(r.storeFor(dir), rootID)
}

// claimContextOpenedAt is when the claim-mutation context a Claim call runs
// under was opened: its deadline less the flat mutation budget. It is exact
// when the claim window is far longer than hookClaimMutationTimeout (callers
// set one), because claimMutationContext then grants exactly that budget.
func claimContextOpenedAt(ctx context.Context) (time.Time, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return time.Time{}, false
	}
	return deadline.Add(-hookClaimMutationTimeout), true
}

// assertRootReadsPrecedeClaimContext proves, without spending wall time, that
// every root read ran before the claim context the served row was claimed under
// was opened — so no read could have spent that context's budget. A read
// resolved inside the tier would land after the context opened.
func assertRootReadsPrecedeClaimContext(t *testing.T, reader *closedRootStoreReader, opened time.Time) {
	t.Helper()
	if opened.IsZero() {
		t.Fatal("the served row was never claimed under a claim context with a deadline")
	}
	reader.mu.Lock()
	lastRead := reader.lastRead
	reader.mu.Unlock()
	if lastRead.IsZero() {
		t.Fatal("no root reads observed; the test would prove nothing")
	}
	if lastRead.After(opened) {
		t.Fatalf("a root read ran %s after the served row's claim context opened; reads must resolve outside the claim-mutation context", lastRead.Sub(opened))
	}
}

func newClosedRootStoreReader(existing ...beads.Bead) *closedRootStoreReader {
	return &closedRootStoreReader{store: beads.NewMemStoreFrom(0, existing, nil)}
}

func closedRootWorkflowRoot(id, status string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Title:    "mol-scoped-work",
		Status:   status,
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_name": "mol-scoped-work"},
	}
}

func closedRootOpsOpts(workQueryJSON string, spy *closedRootClaimSpy, reader *closedRootStoreReader) (hookClaimOps, hookClaimOptions) {
	ops := hookClaimOps{
		Runner:            func(string, string) (string, error) { return workQueryJSON, nil },
		Claim:             spy.fn,
		ListContinuation:  func(context.Context, string, []string, string, string) ([]beads.Bead, error) { return nil, nil },
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
		StampWorkMeta:     noopStampWorkMeta,
		PublishRunMap:     func(string, string, ...string) error { return nil },
	}
	if reader != nil {
		ops.ReadRoot = reader.fn
		ops.TeardownTail = reader.tail
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}
	return ops, opts
}

func routedStepJSON(id, rootID string) string {
	meta := `"gc.routed_to":"worker"`
	if rootID != "" {
		meta += fmt.Sprintf(`,"gc.root_bead_id":%q`, rootID)
	}
	return fmt.Sprintf(`{"id":%q,"status":"open","metadata":{%s}}`, id, meta)
}

// routedStepWithIDJSON is a routed step carrying a real gc.step_id, so the
// closed-root teardown path is exercised.
func routedStepWithIDJSON(id, rootID, stepID string) string {
	return fmt.Sprintf(`{"id":%q,"status":"open","metadata":{"gc.routed_to":"worker","gc.root_bead_id":%q,"gc.step_id":%q}}`, id, rootID, stepID)
}

func stepJSON(b beads.Bead) string {
	raw, err := json.Marshal(b)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func decodeClosedRootResult(t *testing.T, stdout string) hookClaimJSONResult {
	t.Helper()
	var result hookClaimJSONResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &result); err != nil {
		t.Fatalf("decoding hook result %q: %v", stdout, err)
	}
	return result
}

// Brief (i): a closed root with routed open steps — none is served, and the
// diagnostic names the skip.
func TestHookClaimSkipsRoutedStepsWhoseRootIsClosed(t *testing.T) {
	spy := &closedRootClaimSpy{}
	reader := newClosedRootStoreReader(closedRootWorkflowRoot("qc-xqc0b83", "closed"))
	ops, opts := closedRootOpsOpts(
		`[`+routedStepJSON("qc-step-1", "qc-xqc0b83")+`,`+routedStepJSON("qc-step-2", "qc-xqc0b83")+`]`,
		spy, reader,
	)

	var stdout, stderr bytes.Buffer
	// A drain without --drain-ack exits 1 (writeHookClaimDrain); the JSON
	// record below is what says it was a drain rather than an error.
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 1 {
		t.Fatalf("doHookClaim = %d, want 1 (drain, no ack); stderr=%s", code, stderr.String())
	}
	if len(spy.ids) != 0 {
		t.Fatalf("claim mutations = %v, want none — a step off a closed root must never be served", spy.ids)
	}
	if result := decodeClosedRootResult(t, stdout.String()); result.Action != "drain" || result.BeadID != "" {
		t.Fatalf("result = %+v, want a drain serving no bead", result)
	}
	if !strings.Contains(stderr.String(), "skipped: root qc-xqc0b83 closed (status=closed); 2 step(s)") {
		t.Fatalf("stderr = %q, want the skip named as 'skipped: root qc-xqc0b83 closed' with the observed status", stderr.String())
	}
}

// A continuation sibling PREASSIGNED to this seat (the qc-xqc0b83 chain shape)
// arrives through the ready-assignment tier; it must be skipped there too.
func TestHookClaimSkipsReadyAssignmentWhoseRootIsClosed(t *testing.T) {
	spy := &closedRootClaimSpy{}
	reader := newClosedRootStoreReader(closedRootWorkflowRoot("qc-xqc0b83", "closed"))
	ops, opts := closedRootOpsOpts(
		`[{"id":"qc-chain-2","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"qc-xqc0b83","gc.continuation_group":"body"}}]`,
		spy, reader,
	)

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 0 {
		t.Fatalf("claim mutations = %v, want none — a preassigned sibling off a closed root must not be promoted", spy.ids)
	}
	if !strings.Contains(stderr.String(), "skipped: root qc-xqc0b83 closed") {
		t.Fatalf("stderr = %q, want the skip named", stderr.String())
	}
}

// Review item 4: a step skipped by the ready tier and again by the reclaim path
// of the routed tier is ONE step not served, not two.
func TestHookClaimClosedRootSkipIsCountedOncePerStep(t *testing.T) {
	spy := &closedRootClaimSpy{}
	reader := newClosedRootStoreReader(closedRootWorkflowRoot("qc-dup", "closed"))
	ops, opts := closedRootOpsOpts(
		`[{"id":"qc-dup.1","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"qc-dup"}}]`,
		spy, reader,
	)
	opts.AutoReclaimStaleClaims = true
	reclaims := 0
	ops.ReclaimStale = func(context.Context, string, []string, string) (bool, string, error) {
		reclaims++
		return false, "", nil
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 0 || reclaims != 0 {
		t.Fatalf("claims = %v, reclaims = %d, want neither for a closed-root step", spy.ids, reclaims)
	}
	if !strings.Contains(stderr.String(), "skipped: root qc-dup closed (status=closed); 1 step(s) not served") {
		t.Fatalf("stderr = %q, want the step counted once across both tiers", stderr.String())
	}
}

// Review item 2: a root the store reports ABSENT proves nothing — on a
// federated or split city it means "not in the ledger this leg reads". The
// step is served as before, and the line names the unreadable root.
func TestHookClaimServesStepWhoseRootIsNotReadable(t *testing.T) {
	spy := &closedRootClaimSpy{}
	reader := newClosedRootStoreReader() // empty store: the root is not here
	ops, opts := closedRootOpsOpts(`[`+routedStepJSON("qc-orphan", "qc-elsewhere")+`]`, spy, reader)

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 1 || spy.ids[0] != "qc-orphan" {
		t.Fatalf("claim mutations = %v, want qc-orphan served — not-found is not a closed root", spy.ids)
	}
	if !strings.Contains(stderr.String(), "root qc-elsewhere not readable here") ||
		!strings.Contains(stderr.String(), "serving its steps unguarded") {
		t.Fatalf("stderr = %q, want the unreadable root named", stderr.String())
	}
	if strings.Contains(stderr.String(), "skipped: root") {
		t.Errorf("stderr = %q, want no skip for an unreadable root", stderr.String())
	}
}

// Review items 2 and 6: the production read path — a real BdStore — where bd
// show says not found and the wisp fallback query fails. BdStore reports that
// as ErrVerifyIndeterminate (which also satisfies errors.Is ErrNotFound); the
// step is served and the failed read named.
func TestHookClaimServesStepWhenBdStoreCannotProveTheRoot(t *testing.T) {
	var calls []string
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if len(args) > 0 && args[0] == "show" {
			return nil, errors.New(`bd show: no issue found matching "qc-bdroot"`)
		}
		return nil, errors.New("query: dial tcp 127.0.0.1:3307: connect: connection refused")
	}
	store := beads.NewBdStore("/rig", runner)
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+routedStepJSON("qc-step-bd", "qc-bdroot")+`]`, spy, nil)
	ops.ReadRoot = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
		return store.Get(id)
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 1 || spy.ids[0] != "qc-step-bd" {
		t.Fatalf("claim mutations = %v, want qc-step-bd served; bd calls = %v", spy.ids, calls)
	}
	if !strings.Contains(stderr.String(), "root qc-bdroot not readable here") {
		t.Fatalf("stderr = %q, want the unproven root named", stderr.String())
	}
	sawShow, sawQuery := false, false
	for _, c := range calls {
		sawShow = sawShow || strings.HasPrefix(c, "bd show")
		sawQuery = sawQuery || strings.HasPrefix(c, "bd query")
	}
	if !sawShow || !sawQuery {
		t.Errorf("bd calls = %v, want show then the wisp query — the real not-readable path", calls)
	}
}

// A root whose read errors outright is served too (fail open).
func TestHookClaimServesStepWhenRootLookupIsIndeterminate(t *testing.T) {
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+routedStepJSON("qc-step-x", "qc-murky")+`]`, spy, nil)
	ops.ReadRoot = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
		return beads.Bead{}, fmt.Errorf("getting bead %q: %w: dolt i/o timeout", id, beads.ErrVerifyIndeterminate)
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 1 || spy.ids[0] != "qc-step-x" {
		t.Fatalf("claim mutations = %v, want qc-step-x served", spy.ids)
	}
	if !strings.Contains(stderr.String(), "root qc-murky not readable here") {
		t.Errorf("stderr = %q, want the failed root read named", stderr.String())
	}
}

// Brief (ii): an open root is served exactly as today.
func TestHookClaimServesStepWhoseRootIsOpen(t *testing.T) {
	spy := &closedRootClaimSpy{}
	reader := newClosedRootStoreReader(closedRootWorkflowRoot("qc-live", "open"))
	ops, opts := closedRootOpsOpts(`[`+routedStepJSON("qc-step-live", "qc-live")+`]`, spy, reader)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(spy.ids) != 1 || spy.ids[0] != "qc-step-live" {
		t.Fatalf("claim mutations = %v, want qc-step-live claimed", spy.ids)
	}
	if result := decodeClosedRootResult(t, stdout.String()); result.Action != "work" || result.BeadID != "qc-step-live" {
		t.Fatalf("result = %+v, want work on qc-step-live", result)
	}
	if len(reader.reads) != 1 || reader.reads[0] != "qc-live" {
		t.Errorf("root reads = %v, want exactly one read of qc-live — the guard must have looked", reader.reads)
	}
	if strings.Contains(stderr.String(), "skipped: root") {
		t.Errorf("stderr = %q, want no skip for an open root", stderr.String())
	}
}

// Brief (iii): a step with no gc.root_bead_id is served exactly as today,
// without a root read.
func TestHookClaimServesStepWithNoRootID(t *testing.T) {
	spy := &closedRootClaimSpy{}
	reader := newClosedRootStoreReader(closedRootWorkflowRoot("qc-xqc0b83", "closed"))
	ops, opts := closedRootOpsOpts(`[`+routedStepJSON("qc-plain", "")+`]`, spy, reader)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(spy.ids) != 1 || spy.ids[0] != "qc-plain" {
		t.Fatalf("claim mutations = %v, want qc-plain claimed", spy.ids)
	}
	if len(reader.reads) != 0 {
		t.Errorf("root reads = %v, want none for a step with no root id", reader.reads)
	}
}

// teardownMolecule is a closed mol-scoped-work root after its finalizer ran:
// the root is closed, the ordinary step is still open, and the teardown tail
// (the cleanup-worktree step and its first retry attempt, which retry
// expansion strips of gc.scope_role) was deliberately left open.
func teardownMolecule() (root, implement, cleanup, attempt beads.Bead) {
	const rootID = "qc-td"
	step := func(id string, meta map[string]string) beads.Bead {
		m := map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": rootID}
		for k, v := range meta {
			m[k] = v
		}
		return beads.Bead{ID: id, Title: id, Status: "open", Metadata: m}
	}
	root = closedRootWorkflowRoot(rootID, "closed")
	implement = step("qc-td.implement", map[string]string{"gc.step_id": "implement"})
	cleanup = step("qc-td.cleanup", map[string]string{"gc.step_id": "cleanup-worktree", "gc.kind": "cleanup", "gc.scope_role": "teardown"})
	attempt = step("qc-td.cleanup.1", map[string]string{"gc.step_id": "cleanup-worktree", "gc.kind": "cleanup"})
	return root, implement, cleanup, attempt
}

// Review item 1: the finalizer closes the root and leaves teardown open BY
// DESIGN. A closed root's teardown step is served; its ordinary step is not.
func TestHookClaimServesTeardownStepOfClosedRoot(t *testing.T) {
	root, implement, cleanup, attempt := teardownMolecule()
	reader := newClosedRootStoreReader(root, implement, cleanup, attempt)
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+stepJSON(implement)+`,`+stepJSON(cleanup)+`]`, spy, reader)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(spy.ids) != 1 || spy.ids[0] != cleanup.ID {
		t.Fatalf("claim mutations = %v, want only the teardown step %s — worktrees leak if it is skipped", spy.ids, cleanup.ID)
	}
	if !strings.Contains(stderr.String(), "skipped: root qc-td closed (status=closed); 1 step(s) not served (qc-z0fmn0n): qc-td.implement") {
		t.Fatalf("stderr = %q, want the ordinary step (only) named as skipped", stderr.String())
	}
}

// Review item 1: the first retry attempt of the teardown step carries no
// gc.scope_role — only gc.step_id links it back — and is served all the same.
func TestHookClaimServesTeardownRetryAttemptWithoutScopeRole(t *testing.T) {
	root, implement, cleanup, attempt := teardownMolecule()
	cleanup.Status = "closed" // the logical step settled; its attempt is the live work
	reader := newClosedRootStoreReader(root, implement, cleanup, attempt)
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+stepJSON(implement)+`,`+stepJSON(attempt)+`]`, spy, reader)

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 1 || spy.ids[0] != attempt.ID {
		t.Fatalf("claim mutations = %v, want the scope_role-less retry attempt %s served", spy.ids, attempt.ID)
	}
	if !strings.Contains(stderr.String(), "skipped: root qc-td closed") {
		t.Errorf("stderr = %q, want the ordinary step still skipped", stderr.String())
	}
}

// Review item 3: root reads must not spend the claim tier's mutation budget.
// Fifteen steps on distinct closed roots ahead of one open-root step: the
// open-root step is still claimed, the seat does not report a false no_work,
// and every root read ran before the claim context the step was claimed under
// opened — so however slow the reads, they cannot spend its budget.
func TestHookClaimRootReadsDoNotSpendTheClaimBudget(t *testing.T) {
	existing := []beads.Bead{closedRootWorkflowRoot("qc-open-root", "open")}
	rows := make([]string, 0, 16)
	for i := 1; i <= 15; i++ {
		rootID := fmt.Sprintf("qc-dead-%02d", i)
		existing = append(existing, closedRootWorkflowRoot(rootID, "closed"))
		rows = append(rows, routedStepWithIDJSON(fmt.Sprintf("qc-dead-%02d.step", i), rootID, "implement"))
	}
	rows = append(rows, routedStepWithIDJSON("qc-open-root.step", "qc-open-root", "implement"))
	reader := newClosedRootStoreReader(existing...)
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+strings.Join(rows, ",")+`]`, spy, reader)
	ops.ClaimWindow = time.Hour // far past the mutation budget: claimContextOpenedAt is exact
	var servedOpened time.Time
	ops.Claim = func(ctx context.Context, dir string, env []string, id, assignee string) (beads.Bead, bool, error) {
		if id == "qc-open-root.step" {
			servedOpened, _ = claimContextOpenedAt(ctx)
		}
		return spy.fn(ctx, dir, env, id, assignee)
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(spy.ids) != 1 || spy.ids[0] != "qc-open-root.step" {
		t.Fatalf("claim mutations = %v, want the open-root step claimed", spy.ids)
	}
	if result := decodeClosedRootResult(t, stdout.String()); result.Action != "work" {
		t.Fatalf("result = %+v, want work, not a no_work drain", result)
	}
	assertRootReadsPrecedeClaimContext(t, reader, servedOpened)
}

// Brief (iv), review item 4: replay of the qc-4al798r shape through the
// federated claim loop — one closed root with 27 routed open steps. None is
// served, the root is read once for the whole invocation, and the skip line
// carries the count.
func TestHookClaimReplayQc4al798rClosedRootWith27RoutedSteps(t *testing.T) {
	const rootID = "qc-4al798r"
	rows := make([]string, 0, 27)
	for i := 1; i <= 27; i++ {
		rows = append(rows, routedStepWithIDJSON(fmt.Sprintf("qc-4al798r.%d", i), rootID, fmt.Sprintf("step-%d", i)))
	}
	workQuery := `[` + strings.Join(rows, ",") + `]`

	spy := &closedRootClaimSpy{}
	reader := newClosedRootStoreReader(closedRootWorkflowRoot(rootID, "closed"))
	ops, opts := closedRootOpsOpts(workQuery, spy, reader)
	ops.Runner = nil
	legs := []hookStore{{dir: "/rig-q", env: []string{"BEADS_DIR=/rig-q"}}}
	run := func(string, string, []string) (string, error) { return workQuery, nil }

	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("query", "/rig-q", nil, legs, opts, ops, run,
		func(string, error) {}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("claimHookWorkWithRunner = %d, want 1 (drain, no ack); stderr=%s", code, stderr.String())
	}
	if len(spy.ids) != 0 {
		t.Fatalf("claim mutations = %v, want none of the 27 steps served", spy.ids)
	}
	if result := decodeClosedRootResult(t, stdout.String()); result.Action != "drain" {
		t.Fatalf("result = %+v, want a drain", result)
	}
	if len(reader.reads) != 1 || reader.tails != 1 {
		t.Errorf("root reads = %d (%v), teardown-tail builds = %d; want 1 and 1 — once per root per store, not per step", len(reader.reads), reader.reads, reader.tails)
	}
	if !strings.Contains(stderr.String(), "skipped: root qc-4al798r closed (status=closed); 27 step(s)") {
		t.Fatalf("stderr = %q, want 'skipped: root qc-4al798r closed' carrying the 27-step count", stderr.String())
	}
}

// Review item 4 (as narrowed by R2-A/addendum A): two federated legs whose
// stores both hold the same closed root. Each leg reads it once in its OWN
// store — verdicts are not inherited across ledgers — and the skip is still
// reported on ONE line per root per invocation, both steps counted.
func TestHookClaimClosedRootIsReportedOncePerInvocationAcrossLegs(t *testing.T) {
	const rootID = "qc-shared"
	spy := &closedRootClaimSpy{}
	reader := newClosedRootStoreReader(closedRootWorkflowRoot(rootID, "closed"))
	reader.byDir = map[string]*beads.MemStore{
		"/rig-a": beads.NewMemStoreFrom(0, []beads.Bead{closedRootWorkflowRoot(rootID, "closed")}, nil),
		"/rig-b": beads.NewMemStoreFrom(0, []beads.Bead{closedRootWorkflowRoot(rootID, "closed")}, nil),
	}
	ops, opts := closedRootOpsOpts("", spy, reader)
	ops.Runner = nil
	legs := []hookStore{
		{dir: "/rig-a", env: []string{"BEADS_DIR=/rig-a"}},
		{dir: "/rig-b", env: []string{"BEADS_DIR=/rig-b"}},
	}
	run := func(_ string, dir string, _ []string) (string, error) {
		if dir == "/rig-a" {
			return `[` + routedStepJSON("qc-shared.a", rootID) + `]`, nil
		}
		return `[` + routedStepJSON("qc-shared.b", rootID) + `]`, nil
	}

	var stdout, stderr bytes.Buffer
	claimHookWorkWithRunner("query", "/rig-a", nil, legs, opts, ops, run, func(string, error) {}, &stdout, &stderr)

	if len(spy.ids) != 0 {
		t.Fatalf("claim mutations = %v, want none", spy.ids)
	}
	if len(reader.readsAt) != 2 || reader.readsAt[0] == reader.readsAt[1] {
		t.Errorf("root reads = %v, want exactly one per leg store", reader.readsAt)
	}
	if n := strings.Count(stderr.String(), "skipped: root qc-shared closed"); n != 1 {
		t.Fatalf("skip lines = %d, want exactly 1 per root per invocation; stderr=%q", n, stderr.String())
	}
	if !strings.Contains(stderr.String(), "2 step(s) not served (qc-z0fmn0n): ") ||
		!strings.Contains(stderr.String(), "qc-shared.a") || !strings.Contains(stderr.String(), "qc-shared.b") {
		t.Errorf("stderr = %q, want both legs' steps on the one line", stderr.String())
	}
}

// Review item 6: a root that lives ONLY in the relocated class store is
// resolved through the class-routed read seam (the one withClosedRootGuard
// arms), observed closed there, and its step skipped.
func TestHookClaimSkipsStepWhoseRootIsClosedInTheRelocatedStore(t *testing.T) {
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, "gcg-700", map[string]string{"gc.kind": "workflow"})
	if err := class.Close("gcg-700"); err != nil {
		t.Fatalf("closing the relocated root: %v", err)
	}
	route := newClaimRouteFor(t, class)
	spy := &closedRootClaimSpy{}
	workReads := 0
	ops, opts := closedRootOpsOpts(`[`+routedStepJSON("qc-split.step", "gcg-700")+`]`, spy, nil)
	ops.ReadWorkMeta = func(context.Context, string, []string, string, string) (beads.Bead, error) {
		workReads++
		return beads.Bead{}, beads.ErrNotFound // the work store does not hold the root
	}
	ops = withClosedRootGuard(classRoutedHookClaimOps(ops, route))

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 0 {
		t.Fatalf("claim mutations = %v, want none — the root is closed in the relocated store", spy.ids)
	}
	if workReads == 0 {
		t.Errorf("work-store root reads = 0, want the work leg asked first")
	}
	if !strings.Contains(stderr.String(), "skipped: root gcg-700 closed (status=closed)") {
		t.Fatalf("stderr = %q, want the relocated root's skip named", stderr.String())
	}
}

// Review item 5 + round-2 should-fix: a demand-spawned seat whose trigger step
// belongs to a closed root drains no_work; with the teardown tail established
// and the trigger outside it, the classifier files it under its own
// closed_root bucket — not divergence ("still claimable"), and not benign.
func TestDemandDivergenceClassifiesAClosedRootTriggerAsClosedRoot(t *testing.T) {
	const rootID = "qc-dead-root"
	trigger := beads.Bead{
		ID: "qc-dead-root.step", Title: "step", Status: "open",
		Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": rootID, "gc.step_id": "implement"},
	}
	reader := newClosedRootStoreReader(closedRootWorkflowRoot(rootID, "closed"), trigger)
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+stepJSON(trigger)+`]`, spy, reader)
	ops.ReadWorkMeta = reader.fn
	ops.ConfirmBlocked = func(context.Context, string, []string, string, string) (bool, error) { return false, nil }

	status, classification, calls := classifyAfterDrain(t, trigger.ID, opts, ops)

	if len(spy.ids) != 0 || calls != 1 {
		t.Fatalf("claims = %v, divergence calls = %d; want a no_work drain that reaches the classifier once", spy.ids, calls)
	}
	if classification != events.DemandClaimClosedRoot {
		t.Fatalf("classification = %q (status %q), want %q for a closed-root trigger outside the tail", classification, status, events.DemandClaimClosedRoot)
	}

	// Control: the same trigger with no observed closed root IS a divergence,
	// so the verdict above came from the guard, not the fixture.
	if _, got := classifyDemandTrigger(trigger.ID, "/tmp/work", opts, ops); got != events.DemandClaimDivergence {
		t.Fatalf("control classification = %q, want divergence without the guard's verdict", got)
	}
}

// Addendum C: the queried sibling S carries no gc.step_id, so the closed root
// is cached WITHOUT a teardown tail. The trigger T is a teardown retry attempt
// (gc.step_id only) that the query never returned. Nothing established that T
// is outside the tail, so it must stay a divergence — it should have been
// served.
func TestDemandDivergenceWithoutAnEstablishedTailStaysDivergence(t *testing.T) {
	const rootID = "qc-td2"
	sibling := beads.Bead{
		ID: "qc-td2.s", Title: "s", Status: "open",
		Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": rootID},
	}
	teardown := beads.Bead{
		ID: "qc-td2.cleanup", Title: "cleanup", Status: "closed",
		Metadata: map[string]string{"gc.root_bead_id": rootID, "gc.step_id": "cleanup-worktree", "gc.scope_role": "teardown"},
	}
	retry := beads.Bead{
		ID: "qc-td2.cleanup.1", Title: "retry", Status: "open",
		Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": rootID, "gc.step_id": "cleanup-worktree"},
	}
	reader := newClosedRootStoreReader(closedRootWorkflowRoot(rootID, "closed"), sibling, teardown, retry)
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+stepJSON(sibling)+`]`, spy, reader)
	ops.ReadWorkMeta = reader.fn
	ops.ConfirmBlocked = func(context.Context, string, []string, string, string) (bool, error) { return false, nil }

	_, classification, calls := classifyAfterDrain(t, retry.ID, opts, ops)

	if calls != 1 {
		t.Fatalf("divergence calls = %d, want the drain to reach the classifier", calls)
	}
	if reader.tails != 0 {
		t.Fatalf("teardown-tail builds = %d, want 0 — the fixture must leave the tail unbuilt", reader.tails)
	}
	if classification != events.DemandClaimDivergence {
		t.Fatalf("classification = %q, want divergence: no established tail places the retry outside it", classification)
	}
}

// classifyAfterDrain runs one claim that drains and returns what the drain's
// divergence classifier concluded about triggerID, reading the gate the claim
// actually built.
func classifyAfterDrain(t *testing.T, triggerID string, opts hookClaimOptions, ops hookClaimOps) (status, classification string, calls int) {
	t.Helper()
	prev := hookRecordDemandClaimDivergence
	hookRecordDemandClaimDivergence = func(_ string, dir string, opts hookClaimOptions, ops hookClaimOps, _ io.Writer) {
		calls++
		status, classification = classifyDemandTrigger(triggerID, dir, opts, ops)
	}
	t.Cleanup(func() { hookRecordDemandClaimDivergence = prev })
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
	return status, classification, calls
}

// The production entry point (claimHookWork) arms the guard through the
// claim's own read seam — the one the class route wraps.
func TestClosedRootGuardArmsThroughReadWorkMeta(t *testing.T) {
	reader := newClosedRootStoreReader(closedRootWorkflowRoot("qc-r", "closed"))
	ops := withClosedRootGuard(hookClaimOps{ReadWorkMeta: reader.fn})
	if ops.ReadRoot == nil {
		t.Fatal("ReadRoot is nil after withClosedRootGuard; the production path would serve closed-root steps")
	}
	if _, err := ops.ReadRoot(context.Background(), "/d", nil, "qc-r", "a"); err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	if len(reader.reads) != 1 || reader.reads[0] != "qc-r" {
		t.Fatalf("ReadWorkMeta reads = %v, want the guard to read through it", reader.reads)
	}

	// An explicitly supplied ReadRoot is kept.
	called := false
	explicit := func(context.Context, string, []string, string, string) (beads.Bead, error) {
		called = true
		return beads.Bead{}, nil
	}
	ops = withClosedRootGuard(hookClaimOps{ReadRoot: explicit, ReadWorkMeta: reader.fn})
	_, _ = ops.ReadRoot(context.Background(), "/d", nil, "x", "a")
	if !called {
		t.Fatal("withClosedRootGuard replaced an explicit ReadRoot")
	}
}

// R2-A: a rig-scoped worker's first leg reads the root NOT FOUND (it is not
// that ledger's bead) and serves the step unguarded; the claim there fails
// not-found and the leg is dropped. The next leg holds the root CLOSED. That
// leg must read it in its own store and skip the step — a failed read on one
// leg must not unguard another.
func TestHookClaimFailedRootReadOnOneLegDoesNotUnguardAnother(t *testing.T) {
	const rootID = "qc-city-root"
	reader := newClosedRootStoreReader()
	reader.byDir = map[string]*beads.MemStore{
		"/rig-a": beads.NewMemStoreFrom(0, nil, nil), // the rig store: no such root
		"/rig-b": beads.NewMemStoreFrom(0, []beads.Bead{closedRootWorkflowRoot(rootID, "closed")}, nil),
	}
	var claimDirs []string
	ops, opts := closedRootOpsOpts("", &closedRootClaimSpy{}, reader)
	ops.Runner = nil
	ops.Claim = func(_ context.Context, dir string, _ []string, id, assignee string) (beads.Bead, bool, error) {
		claimDirs = append(claimDirs, dir)
		if dir == "/rig-a" {
			return beads.Bead{}, false, fmt.Errorf("claiming %s: %w", id, beads.ErrNotFound)
		}
		return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{}}, true, nil
	}
	legs := []hookStore{
		{dir: "/rig-a", env: []string{"BEADS_DIR=/rig-a"}},
		{dir: "/rig-b", env: []string{"BEADS_DIR=/rig-b"}},
	}
	step := routedStepJSON("qc-city-root.step", rootID)
	run := func(string, string, []string) (string, error) { return `[` + step + `]`, nil }

	var stdout, stderr bytes.Buffer
	claimHookWorkWithRunner("query", "/rig-a", nil, legs, opts, ops, run, func(string, error) {}, &stdout, &stderr)

	for _, dir := range claimDirs {
		if dir == "/rig-b" {
			t.Fatalf("claim attempts = %v, want none on /rig-b — its store holds the root closed", claimDirs)
		}
	}
	reads := map[string]int{}
	for _, r := range reader.readsAt {
		reads[r]++
	}
	if len(reader.readsAt) != 2 || reads["/rig-a|"+rootID] != 1 || reads["/rig-b|"+rootID] != 1 {
		t.Fatalf("root reads = %v, want exactly one per leg store: /rig-a (not found) and /rig-b (closed)", reader.readsAt)
	}
	if !strings.Contains(stderr.String(), "skipped: root qc-city-root closed") {
		t.Fatalf("stderr = %q, want the /rig-b skip named", stderr.String())
	}
}

// Addendum A (mirror of R2-A): the same root id in two stores with different
// states. The first leg's store holds it closed; the second holds it open. The
// second leg's step is served — it does not inherit the first store's verdict.
func TestHookClaimClosedRootInOneStoreDoesNotSuppressAnother(t *testing.T) {
	const rootID = "qc-twin"
	reader := newClosedRootStoreReader()
	reader.byDir = map[string]*beads.MemStore{
		"/rig-a": beads.NewMemStoreFrom(0, []beads.Bead{closedRootWorkflowRoot(rootID, "closed")}, nil),
		"/rig-b": beads.NewMemStoreFrom(0, []beads.Bead{closedRootWorkflowRoot(rootID, "open")}, nil),
	}
	var claimed []string
	ops, opts := closedRootOpsOpts("", &closedRootClaimSpy{}, reader)
	ops.Runner = nil
	ops.Claim = func(_ context.Context, dir string, _ []string, id, assignee string) (beads.Bead, bool, error) {
		claimed = append(claimed, dir+"|"+id)
		return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{}}, true, nil
	}
	legs := []hookStore{
		{dir: "/rig-a", env: []string{"BEADS_DIR=/rig-a"}},
		{dir: "/rig-b", env: []string{"BEADS_DIR=/rig-b"}},
	}
	// /rig-a's row outranks /rig-b's (priority 0 vs 2), so /rig-a — the store
	// holding the root CLOSED — is tried first and caches its verdict first.
	run := func(_ string, dir string, _ []string) (string, error) {
		if dir == "/rig-a" {
			return `[{"id":"qc-twin.a","status":"open","priority":0,"metadata":{"gc.routed_to":"worker","gc.root_bead_id":"qc-twin","gc.step_id":"implement"}}]`, nil
		}
		return `[{"id":"qc-twin.b","status":"open","priority":2,"metadata":{"gc.routed_to":"worker","gc.root_bead_id":"qc-twin","gc.step_id":"implement"}}]`, nil
	}

	var stdout, stderr bytes.Buffer
	claimHookWorkWithRunner("query", "/rig-a", nil, legs, opts, ops, run, func(string, error) {}, &stdout, &stderr)

	if len(claimed) != 1 || claimed[0] != "/rig-b|qc-twin.b" {
		t.Fatalf("claims = %v, want only /rig-b's step (its root is open there); stderr=%s", claimed, stderr.String())
	}
	if !strings.Contains(stderr.String(), "skipped: root qc-twin closed (status=closed); 1 step(s) not served (qc-z0fmn0n): qc-twin.a") {
		t.Errorf("stderr = %q, want only /rig-a's step skipped", stderr.String())
	}
}

// R2-B: per-root cost is O(1) bd calls, measured through a real BdStore over a
// counting runner — one `bd show` for the root and one narrow teardown query —
// and twenty dead molecules ahead of a live step still leave the live step
// claimed inside the claim window. molecule.ListSubtree (the round-2 tail) paid
// a second show plus a Children call per member.
func TestHookClaimClosedRootCostIsConstantBdCallsPerRoot(t *testing.T) {
	var mu sync.Mutex
	shows, total := 0, 0
	runner := func(_ string, _ string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		total++
		if len(args) > 0 && args[0] == "show" {
			shows++
			id := args[len(args)-1]
			return []byte(fmt.Sprintf(`[{"id":%q,"status":"closed","issue_type":"task","metadata":{"gc.kind":"workflow"}}]`, id)), nil
		}
		return []byte(`[]`), nil
	}
	store := beads.NewBdStore("/rig", runner)
	rows := make([]string, 0, 21)
	for i := 1; i <= 20; i++ {
		rows = append(rows, routedStepWithIDJSON(fmt.Sprintf("qc-orphan-%02d.impl", i), fmt.Sprintf("qc-orphan-%02d", i), "implement"))
	}
	rows = append(rows, routedStepJSON("qc-live", ""))
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+strings.Join(rows, ",")+`]`, spy, nil)
	ops.ReadRoot = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) { return store.Get(id) }
	ops.TeardownTail = func(_ context.Context, _ string, _ []string, rootID, _ string) (func(beads.Bead) bool, error) {
		return hookClaimTeardownTail(store, rootID)
	}
	ops.ClaimWindow = 5 * time.Second

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(spy.ids) != 1 || spy.ids[0] != "qc-live" {
		t.Fatalf("claim mutations = %v, want the live step claimed", spy.ids)
	}
	if shows != 20 {
		t.Errorf("bd show calls = %d, want exactly 20 — one per closed root", shows)
	}
	if total > 20*3 {
		t.Errorf("bd calls = %d for 20 closed roots, want at most 3 per root", total)
	}
}

// R2-B / addendum B: resolution is lazy in TIER order. Slow closed roots on
// routed rows ahead of a ready assignment in the query cost nothing: the ready
// assignment is resolved first, is servable, and is promoted inside a claim
// window the slow reads would have exhausted.
func TestHookClaimSlowRootsBehindReadyAssignmentDoNotSpendTheWindow(t *testing.T) {
	existing := []beads.Bead{}
	rows := []string{}
	for i := 1; i <= 10; i++ {
		rootID := fmt.Sprintf("qc-slow-%02d", i)
		existing = append(existing, closedRootWorkflowRoot(rootID, "closed"))
		rows = append(rows, routedStepWithIDJSON(rootID+".impl", rootID, "implement"))
	}
	rows = append(rows, `{"id":"qc-mine","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}`)
	// No read may run at all: the zero-reads assertion below is the proof, so
	// the reads need no simulated latency to make a regression visible.
	reader := newClosedRootStoreReader(existing...)
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+strings.Join(rows, ",")+`]`, spy, reader)
	ops.ClaimWindow = 400 * time.Millisecond

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(spy.ids) != 1 || spy.ids[0] != "qc-mine" {
		t.Fatalf("claim mutations = %v, want the ready assignment promoted", spy.ids)
	}
	if result := decodeClosedRootResult(t, stdout.String()); result.Reason != "ready_assignment" {
		t.Fatalf("result = %+v, want ready_assignment", result)
	}
	if len(reader.reads) != 0 {
		t.Errorf("root reads = %v, want none — nothing behind the first servable row is resolved", reader.reads)
	}
}

// R2-B: the relocated-store teardown call takes no context; runWithDeadline
// bounds it.
func TestRunWithDeadlineReturnsOnContextExpiry(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runWithDeadline(ctx, func() (int, error) {
		<-release
		return 1, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("returned after %s, want promptly at the deadline", elapsed)
	}
}

// Round 4, N1: the first servable row is CONTESTED — its claim CAS loses the
// race (ok=false) — and twenty dead molecules sit behind it, ahead of a live
// row. The pre-tier resolve stopped at the contested row, so those twenty roots
// are still unresolved when the tier reaches them. They must be resolved
// OUTSIDE the tier's claim-mutation context: resolved inside it, slow reads
// spend the claim budget, the live row is never tried, and the seat writes a
// false no_work drain. Proven without wall-clock latency: the context the
// contested claim ran under is already done when any later root is read, and
// the live row is claimed under a context opened after the last read.
func TestHookClaimRootReadsAfterALostRaceDoNotSpendTheClaimBudget(t *testing.T) {
	existing := []beads.Bead{closedRootWorkflowRoot("qc-live-root", "open")}
	rows := []string{routedStepJSON("qc-contested", "")}
	for i := 1; i <= 20; i++ {
		rootID := fmt.Sprintf("qc-behind-%02d", i)
		existing = append(existing, closedRootWorkflowRoot(rootID, "closed"))
		rows = append(rows, routedStepWithIDJSON(rootID+".impl", rootID, "implement"))
	}
	rows = append(rows, routedStepWithIDJSON("qc-live-root.step", "qc-live-root", "implement"))
	reader := newClosedRootStoreReader(existing...)
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+strings.Join(rows, ",")+`]`, spy, reader)
	ops.ClaimWindow = time.Hour // far past the mutation budget: claimContextOpenedAt is exact
	var (
		attempts     []string
		contestedCtx context.Context
		servedOpened time.Time
		readsInside  []string
	)
	ops.ReadRoot = func(ctx context.Context, dir string, env []string, id, assignee string) (beads.Bead, error) {
		// A read while the contested claim's context is still open is a read
		// inside the tier's claim-mutation context: the regression under test.
		if contestedCtx != nil && contestedCtx.Err() == nil {
			readsInside = append(readsInside, id)
		}
		return reader.fn(ctx, dir, env, id, assignee)
	}
	ops.Claim = func(ctx context.Context, dir string, env []string, id, assignee string) (beads.Bead, bool, error) {
		attempts = append(attempts, id)
		if id == "qc-contested" {
			contestedCtx = ctx
			// Lost CAS race: another live claimant owns it.
			return beads.Bead{ID: id, Status: "in_progress", Assignee: "worker-2", Metadata: map[string]string{}}, false, nil
		}
		servedOpened, _ = claimContextOpenedAt(ctx)
		return spy.fn(ctx, dir, env, id, assignee)
	}
	ops.EmitClaimRejected = func(string, string, string) {}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(attempts) != 2 || attempts[0] != "qc-contested" || attempts[1] != "qc-live-root.step" {
		t.Fatalf("claim attempts = %v, want the contested row then the live row", attempts)
	}
	if result := decodeClosedRootResult(t, stdout.String()); result.Action != "work" || result.BeadID != "qc-live-root.step" {
		t.Fatalf("result = %+v, want work on the live row, not a no_work drain; stderr=%s", result, stderr.String())
	}
	if !strings.Contains(stderr.String(), "skipped: root qc-behind-01 closed") {
		t.Errorf("stderr = %q, want the dead molecules still reported as skipped", stderr.String())
	}
	if len(readsInside) != 0 {
		t.Errorf("root reads inside the contested claim's context = %v, want none", readsInside)
	}
	assertRootReadsPrecedeClaimContext(t, reader, servedOpened)
}

// Round 4, N2: on a federated city the drain classifies with the invocation's
// work dir and env, while the closed-root verdict was cached under the leg that
// read it (/rig-b). The classifier must still find that established verdict and
// file the trigger under closed_root — not a plain (false-alarm) divergence.
func TestDemandDivergenceFindsTheClosedRootVerdictOfAnotherLeg(t *testing.T) {
	const rootID = "qc-fed-root"
	trigger := beads.Bead{
		ID: "qc-fed-root.step", Title: "step", Status: "open",
		Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": rootID, "gc.step_id": "implement"},
	}
	rigB := beads.NewMemStoreFrom(0, []beads.Bead{closedRootWorkflowRoot(rootID, "closed"), trigger}, nil)
	reader := newClosedRootStoreReader()
	reader.byDir = map[string]*beads.MemStore{
		"/rig-a": beads.NewMemStoreFrom(0, nil, nil),
		"/rig-b": rigB,
	}
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts("", spy, reader)
	ops.Runner = nil
	// The trigger read is city-wide: it answers from the ledger that holds it.
	ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) { return rigB.Get(id) }
	ops.ConfirmBlocked = func(context.Context, string, []string, string, string) (bool, error) { return false, nil }
	legs := []hookStore{
		{dir: "/rig-a", env: []string{"BEADS_DIR=/rig-a"}},
		{dir: "/rig-b", env: []string{"BEADS_DIR=/rig-b"}},
	}
	run := func(_ string, dir string, _ []string) (string, error) {
		if dir == "/rig-b" {
			return `[` + stepJSON(trigger) + `]`, nil
		}
		return `[]`, nil
	}

	calls := 0
	var classification string
	prev := hookRecordDemandClaimDivergence
	hookRecordDemandClaimDivergence = func(_ string, dir string, opts hookClaimOptions, ops hookClaimOps, _ io.Writer) {
		calls++
		_, classification = classifyDemandTrigger(trigger.ID, dir, opts, ops)
	}
	t.Cleanup(func() { hookRecordDemandClaimDivergence = prev })

	var stdout, stderr bytes.Buffer
	claimHookWorkWithRunner("query", "/city", nil, legs, opts, ops, run, func(string, error) {}, &stdout, &stderr)

	if len(spy.ids) != 0 || calls != 1 {
		t.Fatalf("claims = %v, divergence calls = %d; want a no_work drain that reaches the classifier once; stderr=%s", spy.ids, calls, stderr.String())
	}
	if classification != events.DemandClaimClosedRoot {
		t.Fatalf("classification = %q, want %q: /rig-b established the closed root and its tail", classification, events.DemandClaimClosedRoot)
	}
}

// Round 4, N3: a store is its dir AND its whole env (sameHookStore's notion),
// so two legs sharing a dir and BEADS_DIR but reaching different ledgers keep
// separate verdicts.
func TestHookRootStoreKeyIsTheWholeStoreIdentity(t *testing.T) {
	a := []string{"BEADS_DIR=/x", "GC_DOLT_PORT=3307"}
	b := []string{"BEADS_DIR=/x", "GC_DOLT_PORT=51361"}
	if hookRootStoreKey("/d", a, "qc-r") == hookRootStoreKey("/d", b, "qc-r") {
		t.Fatal("legs differing only outside BEADS_DIR share a verdict key")
	}
	if !sameHookStore(hookStore{dir: "/d", env: a}, hookStore{dir: "/d", env: append([]string(nil), a...)}) ||
		hookRootStoreKey("/d", a, "qc-r") != hookRootStoreKey("/d", append([]string(nil), a...), "qc-r") {
		t.Fatal("the same store must key the same")
	}
	// Unambiguous serialization: moving a boundary is a different store.
	if hookRootStoreKey("/d", []string{"A=1", "B=2"}, "r") == hookRootStoreKey("/d", []string{"A=1\x00B=2"}, "r") {
		t.Fatal("env serialization is ambiguous")
	}
}

// slowGetGraph is a binding graph whose Get blocks until released.
type slowGetGraph struct {
	storebinding.GraphStore
	release chan struct{}
}

func (g slowGetGraph) Get(id string) (beads.Bead, error) {
	<-g.release
	return g.GraphStore.Get(id)
}

// Round 4, N4: the relocated root read takes no context of its own; the class
// route bounds it by the caller's ctx, so a slow binding cannot hold a root
// read (and with it the claim window) past its deadline.
func TestClassRoutedRootReadIsBoundedByItsContext(t *testing.T) {
	route := newCapabilityRefusingRoute(t, "gcg-root")
	release := make(chan struct{})
	defer close(release)
	route.graph = slowGetGraph{GraphStore: route.graph, release: release}
	route.resident["gcg-root"] = true
	ops := classRoutedHookClaimOps(hookFanoutBaseOps(nil), route)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ops.ReadWorkMeta(ctx, "city", nil, "gcg-root", "worker-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded from the bounded relocated read", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("relocated root read returned after %s, want promptly at the deadline", elapsed)
	}
}
