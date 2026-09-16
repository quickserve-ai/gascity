package config

import "testing"

func TestAccountFamilyPrecedence(t *testing.T) {
	tests := []struct {
		name string
		rp   *ResolvedProvider
		want string
	}{
		{"nil provider", nil, ""},
		{"builtin ancestor wins", &ResolvedProvider{Name: "claude-custom", Kind: "codex", BuiltinAncestor: "claude"}, "claude"},
		{"kind fallback during transition", &ResolvedProvider{Name: "claude-legacy", Kind: "claude"}, "claude"},
		{"raw builtin falls back to name", &ResolvedProvider{Name: "claude"}, "claude"},
		{"custom provider with no lineage", &ResolvedProvider{Name: "my-harness"}, "my-harness"},
		{"whitespace ancestor ignored", &ResolvedProvider{Name: "claude", BuiltinAncestor: "  "}, "claude"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rp.AccountFamily(); got != tt.want {
				t.Fatalf("AccountFamily() = %q, want %q", got, tt.want)
			}
		})
	}
}
