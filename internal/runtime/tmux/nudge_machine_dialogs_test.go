package tmux

import (
	"context"
	"slices"
	"testing"
)

// The mid-session dialogs runtime.DismissMidSessionDialogs clears before a
// nudge are machine dialogs: they are dismissed on purpose, because an
// unattended session parked on one never recovers (the session-limit chooser
// does not clear itself when the window resets). The human-prompt guard
// (ga-ubfc7j) runs before that dismissal. The session-limit chooser is a
// numbered list, which the guard reads as a selection prompt, so without an
// exemption every nudge defers on it forever and the dismissal never runs.
// Fixtures mirror internal/runtime's dialog_midsession_test.go.
const (
	machineSessionLimitPane = `You've hit your session limit · resets 8:40am
❯ 1. Stop and wait for limit to reset
  2. Upgrade`

	// A numbered prompt that is NOT one of the known machine dialogs keeps the
	// guard's selection-prompt deferral: the exemption is the dismissal's own
	// match rule, nothing wider.
	genericSelectionPane = `Which environment should I deploy to?
❯ 1. staging
  2. production`
)

// paneUntilEnterExecutor answers every capture-pane with pane until the first
// send-keys carrying "Enter" (the dismissal), and with empty output after it,
// the way a machine dialog retires once it is answered. Everything else is
// silent, as in dialogThenSilentExecutor.
type paneUntilEnterExecutor struct {
	calls   [][]string
	pane    string
	retired bool
}

func (p *paneUntilEnterExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	p.calls = append(p.calls, cp)
	if slices.Contains(args, "send-keys") && slices.Contains(args, "Enter") {
		p.retired = true
	}
	if slices.Contains(args, "capture-pane") && !p.retired {
		return p.pane, nil
	}
	return "", nil
}

func (p *paneUntilEnterExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return p.execute(args)
}

func TestNudgeSessionClearsTheSessionLimitChooserInsteadOfDeferring(t *testing.T) {
	ex := &paneUntilEnterExecutor{pane: machineSessionLimitPane}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	if err := tm.NudgeSession("agent-pane", "hello world"); err != nil {
		t.Fatalf("NudgeSession on the session-limit chooser: %v (want the chooser dismissed and the nudge delivered)", err)
	}
	oneIdx, enterIdx, literalIdx := -1, -1, -1
	for i, c := range ex.calls {
		switch {
		case slices.Contains(c, "send-keys") && slices.Contains(c, "-l") && slices.Contains(c, "hello world") && literalIdx == -1:
			literalIdx = i
		case slices.Contains(c, "send-keys") && slices.Contains(c, "1") && oneIdx == -1:
			oneIdx = i
		case slices.Contains(c, "send-keys") && slices.Contains(c, "Enter") && enterIdx == -1:
			enterIdx = i
		}
	}
	if oneIdx == -1 || enterIdx == -1 || literalIdx == -1 {
		t.Fatalf("want dismissal keys 1, Enter then the literal paste; calls: %v", ex.calls)
	}
	if !(oneIdx < enterIdx && enterIdx < literalIdx) {
		t.Errorf("order: 1 at %d, Enter at %d, paste at %d; want 1 < Enter < paste", oneIdx, enterIdx, literalIdx)
	}
}

func TestNudgeSessionStillDefersOnAGenericSelectionPrompt(t *testing.T) {
	ex := &paneUntilEnterExecutor{pane: genericSelectionPane}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	err := tm.NudgeSession("agent-pane", "hello world")
	if reason, deferred := NudgeDeferredReason(err); !deferred || reason != NudgeDeferReasonSelectionPrompt {
		t.Fatalf("NudgeSession on a generic numbered prompt = %v, want a %s deferral", err, NudgeDeferReasonSelectionPrompt)
	}
	for _, c := range ex.calls {
		if slices.Contains(c, "send-keys") {
			t.Fatalf("sent keys into a prompt meant for a person: %v", c)
		}
	}
}

// The dismissal's own keys must never land on a question or approval. The
// guard ran first on the same screen, but the dismissal sends raw send-keys,
// so it re-reads what it peeked: a question dialog under it means no keys.
func TestDismissMidSessionDialogBeforeNudgeSendsNoKeysOverAQuestionDialog(t *testing.T) {
	fe := &fakeExecutor{outs: []string{questionDialogFixture}}
	tm := &Tmux{cfg: DefaultConfig(), exec: fe}
	if tm.dismissMidSessionDialogBeforeNudge("agent-pane") {
		t.Fatal("dismissed = true over a question dialog")
	}
	for _, c := range fe.calls {
		if slices.Contains(c, "send-keys") {
			t.Fatalf("sent keys over a question dialog: %v", c)
		}
	}
}
