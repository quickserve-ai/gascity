package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
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
	tlTurnDuration    = `{"type":"system","subtype":"turn_duration","timestamp":"2026-01-01T00:00:10.000Z"}`
	tlTurnNoTimestamp = `{"type":"system","subtype":"turn_duration"}`
	tlQueueEnqueue    = `{"type":"queue-operation","operation":"enqueue","content":"<task-notification>"}`
	tlQueueRemove     = `{"type":"queue-operation","operation":"remove"}`
	tlAttachment      = `{"type":"attachment","attachment":{"type":"queued_command"}}`
	tlPRLink          = `{"type":"pr-link"}`
	tlBridgeSession   = `{"type":"bridge-session"}`
	tlFileHistory     = `{"type":"file-history-snapshot"}`
)

var tlIdle = []string{tlUserPrompt, tlAssistEndTurn, tlStopHookSummary, tlTurnDuration}

// tlTurnEnd is tlTurnDuration's timestamp. The prompt that started that turn
// was submitted before it (tlPromptBefore); a prompt submitted after it
// (tlPromptAfter) is a new turn whose user line Claude has not written yet.
var (
	tlTurnEnd      = time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	tlPromptBefore = tlTurnEnd.Add(-5 * time.Second)
	tlPromptAfter  = tlTurnEnd.Add(time.Second)
)

// tlMetaBlock is the session-metadata block Claude Code writes after a turn
// ends and around background-task queue activity, in the type order of a real
// transcript (types only; contents are irrelevant to the gate).
var tlMetaBlock = []string{
	`{"type":"last-prompt"}`, `{"type":"custom-title"}`, `{"type":"agent-name"}`, `{"type":"mode"}`,
	`{"type":"permission-mode"}`, `{"type":"atis-latch"}`, `{"type":"pr-link"}`, `{"type":"bridge-session"}`,
	tlQueueEnqueue, tlQueueRemove, `{"type":"cost-state"}`, `{"type":"frame-link"}`,
	`{"type":"file-history-delta"}`, `{"type":"artifact-comment-monitor"}`, `{"type":"artifact-autoreact-ledger"}`,
}

const transcriptIdleTestKey = "0b6f3c1e-7d2a-4c55-9e8f-2a1b3c4d5e6f"

// transcriptFixture is a seat whose provider declares its own
// CLAUDE_CONFIG_DIR (the per-account layout live seats use) under a private
// HOME, so both the account root and the default ~/.claude/projects root are
// test-owned.
type transcriptFixture struct {
	t                                   *testing.T
	home, workDir, accountDir, cityPath string
	cfg                                 *config.City
	info                                session.Info
}

func newTranscriptFixture(t *testing.T, provider string, providers map[string]config.ProviderSpec) *transcriptFixture {
	t.Helper()
	f := &transcriptFixture{t: t, home: t.TempDir(), workDir: t.TempDir(), accountDir: t.TempDir(), cityPath: t.TempDir()}
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

// promptAt records the seat's prompt-submitted marker, as the seat's
// UserPromptSubmit hook (gc nudge drain --inject) does.
func (f *transcriptFixture) promptAt(at time.Time) {
	f.t.Helper()
	recordClaudePromptSubmitted(f.cityPath, f.info.ID, at)
	if _, ok := readClaudePromptSubmitted(f.cityPath, f.info.ID); !ok {
		f.t.Fatal("prompt marker not readable after write")
	}
}

// write puts lines in the seat's keyed transcript under the projects root.
func (f *transcriptFixture) write(root string, lines []string) {
	f.t.Helper()
	f.writeSlug(root, f.workDir, lines)
}

// writeSlug writes the keyed transcript under the slug of workDir spelled as
// given (a path alias of the seat's work dir yields a second slug).
func (f *transcriptFixture) writeSlug(root, workDir string, lines []string) {
	f.t.Helper()
	dir := filepath.Join(root, sessionlog.ProjectSlug(workDir))
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
	target := resolveNudgeTargetFromSessionInfo(f.cityPath, f.cfg, f.info)
	last := time.Now().Add(-1 * time.Second)
	return pollerSessionIdleEnough(target, nil, 3*time.Second, worker.LiveObservation{LastActivity: &last})
}

// transcriptIdle is the common case: a claude-family seat whose account
// root holds lines (nil = no transcript) and whose last prompt was submitted
// before tlTurnEnd.
func transcriptIdle(t *testing.T, provider string, providers map[string]config.ProviderSpec, lines []string) bool {
	t.Helper()
	f := newTranscriptFixture(t, provider, providers)
	f.promptAt(tlPromptBefore)
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
		"no_timestamp":  {tlUserPrompt, tlAssistEndTurn, tlTurnNoTimestamp},
	} {
		t.Run(name, func(t *testing.T) {
			if transcriptIdle(t, "claude", nil, lines) {
				t.Fatal("pollerSessionIdleEnough = true, want false (today's behaviour) without a definite idle tail")
			}
		})
	}
	t.Run("no_session_key", func(t *testing.T) {
		f := newTranscriptFixture(t, "claude", nil)
		f.promptAt(tlPromptBefore)
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
	f.promptAt(tlPromptBefore)
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
	f.promptAt(tlPromptBefore)
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
	f.promptAt(tlPromptBefore)
	f.write(f.accountRoot(), tlIdle)
	if err := os.Symlink(f.accountDir, filepath.Join(f.home, ".claude")); err != nil {
		t.Fatal(err)
	}
	if !f.idle() {
		t.Fatal("pollerSessionIdleEnough = false, want true: both roots reach the same file")
	}
}

// B-new: Claude appends the user line only after the UserPromptSubmit hooks
// finish (p50 6.9 s, p90 16.7 s on real transcripts), so for that long a
// starting turn's transcript still ends in turn_duration. The prompt marker
// the hook writes first is newer than that turn_duration: not idle.
func TestPollerIdleClaudeTranscriptPromptSubmittedAfterTurnEndStaysBusy(t *testing.T) {
	f := newTranscriptFixture(t, "claude", nil)
	f.write(f.accountRoot(), tlIdle)
	f.promptAt(tlPromptAfter)
	if f.idle() {
		t.Fatal("pollerSessionIdleEnough = true, want false: a prompt was submitted after the last turn_duration")
	}
}

// No marker (the seat has not submitted a prompt since the hook started
// writing one) means the transcript path cannot tell a starting turn from an
// idle one: today's pane-quiet rule applies, so fresh activity is not idle.
func TestPollerIdleClaudeTranscriptNoPromptMarkerKeepsTodaysBehaviour(t *testing.T) {
	f := newTranscriptFixture(t, "claude", nil)
	f.write(f.accountRoot(), tlIdle)
	if f.idle() {
		t.Fatal("pollerSessionIdleEnough = true, want false without a prompt-submitted marker")
	}
}

// N-stale: a session key that points at a frozen transcript ending in
// turn_duration (the seat moved on to another file) never delivers once the
// seat has submitted a prompt since that file froze.
func TestPollerIdleClaudeTranscriptFrozenFileNewerMarkerStaysBusy(t *testing.T) {
	f := newTranscriptFixture(t, "claude", nil)
	f.write(f.accountRoot(), tlIdle)
	path := filepath.Join(f.accountRoot(), sessionlog.ProjectSlug(f.workDir), transcriptIdleTestKey+".jsonl")
	frozen := tlTurnEnd
	if err := os.Chtimes(path, frozen, frozen); err != nil {
		t.Fatal(err)
	}
	f.promptAt(time.Now())
	if f.idle() {
		t.Fatal("pollerSessionIdleEnough = true, want false: frozen transcript older than the last prompt")
	}
}

// F-meta: the session-metadata block (real transcript type order) written
// after the turn ended while a background task runs is bookkeeping.
func TestPollerIdleClaudeTranscriptMetadataBlockAfterTurnEndDelivers(t *testing.T) {
	lines := append(append(append([]string{}, tlIdle...), tlMetaBlock...), tlFileHistory, tlAttachment)
	if !transcriptIdle(t, "claude", nil, lines) {
		t.Fatal("pollerSessionIdleEnough = false, want true: only session metadata follows turn_duration")
	}
}

// F-meta: the same block is sometimes written at prompt submit, before the
// user line. Skipping it reaches the old turn_duration, but the prompt marker
// is newer, so it is still not idle.
func TestPollerIdleClaudeTranscriptMetadataBlockAtPromptSubmitStaysBusy(t *testing.T) {
	f := newTranscriptFixture(t, "claude", nil)
	f.write(f.accountRoot(), append(append([]string{}, tlIdle...), tlMetaBlock...))
	f.promptAt(tlPromptAfter)
	if f.idle() {
		t.Fatal("pollerSessionIdleEnough = true, want false: metadata written at prompt submit, marker newer than turn_duration")
	}
}

// F-alias: within one root, the work dir's path-alias spellings (/var/x and
// /private/var/x on macOS) give two slugs. Two different files for the key
// are ambiguous even when the newer one looks idle.
func TestPollerIdleClaudeTranscriptAliasSlugsAmbiguousNotIdle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("path-alias slugs are macOS-only")
	}
	f := newTranscriptFixture(t, "claude", nil)
	alias := darwinAliasForTest(f.workDir)
	if alias == "" {
		t.Skipf("work dir %q has no /tmp or /var alias", f.workDir)
	}
	f.promptAt(tlPromptBefore)
	f.writeSlug(f.accountRoot(), alias, []string{tlUserPrompt, tlAssistToolUse})
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(f.accountRoot(), sessionlog.ProjectSlug(alias), transcriptIdleTestKey+".jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	f.write(f.accountRoot(), tlIdle)
	if f.idle() {
		t.Fatal("pollerSessionIdleEnough = true, want false: two different transcripts under alias slugs of one work dir")
	}
}

func darwinAliasForTest(dir string) string {
	for _, p := range [][2]string{{"/private/var/", "/var/"}, {"/private/tmp/", "/tmp/"}, {"/var/", "/private/var/"}, {"/tmp/", "/private/tmp/"}} {
		if strings.HasPrefix(dir, p[0]) {
			return p[1] + strings.TrimPrefix(dir, p[0])
		}
	}
	return ""
}

// The seat's UserPromptSubmit hook (gc nudge drain --inject) records the
// prompt marker the gate compares turn_duration against, keyed by
// GC_SESSION_ID, even on the empty-queue fast path.
func TestNudgeDrainInjectRecordsPromptSubmitted(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	writeNamedSessionCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_SESSION_ID", "gc-megheo")

	before := time.Now()
	var stdout, stderr bytes.Buffer
	if code := cmdNudgeDrainWithFormat(nil, true, "claude", &stdout, &stderr); code != 0 {
		t.Fatalf("cmdNudgeDrainWithFormat = %d, want 0; stderr=%q", code, stderr.String())
	}
	at, ok := readClaudePromptSubmitted(cityDir, "gc-megheo")
	if !ok {
		t.Fatal("no prompt-submitted marker after gc nudge drain --inject")
	}
	if at.Before(before.Add(-time.Second)) || at.After(time.Now().Add(time.Second)) {
		t.Fatalf("marker time %v not within the drain call", at)
	}
}
