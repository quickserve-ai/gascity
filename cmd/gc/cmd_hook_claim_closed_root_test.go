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
	"github.com/gastownhall/gascity/internal/molecule"
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

// closedRootStoreReader counts root reads so the invocation cache is
// observable, and reads through a real MemStore.
type closedRootStoreReader struct {
	mu    sync.Mutex
	store *beads.MemStore
	reads []string
	delay time.Duration
}

func (r *closedRootStoreReader) fn(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	r.mu.Lock()
	r.reads = append(r.reads, id)
	r.mu.Unlock()
	return r.store.Get(id)
}

func (r *closedRootStoreReader) tail(_ context.Context, _ string, _ []string, rootID, _ string) (func(beads.Bead) bool, error) {
	return molecule.TeardownTailExclusion(r.store, rootID)
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
// Fifteen steps on distinct closed roots ahead of one open-root step, with a
// root read slower than the budget allows in aggregate: the open-root step is
// still claimed, and the seat does not report a false no_work.
func TestHookClaimRootReadsDoNotSpendTheClaimBudget(t *testing.T) {
	old := hookClaimMutationTimeout
	hookClaimMutationTimeout = 200 * time.Millisecond
	t.Cleanup(func() { hookClaimMutationTimeout = old })

	existing := []beads.Bead{closedRootWorkflowRoot("qc-open-root", "open")}
	rows := make([]string, 0, 16)
	for i := 1; i <= 15; i++ {
		rootID := fmt.Sprintf("qc-dead-%02d", i)
		existing = append(existing, closedRootWorkflowRoot(rootID, "closed"))
		rows = append(rows, routedStepJSON(fmt.Sprintf("qc-dead-%02d.step", i), rootID))
	}
	rows = append(rows, routedStepJSON("qc-open-root.step", "qc-open-root"))
	reader := newClosedRootStoreReader(existing...)
	reader.delay = 20 * time.Millisecond // 16 reads = 320ms > the 200ms claim budget
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+strings.Join(rows, ",")+`]`, spy, reader)

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
}

// Brief (iv), review item 4: replay of the qc-4al798r shape through the
// federated claim loop — one closed root with 27 routed open steps. None is
// served, the root is read once for the whole invocation, and the skip line
// carries the count.
func TestHookClaimReplayQc4al798rClosedRootWith27RoutedSteps(t *testing.T) {
	const rootID = "qc-4al798r"
	rows := make([]string, 0, 27)
	for i := 1; i <= 27; i++ {
		rows = append(rows, routedStepJSON(fmt.Sprintf("qc-4al798r.%d", i), rootID))
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
	if len(reader.reads) != 1 {
		t.Errorf("root reads = %d (%v), want 1 — the root is read once per invocation, not per step", len(reader.reads), reader.reads)
	}
	if !strings.Contains(stderr.String(), "skipped: root qc-4al798r closed (status=closed); 27 step(s)") {
		t.Fatalf("stderr = %q, want 'skipped: root qc-4al798r closed' carrying the 27-step count", stderr.String())
	}
}

// Review item 4: two federated legs serving steps of the SAME closed root share
// one verdict — one read, one line, both steps counted.
func TestHookClaimClosedRootVerdictIsSharedAcrossLegs(t *testing.T) {
	const rootID = "qc-shared"
	spy := &closedRootClaimSpy{}
	reader := newClosedRootStoreReader(closedRootWorkflowRoot(rootID, "closed"))
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
	if len(reader.reads) != 1 {
		t.Errorf("root reads = %v, want exactly 1 across both legs", reader.reads)
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

// Review item 5: a demand-spawned seat whose trigger step belongs to a closed
// root drains no_work; the divergence classifier must read that as benign (the
// guard declined dead work), not "still claimable".
func TestDemandDivergenceTreatsAClosedRootTriggerAsBenign(t *testing.T) {
	const rootID = "qc-dead-root"
	trigger := beads.Bead{ID: "qc-dead-root.step", Title: "step", Status: "open",
		Metadata: map[string]string{"gc.routed_to": "worker", "gc.root_bead_id": rootID}}
	reader := newClosedRootStoreReader(closedRootWorkflowRoot(rootID, "closed"), trigger)
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+stepJSON(trigger)+`]`, spy, reader)
	ops.ReadWorkMeta = reader.fn
	ops.ConfirmBlocked = func(context.Context, string, []string, string, string) (bool, error) { return false, nil }

	var classification, status string
	calls := 0
	prev := hookRecordDemandClaimDivergence
	hookRecordDemandClaimDivergence = func(_ string, dir string, opts hookClaimOptions, ops hookClaimOps, _ io.Writer) {
		calls++
		status, classification = classifyDemandTrigger(trigger.ID, dir, opts, ops)
	}
	t.Cleanup(func() { hookRecordDemandClaimDivergence = prev })

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 0 || calls != 1 {
		t.Fatalf("claims = %v, divergence calls = %d; want a no_work drain that reaches the classifier once", spy.ids, calls)
	}
	if classification != events.DemandClaimBenign {
		t.Fatalf("classification = %q (status %q), want benign for a closed-root trigger", classification, status)
	}

	// Control: the same trigger with no observed closed root IS a divergence,
	// so the benign verdict above came from the guard, not the fixture.
	ops.rootGate = nil
	if _, got := classifyDemandTrigger(trigger.ID, "/tmp/work", opts, ops); got != events.DemandClaimDivergence {
		t.Fatalf("control classification = %q, want divergence without the guard's verdict", got)
	}
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
