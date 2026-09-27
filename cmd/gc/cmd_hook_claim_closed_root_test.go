package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Pins the closed-root router skip (qc-z0fmn0n). On 2026-09-23 two molecule
// roots were closed while their step beads stayed open and routed: qc-xqc0b83,
// whose seat was then handed a six-bead continuation chain off it, and
// qc-4al798r, whose 27 steps stayed routable for an hour. Every seat that
// claimed one burned three worktree attempts on a deleted base branch.
// gc hook --claim must not serve a step whose gc.root_bead_id names a closed
// (or missing) root, and must say so on its diagnostic stream.
//
// The root lookups here read a real beads.MemStore, not a stub that answers
// "open" or "closed" by fiat: MemStore.Get has the same contract as the
// production BdStore.Get (wrapped beads.ErrNotFound for an absent id), so a
// verdict below is one the store actually produced.

type closedRootClaimSpy struct{ ids []string }

func (s *closedRootClaimSpy) fn(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
	s.ids = append(s.ids, id)
	return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{}}, true, nil
}

// closedRootStoreReader counts root lookups so the per-claim cache is
// observable, and reads through a real MemStore.
type closedRootStoreReader struct {
	store *beads.MemStore
	reads []string
}

func (r *closedRootStoreReader) fn(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
	r.reads = append(r.reads, id)
	return r.store.Get(id)
}

func newClosedRootStoreReader(roots ...beads.Bead) *closedRootStoreReader {
	return &closedRootStoreReader{store: beads.NewMemStoreFrom(0, roots, nil)}
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

func decodeClosedRootResult(t *testing.T, stdout string) hookClaimJSONResult {
	t.Helper()
	var result hookClaimJSONResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &result); err != nil {
		t.Fatalf("decoding hook result %q: %v", stdout, err)
	}
	return result
}

// (i) A closed root with routed open steps: none is served, and the
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
	if !strings.Contains(stderr.String(), "skipped: root qc-xqc0b83 closed") {
		t.Fatalf("stderr = %q, want the skip named as 'skipped: root qc-xqc0b83 closed'", stderr.String())
	}
	if !strings.Contains(stderr.String(), "status=closed") {
		t.Errorf("stderr = %q, want the observed root status on the skip line", stderr.String())
	}
}

// A continuation sibling PREASSIGNED to this seat (the qc-xqc0b83 chain shape)
// arrives through the ready-assignment tier, not the routed tier; it must be
// skipped there too.
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

// A root the store reports ABSENT (torn mint, purge) is treated as closed, and
// the line says it was not found rather than claiming a status nobody read.
func TestHookClaimTreatsMissingRootAsClosed(t *testing.T) {
	spy := &closedRootClaimSpy{}
	reader := newClosedRootStoreReader() // empty store: the root does not exist
	ops, opts := closedRootOpsOpts(`[`+routedStepJSON("qc-orphan", "qc-gone")+`]`, spy, reader)

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 0 {
		t.Fatalf("claim mutations = %v, want none — a missing root is treated as closed", spy.ids)
	}
	if !strings.Contains(stderr.String(), "skipped: root qc-gone closed") ||
		!strings.Contains(stderr.String(), "not found") {
		t.Fatalf("stderr = %q, want 'skipped: root qc-gone closed' naming the not-found observation", stderr.String())
	}
	if strings.Contains(stderr.String(), "status=closed") {
		t.Errorf("stderr = %q, a missing root was never read as status=closed", stderr.String())
	}
}

// (ii) An open root is served exactly as today.
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

// (iii) A step with no gc.root_bead_id is served exactly as today, without a
// root read.
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

// A root whose lookup cannot complete proves nothing either way: the step is
// served as today (fail open, like the drain-pending probe) and the line names
// the failed read instead of inventing a status.
func TestHookClaimServesStepWhenRootLookupIsIndeterminate(t *testing.T) {
	spy := &closedRootClaimSpy{}
	ops, opts := closedRootOpsOpts(`[`+routedStepJSON("qc-step-x", "qc-murky")+`]`, spy, nil)
	ops.ReadRoot = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
		return beads.Bead{}, fmt.Errorf("getting bead %q: %w: dolt i/o timeout", id, beads.ErrVerifyIndeterminate)
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 1 || spy.ids[0] != "qc-step-x" {
		t.Fatalf("claim mutations = %v, want qc-step-x served — an unproven absence is not a closed root", spy.ids)
	}
	if strings.Contains(stderr.String(), "skipped: root") {
		t.Errorf("stderr = %q, want no skip on an indeterminate read", stderr.String())
	}
	if !strings.Contains(stderr.String(), "root qc-murky lookup failed") {
		t.Errorf("stderr = %q, want the failed root read named", stderr.String())
	}
}

// (iv) Replay of the qc-4al798r shape through the federated claim loop: one
// closed root with 27 routed open steps. None is served, the root is read ONCE
// for the whole claim, and the skip line carries the count.
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
		t.Errorf("root reads = %d (%v), want 1 — the root is looked up once per claim, not per step", len(reader.reads), reader.reads)
	}
	if !strings.Contains(stderr.String(), "skipped: root qc-4al798r closed") ||
		!strings.Contains(stderr.String(), "27 step(s)") {
		t.Fatalf("stderr = %q, want 'skipped: root qc-4al798r closed' carrying the 27-step count", stderr.String())
	}
}

// The production entry point (claimHookWork) arms the guard through the
// claim's own read seam — the one the class route wraps — so a root in a
// relocated store is resolved where the claim itself would resolve it.
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
