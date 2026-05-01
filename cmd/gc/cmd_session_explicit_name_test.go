package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestSessionExplicitNameForNewSession_SingletonReturnsCanonicalName verifies
// that gc session new on a singleton agent (max_active_sessions=1) derives the
// explicit session name from the session-name primitive ("/"→"--", "."→"__")
// instead of leaving it empty (which previously caused the manager to fall
// back to "s-<beadID>"). This matches the canonical names produced by the rig
// reconciler in cmd/gc/providers.go (agent.SessionNameFor + QualifiedName).
func TestSessionExplicitNameForNewSession_SingletonReturnsCanonicalName(t *testing.T) {
	tests := []struct {
		name       string
		agent      *config.Agent
		alias      string
		cityName   string
		sessionTpl string
		want       string
	}{
		{
			name: "rig-scoped singleton uses primitive -- separator",
			agent: &config.Agent{
				Dir:               "qcore",
				Name:              "syl",
				MaxActiveSessions: intPtr(1),
			},
			want: "qcore--syl",
		},
		{
			name: "binding-scoped singleton uses primitive __ separator",
			agent: &config.Agent{
				BindingName:       "gastown",
				Name:              "mayor",
				MaxActiveSessions: intPtr(1),
			},
			want: "gastown__mayor",
		},
		{
			name: "rig+binding singleton encodes both separators",
			agent: &config.Agent{
				Dir:               "town",
				BindingName:       "gastown",
				Name:              "polecat",
				MaxActiveSessions: intPtr(1),
			},
			want: "town--gastown__polecat",
		},
		{
			name: "bare singleton returns bare name unchanged",
			agent: &config.Agent{
				Name:              "helper",
				MaxActiveSessions: intPtr(1),
			},
			want: "helper",
		},
		{
			name: "explicit alias short-circuits to empty (manager handles aliased path)",
			agent: &config.Agent{
				Dir:               "qcore",
				Name:              "syl",
				MaxActiveSessions: intPtr(1),
			},
			alias: "syl-debug",
			want:  "",
		},
		{
			name:  "nil agent returns empty",
			agent: nil,
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sessionExplicitNameForNewSession(tt.agent, tt.alias, tt.cityName, tt.sessionTpl)
			if err != nil {
				t.Fatalf("sessionExplicitNameForNewSession: %v", err)
			}
			if got != tt.want {
				t.Errorf("sessionExplicitNameForNewSession(...) = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSessionExplicitNameForNewSession_MultiSessionGeneratesAdhoc verifies the
// pre-existing multi-session behavior is unchanged: when an agent supports
// multiple concurrent sessions and no alias is given, an "<base>-adhoc-<hex>"
// name is generated.
func TestSessionExplicitNameForNewSession_MultiSessionGeneratesAdhoc(t *testing.T) {
	agent := &config.Agent{Name: "worker"} // nil MaxActiveSessions → unlimited
	got, err := sessionExplicitNameForNewSession(agent, "", "", "")
	if err != nil {
		t.Fatalf("sessionExplicitNameForNewSession: %v", err)
	}
	if !strings.HasPrefix(got, "worker-adhoc-") {
		t.Errorf("multi-session name = %q, want prefix %q", got, "worker-adhoc-")
	}
}

// TestSessionExplicitNameForNewSession_HonorsCustomSessionTemplate verifies
// that when a city configures a non-default session_template, singleton names
// are rendered through it (matching agent.SessionNameFor's contract).
func TestSessionExplicitNameForNewSession_HonorsCustomSessionTemplate(t *testing.T) {
	agent := &config.Agent{
		Dir:               "qcore",
		Name:              "syl",
		MaxActiveSessions: intPtr(1),
	}
	// Per agent.SessionNameFor: when session_template is set, City and Agent
	// template variables are interpolated.
	got, err := sessionExplicitNameForNewSession(agent, "", "myCity", "{{.City}}-{{.Agent}}")
	if err != nil {
		t.Fatalf("sessionExplicitNameForNewSession: %v", err)
	}
	want := "myCity-qcore--syl"
	if got != want {
		t.Errorf("custom session_template name = %q, want %q", got, want)
	}
}
