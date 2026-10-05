package tmux

import (
	"context"
	"slices"
	"testing"
	"time"
)

// surveyInScrollbackExecutor models a pane whose VISIBLE screen is an idle
// Claude Code prompt with no feedback survey, while the 120-line history
// window ("capture-pane -S -120") still holds an earlier survey's option row
// above later output. Visible-only captures (no "-S") see no survey.
type surveyInScrollbackExecutor struct {
	calls [][]string
}

const surveyScrollbackHistory = `● How is Claude doing this session? (optional)
  1: Bad    2: Fine   3: Good   0: Dismiss

⏺ Done — pushed the branch and replied on the PR.
` + surveyVisibleIdle

const surveyVisibleIdle = `
╭──────────────────────────────────────────────────────────╮
│ ❯                                                        │
╰──────────────────────────────────────────────────────────╯
  ⏵⏵ bypass permissions on (shift+tab to cycle)`

func (e *surveyInScrollbackExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	e.calls = append(e.calls, cp)
	if slices.Contains(args, "capture-pane") {
		if slices.Contains(args, "-S") {
			return surveyScrollbackHistory, nil
		}
		return surveyVisibleIdle, nil
	}
	return "", nil
}

func (e *surveyInScrollbackExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// hq-hfsut (2): a nudge into a pane with no survey on screen must not type
// the survey's "0" dismiss key; otherwise "0" + Enter lands in the empty
// composer as a user message.
func TestNudgeSessionSendsNoSurveyDismissKeyWhenSurveyNotOnScreen(t *testing.T) {
	ex := &surveyInScrollbackExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	if err := tm.NudgeSession("agent-pane", "hello world"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}
	var zeros [][]string
	for _, c := range ex.calls {
		if slices.Contains(c, "send-keys") && slices.Contains(c, "0") {
			zeros = append(zeros, c)
		}
	}
	if len(zeros) != 0 {
		t.Fatalf("sent the survey dismiss key %d time(s) into a pane with no survey on screen: %v", len(zeros), zeros)
	}
}

// ga-da5vmz (gap 2): a survey row that is still on the VISIBLE screen but sits
// above later agent output is stale -- the live survey is drawn directly above
// the composer. The dismisser must send nothing there: its "0" would land in
// the idle composer.
func TestFeedbackSurveyDismisserSendsNoKeysForASurveyAboveLaterOutput(t *testing.T) {
	ex := &paneAfterFirstKeyExecutor{before: surveyScrollbackHistory, after: surveyScrollbackHistory, attached: "0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	if err := tm.DismissFeedbackSurveyModalIfPresent("agent-pane"); err != nil {
		t.Fatalf("DismissFeedbackSurveyModalIfPresent: %v", err)
	}
	if keys := sentKeys(ex.calls); len(keys) != 0 {
		t.Fatalf("sent survey keys for a survey above later output on an idle composer: %v", keys)
	}
}

// The same stale screen through the whole nudge: no "0", and the nudge text is
// the first thing typed after the C-u clear.
func TestNudgeSessionSendsNoSurveyKeyForASurveyAboveLaterOutput(t *testing.T) {
	ex := &paneAfterFirstKeyExecutor{before: surveyScrollbackHistory, after: surveyScrollbackHistory, attached: "0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	if err := tm.NudgeSession("agent-pane", "hello world"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}
	keys := sentKeys(ex.calls)
	var before [][]string
	for _, k := range keys {
		if slices.Contains(k, "-l") {
			break
		}
		if !slices.Contains(k, "C-u") {
			before = append(before, k)
		}
	}
	if len(before) != 0 {
		t.Fatalf("keys sent before the nudge text (besides C-u): %v", before)
	}
}

// Control: a survey that IS the live block above the composer is still
// dismissed, with exactly "0" (no Enter: the survey acts on the digit after
// its debounce, upstream #7013), for both survey variants.
func TestFeedbackSurveyDismisserDismissesALiveSurvey(t *testing.T) {
	for name, pane := range map[string]string{
		"session feedback variant":    feedbackSurveySessionFixture,
		"memory recollection variant": feedbackSurveyMemoryFixture,
	} {
		t.Run(name, func(t *testing.T) {
			ex := &paneAfterFirstKeyExecutor{before: pane, after: surveyVisibleIdle, attached: "0"}
			tm := &Tmux{cfg: DefaultConfig(), exec: ex}

			if err := tm.dismissFeedbackSurvey("agent-pane", func(time.Duration) {}); err != nil {
				t.Fatalf("dismissFeedbackSurvey: %v", err)
			}
			keys := sentKeys(ex.calls)
			if len(keys) != 1 || keys[0][len(keys[0])-1] != "0" {
				t.Fatalf("keys sent = %v, want exactly \"0\"", keys)
			}
		})
	}
}

// ga-da5vmz round 1 (finding 3): the survey's option row is not enough to call
// it live. An agent that QUOTES the row as its last line of output puts it
// directly above an idle composer too; a "0" there lands in the composer. A
// live survey has its header directly above the row.
func TestFeedbackSurveyDismisserSendsNoKeysForAnOptionRowWithoutItsHeader(t *testing.T) {
	for name, pane := range map[string]string{
		"row quoted as the last output line":            "⏺ The survey's option row reads:\n  1: Bad    2: Fine   3: Good   0: Dismiss\n" + surveyVisibleIdle,
		"header further up, not directly above the row": "● How is Claude doing this session? (optional)\n⏺ It renders its options as:\n  1: Bad    2: Fine   3: Good   0: Dismiss\n" + surveyVisibleIdle,
	} {
		t.Run(name, func(t *testing.T) {
			ex := &paneAfterFirstKeyExecutor{before: pane, after: pane, attached: "0"}
			tm := &Tmux{cfg: DefaultConfig(), exec: ex}

			if err := tm.DismissFeedbackSurveyModalIfPresent("agent-pane"); err != nil {
				t.Fatalf("DismissFeedbackSurveyModalIfPresent: %v", err)
			}
			if keys := sentKeys(ex.calls); len(keys) != 0 {
				t.Fatalf("sent survey keys for an option row with no survey header above it: %v", keys)
			}
		})
	}
}

// ga-da5vmz round 1 (finding 4) found a live survey drawn at the bottom of a
// screen with no live composer, below an earlier prompt line ("❯ previous
// request") in the agent's history; feedbackSurveyIsLive still calls it live.
// Upstream #7013 keys the digit only onto a composer it reads as EMPTY, and on
// this screen the composer it reads is that earlier prompt line, so nothing is
// sent: with no empty composer in view there is nowhere safe for the "0".
func TestFeedbackSurveyDismisserKeysNothingBelowAnEarlierPromptLine(t *testing.T) {
	pane := "❯ previous request\n⏺ Done — pushed the branch.\n\n● How is Claude doing this session? (optional)\n  1: Bad    2: Fine   3: Good   0: Dismiss\n"
	if !feedbackSurveyIsLive(pane, DefaultReadyPromptPrefix) {
		t.Fatal("feedbackSurveyIsLive = false, want the survey below the earlier prompt line read as live")
	}
	ex := &paneAfterFirstKeyExecutor{before: pane, after: "❯ previous request\n⏺ Done — pushed the branch.\n", attached: "0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	if err := tm.dismissFeedbackSurvey("agent-pane", func(time.Duration) {}); err != nil {
		t.Fatalf("dismissFeedbackSurvey: %v", err)
	}
	if keys := sentKeys(ex.calls); len(keys) != 0 {
		t.Fatalf("keys sent = %v, want none without an empty composer on screen", keys)
	}
}

// surveyRedrawExecutor shows the live survey until the dismisser sleeps after
// its "0"; from that sleep on it shows the idle composer (stillLive keeps the
// survey up, as when the survey ate the digit). It reports a detached
// session.
type surveyRedrawExecutor struct {
	calls     [][]string
	keyed     bool
	redrawn   bool
	stillLive bool
}

func (s *surveyRedrawExecutor) execute(args []string) (string, error) {
	s.calls = append(s.calls, append([]string(nil), args...))
	switch {
	case slices.Contains(args, "capture-pane"):
		if s.redrawn && !s.stillLive {
			return surveyVisibleIdle, nil
		}
		return feedbackSurveySessionFixture, nil
	case slices.Contains(args, "#{session_attached}"):
		return "0", nil
	case slices.Contains(args, "#{session_name}|#{session_attached}"):
		return "agent-pane|0", nil
	case slices.Contains(args, "send-keys") && args[len(args)-1] == "0":
		s.keyed = true
	}
	return "", nil
}

func (s *surveyRedrawExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return s.execute(args)
}

func (s *surveyRedrawExecutor) sleep(time.Duration) {
	if s.keyed {
		s.redrawn = true
	}
}

// ga-da5vmz round 1 (finding 6) was a retry that re-read the screen straight
// after "0" and Enter and sent a second pair into the now-empty composer.
// Upstream #7013 removed the retry: one digit, then it polls the screen, and
// clears the composer with C-u if the survey did not take the digit. Pinned
// here on the carry's live-survey rule: never a second digit.
func TestFeedbackSurveyDismisserNeverResendsTheDigit(t *testing.T) {
	t.Run("redrawn after the digit: one key", func(t *testing.T) {
		ex := &surveyRedrawExecutor{}
		tm := &Tmux{cfg: DefaultConfig(), exec: ex}

		if err := tm.dismissFeedbackSurvey("agent-pane", ex.sleep); err != nil {
			t.Fatalf("dismissFeedbackSurvey: %v", err)
		}
		if keys := sentKeys(ex.calls); len(keys) != 1 || keys[0][len(keys[0])-1] != "0" {
			t.Fatalf("keys sent = %v, want one \"0\"", keys)
		}
	})
	t.Run("still live after the digit: cleared, not resent", func(t *testing.T) {
		ex := &surveyRedrawExecutor{stillLive: true}
		tm := &Tmux{cfg: DefaultConfig(), exec: ex}

		if err := tm.dismissFeedbackSurvey("agent-pane", ex.sleep); err != nil {
			t.Fatalf("dismissFeedbackSurvey: %v", err)
		}
		keys := sentKeys(ex.calls)
		if len(keys) != 2 || keys[0][len(keys[0])-1] != "0" || keys[1][len(keys[1])-1] != "C-u" {
			t.Fatalf("keys sent = %v, want \"0\" then \"C-u\"", keys)
		}
	})
}
