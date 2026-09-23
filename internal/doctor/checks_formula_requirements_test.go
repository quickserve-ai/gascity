package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/rollout"
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

	t.Run("a reason on a satisfiable requirement is honored when a composed expansion is unsatisfiable", func(t *testing.T) {
		dir := t.TempDir()
		writeDoctorFormula(t, dir, "mol-shadowed", "\nformula = \"mol-shadowed\"\n\n[requires]\nformula_compiler = \">=1.0.0\"\ndisabled_reason = \"disabled through its expansion\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n\n[compose]\n[[compose.expand]]\ntarget = \"work\"\nwith = \"never-expansion\"\n")
		writeDoctorFormula(t, dir, "never-expansion", "\nformula = \"never-expansion\"\ntype = \"expansion\"\n\n[requires]\nformula_compiler = \">=999.0.0\"\ndisabled_reason = \"never compiles\"\n\n[[template]]\nid = \"{target}.child\"\ntitle = \"Child\"\n")
		r := NewFormulaRequirementsCheck(&config.City{
			Daemon:        config.DaemonConfig{FormulaV2: boolPtr(true)},
			FormulaLayers: config.FormulaLayers{City: []string{dir}},
		}, t.TempDir()).Run(&CheckContext{})
		if r.Status != StatusOK || !strings.Contains(strings.Join(r.Details, "\n"), "by a composed requirement: disabled through its expansion") {
			t.Fatalf("a composed unsatisfiable requirement must honor the marker, not warn DISPATCHABLE: %v %q %v", r.Status, r.Message, r.Details)
		}
	})

	t.Run("a disabled formula with graph-only constructs is still just disabled", func(t *testing.T) {
		content := "\nformula = \"mol-shadowed\"\n\n[requires]\nformula_compiler = \">=999.0.0\"\ndisabled_reason = \"shadowed v2 formula\"\n\n[[steps]]\nid = \"commit\"\ntitle = \"Commit\"\n\n[steps.retry]\nmax_attempts = 2\n"
		r := run(t, content)
		if r.Status != StatusOK || !strings.Contains(strings.Join(r.Details, "\n"), "shadowed v2 formula") {
			t.Fatalf("the graph-declaration error must not outlive the marker: %v %q %v", r.Status, r.Message, r.Details)
		}
	})

	t.Run("an inherited reason does not excuse the formula's own unmet requirement", func(t *testing.T) {
		dir := t.TempDir()
		writeDoctorFormula(t, dir, "stale-parent", "\nformula = \"stale-parent\"\n\n[requires]\nformula_compiler = \">=1.0.0\"\ndisabled_reason = \"stale\"\n\n[[steps]]\nid = \"a\"\ntitle = \"A\"\n")
		writeDoctorFormula(t, dir, "own-unmet", "\nformula = \"own-unmet\"\nextends = [\"stale-parent\"]\n\n[requires]\nformula_compiler = \">=999.0.0\"\n")
		r := NewFormulaRequirementsCheck(&config.City{
			Daemon:        config.DaemonConfig{FormulaV2: boolPtr(true)},
			FormulaLayers: config.FormulaLayers{City: []string{dir}},
		}, t.TempDir()).Run(&CheckContext{})
		joined := strings.Join(r.Details, "\n")
		if r.Status != StatusError || strings.Contains(joined, "intentionally disabled city formula \"own-unmet\"") {
			t.Fatalf("own-unmet's own requirement must stay an error: %v %q %v", r.Status, r.Message, r.Details)
		}
	})

	// Codex r5 (#134): Resolve keeps only the first parent's disabled_reason,
	// so a stale marker on one parent must not excuse another parent's
	// unmarked, genuinely unmet requirement.
	t.Run("one parent's stale reason does not excuse another parent's unmet requirement", func(t *testing.T) {
		dir := t.TempDir()
		writeDoctorFormula(t, dir, "stale-parent", "\nformula = \"stale-parent\"\n\n[requires]\nformula_compiler = \">=1.0.0\"\ndisabled_reason = \"stale\"\n\n[[steps]]\nid = \"a\"\ntitle = \"A\"\n")
		writeDoctorFormula(t, dir, "unmet-parent", "\nformula = \"unmet-parent\"\n\n[requires]\nformula_compiler = \">=999.0.0\"\n\n[[steps]]\nid = \"b\"\ntitle = \"B\"\n")
		writeDoctorFormula(t, dir, "multi-child", "\nformula = \"multi-child\"\nextends = [\"stale-parent\", \"unmet-parent\"]\n")
		r := NewFormulaRequirementsCheck(&config.City{
			Daemon:        config.DaemonConfig{FormulaV2: boolPtr(true)},
			FormulaLayers: config.FormulaLayers{City: []string{dir}},
		}, t.TempDir()).Run(&CheckContext{})
		joined := strings.Join(r.Details, "\n")
		if r.Status != StatusError || strings.Contains(joined, "intentionally disabled city formula \"multi-child\"") ||
			!strings.Contains(joined, "error city formula \"multi-child\"") || !strings.Contains(joined, "compiler_requirement_unsatisfied") {
			t.Fatalf("multi-child's unmarked inherited requirement must stay an error: %v %q %v", r.Status, r.Message, r.Details)
		}
	})

	t.Run("a child inherits the reason of the parent that disables it", func(t *testing.T) {
		dir := t.TempDir()
		writeDoctorFormula(t, dir, "stale-parent", "\nformula = \"stale-parent\"\n\n[requires]\nformula_compiler = \">=1.0.0\"\ndisabled_reason = \"stale\"\n\n[[steps]]\nid = \"a\"\ntitle = \"A\"\n")
		writeDoctorFormula(t, dir, "marked-parent", "\nformula = \"marked-parent\"\n\n[requires]\nformula_compiler = \">=999.0.0\"\ndisabled_reason = \"parked on purpose\"\n\n[[steps]]\nid = \"b\"\ntitle = \"B\"\n")
		writeDoctorFormula(t, dir, "multi-child", "\nformula = \"multi-child\"\nextends = [\"stale-parent\", \"marked-parent\"]\n")
		r := NewFormulaRequirementsCheck(&config.City{
			Daemon:        config.DaemonConfig{FormulaV2: boolPtr(true)},
			FormulaLayers: config.FormulaLayers{City: []string{dir}},
		}, t.TempDir()).Run(&CheckContext{})
		var line string
		for _, d := range r.Details {
			if strings.HasPrefix(d, "intentionally disabled city formula \"multi-child\"") {
				line = d
			}
		}
		if r.Status != StatusWarning || !strings.HasSuffix(line, ": parked on purpose") {
			t.Fatalf("multi-child must read disabled by marked-parent's reason, not stale-parent's: %v %q %v", r.Status, r.Message, r.Details)
		}
	})

	t.Run("a reason never excuses an INVALID requirement", func(t *testing.T) {
		content := "\nformula = \"mol-shadowed\"\n\n[requires]\nformula_compiler = \"not-a-version\"\ndisabled_reason = \"x\"\n\n[[steps]]\nid = \"commit\"\ntitle = \"Commit\"\n"
		if r := run(t, content); r.Status != StatusError {
			t.Fatalf("Status = %v, want Error; details:\n%s", r.Status, strings.Join(r.Details, "\n"))
		}
	})
}

// Codex r5 (#134): the composition probe must compile under the city's
// daemon.formula_v2 rollout gate, not the process-wide compile setting that
// earlier checks may have left behind (or none, when only this check runs).
func TestFormulaRequirementsCheckCompositionProbeHonorsConfiguredV2(t *testing.T) {
	write := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		writeDoctorFormula(t, dir, "mol-v2-composed", "\nformula = \"mol-v2-composed\"\n\n[requires]\nformula_compiler = \">=1.0.0\"\ndisabled_reason = \"needs v2 through its expansion\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n\n[compose]\n[[compose.expand]]\ntarget = \"work\"\nwith = \"v2-expansion\"\n")
		writeDoctorFormula(t, dir, "v2-expansion", "\nformula = \"v2-expansion\"\ntype = \"expansion\"\n\n[requires]\nformula_compiler = \">=2.0.0\"\n\n[[template]]\nid = \"{target}.child\"\ntitle = \"Child\"\n")
		return dir
	}
	check := func(dir string, cityV2 bool) *FormulaRequirementsCheck {
		return NewFormulaRequirementsCheck(&config.City{
			Daemon:        config.DaemonConfig{FormulaV2: boolPtr(cityV2)},
			FormulaLayers: config.FormulaLayers{City: []string{dir}},
		}, t.TempDir())
	}
	notDispatchable := func(t *testing.T, r *CheckResult) {
		t.Helper()
		joined := strings.Join(r.Details, "\n")
		if strings.Contains(joined, "DISPATCHABLE") || !strings.Contains(joined, "by a composed requirement: needs v2 through its expansion") {
			t.Fatalf("under formula_v2 = false the v2 expansion disables the formula: %v %q %v", r.Status, r.Message, r.Details)
		}
	}

	t.Run("formula_v2 = false while the process compiles v2: not DISPATCHABLE", func(t *testing.T) {
		dir := write(t)
		// Precondition: the process-wide compile path accepts v2 here, so
		// only the city's gate can make the probe refuse the expansion.
		if _, err := formula.CompileWithoutRuntimeVarValidation(context.Background(), "mol-v2-composed", []string{dir}, nil); err != nil {
			t.Fatalf("precondition: the process-wide compile must accept v2 here: %v", err)
		}
		notDispatchable(t, check(dir, false).Run(&CheckContext{}))
	})

	t.Run("the rollout gate decides: gate false over config true is not DISPATCHABLE", func(t *testing.T) {
		c := check(write(t), true)
		flags := rollout.ForTest(rollout.WithFormulaV2(false))
		c.rolloutFlags = &flags
		notDispatchable(t, c.Run(&CheckContext{}))
	})

	t.Run("the rollout gate decides: gate true over config false is DISPATCHABLE", func(t *testing.T) {
		c := check(write(t), false)
		flags := rollout.ForTest(rollout.WithFormulaV2(true))
		c.rolloutFlags = &flags
		r := c.Run(&CheckContext{})
		if r.Status != StatusWarning || !strings.Contains(strings.Join(r.Details, "\n"), "DISPATCHABLE") {
			t.Fatalf("under formula_v2 = true the formula compiles, so its marker is stale: %v %q %v", r.Status, r.Message, r.Details)
		}
	})
}

// Codex r6 (#134): discovery and compilation key a formula by its FILENAME,
// which may differ from its formula field. The probe and the extends walk must
// evaluate the discovered file, not whatever the field's value resolves to.
func TestFormulaRequirementsCheckEvaluatesTheDiscoveredFileByResolverKey(t *testing.T) {
	const composed = "\nformula = \"mol-field\"\n\n[requires]\nformula_compiler = \">=1.0.0\"\ndisabled_reason = \"needs v2 through its expansion\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n\n[compose]\n[[compose.expand]]\ntarget = \"work\"\nwith = \"v2-expansion\"\n"
	const expansion = "\nformula = \"v2-expansion\"\ntype = \"expansion\"\n\n[requires]\nformula_compiler = \">=2.0.0\"\n\n[[template]]\nid = \"{target}.child\"\ntitle = \"Child\"\n"
	run := func(t *testing.T, dir string, cityV2 bool) *CheckResult {
		t.Helper()
		return NewFormulaRequirementsCheck(&config.City{
			Daemon:        config.DaemonConfig{FormulaV2: boolPtr(cityV2)},
			FormulaLayers: config.FormulaLayers{City: []string{dir}},
		}, t.TempDir()).Run(&CheckContext{})
	}
	wantComposedDisable := func(t *testing.T, r *CheckResult, dir string) {
		t.Helper()
		file := filepath.Join(dir, "mol-file.toml")
		want := fmt.Sprintf("intentionally disabled city formula %q (%s), by a composed requirement: needs v2 through its expansion", "mol-field", file)
		var about []string
		for _, d := range r.Details {
			if strings.Contains(d, "("+file+")") {
				about = append(about, d)
			}
		}
		if !slices.Equal(about, []string{want}) {
			t.Fatalf("the probe must compile mol-file.toml by its resolver key:\nwant only %q\ngot %q", want, about)
		}
	}

	t.Run("no file answers to the formula field", func(t *testing.T) {
		dir := t.TempDir()
		writeDoctorFormula(t, dir, "mol-file", composed)
		writeDoctorFormula(t, dir, "v2-expansion", expansion)
		wantComposedDisable(t, run(t, dir, false), dir)
	})

	t.Run("a different file answers to the formula field", func(t *testing.T) {
		dir := t.TempDir()
		writeDoctorFormula(t, dir, "mol-file", composed)
		writeDoctorFormula(t, dir, "v2-expansion", expansion)
		writeDoctorFormula(t, dir, "mol-field", "\nformula = \"mol-field\"\n\n[requires]\nformula_compiler = \">=1.0.0\"\n\n[[steps]]\nid = \"decoy\"\ntitle = \"Decoy\"\n")
		wantComposedDisable(t, run(t, dir, false), dir)
	})

	t.Run("two parents sharing a formula field are each checked", func(t *testing.T) {
		dir := t.TempDir()
		writeDoctorFormula(t, dir, "p-marked", "\nformula = \"shared-parent\"\n\n[requires]\nformula_compiler = \">=999.0.0\"\ndisabled_reason = \"parked on purpose\"\n\n[[steps]]\nid = \"a\"\ntitle = \"A\"\n")
		writeDoctorFormula(t, dir, "p-unmarked", "\nformula = \"shared-parent\"\n\n[requires]\nformula_compiler = \">=999.0.0\"\n\n[[steps]]\nid = \"b\"\ntitle = \"B\"\n")
		writeDoctorFormula(t, dir, "multi-child", "\nformula = \"multi-child\"\nextends = [\"p-marked\", \"p-unmarked\"]\n")
		r := run(t, dir, true)
		joined := strings.Join(r.Details, "\n")
		if r.Status != StatusError || strings.Contains(joined, "intentionally disabled city formula \"multi-child\"") ||
			!strings.Contains(joined, "error city formula \"multi-child\"") {
			t.Fatalf("p-unmarked's unmarked requirement must keep multi-child an error: %v %q %v", r.Status, r.Message, r.Details)
		}
	})
}
