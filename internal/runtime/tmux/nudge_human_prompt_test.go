package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The fixtures below are the pane captures platform-lead took, one per key,
// while replaying gc's nudge key sequence against a Claude Code
// AskUserQuestion dialog on a scratch seat (ga-ngrme8, 00:45Z 9/29). Each
// Enter the nudge path sent SELECTED the highlighted option and advanced the
// dialog; the second delivery submitted it as "User answered Claude's
// questions" (ga-ubfc7j).

// questionDialogFixture is the dialog as it stood before the first key.
const questionDialogFixture = `other tool before it. Step 2: when it returns, print the raw answers you
received as a fenced JSON block, then stop.
────────────────────────────────────────────────────────────────────────────
──────────────────────────────────────────────────────────────────────────
←  ☐ Q1  ☐ Q2  ☐ Q3  ☐ Q4  ✔ Submit  →
Which approach would you prefer for this feature?
❯ 1. Alpha approach (Recommended)
2. Beta variant
3. Gamma variant
4. Type something.
────────────────────────────────────────────────────────────────────────────
──────────────────────────────────────────────────────────────────────────
5. Chat about this
Enter to select · Tab/Arrow keys to navigate · Esc to cancel`

// questionDialogAfterEnterFixture is the same dialog after one Enter: Q1 is
// answered (☒) and Q2 is up.
const questionDialogAfterEnterFixture = `other tool before it. Step 2: when it returns, print the raw answers you
received as a fenced JSON block, then stop.
────────────────────────────────────────────────────────────────────────────
──────────────────────────────────────────────────────────────────────────
←  ☒ Q1  ☐ Q2  ☐ Q3  ☐ Q4  ✔ Submit  →
How should we handle configuration?
❯ 1. Environment variables (Recommended)
2. Config file Beta
3. Command-line Gamma
4. Type something.
────────────────────────────────────────────────────────────────────────────
──────────────────────────────────────────────────────────────────────────
5. Chat about this
Enter to select · Tab/Arrow keys to navigate · Esc to cancel`

// questionReviewFixture is the dialog's review page: one Enter here submits
// every answer.
const questionReviewFixture = `Each question has three options; the FIRST option label of each ends with "
(Recommended)", the other two are "Beta" and "Gamma" variants. Do not ca
other tool before it. Step 2: when it returns, print the raw answers you
received as a fenced JSON block, then stop.
────────────────────────────────────────────────────────────────────────────
──────────────────────────────────────────────────────────────────────────
←  ☒ Q1  ☒ Q2  ☒ Q3  ☒ Q4  ✔ Submit  →
Review your answers
● Which approach would you prefer for this feature?
→ Alpha approach (Recommended)
● How should we handle configuration?
→ Environment variables (Recommended)
● Which testing strategy do you prefer?
→ Unit tests (Recommended)
● How should we structure the codebase?
→ Modular (Recommended)
Ready to submit your answers?
❯ 1. Submit answers
2. Cancel`

// approvalPromptFixture is Claude Code's current tool-permission prompt: no
// "This command requires approval" line, so parseApprovalPrompt alone does not
// see it.
const approvalPromptFixture = `⏺ Bash(rm -rf build/)
  ⎿  Running…

────────────────────────────────────────────────────────────────────────────
 Bash command

   rm -rf build/
   Remove the build directory

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for rm commands in /repo
   3. No, and tell Claude what to do differently (esc)

 Esc to cancel · Tab to amend`

// legacyApprovalPromptFixture is the older layout parseApprovalPrompt reads
// (kept in step with interaction_test.go).
const legacyApprovalPromptFixture = `● Bash(bd list --assignee=$GC_AGENT --status=in_progress 2>&1)
  ⎿  Running…

────────────────────────────────────────────────────────────────────────────────
 Bash command

   bd list --assignee=$GC_AGENT --status=in_progress 2>&1
   Check for in-progress work (crash recovery)

 This command requires approval

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for: bd list:*
   3. No

 Esc to cancel · Tab to amend · ctrl+e to explain`

// idleComposerFixture is an idle Claude pane: an empty composer, the status
// line below it.
const idleComposerFixture = `⏺ Done. The tests pass.
────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────
  cherub@mac /repo (main)
  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle)`

// humanDraftFixture is the composer holding a human's half-typed message (the
// canary's "I do" before gc's delivery landed in the middle of it).
const humanDraftFixture = `⏺ Done. The tests pass.
────────────────────────────────────────────────────────────────────────────
❯ I do
────────────────────────────────────────────────────────────────────────────
  cherub@mac /repo (main)
  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle)`

// busyFixture is the pane after a submitted turn started.
const busyFixture = `❯ <system-reminder> nudge
✽ Garnishing… (10s · ↓ 853 tokens)
────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────
  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle)`

// quotedDialogAboveComposerFixture is an idle pane whose SCROLLBACK quotes a
// question dialog (a seat reading these very captures). The live composer is
// below it, so nothing on screen is waiting for an answer.
const quotedDialogAboveComposerFixture = `⏺ The bead's pane capture reads:
  ←  ☐ Q1  ☐ Q2  ✔ Submit  →
  Which approach would you prefer for this feature?
  ❯ 1. Alpha approach (Recommended)
  2. Beta variant
  Enter to select · Tab/Arrow keys to navigate · Esc to cancel
  Do you want to proceed?
  ❯ 1. Yes
  2. No
────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────
  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle)`

// panePromptExecutor is a scripted Claude pane for driving NudgeSession end to
// end without a tmux server. capture-pane returns the current screen; an Enter
// may swap the screen (onEnter), which is how a test makes the pane go busy or
// raises a dialog after the first submit.
type panePromptExecutor struct {
	mu         sync.Mutex
	attached   bool
	screen     string
	captureErr error
	// styled answers capture-pane -e (the text-attribute read); empty means
	// the screen itself, which carries no attributes.
	styled    string
	styledErr error
	// clients overrides #{session_attached} (a client COUNT); empty means
	// "1" when attached, else "0".
	clients string
	// failLiteral makes the first N literal sends fail with tmux's transient
	// "not in a mode"; afterFail is the screen once one has failed.
	failLiteral int
	afterFail   string
	onEnter     func(n int) string
	calls       [][]string
	enters      int
}

func (f *panePromptExecutor) execute(args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	switch {
	case tmuxArgsContain(args, "capture-pane") && tmuxArgsContain(args, "-e"):
		if f.styledErr != nil {
			return "", f.styledErr
		}
		if f.styled != "" {
			return f.styled, nil
		}
		return f.screen, nil
	case tmuxArgsContain(args, "capture-pane"):
		if f.captureErr != nil {
			return "", f.captureErr
		}
		return f.screen, nil
	case tmuxArgsContain(args, "#{session_attached}"):
		if f.clients != "" {
			return f.clients, nil
		}
		if f.attached {
			return "1", nil
		}
		return "0", nil
	case tmuxArgsContain(args, "#{bracket_paste_flag}"):
		return "1", nil
	case tmuxArgsContain(args, "#{pane_pid}"):
		return "", nil
	case tmuxArgsContain(args, "list-panes"):
		return "%1\tclaude\t1", nil
	case tmuxArgsContain(args, "list-windows"):
		return "1700000000", nil
	case tmuxArgsContain(args, "show-environment"):
		if args[len(args)-1] == "GC_PROVIDER" {
			return "GC_PROVIDER=claude", nil
		}
		return "", errors.New("unknown variable: " + args[len(args)-1])
	case tmuxArgsContain(args, "send-keys") && tmuxArgsContain(args, "-l") && f.failLiteral > 0:
		f.failLiteral--
		if f.afterFail != "" {
			f.screen = f.afterFail
		}
		return "", errors.New("not in a mode")
	case tmuxArgsContain(args, "send-keys"):
		if !tmuxArgsContain(args, "-l") && args[len(args)-1] == "Enter" {
			f.enters++
			if f.onEnter != nil {
				if next := f.onEnter(f.enters); next != "" {
					f.screen = next
				}
			}
		}
	}
	return "", nil
}

func (f *panePromptExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

// keyCalls returns every call that put input into the pane: send-keys of any
// kind (C-u, literal text, Enter, a survey digit) and bracketed pastes.
func (f *panePromptExecutor) keyCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if tmuxArgsContain(c, "send-keys") || tmuxArgsContain(c, "paste-buffer") {
			out = append(out, c)
		}
	}
	return out
}

func (f *panePromptExecutor) sentKey(key string) bool {
	for _, c := range f.keyCalls() {
		if !tmuxArgsContain(c, "-l") && c[len(c)-1] == key {
			return true
		}
	}
	return false
}

func (f *panePromptExecutor) typed() []string {
	var out []string
	for _, c := range f.keyCalls() {
		if tmuxArgsContain(c, "-l") {
			out = append(out, c[len(c)-1])
		}
	}
	return out
}

func (f *panePromptExecutor) enterCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enters
}

var guardSessionSeq atomic.Int64

// newGuardTestTmux returns a Tmux wired to fe and a session name unique to the
// test, so the package-level per-session nudge lock never serializes two tests.
func newGuardTestTmux(fe *panePromptExecutor) (*Tmux, string) {
	tm := NewTmuxWithConfig(DefaultConfig())
	tm.exec = fe
	return tm, fmt.Sprintf("guard-test-%d", guardSessionSeq.Add(1))
}

const guardTestNudge = "<system-reminder> You have a deferred reminder: check the queue </system-reminder>"

func assertDeferred(t *testing.T, err error, wantReason string) {
	t.Helper()
	if !errors.Is(err, ErrNudgeDeferredHumanPrompt) {
		t.Fatalf("NudgeSession() = %v, want ErrNudgeDeferredHumanPrompt", err)
	}
	if reason, _ := NudgeDeferredReason(err); reason != wantReason {
		t.Fatalf("deferral reason = %q, want %q (err: %v)", reason, wantReason, err)
	}
}

// TestNudgeSessionSendsNoKeysIntoAHumanPrompt is the ga-ubfc7j regression. A
// pane showing a question dialog, its review page or an approval prompt is
// quiet and shows no spinner, so the nudge path used to type into it and press
// Enter up to three times: each Enter picked the highlighted option. Now the
// nudge must send nothing at all -- not the C-u clear, not the survey digit,
// not the text, not Enter -- and say why.
func TestNudgeSessionSendsNoKeysIntoAHumanPrompt(t *testing.T) {
	tests := []struct {
		name       string
		screen     string
		attached   bool
		wantReason string
	}{
		{name: "question dialog, detached", screen: questionDialogFixture, wantReason: NudgeDeferReasonQuestionDialog},
		{name: "question dialog, attached", screen: questionDialogFixture, attached: true, wantReason: NudgeDeferReasonQuestionDialog},
		{name: "question dialog after one answer", screen: questionDialogAfterEnterFixture, wantReason: NudgeDeferReasonQuestionDialog},
		{name: "review your answers page", screen: questionReviewFixture, wantReason: NudgeDeferReasonQuestionDialog},
		{name: "approval prompt", screen: approvalPromptFixture, wantReason: NudgeDeferReasonApprovalPrompt},
		{name: "legacy approval prompt", screen: legacyApprovalPromptFixture, wantReason: NudgeDeferReasonApprovalPrompt},
		{name: "human draft on an attached session", screen: humanDraftFixture, attached: true, wantReason: NudgeDeferReasonHumanDraft},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fe := &panePromptExecutor{screen: tt.screen, attached: tt.attached}
			tm, session := newGuardTestTmux(fe)

			err := tm.NudgeSession(session, guardTestNudge)
			if keys := fe.keyCalls(); len(keys) != 0 {
				t.Fatalf("NudgeSession sent %d key call(s) into a pane holding a human prompt: %q", len(keys), keys)
			}
			assertDeferred(t, err, tt.wantReason)
		})
	}
}

// TestNudgeSessionFailsClosedWhenThePaneCannotBeRead: if the guard cannot see
// the pane it cannot rule a dialog out, so it must not type.
func TestNudgeSessionFailsClosedWhenThePaneCannotBeRead(t *testing.T) {
	fe := &panePromptExecutor{screen: idleComposerFixture, captureErr: errors.New("capture-pane: transient")}
	tm, session := newGuardTestTmux(fe)

	err := tm.NudgeSession(session, guardTestNudge)
	if keys := fe.keyCalls(); len(keys) != 0 {
		t.Fatalf("NudgeSession typed into a pane it could not read: %q", keys)
	}
	assertDeferred(t, err, NudgeDeferReasonCaptureFailed)
}

// TestNudgeSessionDoesNotReEnterIntoADialogRaisedByTheFirstEnter: the first
// Enter submits the text this delivery typed, and the agent may answer it by
// raising a question dialog before a busy spinner is ever observed. The old
// retry loop then re-sent Enter twice more -- answering the dialog. A re-send
// is only allowed while the composer still holds the typed text.
func TestNudgeSessionDoesNotReEnterIntoADialogRaisedByTheFirstEnter(t *testing.T) {
	fe := &panePromptExecutor{screen: idleComposerFixture}
	fe.onEnter = func(n int) string {
		switch n {
		case 1:
			return questionDialogFixture
		default:
			return questionDialogAfterEnterFixture
		}
	}
	tm, session := newGuardTestTmux(fe)

	_ = tm.NudgeSession(session, guardTestNudge)
	if got := fe.enterCount(); got != 1 {
		t.Fatalf("Enter sent %d times; want exactly 1 -- every Enter after the first answered the dialog the agent raised", got)
	}
}

// TestNudgeSessionReEntersOnlyWhileTheTypedTextIsStillDrafted keeps the ga-bwm
// repair: a first Enter that was dropped leaves the typed text in the
// composer, and that (and only that) earns a re-send.
func TestNudgeSessionReEntersOnlyWhileTheTypedTextIsStillDrafted(t *testing.T) {
	drafted := composerFixture("❯ " + guardTestNudge)
	fe := &panePromptExecutor{screen: drafted}
	fe.onEnter = func(n int) string {
		if n == 2 {
			return busyFixture
		}
		return ""
	}
	tm, session := newGuardTestTmux(fe)

	if err := tm.NudgeSession(session, guardTestNudge); err != nil {
		t.Fatalf("NudgeSession() = %v, want nil (the second Enter submitted)", err)
	}
	if got := fe.enterCount(); got != 2 {
		t.Fatalf("Enter sent %d times, want 2 (first dropped, one re-send while the text was still drafted)", got)
	}
}

// TestNudgeSessionDeliversIntoAnIdleComposer is the control: an ordinary idle
// pane still gets C-u, the text and one Enter, exactly as before.
func TestNudgeSessionDeliversIntoAnIdleComposer(t *testing.T) {
	fe := &panePromptExecutor{screen: idleComposerFixture}
	fe.onEnter = func(int) string { return busyFixture }
	tm, session := newGuardTestTmux(fe)

	if err := tm.NudgeSession(session, guardTestNudge); err != nil {
		t.Fatalf("NudgeSession() = %v, want nil", err)
	}
	if !fe.sentKey("C-u") {
		t.Fatal("no C-u clear on a detached idle pane")
	}
	if typed := fe.typed(); len(typed) != 1 || typed[0] != guardTestNudge {
		t.Fatalf("typed = %q, want the nudge once", typed)
	}
	if got := fe.enterCount(); got != 1 {
		t.Fatalf("Enter sent %d times, want 1", got)
	}
}

// TestNudgeSessionClearsAStaleDraftOnADetachedSession pins why only an
// ATTACHED draft defers: with no client attached no human can be typing, so a
// draft on the line is an earlier nudge's lost submit (ga-bwm), and the C-u
// clear that replaces it is still right (ra-3x46cy).
func TestNudgeSessionClearsAStaleDraftOnADetachedSession(t *testing.T) {
	fe := &panePromptExecutor{screen: humanDraftFixture}
	fe.onEnter = func(int) string { return busyFixture }
	tm, session := newGuardTestTmux(fe)

	if err := tm.NudgeSession(session, guardTestNudge); err != nil {
		t.Fatalf("NudgeSession() = %v, want nil", err)
	}
	if !fe.sentKey("C-u") {
		t.Fatal("no C-u clear before typing over a detached draft")
	}
	if typed := fe.typed(); len(typed) != 1 {
		t.Fatalf("typed = %q, want the nudge once", typed)
	}
}

// TestFeedbackSurveyDetectorNeverMatchesAQuestionDialog: the survey dismissal
// sends a DIGIT, and on a question dialog a digit selects an option.
func TestFeedbackSurveyDetectorNeverMatchesAQuestionDialog(t *testing.T) {
	for name, screen := range map[string]string{
		"question dialog":       questionDialogFixture,
		"question after answer": questionDialogAfterEnterFixture,
		"review page":           questionReviewFixture,
		"approval prompt":       approvalPromptFixture,
		"legacy approval":       legacyApprovalPromptFixture,
	} {
		if runtime.ContainsFeedbackSurveyModal(screen) {
			t.Errorf("%s: ContainsFeedbackSurveyModal = true; its dismissal would type a digit into the dialog", name)
		}
	}
}

// TestClassifyHumanPrompt pins the detector itself, including the screens it
// must NOT flag: a stalled seat is the cost of every false positive.
func TestClassifyHumanPrompt(t *testing.T) {
	tests := []struct {
		name     string
		screen   string
		attached bool
		want     string
	}{
		{name: "question dialog", screen: questionDialogFixture, want: NudgeDeferReasonQuestionDialog},
		{name: "review page", screen: questionReviewFixture, want: NudgeDeferReasonQuestionDialog},
		{name: "approval prompt", screen: approvalPromptFixture, want: NudgeDeferReasonApprovalPrompt},
		{name: "legacy approval prompt", screen: legacyApprovalPromptFixture, want: NudgeDeferReasonApprovalPrompt},
		{name: "attached human draft", screen: humanDraftFixture, attached: true, want: NudgeDeferReasonHumanDraft},
		{name: "detached draft", screen: humanDraftFixture, want: ""},
		{name: "idle composer", screen: idleComposerFixture, want: ""},
		{name: "idle composer, attached", screen: idleComposerFixture, attached: true, want: ""},
		{name: "busy pane", screen: busyFixture, want: ""},
		{name: "dialog quoted in scrollback above the live composer", screen: quotedDialogAboveComposerFixture, attached: true, want: ""},
		{name: "feedback survey (its own dismissal handles it)", screen: feedbackSurveySessionFixture, want: ""},
		{name: "attached gc draft left by a lost submit", screen: composerFixture("❯ <system-reminder> earlier nudge"), attached: true, want: ""},
		{name: "attached boxed empty composer", screen: feedbackSurveySessionFixture, attached: true, want: ""},
		{name: "generic numbered selection", screen: "Pick one\n❯ 1. Red\n  2. Blue\n", want: NudgeDeferReasonSelectionPrompt},
		{name: "a numbered list with no cursor is prose", screen: "Plan:\n1. Red\n2. Blue\n❯ \n", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyHumanPrompt(strings.Split(tt.screen, "\n"), DefaultReadyPromptPrefix, tt.attached)
			if got != tt.want {
				t.Fatalf("classifyHumanPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Per-caller refusals (scope correction, 9/29): every path that types into a
// seat's pane reaches the same chokepoint -- the text senders
// (sendKeysLiteralWithRetry / sendStartupKeysLiteralWithRetry) and the submit
// sender (sendNudgeSubmitSequence) -- so none of them can type into a
// question dialog. Callers 1 and 2 (NudgeSession detached / attached) are the
// "question dialog, detached/attached" cases of
// TestNudgeSessionSendsNoKeysIntoAHumanPrompt.

// Caller 3: Provider.Nudge waits for idle and then sends ANYWAY when the wait
// times out. The send-anyway must still be refused.
func TestProviderNudgeRefusesAQuestionDialogAfterItsIdleWait(t *testing.T) {
	fe := &panePromptExecutor{screen: questionDialogFixture}
	tm, session := newGuardTestTmux(fe)
	tm.cfg.NudgeIdleTimeout = 300 * time.Millisecond
	p := &Provider{tm: tm}

	err := p.Nudge(session, runtime.TextContent(guardTestNudge))
	if keys := fe.keyCalls(); len(keys) != 0 {
		t.Fatalf("Provider.Nudge sent %d key call(s) into a question dialog: %q", len(keys), keys)
	}
	assertDeferred(t, err, NudgeDeferReasonQuestionDialog)
}

// Caller 4: the startup nudge path (nudgeStartupSession, driven by the
// 2/4/6/8 s ladder in sendStartupNudgeWithRetry).
func TestStartupNudgeSendsNoKeysIntoAQuestionDialog(t *testing.T) {
	fe := &panePromptExecutor{screen: questionDialogFixture}
	tm, session := newGuardTestTmux(fe)

	err := tm.nudgeStartupSession(session, "startup role prompt")
	if keys := fe.keyCalls(); len(keys) != 0 {
		t.Fatalf("startup nudge sent %d key call(s) into a question dialog: %q", len(keys), keys)
	}
	assertDeferred(t, err, NudgeDeferReasonQuestionDialog)
}

// NudgePane, the pane-addressed sibling of NudgeSession.
func TestNudgePaneSendsNoKeysIntoAQuestionDialog(t *testing.T) {
	fe := &panePromptExecutor{screen: questionDialogFixture}
	tm, _ := newGuardTestTmux(fe)

	err := tm.NudgePane("%1", guardTestNudge)
	if keys := fe.keyCalls(); len(keys) != 0 {
		t.Fatalf("NudgePane sent %d key call(s) into a question dialog: %q", len(keys), keys)
	}
	assertDeferred(t, err, NudgeDeferReasonQuestionDialog)
}

// The chokepoint itself: each sender refuses on its own, whoever calls it.
func TestNudgeKeySendersRefuseAQuestionDialog(t *testing.T) {
	senders := map[string]func(tm *Tmux) error{
		"sendKeysLiteralWithRetry": func(tm *Tmux) error {
			return tm.sendKeysLiteralWithRetry("%1", guardTestNudge, time.Second)
		},
		"sendStartupKeysLiteralWithRetry": func(tm *Tmux) error {
			return tm.sendStartupKeysLiteralWithRetry("%1", guardTestNudge, "claude", time.Second)
		},
		"sendNudgeSubmitSequence": func(tm *Tmux) error {
			return tm.sendNudgeSubmitSequence("%1", []string{"Enter"})
		},
	}
	for name, send := range senders {
		t.Run(name, func(t *testing.T) {
			fe := &panePromptExecutor{screen: questionDialogFixture}
			tm, _ := newGuardTestTmux(fe)
			err := send(tm)
			if keys := fe.keyCalls(); len(keys) != 0 {
				t.Fatalf("%s sent %d key call(s) into a question dialog: %q", name, len(keys), keys)
			}
			assertDeferred(t, err, NudgeDeferReasonQuestionDialog)
		})
	}
}

// TestStartupNudgeRefusedByTheGuardDoesNotFailTheStart pins what caller 4's
// ladder does with the refusal. Nothing was typed, and the session itself is
// verified alive, so -- like an unconfirmed startup submit -- the start
// proceeds with a warning and a durable artifact rather than being torn down
// (a teardown would kill the pane a person may be answering). The ladder does
// not retry it: the prompt on screen needs a human, not a timer.
func TestStartupNudgeRefusedByTheGuardDoesNotFailTheStart(t *testing.T) {
	ops := &fakeStartOps{
		hasSessionResult:           true,
		sendKeysErr:                &NudgeDeferredError{Session: "test", Reason: NudgeDeferReasonQuestionDialog, Stage: nudgeGuardStageBeforeType},
		recordUnconfirmedNudgePath: "/city/.gc/sessions/test/startup-nudge-unconfirmed.log",
	}
	cfg := runtime.Config{Command: "claude", Nudge: "startup prompt"}

	if err := doStartSession(context.Background(), ops, "test", cfg, DefaultConfig().SetupTimeout); err != nil {
		t.Fatalf("doStartSession = %v, want nil: a startup nudge refused by the human-prompt guard sent nothing and must not tear the session down", err)
	}
	callsByMethod(t, ops, "sendKeys", 1)
	callsByMethod(t, ops, "recordUnconfirmedNudge", 1)
}

// Claude's empty composer on a fresh session, captured with and without text
// attributes (capture-pane -e) from a live Claude Code pane on 2026-09-29:
// the prompt glyph is drawn plain and the placeholder faint (SGR 2).
const placeholderComposerFixture = `⏺ Done. The tests pass.
────────────────────────────────────────────────────────────────────────────
❯ Try "how does <filepath> work?"
────────────────────────────────────────────────────────────────────────────
  cherub@mac /repo (main)
  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle)`

const placeholderComposerStyledFixture = "⏺ Done. The tests pass.\n" +
	"\x1b[38;5;244m────────────────────────────────────────────────────────────────────────────\n" +
	"\x1b[39m❯\u00a0\x1b[2mTry\x1b[0m \x1b[2m\"how\x1b[0m \x1b[2mdoes\x1b[0m \x1b[2m<filepath>\x1b[0m \x1b[2mwork?\"\x1b[0m\n" +
	"\x1b[38;5;244m────────────────────────────────────────────────────────────────────────────\n" +
	"\x1b[39m  \x1b[32mcherub@mac\x1b[38;5;246m \x1b[34m/repo (main)\x1b[39m\n" +
	"  \x1b[38;5;246m--\x1b[39m \x1b[38;5;246mINSERT\x1b[39m \x1b[38;5;246m--\x1b[39m ⏵⏵ bypass permissions on (shift+tab to cycle)"

// humanDraftAfterPlaceholderStyledFixture: the person typed "I do" and the
// rest of the line is still faint; the typed part is a draft.
const humanDraftAfterPlaceholderStyledFixture = "⏺ Done. The tests pass.\n" +
	"\x1b[38;5;244m────────────────────────────────────────────────────────────────────────────\n" +
	"\x1b[39m❯\u00a0I do \x1b[2mmore\x1b[0m\n" +
	"\x1b[38;5;244m────────────────────────────────────────────────────────────────────────────\n" +
	"\x1b[39m  cherub@mac /repo (main)\n" +
	"  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle)"

func TestStripTerminalStyle(t *testing.T) {
	tests := []struct {
		name, in, want string
		dropFaint      bool
	}{
		{name: "placeholder kept", in: "\x1b[39m❯ \x1b[2mTry\x1b[0m \x1b[2mit\x1b[0m", want: "❯ Try it"},
		{name: "placeholder dropped", in: "\x1b[39m❯ \x1b[2mTry\x1b[0m \x1b[2mit\x1b[0m", dropFaint: true, want: "❯  "},
		{name: "22 ends faint", in: "\x1b[2mghost\x1b[22mtyped", dropFaint: true, want: "typed"},
		{name: "empty SGR ends faint", in: "\x1b[2mghost\x1b[mtyped", dropFaint: true, want: "typed"},
		{name: "color index 2 is not faint", in: "\x1b[38;5;2mgreen\x1b[39m", dropFaint: true, want: "green"},
		{name: "truecolor args skipped", in: "\x1b[38;2;2;2;2mgrey", dropFaint: true, want: "grey"},
		{name: "faint combined with color", in: "\x1b[2;38;5;246mdim\x1b[0m", dropFaint: true, want: ""},
		{name: "hyperlink OSC stripped", in: "\x1b]8;id=x;https://e.x/\x1b\\link\x1b]8;;\x07", want: "link"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripTerminalStyle(tt.in, tt.dropFaint); got != tt.want {
				t.Fatalf("stripTerminalStyle(%q, %v) = %q, want %q", tt.in, tt.dropFaint, got, tt.want)
			}
		})
	}
}

// TestNudgeSessionTreatsFaintPlaceholderAsAnEmptyComposer: on an attached
// Claude seat the empty composer shows faint placeholder text, which a plain
// capture reads as a human's draft. Deferring on it would hold every nudge to
// an attached seat until the person typed and submitted, so the guard re-reads
// the composer's text attributes and discounts faint runs, and only those.
func TestNudgeSessionTreatsFaintPlaceholderAsAnEmptyComposer(t *testing.T) {
	t.Run("placeholder only: delivers", func(t *testing.T) {
		fe := &panePromptExecutor{screen: placeholderComposerFixture, styled: placeholderComposerStyledFixture, attached: true}
		fe.onEnter = func(int) string { return busyFixture }
		tm, session := newGuardTestTmux(fe)
		if err := tm.NudgeSession(session, guardTestNudge); errors.Is(err, ErrNudgeDeferredHumanPrompt) {
			t.Fatalf("NudgeSession() deferred on a faint placeholder: %v", err)
		}
		if len(fe.keyCalls()) == 0 {
			t.Fatal("NudgeSession sent no keys into an empty composer showing a placeholder")
		}
	})
	t.Run("typed text before the faint tail: defers", func(t *testing.T) {
		fe := &panePromptExecutor{screen: humanDraftFixture, styled: humanDraftAfterPlaceholderStyledFixture, attached: true}
		tm, session := newGuardTestTmux(fe)
		err := tm.NudgeSession(session, guardTestNudge)
		if keys := fe.keyCalls(); len(keys) != 0 {
			t.Fatalf("NudgeSession typed into a human draft: %q", keys)
		}
		assertDeferred(t, err, NudgeDeferReasonHumanDraft)
	})
	t.Run("attribute read fails: defers", func(t *testing.T) {
		fe := &panePromptExecutor{screen: placeholderComposerFixture, styledErr: errors.New("capture-pane: transient"), attached: true}
		tm, session := newGuardTestTmux(fe)
		err := tm.NudgeSession(session, guardTestNudge)
		if keys := fe.keyCalls(); len(keys) != 0 {
			t.Fatalf("NudgeSession typed with the attribute read failing: %q", keys)
		}
		assertDeferred(t, err, NudgeDeferReasonHumanDraft)
	})
}

const rule = "────────────────────────────────────────────────────────────────────────────"

// composerFixture draws Claude's composer box holding body, with scrollback
// above and the status line below.
func composerFixture(body string) string {
	return "⏺ Done. The tests pass.\n" + rule + "\n" + body + "\n" + rule + "\n" +
		"  cherub@mac /repo (main)\n  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle)"
}

// TestNudgeSessionDefersOnEveryShapeOfHumanDraft: the draft is the whole
// composer box, not its first line. A draft whose prompt line is empty and
// whose text is on the next line, and a draft that begins "1." (which the
// dialog anchor skips as an option row), are both a person's unsent message.
func TestNudgeSessionDefersOnEveryShapeOfHumanDraft(t *testing.T) {
	for name, screen := range map[string]string{
		"text on a continuation line": composerFixture("❯ \n  and then I thought"),
		"wrapped over two lines":      composerFixture("❯ first half of a long\n  second half"),
		"starts like a list item":     composerFixture("❯ 1. rename the column"),
	} {
		t.Run(name, func(t *testing.T) {
			fe := &panePromptExecutor{screen: screen, attached: true}
			tm, session := newGuardTestTmux(fe)
			err := tm.NudgeSession(session, guardTestNudge)
			if keys := fe.keyCalls(); len(keys) != 0 {
				t.Fatalf("NudgeSession typed into a human draft: %q", keys)
			}
			assertDeferred(t, err, NudgeDeferReasonHumanDraft)
		})
	}
}

// TestNudgeSessionCountsEveryAttachedClient: #{session_attached} is a count.
// gc's hidden attach client plus a person reads "2"; treating only "1" as
// attached let the nudge C-u a person's draft away and submit over it.
func TestNudgeSessionCountsEveryAttachedClient(t *testing.T) {
	t.Run("two clients, human draft: defers, no C-u", func(t *testing.T) {
		fe := &panePromptExecutor{screen: humanDraftFixture, clients: "2"}
		tm, session := newGuardTestTmux(fe)
		err := tm.NudgeSession(session, guardTestNudge)
		if keys := fe.keyCalls(); len(keys) != 0 {
			t.Fatalf("NudgeSession sent keys over a draft with two clients attached: %q", keys)
		}
		assertDeferred(t, err, NudgeDeferReasonHumanDraft)
	})
	t.Run("two clients, idle: delivers without C-u", func(t *testing.T) {
		fe := &panePromptExecutor{screen: idleComposerFixture, clients: "2"}
		fe.onEnter = func(int) string { return busyFixture }
		tm, session := newGuardTestTmux(fe)
		if err := tm.NudgeSession(session, guardTestNudge); err != nil {
			t.Fatalf("NudgeSession() = %v, want nil", err)
		}
		if fe.sentKey("C-u") {
			t.Fatal("C-u sent with two clients attached")
		}
	})
}

// TestNudgeSessionDoesNotReEnterOnSomeoneElsesPaste: a re-sent Enter needs the
// composer to show exactly what it showed before the first Enter. If the first
// submit landed unobserved and a person then pasted their own text, the
// composer holds a DIFFERENT paste placeholder, and re-sending would submit it.
func TestNudgeSessionDoesNotReEnterOnSomeoneElsesPaste(t *testing.T) {
	ours := composerFixture("❯ [Pasted text #1 +3 lines]")
	theirs := composerFixture("❯ [Pasted text #2 +7 lines]")
	fe := &panePromptExecutor{screen: ours}
	fe.onEnter = func(n int) string {
		if n == 1 {
			return theirs
		}
		return busyFixture
	}
	tm, session := newGuardTestTmux(fe)
	_ = tm.NudgeSession(session, guardTestNudge)
	if got := fe.enterCount(); got != 1 {
		t.Fatalf("Enter sent %d times, want 1: the composer held another paste after the first Enter", got)
	}
}

func TestComposerDraft(t *testing.T) {
	tests := []struct {
		name, screen string
		found        bool
		want         string
	}{
		{name: "empty box", screen: idleComposerFixture, found: true, want: ""},
		{name: "one line", screen: humanDraftFixture, found: true, want: "I do"},
		{name: "continuation", screen: composerFixture("❯ \n  later"), found: true, want: "later"},
		{name: "list item", screen: composerFixture("❯ 1. x"), found: true, want: "1. x"},
		{name: "no box falls back to the last prompt line", screen: "output\n❯ typed", found: true, want: "typed"},
		{name: "box without a prompt falls back", screen: rule + "\nnot a composer\n" + rule + "\n❯ typed", found: true, want: "typed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, got := composerDraft(strings.Split(tt.screen, "\n"), "❯ ")
			if found != tt.found || got != tt.want {
				t.Fatalf("composerDraft() = (%v, %q), want (%v, %q)", found, got, tt.found, tt.want)
			}
		})
	}
}

// TestNudgeTextRetryReChecksThePane: a transient send failure is retried after
// a backoff, and the pane can change in that time. The retry must not reuse
// the first attempt's check.
func TestNudgeTextRetryReChecksThePane(t *testing.T) {
	for name, tt := range map[string]struct {
		after    string
		attached bool
		want     string
	}{
		"dialog raised during the backoff": {after: questionDialogFixture, want: NudgeDeferReasonQuestionDialog},
		"person started a draft":           {after: humanDraftFixture, attached: true, want: NudgeDeferReasonHumanDraft},
	} {
		t.Run(name, func(t *testing.T) {
			fe := &panePromptExecutor{screen: idleComposerFixture, attached: tt.attached, failLiteral: 1, afterFail: tt.after}
			tm, session := newGuardTestTmux(fe)
			err := tm.NudgeSession(session, guardTestNudge)
			// One literal send: the attempt that failed. The retry must not
			// happen.
			if typed := fe.typed(); len(typed) != 1 {
				t.Fatalf("literal sends = %q, want only the failed first attempt", typed)
			}
			if got := fe.enterCount(); got != 0 {
				t.Fatalf("Enter sent %d times", got)
			}
			assertDeferred(t, err, tt.want)
		})
	}
}

// TestModelSwitchDismissLeavesAPersonsPromptAlone: the model-switch matcher
// finds its strings anywhere in the capture, scrollback included; its
// Down+Enter must not land on a question dialog or approval prompt.
func TestModelSwitchDismissLeavesAPersonsPromptAlone(t *testing.T) {
	scrollback := "Switch to gpt-5.4-mini for lower credit usage?\n  2. Keep current model\n"
	for name, screen := range map[string]string{
		"question dialog": scrollback + questionDialogFixture,
		"approval prompt": scrollback + approvalPromptFixture,
	} {
		t.Run(name, func(t *testing.T) {
			fe := &panePromptExecutor{screen: screen}
			tm, session := newGuardTestTmux(fe)
			tm.DismissModelSwitchModalIfPresent(session)
			if keys := fe.keyCalls(); len(keys) != 0 {
				t.Fatalf("model-switch dismiss sent %q into a person's prompt", keys)
			}
		})
	}
	t.Run("strings only in scrollback, attached draft", func(t *testing.T) {
		fe := &panePromptExecutor{screen: scrollback + humanDraftFixture, attached: true}
		tm, session := newGuardTestTmux(fe)
		tm.DismissModelSwitchModalIfPresent(session)
		if keys := fe.keyCalls(); len(keys) != 0 {
			t.Fatalf("model-switch dismiss sent %q over a person's draft", keys)
		}
	})
	t.Run("strings only in scrollback, idle composer", func(t *testing.T) {
		fe := &panePromptExecutor{screen: scrollback + idleComposerFixture}
		tm, session := newGuardTestTmux(fe)
		tm.DismissModelSwitchModalIfPresent(session)
		if keys := fe.keyCalls(); len(keys) != 0 {
			t.Fatalf("model-switch dismiss sent %q with no live modal", keys)
		}
	})
	t.Run("the modal itself is still dismissed", func(t *testing.T) {
		fe := &panePromptExecutor{screen: "Approaching rate limits\nSwitch to gpt-5.4-mini for lower credit usage?\n› 1. Switch to gpt-5.4-mini\n  2. Keep current model\n  3. Keep current model (never show again)\nPress enter to confirm or esc to go back"}
		tm, session := newGuardTestTmux(fe)
		tm.DismissModelSwitchModalIfPresent(session)
		if !fe.sentKey("Down") || fe.enterCount() != 1 {
			t.Fatalf("modal not dismissed: key calls %q", fe.keyCalls())
		}
	})
}

func TestNudgeDeferredAfterTyping(t *testing.T) {
	before := &NudgeDeferredError{Reason: NudgeDeferReasonQuestionDialog, Stage: nudgeGuardStageBeforeType}
	submit := &NudgeDeferredError{Reason: NudgeDeferReasonQuestionDialog, Stage: nudgeGuardStageBeforeSubmit}
	if NudgeDeferredAfterTyping(before) {
		t.Fatal("a before_type deferral reported as after typing")
	}
	if !NudgeDeferredAfterTyping(fmt.Errorf("wrapped: %w", submit)) {
		t.Fatal("a before_submit deferral not reported as after typing")
	}
	if !NudgeDeferredAfterTyping(fmt.Errorf("%w after 2 chunks: %w", errPartialPasteDelivery, before)) {
		t.Fatal("a cut-off chunked paste not reported as after typing")
	}
}

// guardIdleExecutor answers the human-prompt guard's own reads -- the pane
// capture and the attached-client count -- as an idle, detached pane, and
// passes every other call to inner. Tests of the key senders' retry and paste
// mechanics use it so their call-sequence assertions see only the sender's
// calls; the guard itself is tested in this file.
type guardIdleExecutor struct{ inner *fakeExecutor }

func (g guardIdleExecutor) execute(args []string) (string, error) {
	switch {
	case tmuxArgsContain(args, "capture-pane"):
		return idleComposerFixture, nil
	case tmuxArgsContain(args, "#{session_attached}"):
		return "0", nil
	}
	return g.inner.execute(args)
}

func (g guardIdleExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return g.execute(args)
}
