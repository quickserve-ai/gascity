package tmux

import (
	"strings"
	"testing"
)

// Real Claude Code captures (ga-megheo; message text, task descriptions and
// paths replaced). A prompt whose UserPromptSubmit hooks are still running is a
// starting turn and must read busy, though its spinner carries no elapsed
// timer right after "(". A seat whose main turn ended while background agents
// run redraws their timers every second and must NOT read busy, or the queued
// nudge gate never delivers to it; nor must one with only a background shell.
func TestPaneContainsBusyIndicatorClaudeHookAndBackgroundShapes(t *testing.T) {
	rule := strings.Repeat("─", 80)
	for name, tc := range map[string]struct {
		lines []string
		want  bool
	}{
		"prompt_hooks_running": {[]string{
			"✶ Proofing… (running UserPromptSubmit hooks… 1/2 · 3s)", rule, "❯\u00a0", rule,
			"  user@host /tmp/work", "  [░░░░░░░░░░ 0%] Fable 5.1 [F:27%>Wed]",
			"  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents", "    ◉ xhigh · /effort", "    /rc",
		}, true},
		"single_hook_running": {[]string{"✻ Proofing… (running UserPromptSubmit hook… 4s)", "❯\u00a0"}, true},
		"prompt_hooks_running_with_echo": {[]string{
			"❯\u00a0probe one", "✽ Proofing… (running UserPromptSubmit hooks… 1/2 · 9s)", rule, "❯\u00a0", rule,
			"  user@host /tmp/work", "  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents", "    /rc",
		}, true},
		// The phrase anywhere but the live spinner line above the composer is
		// history or quoted text, not a running hook.
		"test_source_line": {[]string{
			`			"✶ Proofing… (running UserPromptSubmit hooks… 1/2 · 3s)", rule, "❯\u00a0", rule,`,
			rule, "❯\u00a0", rule,
		}, false},
		"assistant_prose": {[]string{
			"⏺ The pane showed ✶ Proofing… (running UserPromptSubmit hooks… 1/2 · 3s) while it waited.",
			rule, "❯\u00a0", rule,
		}, false},
		"tool_output": {[]string{
			"  ⎿  ✶ Proofing… (running UserPromptSubmit hooks… 1/2 · 3s)",
			"     ✶ Proofing… (running UserPromptSubmit hooks… 2/2 · 4s)", rule, "❯\u00a0", rule,
		}, false},
		"spinner_in_scrollback": {append(append([]string{"✶ Proofing… (running UserPromptSubmit hooks… 1/2 · 3s)"},
			strings.Split(strings.Repeat("  later output\n", 30), "\n")...), rule, "❯\u00a0", rule, "  user@host /tmp/work"), false},
		"idle_background_agents_ticking": {[]string{
			"✻ Waiting for 2 background agents to finish", rule, "❯\u00a0draft text", rule,
			"  >>  seat * ga-xxxxxx · opus-5.5·high · ctx ██▍·· 48% 482k/1M [acct]",
			"  5h █▉··· 37% · 7d ██▏·· 43% · F █▍··· 27%>Wed",
			"  ⏵⏵ bypass permissions on · 2 shells · ← for agents", "    /rc", "  ⏺ main",
			"  ◯ general-purpose  First task descripti… 1h 25m 34s · ↓ 228.0k tokens",
			"  ◯ general-purpose  Second task descript…    29m 42s · ↓ 236.4k tokens",
		}, false},
		"idle_background_shell": {[]string{
			"! sleep 120", "  ⎿  Command was manually backgrounded by user with ID: b0. Output is being written to:",
			"     /tmp/tasks/b0.output.", rule, "❯\u00a0", rule, "  user@host /tmp/work",
			"  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents", "    /rc",
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := paneContainsBusyIndicator(tc.lines); got != tc.want {
				t.Errorf("paneContainsBusyIndicator = %v, want %v", got, tc.want)
			}
		})
	}
}
