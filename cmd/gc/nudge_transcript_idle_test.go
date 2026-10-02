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
// bookkeeping lines are the live shape seen while a background subagent or
// shell runs after the main turn has ended.
const (
	tlUserPrompt      = `{"type":"user","message":{"role":"user","content":"do the thing"}}`
	tlTaskNotify      = `{"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>b8b4rke14</task-id>\n</task-notification>"}}`
	tlInterrupt       = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`
	tlQuotesInterrupt = `{"type":"user","message":{"role":"user","content":"why did the log say [Request interrupted by user]?"}}`
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

var tlIdle = []string{tlUserPrompt, tlAssistEndTurn, tlStopHookSummary, tlTurnDuration}

const transcriptIdleTestKey = "0b6f3c1e-7d2a-4c55-9e8f-2a1b3c4d5e6f"

// transcriptFixture is a seat whose provider declares its own
// CLAUDE_CONFIG_DIR (the per-account layout live seats use) under a private
// HOME, so both the account root and the default ~/.claude/projects root are
// test-owned.
type transcriptFixture struct {
	t                         *testing.T
	home, workDir, accountDir string
	cfg                       *config.City
	info                      session.Info
}

func newTranscriptFixture(t *testing.T, provider string, providers map[string]config.ProviderSpec) *transcriptFixture {
	t.Helper()
	f := &transcriptFixture{t: t, home: t.TempDir(), workDir: t.TempDir(), accountDir: t.TempDir()}
	t.Setenv("HOME", f.home)
	if providers == nil {
		providers = map[string]config.ProviderSpec{}
	}
	spec := providers[provider]
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	spec.Env["CLAUDE_CONFIG_DIR"] = f.accountDir
	providers[provider] = spec
	f.cfg = &config.City{
		Workspace: config.Workspace{Provider: provider},
		Providers: providers,
		Agents:    []config.Agent{{Name: "worker", Provider: provider, Session: "tmux"}},
	}
	f.info = session.Info{ID: "gc-megheo", AgentName: "worker", Provider: provider,
		WorkDir: f.workDir, SessionName: "sess-worker", SessionKey: transcriptIdleTestKey}
	return f
}

func (f *transcriptFixture) accountRoot() string { return filepath.Join(f.accountDir, "projects") }
func (f *transcriptFixture) defaultRoot() string { return filepath.Join(f.home, ".claude", "projects") }

// write puts lines in the seat's keyed transcript under the projects root.
func (f *transcriptFixture) write(root string, lines []string) {
	f.t.Helper()
	dir := filepath.Join(root, sessionlog.ProjectSlug(f.workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	data := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, transcriptIdleTestKey+".jsonl"), []byte(data), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// idle runs the real gate on the dispatcher-shaped target with pane activity
// 1 s old (fresh, under the 3 s quiescence).
func (f *transcriptFixture) idle() bool {
	target := resolveNudgeTargetFromSessionInfo(f.t.TempDir(), f.cfg, f.info)
	last := time.Now().Add(-1 * time.Second)
	return pollerSessionIdleEnough(target, nil, 3*time.Second, worker.LiveObservation{LastActivity: &last})
}

// transcriptIdle is the common case: a claude-family seat whose account
// root holds lines (nil = no transcript).
func transcriptIdle(t *testing.T, provider string, providers map[string]config.ProviderSpec, lines []string) bool {
	t.Helper()
	f := newTranscriptFixture(t, provider, providers)
	if lines != nil {
		f.write(f.accountRoot(), lines)
	}
	return f.idle()
}

// (a) The deaf-seat case: pane output is fresh (a background task's timer
// ticks every second) but the transcript says the main turn has ended.
func TestPollerIdleClaudeTranscriptTurnEndedDelivers(t *testing.T) {
	if !transcriptIdle(t, "claude", nil, tlIdle) {
		t.Fatal("pollerSessionIdleEnough = false, want true: turn_duration is the last meaningful entry")
	}
}

// (a') Live seats run custom providers based on builtin:claude.
func TestPollerIdleClaudeTranscriptCustomClaudeProvider(t *testing.T) {
	base := "builtin:claude"
	providers := map[string]config.ProviderSpec{"claude-fable-kumar": {Base: &base}}
	if !transcriptIdle(t, "claude-fable-kumar", providers, tlIdle) {
		t.Fatal("pollerSessionIdleEnough = false, want true for a custom provider based on builtin:claude")
	}
}

// (b) Anything but a finished turn is not idle. A bare end_turn is written
// BEFORE the Stop hooks run (and a Stop hook can continue the turn), so only
// turn_duration ends a turn. Interrupt markers and prompt text are never read.
func TestPollerIdleClaudeTranscriptTurnRunningStaysBusy(t *testing.T) {
	for name, lines := range map[string][]string{
		"task_notification":   {tlUserPrompt, tlAssistEndTurn, tlTurnDuration, tlQueueEnqueue, tlQueueRemove, tlTaskNotify},
		"tool_use":            {tlUserPrompt, tlAssistToolUse},
		"user_prompt":         {tlAssistEndTurn, tlTurnDuration, tlUserPrompt, tlAttachment},
		"bare_end_turn":       {tlUserPrompt, tlAssistToolUse, tlAssistEndTurn},
		"stop_hooks_running":  {tlUserPrompt, tlAssistEndTurn, tlStopHookSummary},
		"interrupt_marker":    {tlUserPrompt, tlAssistToolUse, tlInterrupt},
		"prompt_quotes_inter": {tlUserPrompt, tlAssistEndTurn, tlTurnDuration, tlQuotesInterrupt},
		"unknown_system":      {tlUserPrompt, tlAssistEndTurn, tlTurnDuration, `{"type":"system","subtype":"scheduled_task_fire"}`},
	} {
		t.Run(name, func(t *testing.T) {
			if transcriptIdle(t, "claude", nil, lines) {
				t.Fatal("pollerSessionIdleEnough = true, want false: no finished turn is the last meaningful entry")
			}
		})
	}
}

// (c) The live shape: the main turn ended, then only bookkeeping lines were
// appended while a background task ran.
func TestPollerIdleClaudeTranscriptQueueOperationsAfterTurnEnd(t *testing.T) {
	lines := append(append([]string{}, tlIdle...), tlQueueEnqueue, tlQueueEnqueue, tlQueueRemove,
		tlQueueRemove, tlAttachment, tlPRLink, tlBridgeSession, tlFileHistory, tlStopHookSummary)
	if !transcriptIdle(t, "claude", nil, lines) {
		t.Fatal("pollerSessionIdleEnough = false, want true: only bookkeeping lines follow turn_duration")
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
			if transcriptIdle(t, "claude", nil, lines) {
				t.Fatal("pollerSessionIdleEnough = true, want false (today's behaviour) without a definite idle tail")
			}
		})
	}
	t.Run("no_session_key", func(t *testing.T) {
		f := newTranscriptFixture(t, "claude", nil)
		f.write(f.accountRoot(), tlIdle)
		f.info.SessionKey = ""
		if f.idle() {
			t.Fatal("pollerSessionIdleEnough = true, want false when the transcript cannot be resolved")
		}
	})
}

// (e) Every other provider keeps today's behaviour even with an idle-looking
// Claude-format transcript where the claude lookup would find it.
func TestPollerIdleNonClaudeProviderIgnoresTranscript(t *testing.T) {
	if transcriptIdle(t, "codex", nil, tlIdle) {
		t.Fatal("pollerSessionIdleEnough = true, want false for a non-claude provider")
	}
}

// A provider NAMED claude whose city spec declares an empty base (and a
// non-claude command) is not claude, even when the bead's legacy
// provider_kind says claude.
func TestPollerIdleClaudeNamedProviderWithEmptyBaseKeepsTodaysBehaviour(t *testing.T) {
	empty := ""
	f := newTranscriptFixture(t, "claude", map[string]config.ProviderSpec{"claude": {Base: &empty, Command: "my-agent"}})
	f.info.ProviderKind = "claude"
	f.write(f.accountRoot(), tlIdle)
	if f.idle() {
		t.Fatal("pollerSessionIdleEnough = true, want false: provider declares base=\"\" so it is not claude")
	}
}

// The seat's own account root wins, and two different files for one session
// key are ambiguous: a stale idle copy in the default root must not override
// the in-turn transcript in the account root.
func TestPollerIdleClaudeTranscriptAmbiguousRootsNotIdle(t *testing.T) {
	f := newTranscriptFixture(t, "claude", nil)
	f.write(f.defaultRoot(), tlIdle)
	f.write(f.accountRoot(), []string{tlUserPrompt, tlAssistToolUse})
	if f.idle() {
		t.Fatal("pollerSessionIdleEnough = true, want false: two different transcripts for one session key")
	}
}

// The same file reached through a symlinked root (live: ~/.claude ->
// ~/.claude-accounts/<acct>) is one transcript, not an ambiguity.
func TestPollerIdleClaudeTranscriptSymlinkedRootIsOneFile(t *testing.T) {
	f := newTranscriptFixture(t, "claude", nil)
	f.write(f.accountRoot(), tlIdle)
	if err := os.Symlink(f.accountDir, filepath.Join(f.home, ".claude")); err != nil {
		t.Fatal(err)
	}
	if !f.idle() {
		t.Fatal("pollerSessionIdleEnough = false, want true: both roots reach the same file")
	}
}
