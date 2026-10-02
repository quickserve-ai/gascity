package tmux

import (
	"context"
	"slices"
	"testing"
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

// Control: a survey that IS the live block above the composer is still
// dismissed with exactly "0" then Enter, for both survey variants.
func TestFeedbackSurveyDismisserDismissesALiveSurvey(t *testing.T) {
	for name, pane := range map[string]string{
		"session feedback variant":    feedbackSurveySessionFixture,
		"memory recollection variant": feedbackSurveyMemoryFixture,
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
