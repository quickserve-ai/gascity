package tmux

import (
	"errors"
	"testing"
	"time"
)

// TestNudgeSendPathRefusesLongClaudeNudgeIntoUnbracketedPane is the sender
// half of pl-7tq. When a claude pane's bracketed-paste mode cannot be
// confirmed, the nudge path used to type a long nudge as `send-keys -l`
// keystrokes. Measured against a real Claude Code 2.1.283 receiver, keystroke
// nudges of 1-4 KB arrived split across several paste frames, head-cut (378
// bytes starting mid-number) or as two stray bytes, while the sender saw
// success. A nudge over claudeMaxUnbracketedNudgeBytes must now fail with
// ErrNudgeUnbracketedTooLong, sending nothing, so the cut is never reported
// as delivered.
func TestNudgeSendPathRefusesLongClaudeNudgeIntoUnbracketedPane(t *testing.T) {
	tests := []struct {
		name    string
		flag    string
		flagErr error
		text    string
	}{
		{name: "flag 0, single line just past the keystroke limit", flag: "0", text: sizedNudge(claudeMaxUnbracketedNudgeBytes + 1)},
		{name: "flag 0, multi-line", flag: "0", text: multiLineNudge(900)},
		{name: "flag 0, at the oversize band", flag: "0", text: sizedNudge(maxSendKeysLiteralLen)},
		{name: "empty flag and no live agent", flag: "", text: sizedNudge(2048)},
		{name: "flag read error and no live agent", flagErr: errors.New("can't find pane: %1"), text: sizedNudge(2048)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fe := &bracketedPasteExecutor{provider: "claude", pasteFlag: tt.flag, pasteFlagErr: tt.flagErr}
			tm := NewTmuxWithConfig(DefaultConfig())
			tm.exec = fe

			err := tm.sendKeysLiteralWithRetry("%1", tt.text, time.Second)
			if !errors.Is(err, ErrNudgeUnbracketedTooLong) {
				t.Fatalf("sendKeysLiteralWithRetry(%d bytes) = %v, want ErrNudgeUnbracketedTooLong", len(tt.text), err)
			}
			if sent := fe.literalSends(); len(sent) != 0 {
				t.Fatalf("refused nudge was still typed: %d send-keys -l call(s)", len(sent))
			}
			if len(fe.callsWith("paste-buffer")) != 0 || len(fe.callsWith("load-buffer")) != 0 {
				t.Fatalf("refused nudge was pasted into a pane that does not bracket pastes; calls: %q", fe.calls)
			}
		})
	}
}

// TestNudgeSendPathKeepsShortClaudeNudgeOnKeystrokesWhenUnbracketed pins the
// boundary: at claudeMaxUnbracketedNudgeBytes and below, a claude nudge into
// a pane that does not bracket pastes is still typed whole, as before.
func TestNudgeSendPathKeepsShortClaudeNudgeOnKeystrokesWhenUnbracketed(t *testing.T) {
	for _, text := range []string{sizedNudge(claudeMaxUnbracketedNudgeBytes), "HEAD line one\nline two TAIL"} {
		fe := &bracketedPasteExecutor{provider: "claude", pasteFlag: "0"}
		tm := NewTmuxWithConfig(DefaultConfig())
		tm.exec = fe
		if err := tm.sendKeysLiteralWithRetry("%1", text, time.Second); err != nil {
			t.Fatalf("sendKeysLiteralWithRetry(%d bytes) = %v, want nil", len(text), err)
		}
		if sent := fe.literalSends(); len(sent) != 1 || sent[0] != text {
			t.Fatalf("want one send-keys -l carrying the whole %d-byte text; calls: %q", len(text), fe.calls)
		}
	}
}

// TestStartupSendPathDoesNotRefuseUnbracketedClaudeText keeps the refusal on
// the nudge path only. The startup path delivers a role prompt while the agent
// is deliberately not running yet, so the pane cannot bracket pastes; refusing
// there would break every seat's start on a server that cannot report the
// flag (see TestStartupSendPathNeverConfirmsByAgentLiveness).
func TestStartupSendPathDoesNotRefuseUnbracketedClaudeText(t *testing.T) {
	fe := &bracketedPasteExecutor{provider: "claude", pasteFlag: ""}
	tm := NewTmuxWithConfig(DefaultConfig())
	tm.exec = fe
	text := sizedNudge(2048)
	if err := tm.sendLiteralText("%1", text); err != nil {
		t.Fatalf("sendLiteralText() = %v, want nil", err)
	}
	if sent := fe.literalSends(); len(sent) != 1 || sent[0] != text {
		t.Fatalf("startup text not typed whole; calls: %q", fe.calls)
	}
}
