package tmux

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// ErrNudgeDeferredHumanPrompt means a nudge sent NO keys because the target
// pane was showing something that answers to keystrokes on a human's behalf:
// a question dialog, an approval prompt, another numbered selection list, or a
// human's half-typed draft on an attached session (ga-ubfc7j). Claude Code's
// AskUserQuestion dialog is quiet and shows no spinner, so to the old
// quiescence test it looked idle; the typed text was swallowed and every
// Enter the nudge path sent SELECTED the highlighted option, then submitted
// the dialog as "User answered Claude's questions".
//
// The nudge was not delivered and nothing was typed, so a caller that can
// retry must keep the message queued and try again on a later pass. It is not
// a delivery failure: the queue must not spend one of the item's bounded
// attempts on it, or a dialog left open for an hour would dead-letter every
// reminder queued behind it.
var ErrNudgeDeferredHumanPrompt = errors.New("nudge: deferred, the pane is holding a prompt meant for a human")

// Reasons a nudge is deferred. They are recorded verbatim in the nudge
// delivery log, so they are part of its schema.
const (
	// NudgeDeferReasonQuestionDialog: Claude Code's AskUserQuestion dialog
	// (its "☐ Q1 … ✔ Submit" tab strip, its "Enter to select" footer, or its
	// "Review your answers" page).
	NudgeDeferReasonQuestionDialog = "question_dialog"
	// NudgeDeferReasonApprovalPrompt: a tool-permission prompt ("Do you want
	// to proceed?" over numbered Yes/No options, or the older "This command
	// requires approval" layout parseApprovalPrompt reads).
	NudgeDeferReasonApprovalPrompt = "approval_prompt"
	// NudgeDeferReasonSelectionPrompt: any other numbered option list with a
	// cursor on it. Enter picks the highlighted row, so it is refused too.
	NudgeDeferReasonSelectionPrompt = "selection_prompt"
	// NudgeDeferReasonHumanDraft: the composer holds text on an ATTACHED
	// session, so a person may be mid-message. Before the first Enter
	// (stage before_submit) it means the composer holds text that is not
	// the nudge just typed: a person replaced it (ga-da5vmz).
	NudgeDeferReasonHumanDraft = "human_draft"
	// NudgeDeferReasonCaptureFailed: the pane could not be read, so a prompt
	// could not be ruled out. The guard fails closed.
	NudgeDeferReasonCaptureFailed = "capture_failed"
	// NudgeDeferReasonMachineDialogAttached: a mid-session machine dialog
	// (the resume selector, the feedback survey) is on an ATTACHED session.
	// Its pre-nudge dismissal only runs on a detached pane, where no person
	// can be typing, so here the nudge waits for the person to clear it
	// rather than being typed into the dialog.
	NudgeDeferReasonMachineDialogAttached = "machine_dialog_attached"
)

// Stages at which the guard can defer a nudge.
const (
	nudgeGuardStageBeforeType   = "before_type"
	nudgeGuardStageBeforeSubmit = "before_submit"
)

// NudgeDeferredError is the concrete error behind ErrNudgeDeferredHumanPrompt.
// errors.Is(err, ErrNudgeDeferredHumanPrompt) matches it; errors.As recovers
// the session, the reason and the stage for a delivery record.
type NudgeDeferredError struct {
	Session string
	Reason  string
	Stage   string
	// Err is the capture error when Reason is NudgeDeferReasonCaptureFailed.
	Err error
}

func (e *NudgeDeferredError) Error() string {
	msg := fmt.Sprintf("%v: session %q, %s (%s)", ErrNudgeDeferredHumanPrompt, e.Session, e.Reason, e.Stage)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Is makes errors.Is(err, ErrNudgeDeferredHumanPrompt) true.
func (e *NudgeDeferredError) Is(target error) bool { return target == ErrNudgeDeferredHumanPrompt }

// Unwrap exposes the capture error, if any.
func (e *NudgeDeferredError) Unwrap() error { return e.Err }

// NudgeDeferredReason returns the reason a nudge was deferred, and whether err
// is a deferral at all.
func NudgeDeferredReason(err error) (string, bool) {
	var d *NudgeDeferredError
	if errors.As(err, &d) {
		return d.Reason, true
	}
	if errors.Is(err, ErrNudgeDeferredHumanPrompt) {
		return "", true
	}
	return "", false
}

// NudgeDeferredAfterTyping reports whether a deferral came AFTER text reached
// the pane: the submit was withheld (stage before_submit), or a chunked paste
// was cut off part way. Such a deferral sent keys, just no Enter, and an audit
// record must not read it as "nothing reached the pane".
func NudgeDeferredAfterTyping(err error) bool {
	var d *NudgeDeferredError
	if errors.As(err, &d) && d.Stage == nudgeGuardStageBeforeSubmit {
		return true
	}
	return errors.Is(err, errPartialPasteDelivery)
}

// selectionOptionRe matches one row of a numbered selection list once box
// borders are stripped: an optional cursor glyph, then "N." or "N)" and a
// label. Claude draws its cursor as ❯ ("❯ 1. Yes"), Codex as ›. omp's Ask
// box (also what a Codex-model pool worker shows) draws unnumbered radio rows,
// "❯ ○ Alpha (Recommended)" / "○ Beta", captured live 2026-09-29; a radio mark
// stands in for the number.
var selectionOptionRe = regexp.MustCompile(`^([❯›>→▸▶]\s*)?(\d{1,2}[.)]|[○●◉◯])\s+\S`)

// normalizePaneLine folds NBSP, trims, and strips a leading and trailing box
// border so a row drawn inside a bordered box reads like a bare one.
func normalizePaneLine(line string) string {
	s := strings.TrimSpace(strings.ReplaceAll(line, " ", " "))
	s = stripLeadingBoxBorder(s)
	s = strings.TrimRight(s, " \t")
	for _, border := range []string{"│", "┃"} {
		if strings.HasSuffix(s, border) {
			s = strings.TrimRight(strings.TrimSuffix(s, border), " \t")
			break
		}
	}
	return s
}

// selectionOption reports whether a normalized line is a numbered option row,
// and whether the cursor is on it.
func selectionOption(s string) (option, highlighted bool) {
	m := selectionOptionRe.FindStringSubmatch(s)
	if m == nil {
		return false, false
	}
	return true, m[1] != ""
}

// lastComposerLine returns the index of the LAST captured line that is the
// agent's input composer (the ready-prompt prefix followed by whatever is on
// the line) and that remainder, trimmed; -1 when no line is. A highlighted
// option row ("❯ 1. Yes") starts with the same glyph but is a dialog row, not
// the composer, so it is skipped. Earlier composer-looking lines are
// scrollback echoes of past prompts.
func lastComposerLine(lines []string, promptPrefix string) (int, string) {
	prefix := strings.ReplaceAll(promptPrefix, " ", " ")
	bare := strings.TrimSpace(prefix)
	idx, remainder := -1, ""
	for i, line := range lines {
		s := normalizePaneLine(line)
		var rest string
		switch {
		case prefix != "" && strings.HasPrefix(s, prefix):
			rest = s[len(prefix):]
		case bare != "" && s == bare:
			rest = ""
		default:
			continue
		}
		if option, _ := selectionOption(s); option {
			continue
		}
		idx, remainder = i, strings.TrimSpace(rest)
	}
	return idx, remainder
}

// classifyHumanPrompt decides whether a captured pane is showing something a
// nudge must not type into, and why; "" means it is safe.
//
// Only the region BELOW the live composer is examined for dialogs. When a
// dialog is up, Claude draws it in place of the composer, so the last
// composer-looking line is a scrollback echo above the dialog and the dialog
// is below it. When the pane is idle, the live composer is at the bottom and
// anything above it -- including a dialog QUOTED in the agent's output, or a
// seat reading these very captures -- is history, not a prompt. That anchor is
// what keeps the detector from stalling a seat on scrollback.
//
// checkDraft asks whether a non-empty composer should defer too. The caller
// sets it only for an attached claude pane (see Tmux.humanPromptGuard).
func classifyHumanPrompt(lines []string, promptPrefix string, checkDraft bool) string {
	composerIdx, _ := lastComposerLine(lines, promptPrefix)
	if reason := classifyDialog(lines[composerIdx+1:]); reason != "" {
		return reason
	}
	if checkDraft {
		if found, text := composerDraft(lines, promptPrefix); found && text != "" && !isGCNudgeDraft(text) {
			return NudgeDeferReasonHumanDraft
		}
	}
	return ""
}

// composerDraft returns the text in the live composer and whether a composer
// was found. Claude draws its composer between two horizontal rules at the
// bottom of the pane. When the last two rules enclose a block that opens with
// the prompt, the WHOLE block is the draft: a person's draft can wrap, can
// start on a continuation line (the prompt line itself empty), or can begin
// with "1." -- which lastComposerLine skips as a dialog row. Without that box
// it falls back to the last composer-looking line.
func composerDraft(lines []string, promptPrefix string) (bool, string) {
	if text, ok := composerBoxText(lines, promptPrefix); ok {
		return true, text
	}
	idx, remainder := lastComposerLine(lines, promptPrefix)
	return idx >= 0, remainder
}

func composerBoxText(lines []string, promptPrefix string) (string, bool) {
	var rules []int
	for i, line := range lines {
		if isHorizontalRule(line) {
			rules = append(rules, i)
		}
	}
	if len(rules) < 2 {
		return "", false
	}
	top, bottom := rules[len(rules)-2], rules[len(rules)-1]
	prefix := strings.ReplaceAll(promptPrefix, "\u00a0", " ")
	bare := strings.TrimSpace(prefix)
	var parts []string
	opened := false
	for _, line := range lines[top+1 : bottom] {
		s := normalizePaneLine(line)
		if !opened {
			if s == "" {
				continue
			}
			switch {
			case prefix != "" && strings.HasPrefix(s, prefix):
				s = s[len(prefix):]
			case bare != "" && strings.HasPrefix(s, bare):
				s = s[len(bare):]
			default:
				return "", false
			}
			opened = true
		}
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	if !opened {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

// isHorizontalRule reports whether a captured line is a full-width rule such
// as the one Claude draws above and below its composer.
func isHorizontalRule(line string) bool {
	s := strings.TrimSpace(line)
	return utf8.RuneCountInString(s) >= 8 && strings.Trim(s, "─━") == ""
}

// classifyDialog looks for a dialog in the live region of a pane.
func classifyDialog(live []string) string {
	norm := make([]string, 0, len(live))
	options, highlighted := 0, false
	for _, line := range live {
		s := normalizePaneLine(line)
		norm = append(norm, s)
		if option, cursor := selectionOption(s); option {
			options++
			highlighted = highlighted || cursor
		}
	}
	// A cursor on one of at least two numbered rows: Enter picks that row.
	selection := highlighted && options >= 2
	text := strings.Join(norm, "\n")

	for _, s := range norm {
		// The question dialog's footer and its tab strip. Either is the
		// dialog, on any of its pages, including the free-text "Type
		// something." row that has no numbered cursor.
		if strings.Contains(s, "Enter to select") && strings.Contains(s, "Esc to cancel") {
			return NudgeDeferReasonQuestionDialog
		}
		if strings.Contains(s, "Submit") && (strings.Contains(s, "☐") || strings.Contains(s, "☒")) {
			return NudgeDeferReasonQuestionDialog
		}
		// omp's Ask box: "Enter select · n note · ↑/↓ move · Esc cancel".
		// It fired live on a Codex-model pool worker (pl-022h, 04:34Z 9/29).
		if strings.Contains(s, "Enter select") && strings.Contains(s, "Esc cancel") {
			return NudgeDeferReasonQuestionDialog
		}
	}
	if selection && (strings.Contains(text, "Review your answers") || strings.Contains(text, "Ready to submit your answers?")) {
		return NudgeDeferReasonQuestionDialog
	}
	if parseApprovalPrompt(strings.Join(live, "\n")) != nil {
		return NudgeDeferReasonApprovalPrompt
	}
	if selection {
		for _, s := range norm {
			if strings.HasPrefix(s, "Do you want to ") || strings.Contains(s, "Tab to amend") || strings.Contains(s, "don't ask again") {
				return NudgeDeferReasonApprovalPrompt
			}
		}
		return NudgeDeferReasonSelectionPrompt
	}
	return ""
}

// isGCNudgeDraft reports whether a composer draft is gc's own: a queued
// nudge left on the line by a lost submit (ga-bwm) starts with the reminder
// wrapper. It is not a person's message, so it does not hold delivery back.
func isGCNudgeDraft(draft string) bool {
	return strings.HasPrefix(draft, "<system-reminder>")
}

// composerHoldsSent reports whether the live composer still shows the text a
// delivery typed -- the only condition under which re-sending Enter can do
// what it is for (submit a draft whose first Enter was dropped, ga-bwm).
// Anything else on screen (a drained composer, a dialog the agent raised, an
// unreadable pane) means a re-sent Enter would land on something that is not
// our draft.
func composerHoldsSent(lines []string, promptPrefix, sent string) bool {
	found, remainder := composerDraft(lines, promptPrefix)
	if !found {
		return false
	}
	if draft := firstNRunes(strings.TrimSpace(firstNonEmptyLine(sent)), 40); draft != "" && strings.Contains(remainder, draft) {
		return true
	}
	// Long pastes are shown as a placeholder, not their text: Claude's
	// "[Pasted text #N +M lines]", Codex's "[Pasted Content N chars]".
	return strings.Contains(remainder, claudePastePlaceholderPrefix) || strings.Contains(remainder, "[Pasted Content")
}

// composerDraftIsOurs reports whether a composer draft can be only the nudge
// sent, as Claude draws it before the first Enter (ga-da5vmz). Whitespace is
// ignored throughout, since the composer re-wraps long lines. Complete gc
// reminders at the front are set aside (stripLeadingGCReminders): an earlier
// undelivered nudge left on the line, or this one. What remains must be one
// of:
//
//   - nothing;
//   - the opening of this message, at least composerPrefixMinRunes long or
//     the whole message (the paste still rendering);
//   - the end of this message, at least composerTailMinRunes long or the
//     whole message (a tall draft scrolled so only its tail shows);
//   - Claude's paste placeholder and nothing else (a long paste shows only
//     that).
//
// Anything else is not ours: text before or after the nudge, a run from its
// middle, a sentence quoting it, or a placeholder with words beside it. The
// draft only has to CONTAIN a person's words for the first Enter to submit
// them. Known gap: a person who clears the nudge and pastes their own long
// text shows a placeholder, which this cannot tell from ours.
func composerDraftIsOurs(draft, sent string) bool {
	d := stripLeadingGCReminders(squashSpace(draft))
	if d == "" || claudePastePlaceholderOnly.MatchString(d) {
		return true
	}
	m := squashSpace(sent)
	n, whole := utf8.RuneCountInString(d), utf8.RuneCountInString(m)
	if strings.HasPrefix(m, d) && n >= min(whole, composerPrefixMinRunes) {
		return true
	}
	return strings.HasSuffix(m, d) && n >= min(whole, composerTailMinRunes)
}

// composerPrefixMinRunes and composerTailMinRunes are the shortest opening
// and tail of a message (whitespace squashed) composerDraftIsOurs accepts as
// the message: a person's "Yo" is a prefix of "You have mail", and that must
// not count.
const (
	composerPrefixMinRunes = 24
	composerTailMinRunes   = 16
)

// claudePastePlaceholderOnly matches Claude's paste placeholder, whitespace
// squashed, as a whole draft: "[Pasted text #3 +6 lines]" or "[Pasted text #3]".
var claudePastePlaceholderOnly = regexp.MustCompile(`^\[Pastedtext#\d+(\+\d+lines?)?\]$`)

// stripLeadingGCReminders removes complete gc reminders from the front of a
// whitespace-squashed draft: each must open with <system-reminder>, close
// with </system-reminder>, and hold no second opening tag in between.
// Whatever follows the last one is returned as is.
func stripLeadingGCReminders(d string) string {
	const open, closing = "<system-reminder>", "</system-reminder>"
	for strings.HasPrefix(d, open) {
		end := strings.Index(d, closing)
		if end < 0 || strings.Contains(d[len(open):end], open) {
			break
		}
		d = d[end+len(closing):]
	}
	return d
}

// squashSpace drops every whitespace rune from s.
func squashSpace(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// humanPromptGuard captures target and returns a *NudgeDeferredError when the
// pane shows a human prompt.
//
// It is the ONE check every nudge keystroke passes (the chokepoint): the text
// senders sendKeysLiteralWithRetry and sendStartupKeysLiteralWithRetry call it
// before typing, and sendNudgeSubmitSequence calls it (dialogs only) before
// the submit key. Every nudge caller -- NudgeSession on a detached or an
// attached pane, Provider.Nudge's send-after-idle-timeout, the startup nudge
// and its retry ladder, NudgePane -- reaches those senders, so none of them
// can type into a prompt meant for a human. nudgeSession also calls it once
// up front, because its C-u clear and its feedback-survey digit are keys sent
// BEFORE the text; that early call protects those two keys, and the
// chokepoint stays authoritative for the text and the submit. checkDraft adds the attached-draft rule (c); it
// is applied only to claude-family panes, whose empty composer is a bare "❯".
// Other TUIs draw placeholder text in an empty composer ("Type your message
// or @path/to/file"), which the pane text cannot tell from a draft, so for
// them the rule would stall every nudge while a client is attached.
//
// A capture that fails because the session or server is gone is returned as
// is, so callers keep treating it as "session gone"; any other capture failure
// defers, because a prompt could not be ruled out.
func (t *Tmux) humanPromptGuard(session, target, stage string, checkDraft bool) error {
	lines, err := t.CapturePaneLines(target, promptObservationLines)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoServer) {
			return err
		}
		return &NudgeDeferredError{Session: session, Reason: NudgeDeferReasonCaptureFailed, Stage: stage, Err: err}
	}
	if reason := t.classifyPaneLines(session, target, lines, checkDraft); reason != "" {
		return &NudgeDeferredError{Session: session, Reason: reason, Stage: stage}
	}
	return nil
}

// classifyPaneLines is humanPromptGuard's decision on an already-captured
// pane: classifyHumanPrompt, with the draft rule applied only to an attached
// claude pane, and a faint placeholder not counted as a draft.
func (t *Tmux) classifyPaneLines(session, target string, lines []string, checkDraft bool) string {
	draft := checkDraft && t.sessionClientCount(session) != 0 && t.paneIsClaudeFamily(target)
	prefix := t.resolveIdlePromptPrefix(session)
	reason := classifyHumanPrompt(lines, prefix, draft)
	if reason == NudgeDeferReasonHumanDraft && t.composerHoldsOnlyDimText(target, prefix) {
		reason = ""
	}
	return reason
}

// composerHoldsOnlyDimText re-reads the pane WITH its text attributes and
// reports whether the composer's apparent draft is all faint (SGR 2) text.
// An empty Claude composer draws a placeholder there in faint text -- on a
// fresh session `❯ Try "how does <filepath> work?"` -- which a plain capture
// cannot tell from a human's draft, so without this check every nudge to an
// attached seat would wait until the person typed and submitted something.
// Typed text is drawn without attributes (both measured on Claude Code,
// 2026-09-29). Only faint runs are discounted: treating any other styled text
// as a placeholder would let a nudge type into a real draft, and a capture
// that fails keeps the draft rule in force.
func (t *Tmux) composerHoldsOnlyDimText(target, promptPrefix string) bool {
	found, text := t.undimmedComposerDraft(target, promptPrefix)
	return found && text == ""
}

// undimmedDraftIsOurs is the first-submit form of composerHoldsOnlyDimText
// (ga-da5vmz). The attribute re-read is a new capture, so the screen may have
// changed since the plain read; it gets the same ownership rule as that read,
// with faint text dropped. An empty draft passes, as before; so does the
// nudge if it rendered in between. A person's text does not.
func (t *Tmux) undimmedDraftIsOurs(target, promptPrefix, message string) bool {
	found, text := t.undimmedComposerDraft(target, promptPrefix)
	return found && composerDraftIsOurs(text, message)
}

// undimmedComposerDraft re-reads the pane with its text attributes and
// returns the composer draft with faint (SGR 2) text dropped. A failed or
// empty capture reports no composer.
func (t *Tmux) undimmedComposerDraft(target, promptPrefix string) (bool, string) {
	out, err := t.run("capture-pane", "-p", "-e", "-t", target, "-S", fmt.Sprintf("-%d", promptObservationLines))
	if err != nil || out == "" {
		return false, ""
	}
	styled := strings.Split(out, "\n")
	undimmed := make([]string, len(styled))
	for i, line := range styled {
		undimmed[i] = stripTerminalStyle(line, true)
	}
	return composerDraft(undimmed, promptPrefix)
}

// sessionClientCount returns how many tmux clients are attached to the
// session, or -1 when that cannot be read. #{session_attached} is a COUNT:
// IsSessionAttached tests it for "1" only, so gc's own hidden attach client
// plus a person (2) reads as detached there. The nudge path uses this instead,
// and treats -1 as attached: when it cannot tell, it assumes a person may be
// typing.
func (t *Tmux) sessionClientCount(target string) int {
	out, err := t.run("display-message", "-t", target, "-p", "#{session_attached}")
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return -1
	}
	return n
}

// stripTerminalStyle removes escape sequences from one captured line: CSI
// sequences (SGR among them) and OSC strings such as hyperlinks. With
// dropFaint it also drops the text drawn while SGR 2 (faint) is in effect.
func stripTerminalStyle(line string, dropFaint bool) string {
	var b strings.Builder
	faint := false
	for i := 0; i < len(line); {
		if line[i] != 0x1b || i+1 >= len(line) {
			if !dropFaint || !faint {
				b.WriteByte(line[i])
			}
			i++
			continue
		}
		switch line[i+1] {
		case '[':
			j := i + 2
			for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
				j++
			}
			if j >= len(line) {
				return b.String()
			}
			if line[j] == 'm' {
				faint = applySGRFaint(line[i+2:j], faint)
			}
			i = j + 1
		case ']':
			j := i + 2
			for j < len(line) && line[j] != 0x07 && (line[j] != 0x1b || j+1 >= len(line) || line[j+1] != '\\') {
				j++
			}
			switch {
			case j >= len(line):
				return b.String()
			case line[j] == 0x07:
				i = j + 1
			default:
				i = j + 2
			}
		default:
			i += 2
		}
	}
	return b.String()
}

// applySGRFaint folds one SGR parameter list into the faint state: 2 sets it;
// 0, an empty list, and 22 (normal intensity) clear it. Extended color
// arguments (38;5;n, 38;2;r;g;b and their 48/58 forms) are skipped so a
// color index of 2 or 22 is not read as an intensity change.
func applySGRFaint(params string, faint bool) bool {
	if params == "" {
		return false
	}
	fields := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	for k := 0; k < len(fields); k++ {
		switch fields[k] {
		case "0", "00", "22":
			faint = false
		case "2":
			faint = true
		case "38", "48", "58":
			if k+1 < len(fields) {
				switch fields[k+1] {
				case "5":
					k += 2
				case "2":
					k += 4
				}
			}
		}
	}
	return faint
}

// paneIsClaudeFamily resolves target's provider family the way
// targetIsClaudeFamily does, without its process sniff fallback cost when the
// pane declares GC_PROVIDER.
func (t *Tmux) paneIsClaudeFamily(target string) bool {
	if provider := t.providerEnv(target); provider != "" {
		return sessionlog.ProviderFamily(provider) == "claude"
	}
	return t.targetLooksLikeProvider(target, "claude")
}

// resendGate returns the check submitEnterAndConfirmGated runs before a
// RE-sent submit (ga-bwm's repair for a dropped first Enter). The re-send is
// allowed only while a fresh capture shows the composer still holding the
// text this delivery typed and no dialog is up; otherwise it stops the loop
// without an error, and the caller classifies the outcome from the pane as it
// already does. The first submit needs no gate here: sendNudgeSubmitSequence
// (the chokepoint) refuses it itself when a dialog is up.
//
// "The text this delivery typed" is pinned to what the composer showed just
// before the FIRST submit: the re-send needs the composer to still show exactly
// that. Matching the message alone is not enough -- a long paste shows only as
// a placeholder ("[Pasted text #N +M lines]"), and a person who pastes their
// own text after a submit that landed unobserved would otherwise have THEIR
// placeholder submitted. Claude numbers each paste, so theirs differs.
//
// The first submit is gated too, on an ATTACHED claude pane only (ga-da5vmz):
// the composer was empty when the nudge was typed, but in the paste debounce
// a person may have cleared it and typed their own message, which the first
// Enter would submit. When the composer then holds text that is not only the
// nudge (composerDraftIsOurs), the delivery defers after typing -- unless an
// attribute re-read, with faint placeholder text dropped, passes the same
// ownership rule (undimmedDraftIsOurs). Only positive evidence of someone else's text
// defers: an empty or unreadable composer keeps today's behavior, so a slow
// render costs nothing. Other families are not checked: claude is the one
// family whose composer the draft rule models (see humanPromptGuard), and of
// the others only codex reaches this gate.
func (t *Tmux) resendGate(session, target, message string) func(resend bool) (bool, error) {
	promptPrefix := t.resolveIdlePromptPrefix(session)
	snapshot := ""
	return func(resend bool) (bool, error) {
		lines, err := t.CapturePaneLines(target, promptObservationLines)
		if !resend {
			snapshot = ""
			if err == nil {
				if found, text := composerDraft(lines, promptPrefix); found {
					snapshot = text
					if !composerDraftIsOurs(text, message) &&
						t.sessionClientCount(session) != 0 && t.paneIsClaudeFamily(target) &&
						!t.undimmedDraftIsOurs(target, promptPrefix, message) {
						return false, &NudgeDeferredError{Session: session, Reason: NudgeDeferReasonHumanDraft, Stage: nudgeGuardStageBeforeSubmit}
					}
				}
			}
			return true, nil
		}
		if err != nil || snapshot == "" {
			return false, nil
		}
		if classifyHumanPrompt(lines, promptPrefix, false) != "" {
			return false, nil
		}
		if found, text := composerDraft(lines, promptPrefix); !found || text != snapshot {
			return false, nil
		}
		return composerHoldsSent(lines, promptPrefix, message), nil
	}
}
