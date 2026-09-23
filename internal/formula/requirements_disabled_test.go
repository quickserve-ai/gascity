package formula

import (
	"testing"

	"github.com/BurntSushi/toml"
)

// ga-2h3isb: disabled_reason is a recognized [requires] key; other unknown
// keys are still refused, and an empty reason is not a marker.
func TestRequirementsDisabledReason(t *testing.T) {
	var ok struct {
		Requires *Requirements `toml:"requires"`
	}
	if _, err := toml.Decode("[requires]\nformula_compiler = \">=999.0.0\"\ndisabled_reason = \"why\"\n", &ok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ok.Requires.DisabledReason != "why" || ok.Requires.FormulaCompiler != ">=999.0.0" {
		t.Fatalf("decoded %+v", ok.Requires)
	}
	for _, bad := range []string{
		"[requires]\ndisabled_reason = \"\"\n",
		"[requires]\ndisabled_reason = 3\n",
		"[requires]\nmystery = \"x\"\n",
	} {
		var v struct {
			Requires *Requirements `toml:"requires"`
		}
		if _, err := toml.Decode(bad, &v); err == nil {
			t.Errorf("decode(%q) accepted, want an error", bad)
		}
	}
	if !IsUnsatisfiedRequirement(unsatisfiedFormulaCompilerRequirement(">=999.0.0", "", "2.0.0", true)) {
		t.Fatal("IsUnsatisfiedRequirement must recognize the unsatisfied error")
	}
	if IsUnsatisfiedRequirement(invalidFormulaCompilerRequirement("x", nil)) {
		t.Fatal("an invalid requirement is not an unsatisfied one")
	}
}
