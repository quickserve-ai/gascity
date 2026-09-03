package config

import (
	"strings"
	"testing"
)

// resolveClaudeAgent resolves a claude-family agent through the canonical path.
func resolveClaudeAgent(t *testing.T, agent *Agent) *ResolvedProvider {
	t.Helper()
	rp, err := ResolveProvider(agent, nil, explicitBuiltins("claude"), lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider(%s): %v", agent.Name, err)
	}
	return rp
}

func defaultArgsString(rp *ResolvedProvider) string {
	return strings.Join(rp.ResolveDefaultArgs(), " ")
}

func TestSessionNameDefaultsToQualifiedIdentity(t *testing.T) {
	rp := resolveClaudeAgent(t, &Agent{Name: "project-lead", Provider: "claude", Dir: "qcore"})
	if got := rp.EffectiveDefaults[SessionNameOptionKey]; got != "qcore/project-lead" {
		t.Errorf("effective name = %q, want qcore/project-lead", got)
	}
	// The verbatim gc address must reach the command line: the picker row is
	// only addressable if the displayed string is the string you can paste
	// into gc session nudge / gc mail send.
	if args := defaultArgsString(rp); !strings.Contains(args, "--name qcore/project-lead") {
		t.Errorf("default args = %q, want --name qcore/project-lead", args)
	}
}

func TestSessionNamePreservesBindingQualifiedIdentity(t *testing.T) {
	// A binding-qualified identity keeps its dots and slashes verbatim.
	agent := &Agent{Name: "project-lead", Provider: "claude", Dir: "qcore", BindingName: "oversight"}
	rp := resolveClaudeAgent(t, agent)
	want := agent.QualifiedName()
	if want != "qcore/oversight.project-lead" {
		t.Fatalf("fixture QualifiedName = %q, want qcore/oversight.project-lead", want)
	}
	if got := rp.EffectiveDefaults[SessionNameOptionKey]; got != want {
		t.Errorf("effective name = %q, want %q", got, want)
	}
	if args := defaultArgsString(rp); !strings.Contains(args, "--name "+want) {
		t.Errorf("default args = %q, want --name %s", args, want)
	}
}

func TestSessionNameSkipsNonClaudeFamily(t *testing.T) {
	// omp/codex CLIs have no --name flag; an unknown flag would abort the
	// spawn, so non-claude families must stay untouched (multi-harness
	// safety is structural, not detected).
	rp, err := ResolveProvider(&Agent{Name: "deacon", Provider: "codex"}, nil,
		explicitBuiltins("codex"), lookPathOnly("codex"))
	if err != nil {
		t.Fatalf("ResolveProvider(codex): %v", err)
	}
	if _, ok := rp.EffectiveDefaults[SessionNameOptionKey]; ok {
		t.Errorf("codex carries a %q default; want none", SessionNameOptionKey)
	}
	if args := defaultArgsString(rp); strings.Contains(args, "--name") {
		t.Errorf("codex default args = %q, want no --name", args)
	}
}

func TestSessionNameRespectsExplicitOptionDefault(t *testing.T) {
	rp := resolveClaudeAgent(t, &Agent{
		Name:           "mayor",
		Provider:       "claude",
		OptionDefaults: map[string]string{SessionNameOptionKey: "town-crier"},
	})
	if got := rp.EffectiveDefaults[SessionNameOptionKey]; got != "town-crier" {
		t.Errorf("effective name = %q, want town-crier (explicit default wins)", got)
	}
	if args := defaultArgsString(rp); !strings.Contains(args, "--name town-crier") {
		t.Errorf("default args = %q, want --name town-crier", args)
	}
}

func TestSessionNameSkipsStartCommandEscapeHatch(t *testing.T) {
	// start_command owns the whole command line; decorating it would corrupt
	// a user's explicit invocation.
	rp, err := ResolveProvider(&Agent{Name: "custom", StartCommand: "my-agent --flag"}, nil,
		explicitBuiltins("claude"), lookPathOnly("claude"))
	if err != nil {
		t.Fatalf("ResolveProvider(start_command): %v", err)
	}
	if len(rp.OptionsSchema) != 0 {
		t.Fatalf("escape hatch carried a schema: %+v", rp.OptionsSchema)
	}
	if _, ok := rp.EffectiveDefaults[SessionNameOptionKey]; ok {
		t.Errorf("escape hatch carries a %q default; want none", SessionNameOptionKey)
	}
}

func TestEnsureSessionNameOptionDoesNotAliasSiblings(t *testing.T) {
	// Pool siblings share one resolved base. A per-instance overlay copies
	// the struct, so the ensure path must not write through the shared
	// OptionsSchema backing array onto the base or its siblings.
	base := resolveClaudeAgent(t, &Agent{Name: "dog", Provider: "claude"})
	baseName := base.EffectiveDefaults[SessionNameOptionKey]
	if baseName != "dog" {
		t.Fatalf("base name = %q, want dog", baseName)
	}

	instanceA := *base
	EnsureSessionNameOption(&instanceA, "dog-1")
	instanceB := *base
	EnsureSessionNameOption(&instanceB, "dog-2")

	if got := base.EffectiveDefaults[SessionNameOptionKey]; got != "dog" {
		t.Errorf("base name mutated to %q; want dog", got)
	}
	if got := instanceA.EffectiveDefaults[SessionNameOptionKey]; got != "dog-1" {
		t.Errorf("instance A name = %q, want dog-1", got)
	}
	if got := instanceB.EffectiveDefaults[SessionNameOptionKey]; got != "dog-2" {
		t.Errorf("instance B name = %q, want dog-2", got)
	}
	if args := defaultArgsString(&instanceA); !strings.Contains(args, "--name dog-1") {
		t.Errorf("instance A args = %q, want --name dog-1", args)
	}
	if args := defaultArgsString(base); !strings.Contains(args, "--name dog") ||
		strings.Contains(args, "dog-1") || strings.Contains(args, "dog-2") {
		t.Errorf("base args leaked an instance name: %q", args)
	}
}

func TestEnsureSessionNameOptionValueSurvivesValidation(t *testing.T) {
	// The options machinery is select-only: a value only resolves if the
	// schema carries a matching choice. A per-seat identity is synthesized,
	// so it must pass the same validation every other option value does.
	rp := resolveClaudeAgent(t, &Agent{Name: "project-lead", Provider: "claude", Dir: "qcore"})
	if err := ValidateOptionDefaults(rp.OptionsSchema, rp.EffectiveDefaults); err != nil {
		t.Fatalf("ValidateOptionDefaults: %v", err)
	}
	args, err := ResolveExplicitOptions(rp.OptionsSchema, rp.EffectiveDefaults)
	if err != nil {
		t.Fatalf("ResolveExplicitOptions: %v", err)
	}
	if !strings.Contains(strings.Join(args, " "), "--name qcore/project-lead") {
		t.Errorf("resolved args = %v, want --name qcore/project-lead", args)
	}
}
