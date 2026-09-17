package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestResolveTemplateLaunchesShortClaudeModelsAtOneMillion pins the carried
// claude model mapping (CARRY.md, "Claude model enum") on the launch path, above
// upstream's open model option (#5658). Under that option a DECLARED choice wins
// and only an undeclared value reaches the CLI verbatim, so these short values
// launch at the 1M window only while the builtin profile keeps declaring them:
// fleet cities pin model = "opus", and a bare "--model opus" (or upstream's
// "claude-opus-4-8") would put every such seat on the 200k tier without a
// warning. The superseded pins must stay declared for the same reason: dropped,
// "opus-4-8" and "fable-5-200k" would reach the CLI verbatim as ids it does not
// know.
func TestResolveTemplateLaunchesShortClaudeModelsAtOneMillion(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  string
	}{
		{model: "opus", want: "opus[1m]"},
		{model: "opus-5", want: "claude-opus-5[1m]"},
		{model: "fable-5", want: "claude-fable-5[1m]"},
		{model: "opus-4-8", want: "claude-opus-4-8"},
		{model: "fable-5-200k", want: "claude-fable-5"},
		// An explicit canonical pin is emitted verbatim.
		{model: "claude-opus-5[1m]", want: "claude-opus-5[1m]"},
		// An undeclared id reaches the CLI verbatim through the open option.
		{model: "opus[1m]", want: "opus[1m]"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			cityPath := t.TempDir()
			var stderr bytes.Buffer
			params := &agentBuildParams{
				fs:              fsys.OSFS{},
				cityName:        "fleet-city",
				cityPath:        cityPath,
				workspace:       &config.Workspace{Name: "fleet-city"},
				providers:       config.BuiltinProviders(),
				lookPath:        func(string) (string, error) { return "/usr/bin/claude", nil },
				beaconTime:      testBeaconTime,
				sessionTemplate: "",
				beadNames:       make(map[string]string),
				stderr:          &stderr,
			}
			agent := &config.Agent{
				Name:     "mayor",
				Provider: "claude",
				// Declared so the ambient-account guard (ga-ai7gz2) holds whatever
				// the runner's own environment carries.
				Env:            map[string]string{"CLAUDE_CONFIG_DIR": cityPath},
				OptionDefaults: map[string]string{"model": tc.model},
			}

			tp, err := resolveTemplate(params, agent, "fleet-city/mayor", nil)
			if err != nil {
				t.Fatalf("resolveTemplate: %v", err)
			}
			tokens := shellquote.Split(tp.Command)
			var models []string
			for i, tok := range tokens {
				if tok == "--model" && i+1 < len(tokens) {
					models = append(models, tokens[i+1])
				}
			}
			if len(models) != 1 || models[0] != tc.want {
				t.Fatalf("model=%q launched with --model %v, want exactly [%s]; command: %s", tc.model, models, tc.want, tp.Command)
			}
			if strings.Contains(stderr.String(), "WARNING:") {
				t.Fatalf("model=%q warned: %s", tc.model, stderr.String())
			}
		})
	}
}
