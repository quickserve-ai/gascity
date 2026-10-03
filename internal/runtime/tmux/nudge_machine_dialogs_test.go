package tmux

import (
	"context"
	"slices"
	"testing"
)

// Upstream dismisses three mid-session machine dialogs before a nudge
// (runtime.DismissMidSessionDialogs): the resume selector, the feedback survey
// and the provider session-limit chooser. The human-prompt guard (ga-ubfc7j)
// runs first. The session-limit chooser is a numbered list whose words a
// question for the operator can repeat exactly, so the guard defers on it like
// on any numbered prompt, and the dismissal's own keys are re-checked against
// the screen one key at a time. The chooser fixture is sessionLimitChooserPane
// (nudge_dismiss_besteffort_test.go).
const (
	// A question for the operator that satisfies the session-limit matcher
	// and its tail anchor word for word (GPT-6 Astra, review of #196).
	questionWordedLikeTheChooserPane = `When you hit your session limit, what should happen?
❯ 1. Stop and wait
  2. Upgrade`

	genericSelectionPane = `Which environment should I deploy to?
❯ 1. staging
  2. production`

	machineFeedbackPane = `How is Claude doing this session?
❯ 1: Bad    2: Fine    3: Great
  0: Dismiss`
)

// paneAfterFirstKeyExecutor answers every capture-pane with before until the
// first send-keys, and with after from then on: a screen that changes while a
// multi-key dismissal is in flight.
type paneAfterFirstKeyExecutor struct {
	calls    [][]string
	before   string
	after    string
	attached string // "#{session_attached}"; "" reads as unreadable (attached)
	keyed    bool
}

func (p *paneAfterFirstKeyExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	p.calls = append(p.calls, cp)
	if slices.Contains(args, "capture-pane") {
		if p.keyed {
			return p.after, nil
		}
		return p.before, nil
	}
	if slices.Contains(args, "#{session_attached}") {
		return p.attached, nil
	}
	if slices.Contains(args, "send-keys") {
		p.keyed = true
	}
	return "", nil
}

func (p *paneAfterFirstKeyExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return p.execute(args)
}

func sentKeys(calls [][]string) [][]string {
	var out [][]string
	for _, c := range calls {
		if slices.Contains(c, "send-keys") {
			out = append(out, c)
		}
	}
	return out
}

func TestNudgeSessionDefersOnTheSessionLimitChooser(t *testing.T) {
	ex := &paneAfterFirstKeyExecutor{before: sessionLimitChooserPane, after: sessionLimitChooserPane}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	err := tm.NudgeSession("agent-pane", "hello world")
	if reason, deferred := NudgeDeferredReason(err); !deferred || reason != NudgeDeferReasonSelectionPrompt {
		t.Fatalf("NudgeSession on the session-limit chooser = %v, want a %s deferral", err, NudgeDeferReasonSelectionPrompt)
	}
	if keys := sentKeys(ex.calls); len(keys) != 0 {
		t.Fatalf("sent keys on the session-limit chooser: %v", keys)
	}
}

func TestNudgeSessionStillDefersOnAGenericSelectionPrompt(t *testing.T) {
	ex := &paneAfterFirstKeyExecutor{before: genericSelectionPane, after: genericSelectionPane}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	err := tm.NudgeSession("agent-pane", "hello world")
	if reason, deferred := NudgeDeferredReason(err); !deferred || reason != NudgeDeferReasonSelectionPrompt {
		t.Fatalf("NudgeSession on a generic numbered prompt = %v, want a %s deferral", err, NudgeDeferReasonSelectionPrompt)
	}
	if keys := sentKeys(ex.calls); len(keys) != 0 {
		t.Fatalf("sent keys into a prompt meant for a person: %v", keys)
	}
}

// The dismissal's keys are raw send-keys. On its own it must never answer a
// question whose words match a machine dialog's matcher and anchor.
func TestDismissMidSessionDialogBeforeNudgeSendsNoKeysOverAQuestionWordedLikeTheChooser(t *testing.T) {
	ex := &paneAfterFirstKeyExecutor{before: questionWordedLikeTheChooserPane, after: questionWordedLikeTheChooserPane}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	if dismissKeyed(tm) {
		t.Fatal("dismissed = true over a question worded like the session-limit chooser")
	}
	if keys := sentKeys(ex.calls); len(keys) != 0 {
		t.Fatalf("answered the operator's question: %v", keys)
	}
}

func TestDismissMidSessionDialogBeforeNudgeSendsNoKeysOverAQuestionDialog(t *testing.T) {
	ex := &paneAfterFirstKeyExecutor{before: questionDialogFixture, after: questionDialogFixture}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	if dismissKeyed(tm) {
		t.Fatal("dismissed = true over a question dialog")
	}
	if keys := sentKeys(ex.calls); len(keys) != 0 {
		t.Fatalf("sent keys over a question dialog: %v", keys)
	}
}

// A multi-key dismissal re-reads the screen before each key. The feedback
// survey takes "0" then, after a settle delay, Enter. If the screen shows a
// question once "0" has gone out, the Enter must not follow, and the call
// still reports that a key went out so the caller re-reads before its own
// next key.
func TestDismissMidSessionDialogBeforeNudgeStopsWhenTheScreenChangesBetweenKeys(t *testing.T) {
	ex := &paneAfterFirstKeyExecutor{before: machineFeedbackPane, after: questionDialogFixture, attached: "0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	if !dismissKeyed(tm) {
		t.Fatal("dismissMidSessionDialogs = false after a key went out; the caller would skip its re-read")
	}
	keys := sentKeys(ex.calls)
	if len(keys) != 1 || !slices.Contains(keys[0], "0") {
		t.Fatalf("keys sent = %v, want exactly the first dismissal key \"0\"", keys)
	}
}

// paneAfterCapturesExecutor answers the first n capture-pane calls with
// before and every later one with after (a screen that changes between the
// guard's read and the dismissal's peek), and reports a detached session.
type paneAfterCapturesExecutor struct {
	calls    [][]string
	before   string
	after    string
	n        int
	captures int
	attached string // "#{session_attached}" answer; "" means "0"
}

func (p *paneAfterCapturesExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	p.calls = append(p.calls, cp)
	if slices.Contains(args, "capture-pane") {
		p.captures++
		if p.captures <= p.n {
			return p.before, nil
		}
		return p.after, nil
	}
	if slices.Contains(args, "#{session_attached}") {
		if p.attached != "" {
			return p.attached, nil
		}
		return "0", nil
	}
	return "", nil
}

func (p *paneAfterCapturesExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return p.execute(args)
}

// Astra round 2, BLOCK 1: the guard reads the feedback survey (a machine
// dialog it lets through); a question replaces it before the dismissal's
// peek. The dismissal refuses without sending a key, and the nudge must then
// defer instead of falling through to the raw C-u.
func TestNudgeSessionDefersWhenTheDismissalRefusesBeforeItsFirstKey(t *testing.T) {
	ex := &paneAfterCapturesExecutor{before: machineFeedbackPane, after: questionDialogFixture, n: 1}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	err := tm.NudgeSession("agent-pane", "hello world")
	if reason, deferred := NudgeDeferredReason(err); !deferred || reason != NudgeDeferReasonQuestionDialog {
		t.Fatalf("NudgeSession = %v, want a %s deferral", err, NudgeDeferReasonQuestionDialog)
	}
	if keys := sentKeys(ex.calls); len(keys) != 0 {
		t.Fatalf("sent keys after the dismissal refused a question: %v", keys)
	}
}

// Astra round 2 NIT, at the caller: "0" goes out on the feedback survey and
// a question is then on screen. Neither the survey's Enter nor the C-u clear
// may follow; the nudge defers.
func TestNudgeSessionDefersWhenADismissalKeyUncoversAQuestion(t *testing.T) {
	ex := &paneAfterFirstKeyExecutor{before: machineFeedbackPane, after: questionDialogFixture, attached: "0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	err := tm.NudgeSession("agent-pane", "hello world")
	if reason, deferred := NudgeDeferredReason(err); !deferred || reason != NudgeDeferReasonQuestionDialog {
		t.Fatalf("NudgeSession = %v, want a %s deferral", err, NudgeDeferReasonQuestionDialog)
	}
	keys := sentKeys(ex.calls)
	if len(keys) != 1 || !slices.Contains(keys[0], "0") {
		t.Fatalf("keys sent = %v, want only the survey's \"0\" (no Enter, no C-u, no text)", keys)
	}
}

// Astra round 2, BLOCK 2: on an ATTACHED session a person may be typing, and
// the per-key re-read cannot see a draft. The dismissal does not run there;
// a machine dialog on an attached pane defers the nudge.
func TestNudgeSessionDefersOnAMachineDialogWhenAttached(t *testing.T) {
	ex := &paneAfterFirstKeyExecutor{before: machineFeedbackPane, after: machineFeedbackPane, attached: "1"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	err := tm.NudgeSession("agent-pane", "hello world")
	if reason, deferred := NudgeDeferredReason(err); !deferred || reason != NudgeDeferReasonMachineDialogAttached {
		t.Fatalf("NudgeSession = %v, want a %s deferral", err, NudgeDeferReasonMachineDialogAttached)
	}
	if keys := sentKeys(ex.calls); len(keys) != 0 {
		t.Fatalf("sent keys on an attached session holding a machine dialog: %v", keys)
	}
}

// Astra round 3: the submit guard runs before EVERY key of a multi-key submit
// sequence. A question that appears inside the settle between Escape and
// Enter gets neither the Enter nor any later key.
func TestSendNudgeSubmitSequenceGuardsEveryKey(t *testing.T) {
	ex := &paneAfterFirstKeyExecutor{before: idleComposerFixture, after: questionDialogFixture, attached: "0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	err := tm.sendNudgeSubmitSequence("agent-pane", []string{"Escape", "Enter"})
	if reason, deferred := NudgeDeferredReason(err); !deferred || reason != NudgeDeferReasonQuestionDialog {
		t.Fatalf("sendNudgeSubmitSequence = %v, want a %s deferral before the second key", err, NudgeDeferReasonQuestionDialog)
	}
	keys := sentKeys(ex.calls)
	if len(keys) != 1 || !slices.Contains(keys[0], "Escape") {
		t.Fatalf("keys sent = %v, want only the first key (Escape)", keys)
	}
}

// Astra round 3: on an attached session the fresh visible read that looks
// for a machine dialog also honors a prompt meant for a person that appeared
// after the guard's read.
func TestNudgeSessionDefersOnAQuestionSeenByTheAttachedCheck(t *testing.T) {
	ex := &paneAfterCapturesExecutor{before: idleComposerFixture, after: questionDialogFixture, n: 1, attached: "1"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	err := tm.NudgeSession("agent-pane", "hello world")
	if reason, deferred := NudgeDeferredReason(err); !deferred || reason != NudgeDeferReasonQuestionDialog {
		t.Fatalf("NudgeSession = %v, want a %s deferral", err, NudgeDeferReasonQuestionDialog)
	}
	if keys := sentKeys(ex.calls); len(keys) != 0 {
		t.Fatalf("sent keys after the attached check saw a question: %v", keys)
	}
}

// Astra round 3: a survey row directly above a composer can still have a
// live question drawn below that composer. It must get no "0" and no Enter:
// the full guard runs before each of the dismisser's keys.
func TestFeedbackSurveyDismisserSendsNoKeysOverALiveQuestion(t *testing.T) {
	pane := feedbackSurveySessionFixture + "\n" + questionDialogFixture
	ex := &paneAfterFirstKeyExecutor{before: pane, after: pane, attached: "0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	tm.DismissFeedbackSurveyModalIfPresent("agent-pane")
	if keys := sentKeys(ex.calls); len(keys) != 0 {
		t.Fatalf("sent survey keys over a live question: %v", keys)
	}
}

// Astra round 4: the model-switch dismissal sends Down, settles, then Enter.
// If a question replaced the modal in between, the Enter must not follow.
func TestModelSwitchDismissStopsWhenAQuestionReplacesTheModalBetweenKeys(t *testing.T) {
	modal := "Approaching rate limits\nSwitch to gpt-5.4-mini for lower credit usage?\n› 1. Switch to gpt-5.4-mini\n  2. Keep current model\n  3. Keep current model (never show again)\nPress enter to confirm or esc to go back"
	ex := &paneAfterFirstKeyExecutor{before: modal, after: questionDialogFixture, attached: "0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	tm.DismissModelSwitchModalIfPresent("agent-pane")
	keys := sentKeys(ex.calls)
	if len(keys) != 1 || !slices.Contains(keys[0], "Down") {
		t.Fatalf("keys sent = %v, want only Down (no Enter onto the question)", keys)
	}
}

// Astra round 5: after Down, one capture shows the old model-switch strings
// with a live approval below them. The approval decides; Enter must not go.
func TestModelSwitchDismissStopsWhenAnApprovalAppearsBelowTheModal(t *testing.T) {
	modal := "Approaching rate limits\nSwitch to gpt-5.4-mini for lower credit usage?\n› 1. Switch to gpt-5.4-mini\n  2. Keep current model\n  3. Keep current model (never show again)\nPress enter to confirm or esc to go back"
	ex := &paneAfterFirstKeyExecutor{before: modal, after: modal + "\n" + approvalPromptFixture, attached: "0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	tm.DismissModelSwitchModalIfPresent("agent-pane")
	keys := sentKeys(ex.calls)
	if len(keys) != 1 || !slices.Contains(keys[0], "Down") {
		t.Fatalf("keys sent = %v, want only Down (no Enter onto the approval)", keys)
	}
}

// dismissKeyed runs the pre-nudge dismissal on the test pane and reports
// whether any dismissal key went out.
func dismissKeyed(tm *Tmux) bool {
	keyed, _ := tm.dismissMidSessionDialogs("agent-pane")
	return keyed
}

// scriptedCaptureExecutor answers every capture-pane with before until the
// first send-keys; from then on it answers the i-th capture with afterKey[i]
// (the last screen repeats). It reports a detached session.
type scriptedCaptureExecutor struct {
	calls    [][]string
	before   string
	afterKey []string
	keyed    bool
	captures int
}

func (s *scriptedCaptureExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	s.calls = append(s.calls, cp)
	if slices.Contains(args, "capture-pane") {
		if !s.keyed {
			return s.before, nil
		}
		i := min(s.captures, len(s.afterKey)-1)
		s.captures++
		return s.afterKey[i], nil
	}
	if slices.Contains(args, "#{session_attached}") {
		return "0", nil
	}
	if slices.Contains(args, "send-keys") {
		s.keyed = true
	}
	return "", nil
}

func (s *scriptedCaptureExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return s.execute(args)
}

// ga-da5vmz (nit on Astra round 5): the pre-Enter check must classify the
// screen and find the modal on ONE capture. Reverting to two reads (guard
// capture, then a separate modal capture) reopens a window in which the screen
// the guard approved is not the screen the modal check read. The approval
// test above cannot see that revert: its single post-Down screen refuses on
// either code. Two checks here can:
//
//   - the capture count between Down and Enter is exactly one;
//   - a sequence whose FIRST post-Down read is a person's numbered question and
//     whose SECOND read is the modal again gets no Enter. The one-capture code
//     refuses on the question; the two-capture code classified the question as
//     the modal's own selection reading and then found the modal on its second
//     read, and sent Enter onto the question.
func TestModelSwitchDismissDecidesEachKeyOnOneCapture(t *testing.T) {
	modal := "Approaching rate limits\nSwitch to gpt-5.4-mini for lower credit usage?\n› 1. Switch to gpt-5.4-mini\n  2. Keep current model\n  3. Keep current model (never show again)\nPress enter to confirm or esc to go back"

	t.Run("one capture between Down and Enter", func(t *testing.T) {
		ex := &scriptedCaptureExecutor{before: modal, afterKey: []string{modal}}
		tm := &Tmux{cfg: DefaultConfig(), exec: ex}

		tm.DismissModelSwitchModalIfPresent("agent-pane")
		down, enter := -1, -1
		for i, c := range ex.calls {
			if slices.Contains(c, "send-keys") && slices.Contains(c, "Down") {
				down = i
			}
			if slices.Contains(c, "send-keys") && slices.Contains(c, "Enter") {
				enter = i
			}
		}
		if down < 0 || enter < 0 {
			t.Fatalf("want Down then Enter on a live modal; calls: %v", ex.calls)
		}
		captures := 0
		for _, c := range ex.calls[down+1 : enter] {
			if slices.Contains(c, "capture-pane") {
				captures++
			}
		}
		if captures != 1 {
			t.Fatalf("capture-pane calls between Down and Enter = %d, want 1 (classify and match the modal on the same read)", captures)
		}
	})

	t.Run("a question on the first post-Down read gets no Enter", func(t *testing.T) {
		ex := &scriptedCaptureExecutor{before: modal, afterKey: []string{genericSelectionPane, modal}}
		tm := &Tmux{cfg: DefaultConfig(), exec: ex}

		tm.DismissModelSwitchModalIfPresent("agent-pane")
		keys := sentKeys(ex.calls)
		if len(keys) != 1 || !slices.Contains(keys[0], "Down") {
			t.Fatalf("keys sent = %v, want only Down (no Enter onto the question)", keys)
		}
	})
}
