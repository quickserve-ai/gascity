package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// This file is the ga-k74enr regression suite (incident 2026-09-25: 61
// workflow roots quarantined). The control dispatcher's readiness scan picked
// READY beads assigned or routed to it without looking at gc.kind, so a
// workflow ROOT (gc.kind=workflow) routed to the control target reached
// dispatch.ProcessControl, failed with `unsupported control bead kind
// "workflow"`, and was quarantined (closed hard-failed). Selection must admit
// only beadmeta.ControlKinds, and it must drop the rest BEFORE the per-tier
// workflowServeScanLimit cap -- otherwise a backlog of routed roots fills the
// oldest-N window and starves the real control beads behind it.

const kindFilterControlTarget = "gascity/control-dispatcher"

func kindFilterParsedQuery(t *testing.T) (parsedControlReadyQuery, []string) {
	t.Helper()
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	parsed, ok := parseControlReadyQuery(query)
	if !ok {
		t.Fatalf("parseControlReadyQuery: query not recognized: %q", query)
	}
	envList := []string{
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	}
	return parsed, envList
}

func workflowRootFixture(id string, created time.Time, extra map[string]string) beads.Bead {
	md := map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
		"gc.formula_contract":    "graph.v2",
	}
	for k, v := range extra {
		md[k] = v
	}
	return beads.Bead{ID: id, Type: "task", Status: "open", CreatedAt: created, Metadata: md}
}

// TestEvaluateControlReadyRouteTierDropsNonControlKindBeforeCap: more
// unassigned workflow roots than the per-route cap, all routed to the control
// target and all OLDER than one real control bead routed the same way. The
// control bead must be served and no root may be.
func TestEvaluateControlReadyRouteTierDropsNonControlKindBeforeCap(t *testing.T) {
	for _, routeKey := range []string{beadmeta.RoutedToMetadataKey, beadmeta.RunTargetMetadataKey} {
		t.Run(routeKey, func(t *testing.T) {
			parsed, envList := kindFilterParsedQuery(t)
			var ready []beads.Bead
			for i := 0; i < workflowServeScanLimit+5; i++ {
				ready = append(ready, workflowRootFixture(
					fmt.Sprintf("ga-kf-route-%s-root-%02d", routeKey, i),
					time.Unix(int64(100+i), 0),
					map[string]string{routeKey: kindFilterControlTarget},
				))
			}
			controlID := "ga-kf-route-" + routeKey + "-check"
			ready = append(ready, beads.Bead{
				ID: controlID, Type: "task", Status: "open", CreatedAt: time.Unix(10_000, 0),
				Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindCheck, routeKey: kindFilterControlTarget},
			})

			got := beadIDs(evaluateControlReady(ready, parsed, envList))
			if !slices.Equal(got, []string{controlID}) {
				t.Fatalf("evaluateControlReady ids = %v, want only the control bead %s (workflow roots must never be served, and must not crowd the control bead out of the %d-bead window)", got, controlID, workflowServeScanLimit)
			}
		})
	}
}

// TestEvaluateControlReadyAssigneeTierDropsNonControlKindBeforeCap is the same
// property on the assignee tier: roots assigned to the control session name
// ahead of a real control bead in ready order.
func TestEvaluateControlReadyAssigneeTierDropsNonControlKindBeforeCap(t *testing.T) {
	parsed, envList := kindFilterParsedQuery(t)
	var ready []beads.Bead
	for i := 0; i < workflowServeScanLimit+5; i++ {
		root := workflowRootFixture(fmt.Sprintf("ga-kf-assigned-root-%02d", i), time.Unix(int64(100+i), 0), nil)
		root.Assignee = "gascity--control-dispatcher"
		ready = append(ready, root)
	}
	ready = append(ready, beads.Bead{
		ID: "ga-kf-assigned-check", Type: "task", Status: "open", CreatedAt: time.Unix(10_000, 0),
		Assignee: "gascity--control-dispatcher",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindCheck},
	})

	got := beadIDs(evaluateControlReady(ready, parsed, envList))
	if !slices.Equal(got, []string{"ga-kf-assigned-check"}) {
		t.Fatalf("evaluateControlReady ids = %v, want only ga-kf-assigned-check (assigned workflow roots must never be served, and must not crowd the control bead out of the %d-bead window)", got, workflowServeScanLimit)
	}
}

// TestEvaluateControlReadyTracesNonControlKindOnce: a dropped bead must not
// vanish silently, and a bead that stays ready must not spam the trace on
// every tick.
func TestEvaluateControlReadyTracesNonControlKindOnce(t *testing.T) {
	resetNotControlKindTracedForTest(t)
	tracePath := filepath.Join(t.TempDir(), "workflow-trace.log")
	t.Setenv("GC_WORKFLOW_TRACE", tracePath)
	t.Setenv("GC_SLING_TRACE", "")

	parsed, envList := kindFilterParsedQuery(t)
	ready := []beads.Bead{
		workflowRootFixture("ga-kf-trace-root", time.Unix(100, 0), map[string]string{beadmeta.RoutedToMetadataKey: kindFilterControlTarget}),
		{ID: "ga-kf-trace-check", Type: "task", Status: "open", CreatedAt: time.Unix(200, 0), Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindCheck, beadmeta.RoutedToMetadataKey: kindFilterControlTarget}},
		// Not ours: a non-control bead routed elsewhere must not be traced.
		workflowRootFixture("ga-kf-trace-elsewhere", time.Unix(100, 0), map[string]string{beadmeta.RoutedToMetadataKey: "gascity/builder"}),
	}
	for tick := 0; tick < 3; tick++ {
		evaluateControlReady(ready, parsed, envList)
	}

	raw, err := os.ReadFile(tracePath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read trace: %v", err)
	}
	var skipLines, countLines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "reason=not_control_kind") {
			skipLines = append(skipLines, line)
		}
		if strings.Contains(line, "serve control-ready dropped-non-control count=") {
			countLines = append(countLines, line)
		}
	}
	// The per-tick count line is NOT deduped: a misrouted bead that stays
	// ready stays visible on every scan, counting only beads on our routes.
	if len(countLines) != 3 {
		t.Fatalf("dropped-non-control count lines = %d, want 1 per tick (3); trace:\n%s", len(countLines), raw)
	}
	for _, line := range countLines {
		if !strings.Contains(line, "count=1") {
			t.Fatalf("count line %q, want count=1 (the bead routed elsewhere is not ours)", line)
		}
	}
	if len(skipLines) != 1 {
		t.Fatalf("not_control_kind trace lines = %d, want exactly 1 across 3 ticks; trace:\n%s", len(skipLines), raw)
	}
	line := skipLines[0]
	for _, want := range []string{"bead=ga-kf-trace-root", "kind=workflow", beadmeta.RoutedToMetadataKey + "=" + kindFilterControlTarget} {
		if !strings.Contains(line, want) {
			t.Fatalf("trace line %q missing %q", line, want)
		}
	}
}

// TestControlReadyShellReduceDropsNonControlKinds pins the jq twin to the Go
// merge by executing the shipped reduce, exactly as
// TestControlReadyShellReduceDropsFailedPartialMolecules does for the
// molecule_failed condition.
func TestControlReadyShellReduceDropsNonControlKinds(t *testing.T) {
	jqPath, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not installed; the control-ready shell query merges with jq")
	}
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	program := controlReadyShellReduceProgram(t, query)

	assigned := []beads.Bead{
		{ID: "ga-kf-jq-root-assigned", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
		{ID: "ga-kf-jq-retry", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRetry}},
	}
	routed := []beads.Bead{
		{ID: "ga-kf-jq-root-routed", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.RoutedToMetadataKey: kindFilterControlTarget}},
		{ID: "ga-kf-jq-kindless", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: kindFilterControlTarget}},
		{ID: "ga-kf-jq-run", Metadata: map[string]string{beadmeta.KindMetadataKey: "run"}},
		{ID: "ga-kf-jq-check", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindCheck}},
	}

	var tiers bytes.Buffer
	for _, group := range [][]beads.Bead{assigned, routed} {
		encoded, err := json.Marshal(group)
		if err != nil {
			t.Fatalf("marshal fixture group: %v", err)
		}
		tiers.Write(encoded)
		tiers.WriteByte('\n')
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "ready-tiers.json")
	if err := os.WriteFile(fixture, tiers.Bytes(), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	out := runExternalOutput(t, dir, jqPath, "-s", program, fixture)
	var shellMerged []beads.Bead
	if err := json.Unmarshal(out, &shellMerged); err != nil {
		t.Fatalf("decode jq output %q: %v", out, err)
	}

	got := beadIDs(shellMerged)
	want := []string{"ga-kf-jq-retry", "ga-kf-jq-check"}
	if !slices.Equal(got, want) {
		t.Fatalf("the shipped jq reduce admits %v, want %v (only control kinds)", got, want)
	}
	if goMerged := beadIDs(mergeControlReadyGroups(assigned, routed)); !slices.Equal(goMerged, got) {
		t.Fatalf("the shipped jq reduce admits %v, the Go merge admits %v -- the two dispatch surfaces have drifted", got, goMerged)
	}
}

// assertBeadUntouched fails unless the bead is exactly as it was before the
// dispatch attempt: still open, no quarantine label, no quarantine or
// disposition metadata, and no metadata written at all.
func assertBeadUntouched(t *testing.T, store beads.Store, before beads.Bead) {
	t.Helper()
	after, err := store.Get(before.ID)
	if err != nil {
		t.Fatalf("get %s: %v", before.ID, err)
	}
	if after.Status != "open" {
		t.Fatalf("%s status = %q, want open", before.ID, after.Status)
	}
	if slices.Contains(after.Labels, "gc:control-quarantined") {
		t.Fatalf("%s labels = %v, want no gc:control-quarantined", before.ID, after.Labels)
	}
	for _, key := range []string{"gc.control_quarantined", "gc.control_quarantined_at", "gc.final_disposition"} {
		if got := after.Metadata[key]; got != "" {
			t.Fatalf("%s %s = %q, want unset", before.ID, key, got)
		}
	}
	if len(after.Metadata) != len(before.Metadata) {
		t.Fatalf("%s metadata = %v, want unchanged %v", before.ID, after.Metadata, before.Metadata)
	}
	for k, v := range before.Metadata {
		if after.Metadata[k] != v {
			t.Fatalf("%s metadata[%s] = %q, want unchanged %q", before.ID, k, after.Metadata[k], v)
		}
	}
}

// createKindFilterBeads seeds the two bead shapes the incident closed: a
// workflow ROOT (gc.kind=workflow) and an ordinary workflow STEP (no gc.kind),
// both the kind of thing a review seat's own work query hands back.
func createKindFilterBeads(t *testing.T, store beads.Store) (root, step beads.Bead) {
	t.Helper()
	root, err := store.Create(beads.Bead{
		Title: "workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
			"gc.formula_contract":    "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	step, err = store.Create(beads.Bead{
		Title: "review step",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			"gc.step_id":                   "review",
			beadmeta.RoutedToMetadataKey:   "qcore/review.orchestrator",
		},
	})
	if err != nil {
		t.Fatalf("create workflow step: %v", err)
	}
	return root, step
}

// TestRunControlDispatcherRefusesNonControlKindWithoutQuarantine is the
// primary invariant (ga-k74enr): the one-shot dispatcher itself refuses a bead
// whose gc.kind is not a control kind, with an error, and writes nothing. The
// incident's 83 closes (61 roots + 22 kindless steps) went through this entry.
func TestRunControlDispatcherRefusesNonControlKindWithoutQuarantine(t *testing.T) {
	clearGCEnv(t)
	store := beads.NewMemStore()
	root, step := createKindFilterBeads(t, store)
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	for _, b := range []beads.Bead{root, step} {
		var stderr bytes.Buffer
		err := runControlDispatcherWithStoreAndConfig(t.TempDir(), t.TempDir(), store, b.ID, cfg, io.Discard, &stderr)
		if err == nil {
			t.Fatalf("runControlDispatcherWithStoreAndConfig(%s kind=%q) = nil, want a not-a-control-bead error; stderr=%q", b.ID, b.Metadata[beadmeta.KindMetadataKey], stderr.String())
		}
		if !strings.Contains(err.Error(), "not a control bead") || !strings.Contains(err.Error(), b.ID) {
			t.Fatalf("error = %q, want it to say %q and name %s", err, "not a control bead", b.ID)
		}
		if strings.Contains(stderr.String(), "quarantined") {
			t.Fatalf("stderr = %q, want no quarantine", stderr.String())
		}
		assertBeadUntouched(t, store, b)
	}
}

// TestRunWorkflowServeSkipsNonControlKindWithoutQuarantine is the serve-loop
// side: a serve loop run without --follow inside a worker seat uses that
// seat's own work query, which hands back workflow roots and kindless steps.
// The loop must return nil, leave both beads exactly as they were, and say
// what it skipped.
func TestRunWorkflowServeSkipsNonControlKindWithoutQuarantine(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	tracePath := filepath.Join(t.TempDir(), "workflow-trace.log")
	t.Setenv("GC_WORKFLOW_TRACE", tracePath)

	prevCityFlag := cityFlag
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	cityFlag = ""
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	store := beads.NewMemStore()
	root, step := createKindFilterBeads(t, store)

	// Both beads stay ready, so a real work query returns them on every poll.
	// Bound the calls so a regression that spins on them fails instead of
	// hanging the suite.
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		if calls > 5 {
			return nil, nil
		}
		return []hookBead{
			{ID: root.ID, Metadata: hookBeadMetadata(root.Metadata)},
			{ID: step.ID, Metadata: hookBeadMetadata(step.Metadata)},
		}, nil
	}
	var dispatched []string
	controlDispatcherServe = func(cityPath, storePath, beadID string, stdout, stderr io.Writer) error {
		dispatched = append(dispatched, beadID)
		cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
		return runControlDispatcherWithStoreAndConfig(cityPath, storePath, store, beadID, cfg, stdout, stderr)
	}

	var stderr bytes.Buffer
	if err := runWorkflowServe("", false, io.Discard, &stderr); err != nil {
		t.Fatalf("runWorkflowServe: %v", err)
	}

	if slices.Contains(dispatched, root.ID) {
		t.Fatalf("controlDispatcherServe called for workflow root %s (dispatched=%v), want it skipped before dispatch", root.ID, dispatched)
	}
	if calls > 5 {
		t.Fatalf("workflowServeList calls = %d, want the serve loop to stop polling a queue that holds only skipped beads", calls)
	}
	assertBeadUntouched(t, store, root)
	assertBeadUntouched(t, store, step)

	raw, _ := os.ReadFile(tracePath)
	for _, b := range []beads.Bead{root, step} {
		if !strings.Contains(stderr.String(), b.ID) || !strings.Contains(stderr.String(), "not_control_kind") {
			t.Fatalf("stderr = %q, want a not_control_kind skip naming %s", stderr.String(), b.ID)
		}
		if !strings.Contains(string(raw), "bead="+b.ID) || !strings.Contains(string(raw), "reason=not_control_kind") {
			t.Fatalf("trace = %q, want a not_control_kind skip line for %s", raw, b.ID)
		}
	}
	if strings.Contains(stderr.String(), "quarantined bead=") {
		t.Fatalf("stderr = %q, want no quarantine", stderr.String())
	}
}

// resetNotControlKindTracedForTest empties the process-global once-per-bead
// trace set before and after the calling test, so the "traced once" assertion
// holds under -count=N and does not depend on test order.
func resetNotControlKindTracedForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		notControlKindTraced.mu.Lock()
		notControlKindTraced.ids = map[string]struct{}{}
		notControlKindTraced.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}
