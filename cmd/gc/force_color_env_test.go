package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestResolveTemplate_ForceColorForInteractiveTmuxAgents verifies the
// FORCE_COLOR=3 default injected for interactive tmux TUI sessions (fixes the
// residual half of ga-od2: Claude renders monochrome under the managed tmux
// server even with a clean color env). The default must be scoped to the tmux
// runtime and be overridable by config.
func TestResolveTemplate_ForceColorForInteractiveTmuxAgents(t *testing.T) {
	cases := []struct {
		name            string
		agent           config.Agent
		citySession     string // cfg.Session.Provider (runtime selector)
		wantForceColor  string // "" means "must be absent"
		wantAbsentForce bool
	}{
		{
			name:           "default tmux agent gets truecolor default",
			agent:          config.Agent{Name: "mayor", StartCommand: "true"},
			wantForceColor: "3",
		},
		{
			name:           "explicit agent FORCE_COLOR is preserved",
			agent:          config.Agent{Name: "mayor", StartCommand: "true", Env: map[string]string{"FORCE_COLOR": "1"}},
			wantForceColor: "1",
		},
		{
			name:            "NO_COLOR opts out of the forced default",
			agent:           config.Agent{Name: "mayor", StartCommand: "true", Env: map[string]string{"NO_COLOR": "1"}},
			wantAbsentForce: true,
		},
		{
			name:            "subprocess runtime is not force-colored",
			agent:           config.Agent{Name: "worker", StartCommand: "true"},
			citySession:     "subprocess",
			wantAbsentForce: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Session:   config.SessionConfig{Provider: tc.citySession},
				Agents:    []config.Agent{tc.agent},
			}
			store := beads.NewMemStore()
			var stderr bytes.Buffer
			bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)

			tp, err := resolveTemplate(bp, &cfg.Agents[0], tc.agent.Name, nil)
			if err != nil {
				t.Fatalf("resolveTemplate: %v\nstderr: %s", err, stderr.String())
			}

			got, present := tp.Env["FORCE_COLOR"]
			if tc.wantAbsentForce {
				if present {
					t.Fatalf("FORCE_COLOR = %q, want absent", got)
				}
				return
			}
			if got != tc.wantForceColor {
				t.Fatalf("FORCE_COLOR = %q (present=%v), want %q", got, present, tc.wantForceColor)
			}
		})
	}
}
