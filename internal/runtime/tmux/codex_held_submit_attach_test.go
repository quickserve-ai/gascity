package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// attachingCodexPane is a codexTurnPane a person attaches to partway through a
// nudge whose submit a running turn holds back, and then types into.
type attachingCodexPane struct {
	*codexTurnPane

	// attachAt is when the person attaches: attachAfterTyping, right after
	// the nudge types its message; attachAfterFirstClearPress, right after
	// the first Ctrl-U that follows it; "" never.
	attachAt string
	// typed is what the person types into the composer once attached.
	typed string

	afterTyping bool
}

const (
	attachAfterTyping           = "typed"
	attachAfterFirstClearPress  = "clear"
	codexHeldSubmitPersonsWords = " and the words I was typing"
)

func (p *attachingCodexPane) execute(args []string) (string, error) {
	out, err := p.codexTurnPane.execute(args)
	sub := tmuxSubcommand(args)
	typing := sub == "paste-buffer" || (sub == "send-keys" && slices.Contains(args, "-l"))
	ctrlU := sub == "send-keys" && slices.Contains(args, "C-u")
	p.mu.Lock()
	defer p.mu.Unlock()
	attach := (p.attachAt == attachAfterTyping && typing) ||
		(p.attachAt == attachAfterFirstClearPress && p.afterTyping && ctrlU)
	if typing {
		p.afterTyping = true
	}
	if attach && !p.attached {
		p.attached = true
		p.draft += p.typed
	}
	return out, err
}

func (p *attachingCodexPane) executeCtx(_ context.Context, args []string) (string, error) {
	return p.execute(args)
}

// ctrlUAfterTyping counts the Ctrl-U presses that reached the pane after the
// nudge typed its message: the presses of the held submit's clear, not the
// clear before the paste.
func ctrlUAfterTyping(events []string) int {
	typed := slices.IndexFunc(events, func(e string) bool {
		return strings.HasPrefix(e, "type:") || strings.HasPrefix(e, "paste:")
	})
	if typed < 0 {
		return 0
	}
	n := 0
	for _, e := range events[typed+1:] {
		if e == "key:C-u" {
			n++
		}
	}
	return n
}

// TestCodexHeldSubmitClearStopsForAPersonWhoAttachesDuringTheHold is the
// human-edit-during-hold case of ga-ubfc7j. The nudge types into a detached
// pane whose startup turn then holds its submit for the whole confirm window;
// a person attaches during that hold and types beside the message. The clear
// of the held message sends no Ctrl-U once the client count is not zero,
// checked before the clear and again before every press, so the person's
// words survive and the nudge fails as held (errNudgeSubmitHeld), never as a
// deferral. Nobody attaching is upstream's clear, unchanged: the message is
// cleared and the nudge defers with runtime.ErrSessionBusy.
func TestCodexHeldSubmitClearStopsForAPersonWhoAttachesDuringTheHold(t *testing.T) {
	const multiline = "first line of the nudge\nsecond line\nthird line"
	for _, tc := range []struct {
		name     string
		message  string
		setup    func(*codexTurnPane)
		attachAt string
		// wantHeld: the nudge fails with errNudgeSubmitHeld; otherwise it
		// defers with runtime.ErrSessionBusy, its message cleared.
		wantHeld bool
		// wantPresses is how many Ctrl-U presses follow the typed message;
		// -1 means at least one.
		wantPresses int
	}{
		{
			name:        "a person attaches after the message is typed",
			message:     codexTurnMarker,
			attachAt:    attachAfterTyping,
			wantHeld:    true,
			wantPresses: 0,
		},
		{
			name:        "a person attaches between two presses of the clear",
			message:     multiline,
			setup:       func(p *codexTurnPane) { p.ctrlUKillsLine = true },
			attachAt:    attachAfterFirstClearPress,
			wantHeld:    true,
			wantPresses: 1,
		},
		{
			name:        "nobody attaches",
			message:     codexTurnMarker,
			wantPresses: -1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := "codex-held-attach-" + strings.ReplaceAll(tc.name, " ", "-")
			t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
			var pane *attachingCodexPane
			var err error
			synctest.Test(t, func(t *testing.T) {
				inner := newCodexTurnPane(t, session)
				inner.startEarlierTurn(time.Hour)
				if tc.setup != nil {
					tc.setup(inner)
				}
				pane = &attachingCodexPane{codexTurnPane: inner, attachAt: tc.attachAt, typed: codexHeldSubmitPersonsWords}
				tm := &Tmux{cfg: DefaultConfig(), exec: pane, codexRolloutHeld: inner.heldRollout}
				err = tm.nudgeStartupSession(session, tc.message)
				inner.stop()
			})

			_, aborted, events := pane.snapshot()
			if keys := pane.keysPressed(); slices.Contains(keys, "Escape") || slices.Contains(keys, "Enter") {
				t.Fatalf("keys = %q, want no submit into the running turn", keys)
			}
			if aborted != 0 {
				t.Fatalf("aborted %d turn(s), want none", aborted)
			}
			presses := ctrlUAfterTyping(events)
			if tc.wantPresses >= 0 && presses != tc.wantPresses {
				t.Fatalf("%d Ctrl-U press(es) after the typed message, want %d; events %q", presses, tc.wantPresses, events)
			}
			if tc.wantPresses < 0 && presses == 0 {
				t.Fatalf("no Ctrl-U after the typed message, want the held message cleared; events %q", events)
			}
			draft := pane.composer()
			if !tc.wantHeld {
				if !errors.Is(err, runtime.ErrSessionBusy) || errors.Is(err, errNudgeSubmitHeld) {
					t.Fatalf("nudgeStartupSession = %v, want runtime.ErrSessionBusy", err)
				}
				if draft != "" {
					t.Fatalf("composer holds %q, want the typed message cleared", draft)
				}
				return
			}
			if !errors.Is(err, errNudgeSubmitHeld) || errors.Is(err, runtime.ErrSessionBusy) || errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed) {
				t.Fatalf("nudgeStartupSession = %v, want errNudgeSubmitHeld, neither runtime.ErrSessionBusy nor unconfirmed", err)
			}
			if !strings.Contains(err.Error(), "a client attached") && !strings.Contains(err.Error(), "a client was attached") {
				t.Fatalf("nudgeStartupSession = %v, want its text to say a client was attached", err)
			}
			if !strings.HasSuffix(draft, codexHeldSubmitPersonsWords) {
				t.Fatalf("composer holds %q, want the person's words %q kept", draft, codexHeldSubmitPersonsWords)
			}
			if tc.wantPresses == 0 && draft != tc.message+codexHeldSubmitPersonsWords {
				t.Fatalf("composer holds %q, want the message and the person's words untouched", draft)
			}
		})
	}
}
