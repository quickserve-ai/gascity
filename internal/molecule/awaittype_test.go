package molecule

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// ga-knhu61: every machinery gate creator must set await_type AT CREATE —
// the beads create seam defaults empty to "human" and the on-creation
// notifier pages. These pin the two molecule-side creators.

func TestDeferBeadRoutingSetsAwaitTypeBeadAtCreate(t *testing.T) {
	b := beads.Bead{Type: "task", Title: "step"}
	deferBeadRouting(&b)
	if b.Type != "gate" {
		t.Fatalf("Type = %q, want gate", b.Type)
	}
	if b.AwaitType != beads.AwaitBead {
		t.Fatalf("AwaitType = %q, want %q — an empty value is defaulted to human by the create seam and pages", b.AwaitType, beads.AwaitBead)
	}
}

func TestDeferBeadRoutingReadyExcludedBeadStaysUntyped(t *testing.T) {
	b := beads.Bead{Type: "gate", Title: "already a gate"}
	deferBeadRouting(&b)
	if b.AwaitType != "" {
		t.Fatalf("a ready-excluded bead is not converted; AwaitType = %q, want empty", b.AwaitType)
	}
}

func TestGateAwaitTypeForStep(t *testing.T) {
	cases := []struct {
		name     string
		stepType string
		gate     *formula.RecipeGate
		want     string
	}{
		{"non-gate step", "task", nil, ""},
		{"gate with no RecipeGate", "gate", nil, beads.AwaitBead},
		{"out-of-vocabulary gate type", "gate", &formula.RecipeGate{Type: "all-children"}, beads.AwaitBead},
		{"in-vocabulary timer", "gate", &formula.RecipeGate{Type: "timer"}, beads.AwaitTimer},
		{"in-vocabulary gh:run", "gate", &formula.RecipeGate{Type: "gh:run"}, beads.AwaitGHRun},
		{"explicit human is honored", "gate", &formula.RecipeGate{Type: "human"}, beads.AwaitHuman},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gateAwaitTypeForStep(tc.stepType, tc.gate); got != tc.want {
				t.Fatalf("gateAwaitTypeForStep(%q, %+v) = %q, want %q", tc.stepType, tc.gate, got, tc.want)
			}
		})
	}
}

func TestStepToBeadGateCarriesAwaitType(t *testing.T) {
	step := formula.RecipeStep{
		ID:    "gate-x",
		Title: "Gate: all-children",
		Type:  "gate",
		Gate:  &formula.RecipeGate{Type: "all-children"},
	}
	b := stepToBead(step, nil, nil)
	if b.AwaitType != beads.AwaitBead {
		t.Fatalf("AwaitType = %q, want %q", b.AwaitType, beads.AwaitBead)
	}
	node, err := recipeStepToGraphNode(step, nil, nil)
	if err != nil {
		t.Fatalf("recipeStepToGraphNode: %v", err)
	}
	if node.AwaitType != beads.AwaitBead {
		t.Fatalf("graph node AwaitType = %q, want %q — the graph.v2 path mints gates too", node.AwaitType, beads.AwaitBead)
	}
}
