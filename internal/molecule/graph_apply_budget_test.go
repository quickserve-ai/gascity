package molecule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// A native graph apply that spends its OWN derived budget
// (beads.nativeGraphApplyDeadline: 120s + 2s per node/edge) must not be
// retried from scratch. The retry re-derives an identical budget over an
// identical plan, so it can only double the store load before hitting the
// same wall — and when the second attempt also fails, Instantiate drops both
// errors and falls through to the sequential path, producing a third complete
// creation. A 29-node/60-edge sling therefore burned 2x298s inside an
// external 480s ceiling and died as an opaque SIGKILL with empty stdout
// (gastownhall/gascity#6333).
//
// Genuine transients (a dropped connection, one slow statement) keep their
// retry; the discriminator is the typed beads.ErrGraphApplyBudgetExhausted,
// which the beads layer attaches only when the apply's own context expired
// while the caller's context was still live. See
// TestApplyGraphPlanWrapsOwnDeadlineAsBudgetExhausted in internal/beads for
// the proof that the wrap fires.
func TestInstantiateDoesNotReapplyAfterItsOwnDeadlineExhausted(t *testing.T) {
	budgetExhausted := fmt.Errorf(
		"native graph apply: %w after 4m58s budget for 29 nodes/60 edges: adding edge ga-wisp-f5tz43->ga-wisp-oog3my: failed to check for dependency cycle: %w",
		beads.ErrGraphApplyBudgetExhausted, context.DeadlineExceeded)
	store := &graphApplySpyStore{
		MemStore: beads.NewMemStore(),
		// Two seeded failures: today's single retry consumes the second
		// and then falls through to the sequential path. A store that
		// stops after the first leaves the second entry untouched.
		errs: []error{budgetExhausted, budgetExhausted},
	}
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(true)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })

	recipe := &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow"}},
			{ID: "wf.step", Title: "Work", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.step", DependsOnID: "wf", Type: "parent-child"},
		},
	}

	_, err := Instantiate(context.Background(), store, recipe, Options{})

	if store.calls != 1 {
		t.Errorf("ApplyGraphPlan calls = %d, want 1 (a spent budget must not be re-applied)", store.calls)
	}
	if err == nil {
		t.Error("Instantiate error = nil, want the budget-exhausted apply error")
	} else {
		// The marker must survive every wrapper between the store and here:
		// ApplyGraphPlan -> the caching graph-apply wrappers -> the
		// storebinding adapter and lifecycle wrappers ->
		// instantiateViaGraphApply. A wrapper that reformats with %v instead
		// of %w would silently restore the retry.
		if !errors.Is(err, beads.ErrGraphApplyBudgetExhausted) {
			t.Errorf("error = %v, want errors.Is(err, beads.ErrGraphApplyBudgetExhausted) through the wrapper chain", err)
		}
		if !strings.Contains(err.Error(), "adding edge") {
			t.Errorf("error = %v, want the failing edge surfaced to the caller", err)
		}
		if !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Errorf("error = %v, want the deadline cause surfaced to the caller", err)
		}
	}
	created, listErr := store.ListOpen()
	if listErr != nil {
		t.Fatalf("ListOpen: %v", listErr)
	}
	if len(created) != 0 {
		t.Errorf("sequential fallback created %d beads after a budget-exhausted apply, want 0", len(created))
	}
}

// The other side of the discrimination. A statement can fail on a deadline of
// its own while the apply's context and the caller's are both still live —
// nothing has been spent then, the beads layer leaves the error unmarked, and
// the existing single retry must still run. The needle "deadline exceeded" in
// the text is not what decides this; the typed marker is.
func TestInstantiateStillRetriesAStatementDeadlineWhileBothContextsLive(t *testing.T) {
	// synctest: the retry's real graphApplyTransientRetryDelay elapses on the
	// bubble's synthetic clock, so pinning the retry costs no wall time.
	synctest.Test(t, func(t *testing.T) {
		store := &graphApplySpyStore{
			MemStore: beads.NewMemStore(),
			errs: []error{fmt.Errorf(
				"native graph apply: adding edge ga-wisp-f5tz43->ga-wisp-oog3my: failed to check for dependency cycle: %w",
				context.DeadlineExceeded)},
		}
		prev := IsGraphApplyEnabled()
		SetGraphApplyEnabled(true)
		t.Cleanup(func() { SetGraphApplyEnabled(prev) })

		recipe := &formula.Recipe{
			Name: "wf",
			Steps: []formula.RecipeStep{
				{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow"}},
				{ID: "wf.step", Title: "Work", Type: "task"},
			},
			Deps: []formula.RecipeDep{
				{StepID: "wf.step", DependsOnID: "wf", Type: "parent-child"},
			},
		}

		result, err := Instantiate(context.Background(), store, recipe, Options{})
		if err != nil {
			t.Fatalf("Instantiate: %v", err)
		}
		if store.calls != 2 {
			t.Errorf("ApplyGraphPlan calls = %d, want 2 (an unmarked deadline keeps its retry)", store.calls)
		}
		if result.Created != 2 {
			t.Errorf("Created = %d, want 2 from the successful retry", result.Created)
		}
	})
}
