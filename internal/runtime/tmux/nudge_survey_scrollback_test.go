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
// the composer. The dismisser must send nothing there: its "0" and Enter
// would land in the idle composer and submit "0" as a user message.
func TestFeedbackSurveyDismisserSendsNoKeysForASurveyAboveLaterOutput(t *testing.T) {
	ex := &paneAfterFirstKeyExecutor{before: surveyScrollbackHistory, after: surveyScrollbackHistory, attached: "0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	tm.DismissFeedbackSurveyModalIfPresent("agent-pane")
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

// feedbackSurveyRuleComposerFixture is the session survey above the composer
// current Claude Code draws between two horizontal rules (the
// idleComposerFixture shape), not the rounded box of the older captures. It is
// ASSEMBLED, not captured: no live pane showed a survey on 2026-10-02. The
// survey block's layout -- a "●" header row directly above a two-space
// indented option row, cells 10 wide, no gap -- is read from the survey
// component in the Claude Code 2.1.288 bundle (ga-da5vmz).
const feedbackSurveyRuleComposerFixture = `⏺ Done — pushed the branch and replied on the PR.

● How is Claude doing this session? (optional)
  1: Bad    2: Fine   3: Good   0: Dismiss

` + rule + "\n❯\n" + rule + "\n  -- INSERT -- ⏵⏵ bypass permissions on (shift+tab to cycle)"

// Control: a survey that IS the live block above the composer is still
// dismissed with exactly "0" then Enter, for both survey variants and both
// composer styles.
func TestFeedbackSurveyDismisserDismissesALiveSurvey(t *testing.T) {
	for name, pane := range map[string]string{
		"session feedback variant":    feedbackSurveySessionFixture,
		"memory recollection variant": feedbackSurveyMemoryFixture,
		"horizontal-rule composer":    feedbackSurveyRuleComposerFixture,
	} {
		t.Run(name, func(t *testing.T) {
			ex := &paneAfterFirstKeyExecutor{before: pane, after: surveyVisibleIdle, attached: "0"}
			tm := &Tmux{cfg: DefaultConfig(), exec: ex}

			tm.DismissFeedbackSurveyModalIfPresent("agent-pane")
			keys := sentKeys(ex.calls)
			if len(keys) != 2 || keys[0][len(keys[0])-1] != "0" || keys[1][len(keys[1])-1] != "Enter" {
				t.Fatalf("keys sent = %v, want exactly \"0\" then \"Enter\"", keys)
			}
		})
	}
}

// ga-da5vmz round 1 (finding 3): the survey's option row is not enough to call
// it live. An agent that QUOTES the row as its last line of output puts it
// directly above an idle composer too; a "0" and Enter there submit "0" as a
// user message. A live survey has its header directly above the row.
func TestFeedbackSurveyDismisserSendsNoKeysForAnOptionRowWithoutItsHeader(t *testing.T) {
	for name, pane := range map[string]string{
		"row quoted as the last output line":            "⏺ The survey's option row reads:\n  1: Bad    2: Fine   3: Good   0: Dismiss\n" + surveyVisibleIdle,
		"header further up, not directly above the row": "● How is Claude doing this session? (optional)\n⏺ It renders its options as:\n  1: Bad    2: Fine   3: Good   0: Dismiss\n" + surveyVisibleIdle,
	} {
		t.Run(name, func(t *testing.T) {
			ex := &paneAfterFirstKeyExecutor{before: pane, after: pane, attached: "0"}
			tm := &Tmux{cfg: DefaultConfig(), exec: ex}

			tm.DismissFeedbackSurveyModalIfPresent("agent-pane")
			if keys := sentKeys(ex.calls); len(keys) != 0 {
				t.Fatalf("sent survey keys for an option row with no survey header above it: %v", keys)
			}
		})
	}
}

// ga-da5vmz round 1 (finding 4): with no live composer on screen, an earlier
// prompt line ("❯ previous request") in the agent's history is not the anchor.
// A live survey drawn at the bottom of the screen below it is still dismissed.
func TestFeedbackSurveyDismisserDismissesALiveSurveyBelowAnEarlierPromptLine(t *testing.T) {
	pane := "❯ previous request\n⏺ Done — pushed the branch.\n\n● How is Claude doing this session? (optional)\n  1: Bad    2: Fine   3: Good   0: Dismiss\n"
	ex := &paneAfterFirstKeyExecutor{before: pane, after: "❯ previous request\n⏺ Done — pushed the branch.\n", attached: "0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	tm.dismissFeedbackSurvey("agent-pane", func(time.Duration) {})
	keys := sentKeys(ex.calls)
	if len(keys) != 2 || keys[0][len(keys[0])-1] != "0" || keys[1][len(keys[1])-1] != "Enter" {
		t.Fatalf("keys sent = %v, want exactly \"0\" then \"Enter\"", keys)
	}
}

// surveyRedrawExecutor shows the live survey until the dismisser sleeps after
// its Enter; from that sleep on it shows the idle composer (stillLive keeps
// the survey up, as when the survey ate the digit). It reports a detached
// session.
type surveyRedrawExecutor struct {
	calls     [][]string
	entered   bool
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
	case slices.Contains(args, "send-keys") && args[len(args)-1] == "Enter":
		s.entered = true
	}
	return "", nil
}

func (s *surveyRedrawExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return s.execute(args)
}

func (s *surveyRedrawExecutor) sleep(time.Duration) {
	if s.entered {
		s.redrawn = true
	}
}

// ga-da5vmz round 1 (finding 6): the dismisser re-reads the screen after its
// "0" and Enter and retries once if the survey is still there. Read straight
// after the Enter, the screen can still show the survey Claude has not yet
// redrawn, and the retry's "0" and Enter then land in the now-empty composer
// as a user message. The retry waits for the redraw first, and sends its pair
// only if the survey is still live after that wait.
func TestFeedbackSurveyDismisserWaitsForTheRedrawBeforeRetrying(t *testing.T) {
	t.Run("redrawn during the wait: one pair", func(t *testing.T) {
		ex := &surveyRedrawExecutor{}
		tm := &Tmux{cfg: DefaultConfig(), exec: ex}

		tm.dismissFeedbackSurvey("agent-pane", ex.sleep)
		if keys := sentKeys(ex.calls); len(keys) != 2 {
			t.Fatalf("keys sent = %v, want one \"0\" and \"Enter\" pair", keys)
		}
	})
	t.Run("still live after the wait: retried", func(t *testing.T) {
		ex := &surveyRedrawExecutor{stillLive: true}
		tm := &Tmux{cfg: DefaultConfig(), exec: ex}

		tm.dismissFeedbackSurvey("agent-pane", ex.sleep)
		if keys := sentKeys(ex.calls); len(keys) != 4 {
			t.Fatalf("keys sent = %v, want two \"0\" and \"Enter\" pairs", keys)
		}
	})
}
