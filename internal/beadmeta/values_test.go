package beadmeta

import "testing"

// TestPinnedKindValues pins the gc.kind value vocabulary. These exact strings
// are written into persistent bead metadata and branched on across dispatch,
// formula compilation, and the API projection — renaming a constant's value
// silently orphans every persisted bead carrying the old string, so any change
// here must come with a data-migration story.
func TestPinnedKindValues(t *testing.T) {
	pinned := map[string]string{
		KindRetry:            "retry",
		KindRalph:            "ralph",
		KindCheck:            "check",
		KindRetryEval:        "retry-eval",
		KindFanout:           "fanout",
		KindDrain:            "drain",
		KindScopeCheck:       "scope-check",
		KindWorkflowFinalize: "workflow-finalize",
		KindScope:            "scope",
		KindCleanup:          "cleanup",
		KindRun:              "run",
		KindRetryRun:         "retry-run",
		KindWorkflow:         "workflow",
		KindWisp:             "wisp",
		KindSpec:             "spec",
	}
	for got, want := range pinned {
		if got != want {
			t.Errorf("pinned kind value drift: got %q, want %q", got, want)
		}
	}
}

// TestPinnedOutcomeAndFailureClassValues pins the outcome and failure-class
// value vocabularies for the same persisted-data reason as the kind values.
func TestPinnedOutcomeAndFailureClassValues(t *testing.T) {
	pinned := map[string]string{
		OutcomePass:           "pass",
		OutcomeFail:           "fail",
		OutcomeSkipped:        "skipped",
		OutcomeMissingRoot:    "missing_root",
		FailureClassTransient: "transient",
		FailureClassHard:      "hard",
	}
	for got, want := range pinned {
		if got != want {
			t.Errorf("pinned value drift: got %q, want %q", got, want)
		}
	}
}

// TestIsRecognizedFailureClass pins the membership predicate used by the retry
// and Ralph dispatchers to decide whether a gc.failure_class value can be acted
// on. The real-world out-of-vocabulary cases below are the four distinct
// spellings observed on the 2026-08-12T00:55Z pour (ga-033u0e) — every one of
// them an agent-authored name for the same permanent condition.
func TestIsRecognizedFailureClass(t *testing.T) {
	recognized := []string{"transient", "hard", "", "  ", " transient ", "\thard\n"}
	for _, class := range recognized {
		if !IsRecognizedFailureClass(class) {
			t.Errorf("IsRecognizedFailureClass(%q) = false, want true", class)
		}
	}
	unrecognized := []string{
		"missing-root-bead",
		"missing-workflow-root-bead",
		"root_bead_missing",
		"missing_workflow_root",
		"Transient",
		"HARD",
		"mystery",
	}
	for _, class := range unrecognized {
		if IsRecognizedFailureClass(class) {
			t.Errorf("IsRecognizedFailureClass(%q) = true, want false", class)
		}
	}
}

// TestPinnedVocabularyValues pins the remaining per-key value vocabularies
// (formula contract, scope roles, state machines, dispositions, modes). All of
// these are persisted into bead metadata; the persisted-data caveat from
// TestPinnedKindValues applies equally here. String overlaps across domains
// are intentional: DispositionPass == OutcomePass as strings, but the domains
// are distinct.
func TestPinnedVocabularyValues(t *testing.T) {
	pinned := map[string]string{
		FormulaContractGraphV2:          "graph.v2",
		ScopeRoleBody:                   "body",
		ScopeRoleMember:                 "member",
		ScopeRoleControl:                "control",
		ScopeRoleTeardown:               "teardown",
		DrainStatePending:               "pending",
		DrainStateExpanding:             "expanding",
		DrainStateExpanded:              "expanded",
		DrainStateCompleting:            "completing",
		DrainStateSucceeded:             "succeeded",
		DrainStateFailed:                "failed",
		SpawnStateSpawning:              "spawning",
		SpawnStateSpawned:               "spawned",
		DispositionPass:                 "pass",
		DispositionHardFail:             "hard_fail",
		DispositionSoftFail:             "soft_fail",
		DispositionControllerError:      "controller_error",
		DispositionOrphanedWorkflow:     "orphaned_workflow",
		DispositionControlQuarantine:    "control_quarantined",
		FanoutModeParallel:              "parallel",
		FanoutModeSequential:            "sequential",
		DrainContextSeparate:            "separate",
		DrainContextShared:              "shared",
		DrainMemberAccessRead:           "read",
		DrainMemberAccessExclusive:      "exclusive",
		DrainOnItemFailureSkipRemaining: "skip_remaining",
		DrainOnItemFailureContinue:      "continue",
		CheckModeExec:                   "exec",
		ScopeKindCity:                   "city",
		ScopeKindRig:                    "rig",
	}
	for got, want := range pinned {
		if got != want {
			t.Errorf("pinned value drift: got %q, want %q", got, want)
		}
	}
}
