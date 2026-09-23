package formula

import (
	"os"
	"path/filepath"
	"strings"
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

// ga-2h3isb (Codex P2 on #134): Resolve rebuilds Requires from the merged
// constraint set, which must not drop disabled_reason. A child of a disabled
// parent inherits the parent's reason unless it declares its own.
func TestResolvePreservesDisabledReason(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("base", `
formula = "base"

[requires]
formula_compiler = ">=999.0.0"
disabled_reason = "parent reason"

[[steps]]
id = "a"
title = "A"
`)
	write("own", `
formula = "own"
extends = ["base"]

[requires]
formula_compiler = ">=1.0.0"
disabled_reason = "child reason"
`)
	write("inherits", `
formula = "inherits"
extends = ["base"]

[requires]
formula_compiler = ">=1.0.0"
`)
	write("bare", `
formula = "bare"
extends = ["base"]
`)
	for name, want := range map[string]string{
		"base":     "parent reason",
		"own":      "child reason",
		"inherits": "parent reason",
		"bare":     "parent reason",
	} {
		p := NewParser(dir)
		f, err := p.LoadByName(name)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		resolved, err := p.Resolve(f)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if resolved.Requires == nil || resolved.Requires.DisabledReason != want {
			t.Errorf("%s: resolved Requires = %+v, want disabled_reason %q", name, resolved.Requires, want)
		}
		if !IsUnsatisfiedRequirement(ValidateHostRequirements(resolved, true)) {
			t.Errorf("%s: resolved formula must stay unsatisfiable", name)
		}
	}
}

func TestUnknownRequirementNamesDisabledReason(t *testing.T) {
	err := unknownRequirementError("mystery")
	if !strings.Contains(err.Error(), "supported requirements: formula_compiler, disabled_reason") {
		t.Fatalf("unknown-requirement diagnostic omits disabled_reason: %v", err)
	}
}
