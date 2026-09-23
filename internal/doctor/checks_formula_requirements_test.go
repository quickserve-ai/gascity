package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestFormulaRequirementsCheckOK(t *testing.T) {
	dir := t.TempDir()
	writeDoctorFormula(t, dir, "review", `
formula = "review"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "review"
title = "Review"
`)

	check := NewFormulaRequirementsCheck(&config.City{
		Daemon: config.DaemonConfig{FormulaV2: boolPtr(true)},
		FormulaLayers: config.FormulaLayers{
			City: []string{dir},
		},
	}, t.TempDir())

	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want OK; details:\n%s", result.Status, strings.Join(result.Details, "\n"))
	}
}

func TestFormulaRequirementsCheckReportsRequirementDiagnosticsAcrossLayers(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	writeDoctorFormula(t, cityDir, "legacy-contract", `
formula = "legacy-contract"
contract = "graph.v2"

[[steps]]
id = "work"
title = "Work"
`)
	writeDoctorFormula(t, cityDir, "missing-requirement", `
formula = "missing-requirement"

[[steps]]
id = "work"
title = "Work"
metadata = { "gc.on_fail" = "abort_scope" }
`)
	writeDoctorFormula(t, cityDir, "disabled-v2", `
formula = "disabled-v2"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "work"
title = "Work"
`)
	writeDoctorFormula(t, cityDir, "unknown-axis", `
formula = "unknown-axis"

[requires]
state_store = ">=2.0.0"

[[steps]]
id = "work"
title = "Work"
`)
	writeDoctorFormula(t, cityDir, "legacy-parent", `
formula = "legacy-parent"

[requires]
formula_compiler = "<2.0.0"

[[steps]]
id = "legacy"
title = "Legacy"
`)
	writeDoctorFormula(t, cityDir, "v2-parent", `
formula = "v2-parent"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "v2"
title = "V2"
`)
	writeDoctorFormula(t, cityDir, "conflict-child", `
formula = "conflict-child"
extends = ["legacy-parent", "v2-parent"]

[[steps]]
id = "work"
title = "Work"
`)
	writeDoctorFormula(t, rigDir, "invalid-rig", `
formula = "invalid-rig"

[requires]
formula_compiler = "not-a-comparator"

[[steps]]
id = "work"
title = "Work"
`)

	check := NewFormulaRequirementsCheck(&config.City{
		Daemon: config.DaemonConfig{FormulaV2: boolPtr(false)},
		FormulaLayers: config.FormulaLayers{
			City: []string{cityDir},
			Rigs: map[string][]string{"proj": {rigDir}},
		},
	}, t.TempDir())

	result := check.Run(&CheckContext{})
	if result.Status != StatusError {
		t.Fatalf("Status = %v, want error; details:\n%s", result.Status, strings.Join(result.Details, "\n"))
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		`deprecated contract = "graph.v2"`,
		"graph-only constructs",
		"[daemon] formula_v2 is disabled",
		"formula.requirement_unknown",
		"formula.compiler_requirement_invalid",
		"formula.compiler_requirement_conflict",
		"rig:proj",
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	if result.FixHint == "" {
		t.Fatal("FixHint is empty")
	}
}

func TestFormulaRequirementsCheckDeduplicatesCityDiagnosticsInRigLayers(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	writeDoctorFormula(t, cityDir, "shared-missing", `
formula = "shared-missing"

[[steps]]
id = "work"
title = "Work"
metadata = { "gc.on_fail" = "abort_scope" }
`)
	writeDoctorFormula(t, rigDir, "rig-invalid", `
formula = "rig-invalid"

[requires]
formula_compiler = "not-a-comparator"

[[steps]]
id = "work"
title = "Work"
`)

	check := NewFormulaRequirementsCheck(&config.City{
		Daemon: config.DaemonConfig{FormulaV2: boolPtr(true)},
		FormulaLayers: config.FormulaLayers{
			City: []string{cityDir},
			Rigs: map[string][]string{"proj": {cityDir, rigDir}},
		},
	}, t.TempDir())

	result := check.Run(&CheckContext{})
	if result.Status != StatusError {
		t.Fatalf("Status = %v, want error; details:\n%s", result.Status, strings.Join(result.Details, "\n"))
	}
	details := strings.Join(result.Details, "\n")
	if got := strings.Count(details, `formula "shared-missing"`); got != 1 {
		t.Fatalf("shared city diagnostic count = %d, want 1; details:\n%s", got, details)
	}
	if !strings.Contains(details, `formula "rig-invalid"`) {
		t.Fatalf("details missing rig-specific diagnostic:\n%s", details)
	}
}

func TestFormulaRequirementsCheckReportsGraphConstructMissingCompilerRequirement(t *testing.T) {
	dir := t.TempDir()
	writeDoctorFormula(t, dir, "retry-without-requirement", `
formula = "retry-without-requirement"

[[steps]]
id = "work"
title = "Work"

[steps.retry]
max_attempts = 2
`)

	check := NewFormulaRequirementsCheck(&config.City{
		Daemon: config.DaemonConfig{FormulaV2: boolPtr(true)},
		FormulaLayers: config.FormulaLayers{
			City: []string{dir},
		},
	}, t.TempDir())

	result := check.Run(&CheckContext{})
	if result.Status != StatusError {
		t.Fatalf("Status = %v, want error; details:\n%s", result.Status, strings.Join(result.Details, "\n"))
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"retry-without-requirement",
		"graph-only constructs",
		`[requires] formula_compiler = ">=2.0.0"`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
}

func TestFormulaRequirementsCheckReportsNonRequirementLoadFailures(t *testing.T) {
	dir := t.TempDir()
	writeDoctorFormula(t, dir, "broken", `
formula = "broken"
[[steps]
id = "work"
`)
	writeDoctorFormula(t, dir, "missing-parent", `
formula = "missing-parent"
extends = ["absent-parent"]

[[steps]]
id = "work"
title = "Work"
`)

	check := NewFormulaRequirementsCheck(&config.City{
		Daemon: config.DaemonConfig{FormulaV2: boolPtr(true)},
		FormulaLayers: config.FormulaLayers{
			City: []string{dir},
		},
	}, t.TempDir())

	result := check.Run(&CheckContext{})
	if result.Status != StatusError {
		t.Fatalf("Status = %v, want error; details:\n%s", result.Status, strings.Join(result.Details, "\n"))
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		"broken",
		"parse formula",
		"missing-parent",
		"resolve formula",
		`absent-parent`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
}

func TestFormulaRequirementsCheckHonorsGCFormulaRef(t *testing.T) {
	doctorGitOK(t)
	repo := doctorInitRepo(t)
	formulaDir := filepath.Join(repo, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDoctorFormula(t, formulaDir, "ref-stable", `
formula = "ref-stable"

[[steps]]
id = "work"
title = "Work"
`)
	doctorRunGit(t, repo, "add", "formulas/ref-stable.toml")
	doctorRunGit(t, repo, "commit", "-m", "add ref-stable formula")

	writeDoctorFormula(t, formulaDir, "ref-stable", `
formula = "ref-stable"

[[steps]]
id = "work"
title = "Work"
metadata = { "gc.on_fail" = "abort_scope" }
`)

	t.Setenv("GC_FORMULA_REF", "main")
	check := NewFormulaRequirementsCheck(&config.City{
		Daemon: config.DaemonConfig{FormulaV2: boolPtr(true)},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulaDir},
		},
	}, t.TempDir())

	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want OK; details:\n%s", result.Status, strings.Join(result.Details, "\n"))
	}
}

func writeDoctorFormula(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func doctorGitOK(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available in PATH")
	}
}

func doctorInitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	doctorRunGit(t, root, "init", "-b", "main")
	doctorRunGit(t, root, "config", "user.email", "test@example.com")
	doctorRunGit(t, root, "config", "user.name", "test")
	doctorRunGit(t, root, "config", "commit.gpgsign", "false")
	return root
}

func doctorRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// ga-2h3isb: a rig can shadow a pack formula with an unsatisfiable
// requirement ON PURPOSE. With a disabled_reason, doctor reports it as
// intentionally disabled instead of as a blocking defect a patrol "fixes".
func TestFormulaRequirementsCheckIntentionallyDisabled(t *testing.T) {
	stub := func(reason string) string {
		s := "\nformula = \"mol-shadowed\"\n\n[requires]\nformula_compiler = \">=999.0.0\"\n"
		if reason != "" {
			s += "disabled_reason = \"" + reason + "\"\n"
		}
		return s + "\n[[steps]]\nid = \"commit\"\ntitle = \"Commit\"\n"
	}
	run := func(t *testing.T, content string) *CheckResult {
		t.Helper()
		dir := t.TempDir()
		writeDoctorFormula(t, dir, "mol-shadowed", content)
		return NewFormulaRequirementsCheck(&config.City{
			Daemon:        config.DaemonConfig{FormulaV2: boolPtr(true)},
			FormulaLayers: config.FormulaLayers{City: []string{dir}},
		}, t.TempDir()).Run(&CheckContext{})
	}

	t.Run("declared disabled reads as disabled, not a defect", func(t *testing.T) {
		r := run(t, stub("closes direct-push-to-base on the platform rig (ga-82cffd)"))
		if r.Status != StatusOK {
			t.Fatalf("Status = %v, want OK; details:\n%s", r.Status, strings.Join(r.Details, "\n"))
		}
		if !strings.Contains(r.Message, "1 intentionally disabled") ||
			!strings.Contains(strings.Join(r.Details, "\n"), "intentionally disabled city formula \"mol-shadowed\"") ||
			!strings.Contains(strings.Join(r.Details, "\n"), "ga-82cffd") {
			t.Fatalf("the disabled formula and its reason must be reported: %q %v", r.Message, r.Details)
		}
	})

	t.Run("control: the same stub without a reason is still an error", func(t *testing.T) {
		if r := run(t, stub("")); r.Status != StatusError {
			t.Fatalf("Status = %v, want Error; details:\n%s", r.Status, strings.Join(r.Details, "\n"))
		}
	})

	t.Run("a reason on a satisfiable requirement warns: the formula is dispatchable", func(t *testing.T) {
		content := "\nformula = \"mol-shadowed\"\n\n[requires]\nformula_compiler = \">=2.0.0\"\ndisabled_reason = \"stale marker\"\n\n[[steps]]\nid = \"commit\"\ntitle = \"Commit\"\n"
		r := run(t, content)
		if r.Status != StatusWarning || !strings.Contains(strings.Join(r.Details, "\n"), "DISPATCHABLE") {
			t.Fatalf("want a Warning naming the dispatchable formula: %v %q %v", r.Status, r.Message, r.Details)
		}
	})

	t.Run("a reason never excuses an INVALID requirement", func(t *testing.T) {
		content := "\nformula = \"mol-shadowed\"\n\n[requires]\nformula_compiler = \"not-a-version\"\ndisabled_reason = \"x\"\n\n[[steps]]\nid = \"commit\"\ntitle = \"Commit\"\n"
		if r := run(t, content); r.Status != StatusError {
			t.Fatalf("Status = %v, want Error; details:\n%s", r.Status, strings.Join(r.Details, "\n"))
		}
	})
}
