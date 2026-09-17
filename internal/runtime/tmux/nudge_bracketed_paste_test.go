package tmux

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// bracketedPasteExecutor is a recorded tmux runner for the nudge send path. It
// answers show-environment with the configured GC_PROVIDER (an empty provider
// reports the variable unset, and the empty display-message reply then makes
// the process sniff come back negative), answers the #{bracket_paste_flag} read
// with pasteFlag or pasteFlagErr, and reads each load-buffer file at load time,
// because pasteLiteralText deletes that file before returning.
type bracketedPasteExecutor struct {
	provider     string
	pasteFlag    string
	pasteFlagErr error
	// panePID is the answer to a #{pane_pid} read. Empty means "no pane",
	// which makes the provider process sniff -- and so the agent-liveness
	// fallback in paneBracketsPaste -- come back negative.
	panePID string
	calls   [][]string
	loaded  []string
}

func (f *bracketedPasteExecutor) execute(args []string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	switch {
	case tmuxArgsContain(args, "#{bracket_paste_flag}"):
		return f.pasteFlag, f.pasteFlagErr
	case tmuxArgsContain(args, "#{pane_pid}"):
		return f.panePID, nil
	case tmuxArgsContain(args, "show-environment"):
		if f.provider == "" {
			return "", errors.New("unknown variable: GC_PROVIDER")
		}
		return "GC_PROVIDER=" + f.provider, nil
	case tmuxArgsContain(args, "load-buffer"):
		data, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return "", err
		}
		f.loaded = append(f.loaded, string(data))
	}
	return "", nil
}

func (f *bracketedPasteExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

// literalSends returns the text argument of every `send-keys -l` call.
func (f *bracketedPasteExecutor) literalSends() []string {
	var sent []string
	for _, call := range f.calls {
		if tmuxArgsContain(call, "send-keys") && tmuxArgsContain(call, "-l") {
			sent = append(sent, call[len(call)-1])
		}
	}
	return sent
}

func (f *bracketedPasteExecutor) callsWith(verb string) [][]string {
	var matched [][]string
	for _, call := range f.calls {
		if tmuxArgsContain(call, verb) {
			matched = append(matched, call)
		}
	}
	return matched
}

// sizedNudge returns an ASCII, single-line nudge of exactly n bytes whose head
// and tail are distinguishable, so a delivery that kept only one end would not
// compare equal.
func sizedNudge(n int) string {
	const head, tail = "HEAD-SENTINEL ", " TAIL-SENTINEL"
	return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
}

// multiLineNudge returns a multi-line nudge of at least n bytes.
func multiLineNudge(n int) string {
	var b strings.Builder
	b.WriteString("HEAD-SENTINEL relayed ask\n")
	for b.Len() < n {
		b.WriteString("a line of relayed context that a seat has to read in full\n")
	}
	b.WriteString("TAIL-SENTINEL")
	return b.String()
}

// TestNudgeSendPathBracketsLongOrMultilineClaudeText is the ga-6qfgdo
// regression table. Claude Code reads its pty in 1022-byte chunks; an
// UNBRACKETED run over 800 characters goes to its paste handler and a shorter
// one to its typed editor, which replaces the draft. So a nudge that arrives as
// several reads loses everything but the last read, and every newline is lost
// under modifyOtherKeys. A bracketed paste arrives as one paste event instead.
// Every Claude nudge over 512 bytes or with a newline must therefore reach tmux
// as load-buffer + `paste-buffer -p`, never as `send-keys -l`.
//
// sendKeysLiteralWithRetry is the text sender NudgeSession and NudgePane use.
func TestNudgeSendPathBracketsLongOrMultilineClaudeText(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		wantPaste bool
	}{
		{"512 bytes stays on send-keys", sizedNudge(512), false},
		{"short single line stays on send-keys", "gc: you have mail", false},
		{"513 bytes", sizedNudge(513), true},
		{"800 bytes", sizedNudge(800), true},
		{"801 bytes", sizedNudge(801), true},
		{"1022 bytes", sizedNudge(1022), true},
		{"1023 bytes", sizedNudge(1023), true},
		{"2646 bytes", sizedNudge(2646), true},
		{"4096 bytes", sizedNudge(maxSendKeysLiteralLen), true},
		{"4097 bytes keeps the oversized paste path", sizedNudge(maxSendKeysLiteralLen + 1), true},
		{"short multi-line", "HEAD line one\nline two\nTAIL line three", true},
		{"trailing newline only", "gc: you have mail\n", true},
		{"2646-byte multi-line", multiLineNudge(2646), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fe := &bracketedPasteExecutor{provider: "claude", pasteFlag: "1"}
			tm := NewTmuxWithConfig(DefaultConfig())
			tm.exec = fe

			if err := tm.sendKeysLiteralWithRetry("%1", tt.text, time.Second); err != nil {
				t.Fatalf("sendKeysLiteralWithRetry(%d bytes) = %v, want nil", len(tt.text), err)
			}

			literal := fe.literalSends()
			loads := fe.callsWith("load-buffer")
			pastes := fe.callsWith("paste-buffer")
			if !tt.wantPaste {
				if len(literal) != 1 || literal[0] != tt.text {
					t.Fatalf("%d-byte claude nudge: send-keys -l texts = %d call(s), want exactly one carrying the whole text; calls: %q", len(tt.text), len(literal), fe.calls)
				}
				if len(loads) != 0 || len(pastes) != 0 {
					t.Fatalf("%d-byte claude nudge used the paste buffer, want plain send-keys -l; calls: %q", len(tt.text), fe.calls)
				}
				return
			}

			if len(literal) != 0 {
				t.Fatalf("%d-byte claude nudge (newlines=%d) went out as UNBRACKETED send-keys -l, which Claude Code reassembles tail-only; calls: %q",
					len(tt.text), strings.Count(tt.text, "\n"), fe.calls)
			}
			if len(loads) != 1 || len(pastes) != 1 {
				t.Fatalf("%d-byte claude nudge: load-buffer calls = %d, paste-buffer calls = %d, want 1 and 1; calls: %q", len(tt.text), len(loads), len(pastes), fe.calls)
			}
			paste := "\x00" + strings.Join(pastes[0], "\x00") + "\x00"
			for _, want := range []string{"\x00-p\x00", "\x00-t\x00%1\x00"} {
				if !strings.Contains(paste, want) {
					t.Fatalf("paste-buffer call = %q, missing %q (without -p the paste is not bracketed)", pastes[0], want)
				}
			}
			if len(fe.loaded) != 1 || fe.loaded[0] != tt.text {
				t.Fatalf("paste buffer did not carry the nudge byte-for-byte (loaded %d buffer(s))", len(fe.loaded))
			}
		})
	}
}

// TestNudgeSendPathLeavesNonClaudeProvidersOnSendKeys pins that the bracketed
// routing is Claude-only. Every other family keeps its historical delivery for
// text up to maxSendKeysLiteralLen: their TUIs' handling of a mid-size bracketed
// paste followed by their submit sequence has not been verified (codex's submit
// sequence, for one, was tuned against a send-keys burst). Above the limit every
// family already pastes, and that stays true.
func TestNudgeSendPathLeavesNonClaudeProvidersOnSendKeys(t *testing.T) {
	text := multiLineNudge(2646)
	for _, provider := range []string{"codex", "gemini", "omp", "pi", "opencode", "copilot", "kimi", ""} {
		t.Run("provider="+provider, func(t *testing.T) {
			// The pane reports bracketed paste on, so only the family check
			// keeps these providers on send-keys.
			fe := &bracketedPasteExecutor{provider: provider, pasteFlag: "1"}
			tm := NewTmuxWithConfig(DefaultConfig())
			tm.exec = fe

			if err := tm.sendKeysLiteralWithRetry("%1", text, time.Second); err != nil {
				t.Fatalf("sendKeysLiteralWithRetry() = %v, want nil", err)
			}
			if literal := fe.literalSends(); len(literal) != 1 || literal[0] != text {
				t.Fatalf("provider %q: send-keys -l texts = %d call(s), want exactly one carrying the whole text; calls: %q", provider, len(literal), fe.calls)
			}
			if pastes := fe.callsWith("paste-buffer"); len(pastes) != 0 {
				t.Fatalf("provider %q: nudge used paste-buffer, want unchanged send-keys -l delivery; calls: %q", provider, fe.calls)
			}
		})
	}

	t.Run("over the send-keys limit every family still pastes", func(t *testing.T) {
		fe := &bracketedPasteExecutor{provider: "codex"}
		tm := NewTmuxWithConfig(DefaultConfig())
		tm.exec = fe

		oversized := sizedNudge(maxSendKeysLiteralLen + 1)
		if err := tm.sendKeysLiteralWithRetry("%1", oversized, time.Second); err != nil {
			t.Fatalf("sendKeysLiteralWithRetry() = %v, want nil", err)
		}
		if literal := fe.literalSends(); len(literal) != 0 {
			t.Fatalf("oversized codex nudge went out as send-keys -l; calls: %q", fe.calls)
		}
		if pastes := fe.callsWith("paste-buffer"); len(pastes) != 1 {
			t.Fatalf("oversized codex nudge: paste-buffer calls = %d, want 1; calls: %q", len(pastes), fe.calls)
		}
	})
}

// TestNudgeSendPathPastesOnlyWhenPaneBracketPasteFlagIsOn pins the guard in
// front of the claude paste route. `paste-buffer -p` brackets a paste only when
// the pane's application has turned bracketed paste on; with
// #{bracket_paste_flag} at 0, tmux writes a multi-line nudge raw as
// "line1\rline2\r...", and each CR submits a line on its own. So a claude nudge
// goes out as a paste only when the pane is confirmed to bracket it. A flag of
// "0" refuses outright; an empty answer or a failed read means the server cannot
// tell (tmux before 3.7), and this executor reports no pane process, so the
// agent-liveness fallback is negative too and the send-keys path this nudge took
// before ga-6qfgdo is kept. None of them is an error.
//
// The fallback itself is covered by
// TestNudgeSendPathConfirmsBracketedPasteByAgentLiveness (ga-p93v6w).
func TestNudgeSendPathPastesOnlyWhenPaneBracketPasteFlagIsOn(t *testing.T) {
	text := multiLineNudge(900)
	tests := []struct {
		name      string
		flag      string
		flagErr   error
		wantPaste bool
	}{
		{name: "flag 1 pastes", flag: "1", wantPaste: true},
		{name: "flag 0 keeps send-keys", flag: "0"},
		{name: "empty flag (tmux before 3.7 lacks the format) with no live agent keeps send-keys", flag: ""},
		{name: "flag read error with no live agent keeps send-keys", flagErr: errors.New("can't find pane: %1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fe := &bracketedPasteExecutor{provider: "claude", pasteFlag: tt.flag, pasteFlagErr: tt.flagErr}
			tm := NewTmuxWithConfig(DefaultConfig())
			tm.exec = fe

			if err := tm.sendKeysLiteralWithRetry("%1", text, time.Second); err != nil {
				t.Fatalf("sendKeysLiteralWithRetry() = %v, want nil", err)
			}

			literal := fe.literalSends()
			pastes := fe.callsWith("paste-buffer")
			if tt.wantPaste {
				if len(literal) != 0 || len(pastes) != 1 {
					t.Fatalf("flag %q: send-keys -l calls = %d, paste-buffer calls = %d, want 0 and 1; calls: %q", tt.flag, len(literal), len(pastes), fe.calls)
				}
			} else {
				if len(pastes) != 0 || len(fe.callsWith("load-buffer")) != 0 {
					t.Fatalf("flag %q (err %v): nudge used the paste buffer into a pane without bracketed paste, which submits it line by line; calls: %q", tt.flag, tt.flagErr, fe.calls)
				}
				if len(literal) != 1 || literal[0] != text {
					t.Fatalf("flag %q (err %v): send-keys -l texts = %d call(s), want exactly one carrying the whole text; calls: %q", tt.flag, tt.flagErr, len(literal), fe.calls)
				}
			}

			reads := fe.callsWith("#{bracket_paste_flag}")
			if len(reads) != 1 || !tmuxArgsContain(reads[0], "display-message") || !tmuxArgsContain(reads[0], "%1") {
				t.Fatalf("bracket_paste_flag reads = %q, want one display-message against %%1; calls: %q", reads, fe.calls)
			}
		})
	}
}

// TestPaneShowsDrainedComposerTreatsClaudePastePlaceholderAsDraft covers the
// submit-confirm fallback once long nudges are pasted. Claude Code collapses a
// paste over 800 characters, or with more than two line breaks, into a
// "[Pasted text #N ...]" placeholder, so an unsubmitted pasted nudge sits in the
// composer as that placeholder instead of its first line. Read as "drained", it
// would be reported ErrNudgeSubmitDeliveredUnobserved, which callers never
// retry, and the nudge would be lost while it still sat unsubmitted.
func TestPaneShowsDrainedComposerTreatsClaudePastePlaceholderAsDraft(t *testing.T) {
	sent := multiLineNudge(2646)
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"placeholder with line count still in composer", []string{"❯ [Pasted text #1 +46 lines]"}, false},
		{"single-line placeholder still in composer", []string{"❯ [Pasted text #3]"}, false},
		{"placeholder in transcript, composer bare", []string{"❯ [Pasted text #1 +46 lines]", "✻ Worked for 3s", "❯ "}, true},
		{"bare composer", []string{"❯ "}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := paneShowsDrainedComposer(tt.lines, sent); got != tt.want {
				t.Fatalf("paneShowsDrainedComposer(%q) = %v, want %v", tt.lines, got, tt.want)
			}
		})
	}
}
