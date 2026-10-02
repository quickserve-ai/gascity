package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
)

// Transcript lines in the shape Claude Code writes them (ga-megheo). The
// queue-operation and attachment lines are the live shape seen while a
// background subagent or shell runs after the main turn has ended.
const (
	tlUserPrompt      = `{"type":"user","message":{"role":"user","content":"do the thing"}}`
	tlTaskNotify      = `{"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>b8b4rke14</task-id>\n</task-notification>"}}`
	tlAssistToolUse   = `{"type":"assistant","message":{"role":"assistant","model":"claude-opus","stop_reason":"tool_use"}}`
	tlAssistEndTurn   = `{"type":"assistant","message":{"role":"assistant","model":"claude-opus","stop_reason":"end_turn"}}`
	tlStopHookSummary = `{"type":"system","subtype":"stop_hook_summary"}`
	tlTurnDuration    = `{"type":"system","subtype":"turn_duration"}`
	tlQueueEnqueue    = `{"type":"queue-operation","operation":"enqueue","content":"<task-notification>"}`
	tlQueueRemove     = `{"type":"queue-operation","operation":"remove"}`
	tlAttachment      = `{"type":"attachment","attachment":{"type":"queued_command"}}`
	tlPRLink          = `{"type":"pr-link"}`
	tlBridgeSession   = `{"type":"bridge-session"}`
	tlFileHistory     = `{"type":"file-history-snapshot"}`
)

const transcriptIdleTestKey = "0b6f3c1e-7d2a-4c55-9e8f-2a1b3c4d5e6f"

// transcriptIdleTarget builds a dispatcher-shaped nudge target (through the
// real resolveNudgeTargetFromSessionInfo) whose Claude transcript, when lines
// is non-nil, holds lines. The transcript lives under the seat's
// CLAUDE_CONFIG_DIR/projects, the per-account layout live seats use.
func transcriptIdleTarget(t *testing.T, provider string, providers map[string]config.ProviderSpec, lines []string) nudgeTarget {
	t.Helper()
	return transcriptIdleTargetKeyed(t, provider, providers, lines, transcriptIdleTestKey)
}

func transcriptIdleTargetKeyed(t *testing.T, provider string, providers map[string]config.ProviderSpec, lines []string, sessionKey string) nudgeTarget {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // keep ~/.claude/projects out of the search
	workDir := t.TempDir()
	accountDir := t.TempDir()
	if providers == nil {
		providers = map[string]config.ProviderSpec{}
	}
	spec := providers[provider]
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	spec.Env["CLAUDE_CONFIG_DIR"] = accountDir
	providers[provider] = spec
	if lines != nil {
		dir := filepath.Join(accountDir, "projects", sessionlog.ProjectSlug(workDir))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		data := strings.Join(lines, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, transcriptIdleTestKey+".jsonl"), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		Workspace: config.Workspace{Provider: provider},
		Providers: providers,
		Agents:    []config.Agent{{Name: "worker", Provider: provider, Session: "tmux"}},
	}
	return resolveNudgeTargetFromSessionInfo(t.TempDir(), cfg, session.Info{
		ID:          "gc-megheo",
		AgentName:   "worker",
		Provider:    provider,
		WorkDir:     workDir,
		SessionName: "sess-worker",
		SessionKey:  sessionKey,
	})
}

func freshActivity() worker.LiveObservation {
	last := time.Now().Add(-1 * time.Second)
	return worker.LiveObservation{LastActivity: &last}
}

// (a) The deaf-seat case: pane output is fresh (a background task's timer
// ticks every second) but the transcript says the main turn has ended.
func TestPollerIdleClaudeTranscriptTurnEndedDelivers(t *testing.T) {
	for name, lines := range map[string][]string{
		"turn_duration": {tlUserPrompt, tlAssistEndTurn, tlStopHookSummary, tlTurnDuration},
		"end_turn":      {tlUserPrompt, tlAssistToolUse, tlAssistEndTurn},
	} {
		t.Run(name, func(t *testing.T) {
			target := transcriptIdleTarget(t, "claude", nil, lines)
			if !pollerSessionIdleEnough(target, nil, 3*time.Second, freshActivity()) {
				t.Fatal("pollerSessionIdleEnough = false, want true: claude transcript says the main turn ended")
			}
		})
	}
}

// (a') Live seats run custom providers based on builtin:claude.
func TestPollerIdleClaudeTranscriptCustomClaudeProvider(t *testing.T) {
	base := "builtin:claude"
	providers := map[string]config.ProviderSpec{"claude-fable-kumar": {Base: &base}}
	target := transcriptIdleTarget(t, "claude-fable-kumar", providers, []string{tlUserPrompt, tlAssistEndTurn, tlTurnDuration})
	if !pollerSessionIdleEnough(target, nil, 3*time.Second, freshActivity()) {
		t.Fatal("pollerSessionIdleEnough = false, want true for a custom provider based on builtin:claude")
	}
}

// (b) A running turn — a task-notification turn or a tool call — is not idle.
func TestPollerIdleClaudeTranscriptTurnRunningStaysBusy(t *testing.T) {
	for name, lines := range map[string][]string{
		"task_notification": {tlUserPrompt, tlAssistEndTurn, tlTurnDuration, tlQueueEnqueue, tlQueueRemove, tlTaskNotify},
		"tool_use":          {tlUserPrompt, tlAssistToolUse},
		"user_prompt":       {tlAssistEndTurn, tlTurnDuration, tlUserPrompt, tlAttachment},
	} {
		t.Run(name, func(t *testing.T) {
			target := transcriptIdleTarget(t, "claude", nil, lines)
			if pollerSessionIdleEnough(target, nil, 3*time.Second, freshActivity()) {
				t.Fatal("pollerSessionIdleEnough = true, want false while a turn is running")
			}
		})
	}
}

// (c) The live shape: the main turn ended, then only queue-operation,
// attachment and other non-turn lines were appended while a background task ran.
func TestPollerIdleClaudeTranscriptQueueOperationsAfterTurnEnd(t *testing.T) {
	lines := []string{tlUserPrompt, tlAssistEndTurn, tlStopHookSummary, tlTurnDuration,
		tlQueueEnqueue, tlQueueEnqueue, tlQueueRemove, tlQueueRemove, tlAttachment,
		tlPRLink, tlBridgeSession, tlFileHistory}
	target := transcriptIdleTarget(t, "claude", nil, lines)
	if !pollerSessionIdleEnough(target, nil, 3*time.Second, freshActivity()) {
		t.Fatal("pollerSessionIdleEnough = false, want true: only queue-operation/attachment lines follow turn_duration")
	}
}

// (d) No transcript, an unknown tail, or a torn last line keep today's
// behaviour: fresh pane activity means not idle.
func TestPollerIdleClaudeTranscriptUnknownKeepsTodaysBehaviour(t *testing.T) {
	for name, lines := range map[string][]string{
		"no_transcript": nil,
		"unknown_tail":  {tlQueueEnqueue, tlQueueRemove, tlAttachment},
		"torn_last":     {tlUserPrompt, tlAssistEndTurn, tlTurnDuration, `{"type":"user","message":{"role":"us`},
	} {
		t.Run(name, func(t *testing.T) {
			target := transcriptIdleTarget(t, "claude", nil, lines)
			if pollerSessionIdleEnough(target, nil, 3*time.Second, freshActivity()) {
				t.Fatal("pollerSessionIdleEnough = true, want false (today's behaviour) without a definite idle tail")
			}
		})
	}
	t.Run("no_session_key", func(t *testing.T) {
		target := transcriptIdleTargetKeyed(t, "claude", nil, []string{tlUserPrompt, tlAssistEndTurn, tlTurnDuration}, "")
		if pollerSessionIdleEnough(target, nil, 3*time.Second, freshActivity()) {
			t.Fatal("pollerSessionIdleEnough = true, want false when the transcript cannot be resolved")
		}
	})
}

// (e) Every other provider keeps today's behaviour even with an idle-looking
// Claude-format transcript where the claude lookup would find it.
func TestPollerIdleNonClaudeProviderIgnoresTranscript(t *testing.T) {
	target := transcriptIdleTarget(t, "codex", nil, []string{tlUserPrompt, tlAssistEndTurn, tlTurnDuration})
	if pollerSessionIdleEnough(target, nil, 3*time.Second, freshActivity()) {
		t.Fatal("pollerSessionIdleEnough = true, want false for a non-claude provider")
	}
}
