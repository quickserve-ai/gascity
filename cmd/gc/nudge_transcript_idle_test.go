package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	gcruntime "github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
)

// Transcript lines in the shape Claude Code writes them (ga-megheo). The
// bookkeeping lines are the live shape seen while a background subagent or
// shell runs after the main turn has ended.
const (
	tlUserPrompt      = `{"type":"user","message":{"role":"user","content":"do the thing"}}`
	tlAssistEndTurn   = `{"type":"assistant","message":{"role":"assistant","model":"claude-opus","stop_reason":"end_turn"}}`
	tlStopHookSummary = `{"type":"system","subtype":"stop_hook_summary"}`
	tlTurnDuration    = `{"type":"system","subtype":"turn_duration","timestamp":"2026-01-01T00:00:10.000Z"}`
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
	pane                                *paneStub
}

// paneStub is the seat's runtime as the gate sees it: SnapshotIdle answers in
// order (the last answer repeats), or err.
type paneStub struct {
	gcruntime.Provider
	answers []bool
	err     error
	calls   int
}

func (p *paneStub) SnapshotIdle(string) (bool, error) {
	p.calls++
	if p.err != nil {
		return false, p.err
	}
	return p.answers[min(p.calls, len(p.answers))-1], nil
}

func newTranscriptFixture(t *testing.T, provider string, providers map[string]config.ProviderSpec) *transcriptFixture {
	t.Helper()
	f := &transcriptFixture{
		t: t, home: t.TempDir(), workDir: t.TempDir(), accountDir: t.TempDir(), cityPath: t.TempDir(),
		pane: &paneStub{answers: []bool{true}},
	}
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
	f.info = session.Info{
		ID: "gc-megheo", AgentName: "worker", Provider: provider,
		WorkDir: f.workDir, SessionName: "sess-worker", SessionKey: transcriptIdleTestKey,
	}
	return f
}

func (f *transcriptFixture) accountRoot() string { return filepath.Join(f.accountDir, "projects") }

// promptAt records the seat's prompt-submitted marker, as the seat's
// UserPromptSubmit hook (gc nudge drain --inject) does.
func (f *transcriptFixture) promptAt(at time.Time) {
	f.t.Helper()
	if err := recordClaudePromptSubmitted(f.cityPath, f.info.ID, at); err != nil {
		f.t.Fatalf("record prompt marker: %v", err)
	}
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
// 1 s old (fresh, under the 3 s quiescence) and a pane that shows no live
// working indicator unless the test says otherwise.
func (f *transcriptFixture) idle() bool {
	return f.idleWith(f.pane)
}

func (f *transcriptFixture) idleWith(sp gcruntime.Provider) bool {
	target := resolveNudgeTargetFromSessionInfo(f.cityPath, f.cfg, f.info)
	last := time.Now().Add(-1 * time.Second)
	return pollerSessionIdleEnough(target, sp, 3*time.Second, worker.LiveObservation{LastActivity: &last})
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

// R3-1: the marker can be missing or stale (the hook drains stdin before it
// starts gc, seats resume into an old file, some prompts never run the
// hook), so the pane is an independent fence: a live Claude working
// indicator (e.g. "running UserPromptSubmit hooks") reads busy even when the
// transcript and marker both say ended. No pane reading reads busy too.
func TestPollerIdleClaudeTranscriptPaneWorkingStaysBusy(t *testing.T) {
	for name, sp := range map[string]gcruntime.Provider{
		"pane_working": &paneStub{answers: []bool{false}},
		"pane_error":   &paneStub{err: errors.New("capture failed")},
		"no_runtime":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newTranscriptFixture(t, "claude", nil)
			f.promptAt(tlPromptBefore)
			f.write(f.accountRoot(), tlIdle)
			if f.idleWith(sp) {
				t.Fatal("pollerSessionIdleEnough = true, want false without an idle pane reading")
			}
		})
	}
}

// R3-3: a failed marker write must not leave the previous marker looking
// valid; with no marker the transcript path does not deliver.
func TestRecordClaudePromptSubmittedFailedWriteRemovesOldMarker(t *testing.T) {
	city := t.TempDir()
	if err := recordClaudePromptSubmitted(city, "gc-1", tlPromptBefore); err != nil {
		t.Fatal(err)
	}
	failWrite := func(string, []byte) error { return errors.New("disk full") }
	orig := claudePromptMarkerWrite
	claudePromptMarkerWrite = failWrite
	t.Cleanup(func() { claudePromptMarkerWrite = orig })
	if err := recordClaudePromptSubmitted(city, "gc-1", time.Now()); err != nil {
		t.Fatalf("recordClaudePromptSubmitted = %v, want nil once the stale marker is removed", err)
	}
	if _, ok := readClaudePromptSubmitted(city, "gc-1"); ok {
		t.Fatal("old marker still readable after a failed write")
	}
}

// R3-3: when the stale marker cannot be removed either, the caller gets an
// error to log.
func TestRecordClaudePromptSubmittedReportsUnremovableStaleMarker(t *testing.T) {
	city := t.TempDir()
	if err := recordClaudePromptSubmitted(city, "gc-1", tlPromptBefore); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(claudePromptMarkerPath(city, "gc-1"))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := recordClaudePromptSubmitted(city, "gc-1", time.Now()); err == nil {
		t.Fatal("recordClaudePromptSubmitted = nil, want an error: write failed and stale marker kept")
	}
}

// R3-6: markers older than 7 days are pruned (at most once a day), so the
// directory does not grow with every session ever started.
func TestRecordClaudePromptSubmittedPrunesOldMarkers(t *testing.T) {
	city := t.TempDir()
	for _, id := range []string{"gc-old", "gc-recent"} {
		if err := recordClaudePromptSubmitted(city, id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(claudePromptMarkerPath(city, "gc-old"), old, old); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(claudePromptMarkerPath(city, "gc-old"))
	_ = os.Chtimes(filepath.Join(dir, ".pruned"), old, old)
	if err := recordClaudePromptSubmitted(city, "gc-new", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := readClaudePromptSubmitted(city, "gc-old"); ok {
		t.Fatal("8-day-old marker not pruned")
	}
	if _, ok := readClaudePromptSubmitted(city, "gc-recent"); !ok {
		t.Fatal("recent marker pruned")
	}
}

// claudeDispatchSeat is a claude seat the dispatcher sees with fresh pane
// activity, a transcript whose turn ended at tlTurnEnd, and a prompt marker
// from before that turn ended. deliver runs one dispatcher pass.
type claudeDispatchSeat struct {
	t          *testing.T
	dir        string
	transcript string
	fake       *gcruntime.Fake
	pane       *paneRuntime
	deliver    func() bool
}

// paneRuntime is a fake runtime whose pane answers SnapshotIdle in order (the
// last answer repeats).
type paneRuntime struct {
	*gcruntime.Fake
	answers []bool
	calls   int
	onNudge func()
}

func (p *paneRuntime) Nudge(name string, content []gcruntime.ContentBlock) error {
	if p.onNudge != nil {
		p.onNudge()
	}
	return p.Fake.Nudge(name, content)
}

func (p *paneRuntime) SnapshotIdle(string) (bool, error) {
	p.calls++
	return p.answers[min(p.calls, len(p.answers))-1], nil
}

func newClaudeDispatchSeat(t *testing.T, answers ...bool) *claudeDispatchSeat {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := gcruntime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", Hints: gcruntime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", gcruntime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tdir := filepath.Join(home, ".claude", "projects", sessionlog.ProjectSlug(dir))
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tdir, transcriptIdleTestKey+".jsonl"), []byte(strings.Join(tlIdle, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := recordClaudePromptSubmitted(dir, info.ID, tlPromptBefore); err != nil {
		t.Fatal(err)
	}
	fresh := time.Now().Add(-1 * time.Second)
	fake.Activity = map[string]time.Time{info.SessionName: fresh}
	target := nudgeTarget{
		cityPath: dir, agent: config.Agent{Name: "worker"}, sessionID: info.ID,
		resolved: &config.ResolvedProvider{Name: "claude"}, sessionName: info.SessionName,
		transcriptWorkDir: dir, transcriptSessionKey: transcriptIdleTestKey, providerAncestor: "claude",
	}
	pane := &paneRuntime{Fake: fake, answers: answers}
	seat := &claudeDispatchSeat{t: t, dir: dir, fake: fake, pane: pane,
		transcript: filepath.Join(tdir, transcriptIdleTestKey+".jsonl")}
	seat.deliver = func() bool {
		t.Helper()
		delivered, err := tryDeliverQueuedNudgesByPoller(target, store, store, pane, 3*time.Second,
			worker.LiveObservation{Running: true, LastActivity: &fresh})
		if err != nil {
			t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
		}
		return delivered
	}
	return seat
}

func (s *claudeDispatchSeat) enqueue(text string) {
	s.t.Helper()
	if err := enqueueQueuedNudge(s.dir, newQueuedNudge("worker", text, time.Now().Add(-time.Minute))); err != nil {
		s.t.Fatalf("enqueueQueuedNudge: %v", err)
	}
}

func (s *claudeDispatchSeat) nudgeCalls() int {
	n := 0
	for _, call := range s.fake.Calls {
		if call.Method == "Nudge" {
			n++
		}
	}
	return n
}

// The deaf-seat case end to end through the dispatcher: delivered.
func TestTryDeliverQueuedNudgesClaudeTranscriptIdleDelivers(t *testing.T) {
	seat := newClaudeDispatchSeat(t, true)
	seat.enqueue("first")
	if !seat.deliver() || seat.nudgeCalls() != 1 {
		t.Fatalf("delivered nudges = %d, want 1", seat.nudgeCalls())
	}
}

// R3-2: the dispatcher's own delivery starts a turn. A second nudge in the
// next pass, before that turn's hook writes the marker, must wait.
func TestTryDeliverQueuedNudgesClaudeBackToBackSecondWaits(t *testing.T) {
	seat := newClaudeDispatchSeat(t, true)
	seat.enqueue("first")
	if !seat.deliver() {
		t.Fatal("first nudge not delivered")
	}
	seat.enqueue("second")
	if seat.deliver() || seat.nudgeCalls() != 1 {
		t.Fatalf("second nudge delivered into the turn the first one started (nudge calls = %d)", seat.nudgeCalls())
	}
}

// R3-4: the decision is re-checked right before delivery, after the queue
// and store work: a pane that turned busy in between keeps the nudge queued.
func TestTryDeliverQueuedNudgesClaudeRechecksBeforeDelivery(t *testing.T) {
	seat := newClaudeDispatchSeat(t, true, false)
	seat.enqueue("first")
	if seat.deliver() || seat.nudgeCalls() != 0 {
		t.Fatalf("delivered although the pane went busy before delivery (nudge calls = %d)", seat.nudgeCalls())
	}
	pending, inFlight, _, err := listQueuedNudges(seat.dir, "worker", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || len(inFlight) != 0 {
		t.Fatalf("pending=%d inFlight=%d, want the nudge back in pending", len(pending), len(inFlight))
	}
}

// The dispatcher's marker carries the time just before its submit, not the
// time confirmation returned: a short turn that ends while confirmation is
// still pending must not leave the seat blocked behind its own marker.
func TestTryDeliverQueuedNudgesClaudeTurnEndedBeforeConfirmationNextDelivers(t *testing.T) {
	seat := newClaudeDispatchSeat(t, true)
	seat.pane.onNudge = func() {
		time.Sleep(5 * time.Millisecond)
		ended := `{"type":"system","subtype":"turn_duration","timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
		lines := []string{tlUserPrompt, tlAssistEndTurn, ended}
		if err := os.WriteFile(seat.transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Error(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	seat.enqueue("first")
	if !seat.deliver() {
		t.Fatal("first nudge not delivered")
	}
	seat.pane.onNudge = nil
	seat.enqueue("second")
	if !seat.deliver() || seat.nudgeCalls() != 2 {
		t.Fatalf("second nudge held although the first turn ended (nudge calls = %d)", seat.nudgeCalls())
	}
}

// Marker names that start with "." are never session ids (".pruned" is the
// prune stamp).
func TestClaudePromptMarkerPathRejectsDotNames(t *testing.T) {
	for _, id := range []string{".pruned", ".x", "."} {
		if got := claudePromptMarkerPath(t.TempDir(), id); got != "" {
			t.Errorf("claudePromptMarkerPath(%q) = %q, want \"\"", id, got)
		}
	}
}

// The dispatcher's submit-time stamp never moves the marker back: a newer
// marker the seat's own hook wrote meanwhile stays.
func TestStampClaudePromptOnDeliveryKeepsNewerMarker(t *testing.T) {
	city := t.TempDir()
	target := nudgeTarget{cityPath: city, sessionID: "gc-1", providerAncestor: "claude"}
	newer := tlPromptAfter.Add(10 * time.Second)
	if err := recordClaudePromptSubmitted(city, "gc-1", newer); err != nil {
		t.Fatal(err)
	}
	if err := stampClaudePromptOnDelivery(target, tlPromptAfter); err != nil {
		t.Fatal(err)
	}
	if got, ok := readClaudePromptSubmitted(city, "gc-1"); !ok || !got.Equal(newer) {
		t.Fatalf("marker = %v (ok=%v), want the newer %v kept", got, ok, newer)
	}
}
