package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// undeclaredClaudeAccountSeats must mirror the runtime guard's merged-env
// view: a claude-family seat is flagged only when NO layer (workspace,
// provider, agent) declares CLAUDE_CONFIG_DIR (ga-xd3bjx item 4).
func basePtr(s string) *string { return &s }

func TestUndeclaredClaudeAccountSeats(t *testing.T) {
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{
			"claude-bare":     {Base: basePtr("builtin:claude")},
			"claude-declared": {Base: basePtr("builtin:claude"), Env: map[string]string{"CLAUDE_CONFIG_DIR": "/accounts/a"}},
			"codex":           {Base: basePtr("builtin:codex")},
		},
		Agents: []config.Agent{
			{Name: "undeclared-seat", Provider: "claude-bare"},
			{Name: "provider-declared-seat", Provider: "claude-declared"},
			{Name: "agent-declared-seat", Provider: "claude-bare", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/accounts/b"}},
			{Name: "other-family-seat", Provider: "codex"},
		},
	}
	got := undeclaredClaudeAccountSeats(cfg)
	if len(got) != 1 {
		t.Fatalf("undeclaredClaudeAccountSeats = %v, want exactly the one undeclared claude seat", got)
	}
	want := `agent "undeclared-seat" (provider "claude-bare")`
	if got[0] != want {
		t.Fatalf("undeclaredClaudeAccountSeats[0] = %q, want %q", got[0], want)
	}
}

// A workspace-level declaration clears every claude seat: this is the same
// layer the CLI create path now merges (ga-xd3bjx item 3), so the doctor
// check and the runtime guard agree on what "declared" means.
func TestUndeclaredClaudeAccountSeatsWorkspaceDeclared(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Env: map[string]string{"CLAUDE_CONFIG_DIR": "/accounts/city"}},
		Providers: map[string]config.ProviderSpec{
			"claude-bare": {Base: basePtr("builtin:claude")},
		},
		Agents: []config.Agent{
			{Name: "seat", Provider: "claude-bare"},
		},
	}
	if got := undeclaredClaudeAccountSeats(cfg); len(got) != 0 {
		t.Fatalf("undeclaredClaudeAccountSeats = %v, want none with a workspace-level declaration", got)
	}
}
