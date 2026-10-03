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
		// One hook: Claude prints no ellipsis after "hook" (Claude Code 2.1.288
		// builds "running <event> hook" + "… n/m" only when more than one runs).
		"single_hook_running": {[]string{"✻ Proofing… (running UserPromptSubmit hook · 3s)", rule, "❯\u00a0", rule}, true},
		"single_post_tool_hook_mid_turn": {[]string{
			"✢ Transmogrifying… (running PostToolUse hook · 3m 37s · ↓ 12.1k tokens)", rule, "❯\u00a0", rule,
		}, true},
		"stop_hooks_mid_turn": {[]string{"✳ Musing… (running Stop hooks… 2/3 · 2m 28s · ↓ 2.3k tokens)", rule, "❯\u00a0", rule}, true},
		// 80-column panes wrap a long spinner onto a second row.
		"wrapped_spinner_tail": {[]string{
			"✢ Transmogrifying… (running PostToolUse hook · 3m 37s · ↓ 12.1k", "  tokens)", rule, "❯\u00a0", rule,
		}, true},
		"wrapped_spinner_status": {[]string{
			"✢ Transmogrifying…", "  (running PostToolUse hook · 3m 37s · ↓ 12.1k tokens)", rule, "❯\u00a0", rule,
		}, true},
		"prompt_hooks_running_with_echo": {[]string{
			"❯\u00a0probe one", "✽ Proofing… (running UserPromptSubmit hooks… 1/2 · 9s)", rule, "❯\u00a0", rule,
			"  user@host /tmp/work", "  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents", "    /rc",
		}, true},
		// The prompt echo is drawn before the new composer: the spinner below it
		// is live.
		"echo_spinner_no_composer": {[]string{
			"❯\u00a0probe one", "✽ Proofing… (running UserPromptSubmit hooks… 1/2 · 1s)",
			"  user@host /tmp/work", "  -- INSERT -- ⏵⏵ auto mode on · ← for agents",
		}, true},
		"spinner_hint_lines_composer": {[]string{
			"✶ Proofing… (running UserPromptSubmit hooks… 1/2 · 3s)",
			"  ⎿  Tip: one", "     two", "     three", "     four", "     five",
			rule, "❯\u00a0", rule, "  user@host /tmp/work",
		}, true},
		// An idle seat's background-agent rows below the composer do not push
		// a live spinner out of the window.
		"spinner_above_many_background_agents": {append([]string{
			"✶ Proofing… (running UserPromptSubmit hooks… 1/2 · 3s)", rule, "❯\u00a0", rule,
			"  >>  seat * ga-xxxxxx · opus-5.5·high", "  5h █▉··· 37%", "  ⏵⏵ bypass permissions on · 2 shells · ← for agents",
			"    /rc", "  ⏺ main"},
			strings.Split(strings.TrimSuffix(strings.Repeat("  ◯ general-purpose  Task descripti…    29m 42s · ↓ 236.4k tokens\n", 8), "\n"), "\n")...), true},
		// The plain turn-start spinner right after the hook phase: a bare head,
		// or a timer with no separator (live shapes, 8 seats).
		"bare_head_over_tip_row": {[]string{
			"✢ Deliberating…", "  ⎿  Tip: a tip row", rule, "❯\u00a0", rule, "  user@host /tmp/work",
		}, true},
		// Live woodhouse layout: 14 non-empty rows up, 4 of them indented
		// background-agent rows, which do not use window budget.
		"bare_head_above_background_agents": {[]string{
			"✽ Onioning…", "  ⎿  Tip: a tip row", rule, "❯\u00a0", rule,
			"  >>  seat * ga-xxxxxx · opus-5.5·high", "  5h █▉··· 37% · 7d ██▏·· 43%",
			"  ⏵⏵ bypass permissions on · 2 shells · ← for agents", "    ◉ high · /effort", "    /rc", "  ⏺ main",
			"  ◯ general-purpose  First task descripti… 1h 25m 34s · ↓ 228.0k tokens",
			"  ◯ general-purpose  Second task descript…    29m 42s · ↓ 236.4k tokens",
			"  ◯ general-purpose  Third task descripti…     4m 02s · ↓ 12.4k tokens",
		}, true},
		"bare_head_over_composer_rule": {[]string{"✢ Deliberating…", rule, "❯\u00a0", rule}, true},
		"timer_only_seconds":           {[]string{"✻ Hyperspacing… (22s)", rule, "❯\u00a0", rule}, true},
		"timer_only_minutes":           {[]string{"· Deliberating… (1m 16s)", rule, "❯\u00a0", rule}, true},
		// Idle chrome has no ellipsis after the verb.
		"done_worked_for":        {[]string{"✻ Worked for 3m 38s", rule, "❯\u00a0", rule}, false},
		"waiting_for_background": {[]string{"✻ Waiting for 2 background agents to finish", rule, "❯\u00a0", rule}, false},
		"done_brewed_for":        {[]string{"✻ Brewed for 48s · done 11:19 PM · 1 shell still running", rule, "❯\u00a0", rule}, false},
		"bare_head_in_scrollback": {append(append([]string{"✢ Deliberating…"},
			strings.Split(strings.Repeat("  later output\n", 30), "\n")...), rule, "❯\u00a0", rule), false},
		// Later assistant output means the spinner above it is stale.
		"spinner_then_assistant_output": {[]string{
			"✶ Proofing… (running UserPromptSubmit hooks… 1/2 · 3s)", "⏺ Done.", rule, "❯\u00a0", rule,
		}, false},
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
