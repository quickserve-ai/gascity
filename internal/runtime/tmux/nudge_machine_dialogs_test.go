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
	calls  [][]string
	before string
	after  string
	keyed  bool
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
	if tm.dismissMidSessionDialogBeforeNudge("agent-pane") {
		t.Fatal("dismissed = true over a question worded like the session-limit chooser")
	}
	if keys := sentKeys(ex.calls); len(keys) != 0 {
		t.Fatalf("answered the operator's question: %v", keys)
	}
}

func TestDismissMidSessionDialogBeforeNudgeSendsNoKeysOverAQuestionDialog(t *testing.T) {
	ex := &paneAfterFirstKeyExecutor{before: questionDialogFixture, after: questionDialogFixture}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	if tm.dismissMidSessionDialogBeforeNudge("agent-pane") {
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
	ex := &paneAfterFirstKeyExecutor{before: machineFeedbackPane, after: questionDialogFixture}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	if !tm.dismissMidSessionDialogBeforeNudge("agent-pane") {
		t.Fatal("dismissMidSessionDialogBeforeNudge = false after a key went out; the caller would skip its re-read")
	}
	keys := sentKeys(ex.calls)
	if len(keys) != 1 || !slices.Contains(keys[0], "0") {
		t.Fatalf("keys sent = %v, want exactly the first dismissal key \"0\"", keys)
	}
}
