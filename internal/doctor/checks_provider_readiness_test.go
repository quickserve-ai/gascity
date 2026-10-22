package doctor

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func providerReadinessTestCity() *config.City {
	return &config.City{
		Workspace: config.Workspace{Provider: "good"},
		Providers: map[string]config.ProviderSpec{
			"good": {Command: "goodtool", ReadyDelayMs: 5000},
			"bare": {Command: "baretool"},
		},
	}
}

func TestProviderReadinessFlagsSignallessComposition(t *testing.T) {
	cfg := providerReadinessTestCity()
	zero := 0
	cfg.Agents = []config.Agent{
		{Name: "healthy", Provider: "good"},
		// The config-level hole the builtin-table test cannot see: an
		// agent-level ready_delay_ms = 0 override is pointer-typed, parses
		// fine, and zeroes out the only signal its provider has.
		{Name: "zeroed", Provider: "good", ReadyDelayMs: &zero},
		{Name: "bare-rider", Provider: "bare"},
	}

	c := NewProviderReadinessCheck(cfg)
	c.lookPath = func(name string) (string, error) { return "/fake/" + name, nil }
	r := c.Run(nil)
	if r.Status != StatusError {
		t.Fatalf("status = %v, want StatusError; message=%q details=%v", r.Status, r.Message, r.Details)
	}
	joined := strings.Join(r.Details, "\n")
	for _, want := range []string{"zeroed", "bare-rider"} {
		if !strings.Contains(joined, want) {
			t.Errorf("details missing offender %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "healthy") {
		t.Errorf("agent with a readiness signal must not be flagged:\n%s", joined)
	}
	if !strings.Contains(r.Message, "state=creating") {
		t.Errorf("message should name the consequence, got %q", r.Message)
	}
}

func TestProviderReadinessSkipsSuspendedAndOneShot(t *testing.T) {
	cfg := providerReadinessTestCity()
	cfg.Agents = []config.Agent{
		{Name: "parked", Provider: "bare", Suspended: true},
		{Name: "bounded", Provider: "bare", Lifecycle: "one_shot"},
		// start_command = the operator's own lifecycle; no provider TUI
		// exists whose welcome screen could eat the first prompt.
		{Name: "dispatcher", StartCommand: "sh /loop.sh"},
	}

	c := NewProviderReadinessCheck(cfg)
	c.lookPath = func(name string) (string, error) { return "/fake/" + name, nil }
	r := c.Run(nil)
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK (suspended and one-shot agents are out of scope); message=%q details=%v", r.Status, r.Message, r.Details)
	}
}

func TestProviderReadinessOKOnNilConfig(t *testing.T) {
	r := NewProviderReadinessCheck(nil).Run(nil)
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK on nil config", r.Status)
	}
}
