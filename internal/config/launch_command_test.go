package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildProviderLaunchCommandAddsDefaultsAndSettings(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := BuiltinProviders()["claude"]
	rp := specToResolved("claude", &spec)

	got, err := BuildProviderLaunchCommand(dir, rp, nil, "")
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommand: %v", err)
	}

	wantCommand := fmt.Sprintf("claude --dangerously-skip-permissions --effort max --settings %q", filepath.Join(dir, ".gc", "settings.json"))
	if got.Command != wantCommand {
		t.Fatalf("Command = %q, want %q", got.Command, wantCommand)
	}
	if got.SettingsPath != filepath.Join(dir, ".gc", "settings.json") {
		t.Fatalf("SettingsPath = %q, want %q", got.SettingsPath, filepath.Join(dir, ".gc", "settings.json"))
	}
	if got.SettingsRel != filepath.Join(".gc", "settings.json") {
		t.Fatalf("SettingsRel = %q, want %q", got.SettingsRel, filepath.Join(".gc", "settings.json"))
	}
}

func TestBuildProviderLaunchCommandAppliesOptionOverrides(t *testing.T) {
	spec := BuiltinProviders()["claude"]
	rp := specToResolved("claude", &spec)

	got, err := BuildProviderLaunchCommand("", rp, map[string]string{
		"permission_mode": "plan",
		"effort":          "low",
	}, "")
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommand: %v", err)
	}

	want := "claude --permission-mode plan --effort low"
	if got.Command != want {
		t.Fatalf("Command = %q, want %q", got.Command, want)
	}
	if got.SettingsPath != "" || got.SettingsRel != "" {
		t.Fatalf("unexpected settings source: %#v", got)
	}
}

func TestBuildProviderLaunchCommandIgnoresInitialMessageOverride(t *testing.T) {
	spec := BuiltinProviders()["claude"]
	rp := specToResolved("claude", &spec)

	got, err := BuildProviderLaunchCommand("", rp, map[string]string{
		"initial_message": "hello",
		"effort":          "low",
	}, "")
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommand: %v", err)
	}

	want := "claude --dangerously-skip-permissions --effort low"
	if got.Command != want {
		t.Fatalf("Command = %q, want %q", got.Command, want)
	}
}

func TestProviderOptionMapCapacity(t *testing.T) {
	tests := []struct {
		name         string
		defaultsLen  int
		overridesLen int
		want         int
	}{
		{
			name:         "adds safe override capacity",
			defaultsLen:  2,
			overridesLen: 3,
			want:         5,
		},
		{
			name:         "keeps defaults when overrides are empty",
			defaultsLen:  4,
			overridesLen: 0,
			want:         4,
		},
		{
			name:         "uses exact boundary when addition is safe",
			defaultsLen:  math.MaxInt - 1,
			overridesLen: 1,
			want:         math.MaxInt,
		},
		{
			name:         "skips override capacity when addition would overflow",
			defaultsLen:  math.MaxInt,
			overridesLen: 1,
			want:         math.MaxInt,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := providerOptionMapCapacity(tt.defaultsLen, tt.overridesLen); got != tt.want {
				t.Fatalf("providerOptionMapCapacity(%d, %d) = %d, want %d", tt.defaultsLen, tt.overridesLen, got, tt.want)
			}
		})
	}
}

func TestBuildProviderLaunchCommandUsesACPCommand(t *testing.T) {
	rp := &ResolvedProvider{
		Command: "custom-opencode",
		ACPArgs: []string{"acp"},
	}

	t.Run("acp transport uses ACPCommandString", func(t *testing.T) {
		got, err := BuildProviderLaunchCommand("", rp, nil, "acp")
		if err != nil {
			t.Fatalf("BuildProviderLaunchCommand: %v", err)
		}
		want := "custom-opencode acp"
		if got.Command != want {
			t.Fatalf("Command = %q, want %q", got.Command, want)
		}
	})

	t.Run("default transport uses CommandString", func(t *testing.T) {
		got, err := BuildProviderLaunchCommand("", rp, nil, "")
		if err != nil {
			t.Fatalf("BuildProviderLaunchCommand: %v", err)
		}
		want := "custom-opencode"
		if got.Command != want {
			t.Fatalf("Command = %q, want %q", got.Command, want)
		}
	})

	t.Run("tmux transport uses CommandString", func(t *testing.T) {
		got, err := BuildProviderLaunchCommand("", rp, nil, "tmux")
		if err != nil {
			t.Fatalf("BuildProviderLaunchCommand: %v", err)
		}
		want := "custom-opencode"
		if got.Command != want {
			t.Fatalf("Command = %q, want %q", got.Command, want)
		}
	})

	t.Run("unknown transport errors", func(t *testing.T) {
		_, err := BuildProviderLaunchCommand("", rp, nil, "stdio")
		if err == nil {
			t.Fatal("BuildProviderLaunchCommand() error = nil, want unknown transport error")
		}
	})
}

func TestBuildProviderLaunchCommandWithoutOptionsSkipsDefaultsButKeepsSettings(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := BuiltinProviders()["claude"]
	rp := specToResolved("claude", &spec)

	got, err := BuildProviderLaunchCommandWithoutOptions(dir, rp, "")
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommandWithoutOptions: %v", err)
	}

	wantCommand := fmt.Sprintf("claude --settings %q", filepath.Join(dir, ".gc", "settings.json"))
	if got.Command != wantCommand {
		t.Fatalf("Command = %q, want %q", got.Command, wantCommand)
	}
	if got.SettingsPath != filepath.Join(dir, ".gc", "settings.json") {
		t.Fatalf("SettingsPath = %q, want %q", got.SettingsPath, filepath.Join(dir, ".gc", "settings.json"))
	}
	if got.SettingsRel != filepath.Join(".gc", "settings.json") {
		t.Fatalf("SettingsRel = %q, want %q", got.SettingsRel, filepath.Join(".gc", "settings.json"))
	}
}

func TestBuildProviderLaunchCommandWithoutOptionsUsesBuiltinAncestorForSettings(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(runtimeDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rp := &ResolvedProvider{
		Name:            "claude-max",
		BuiltinAncestor: "claude",
		Command:         "aimux",
		Args:            []string{"run", "claude", "--"},
	}

	got, err := BuildProviderLaunchCommandWithoutOptions(dir, rp, "")
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommandWithoutOptions: %v", err)
	}

	if want := fmt.Sprintf("--settings %q", settingsPath); !strings.Contains(got.Command, want) {
		t.Fatalf("Command = %q, want settings arg %q", got.Command, want)
	}
	if count := strings.Count(got.Command, "--settings"); count != 1 {
		t.Fatalf("Command has %d --settings flags, want 1: %q", count, got.Command)
	}
	if got.SettingsPath != settingsPath {
		t.Fatalf("SettingsPath = %q, want %q", got.SettingsPath, settingsPath)
	}
}

func TestBuildProviderLaunchCommandWithoutOptionsIgnoresDeprecatedKindForSettings(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rp := &ResolvedProvider{
		Name:    "custom-provider",
		Kind:    "claude",
		Command: "custom-provider",
	}

	got, err := BuildProviderLaunchCommandWithoutOptions(dir, rp, "")
	if err != nil {
		t.Fatalf("BuildProviderLaunchCommandWithoutOptions: %v", err)
	}
	if strings.Contains(got.Command, "--settings") {
		t.Fatalf("Command = %q, want no settings from deprecated Kind fallback", got.Command)
	}
	if got.SettingsPath != "" || got.SettingsRel != "" {
		t.Fatalf("unexpected settings source from deprecated Kind fallback: %#v", got)
	}
}

func TestAppendClaudeSessionNameGates(t *testing.T) {
	base := &ResolvedProvider{BuiltinAncestor: "claude", SessionDisplayName: "qcore/oversight.project-lead"}

	if got := appendClaudeSessionName("claude --effort max", base, ""); got != "claude --effort max --name qcore/oversight.project-lead" {
		t.Fatalf("tmux claude append = %q", got)
	}
	// Identity needing quoting.
	quoted := &ResolvedProvider{BuiltinAncestor: "claude", SessionDisplayName: "a b"}
	if got := appendClaudeSessionName("claude", quoted, "tmux"); got != `claude --name "a b"` && got != "claude --name 'a b'" {
		t.Fatalf("quoted append = %q", got)
	}
	// ACP transport never gets the flag.
	if got := appendClaudeSessionName("claude-code-acp", base, SessionTransportACP); got != "claude-code-acp" {
		t.Fatalf("acp append = %q", got)
	}
	// Non-claude family untouched.
	omp := &ResolvedProvider{BuiltinAncestor: "omp", SessionDisplayName: "deacon"}
	if got := appendClaudeSessionName("omp run", omp, ""); got != "omp run" {
		t.Fatalf("non-claude append = %q", got)
	}
	// Explicit --name in the command wins.
	if got := appendClaudeSessionName("claude --name custom", base, ""); got != "claude --name custom" {
		t.Fatalf("explicit --name overridden: %q", got)
	}
	// No identity -> untouched.
	anon := &ResolvedProvider{BuiltinAncestor: "claude"}
	if got := appendClaudeSessionName("claude", anon, ""); got != "claude" {
		t.Fatalf("anonymous append = %q", got)
	}
}

func TestAppendClaudeSessionNameIdentity(t *testing.T) {
	base := &ResolvedProvider{BuiltinAncestor: "claude", SessionDisplayName: "qcore/cherub-law.mallory"}

	// Explicit identity wins over the template-qualified SessionDisplayName —
	// the addressable form is what a picker row must show (ga-n0rvsk req 1).
	if got := AppendClaudeSessionNameIdentity("claude", base, "", "qcore/mallory"); got != "claude --name qcore/mallory" {
		t.Fatalf("identity append = %q", got)
	}
	// Empty identity -> untouched (callers may pass a missing metadata field).
	if got := AppendClaudeSessionNameIdentity("claude", base, "", ""); got != "claude" {
		t.Fatalf("empty identity append = %q", got)
	}
	// Escape-hatch resolutions never stamp SessionDisplayName; a
	// caller-supplied identity must not bypass that gate — the user owns
	// that argv.
	escape := &ResolvedProvider{BuiltinAncestor: "claude"}
	if got := AppendClaudeSessionNameIdentity("claude --custom", escape, "", "woodhouse"); got != "claude --custom" {
		t.Fatalf("escape-hatch append = %q", got)
	}
	// Family and transport gates still hold with an explicit identity.
	omp := &ResolvedProvider{BuiltinAncestor: "omp", SessionDisplayName: "deacon"}
	if got := AppendClaudeSessionNameIdentity("omp run", omp, "", "deacon"); got != "omp run" {
		t.Fatalf("non-claude identity append = %q", got)
	}
	if got := AppendClaudeSessionNameIdentity("claude-code-acp", base, SessionTransportACP, "qcore/mallory"); got != "claude-code-acp" {
		t.Fatalf("acp identity append = %q", got)
	}
}

func TestBuildProviderResumeCommandCarriesSessionName(t *testing.T) {
	rp := &ResolvedProvider{
		BuiltinAncestor:    "claude",
		SessionDisplayName: "woodhouse",
		ResumeCommand:      "claude --resume {{session_key}}",
	}
	got, err := BuildProviderResumeCommand(rp, nil)
	if err != nil {
		t.Fatalf("BuildProviderResumeCommand: %v", err)
	}
	if got != "claude --resume {{session_key}} --name woodhouse" {
		t.Fatalf("resume command = %q", got)
	}
}

func TestResolveProviderStampsSessionDisplayName(t *testing.T) {
	agent := Agent{Name: "woodhouse", Provider: "claude"}
	ws := Workspace{}
	resolved, err := ResolveProvider(&agent, &ws, map[string]ProviderSpec{"claude": BuiltinProviders()["claude"]}, func(string) (string, error) { return "/usr/bin/claude", nil })
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if resolved.SessionDisplayName != agent.QualifiedName() {
		t.Fatalf("SessionDisplayName = %q, want %q", resolved.SessionDisplayName, agent.QualifiedName())
	}
}

func TestValidateAgentsWakeTransportEnum(t *testing.T) {
	ok := []Agent{
		{Name: "a"},
		{Name: "b", WakeTransport: WakeTransportSession},
		{Name: "c", WakeTransport: WakeTransportClaudeCloud},
	}
	if err := ValidateAgents(ok); err != nil {
		t.Fatalf("valid wake_transport values rejected: %v", err)
	}
	bad := []Agent{{Name: "d", WakeTransport: "carrier-pigeon"}}
	err := ValidateAgents(bad)
	if err == nil || !strings.Contains(err.Error(), "wake_transport") {
		t.Fatalf("invalid wake_transport accepted: %v", err)
	}
}
