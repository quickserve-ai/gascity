package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// Dead-letter bounces (ga-vhfa property 2).
//
// A queued nudge that dies (TTL expiry, retry exhaustion, unresolved target,
// session-fence mismatch, manual drop) moves to the queue's Dead list long
// after the sender's command exited 0. Without a bounce the only record of the
// loss is state.json, which no sender reads. withNudgeQueueState therefore
// hands every item that newly entered Dead to reportDeadLetteredNudges, which
// mails the sender one plain notice. Unread mail is injected into a seat's next
// turn, so the loss surfaces where the sender looks.
//
// The notice is deliberately inert:
//   - It is a plain mail, never --notify and never a nudge, so a bounce cannot
//     enqueue a nudge that could itself dead-letter and bounce.
//   - Its body never carries the nudge's message text. Sender is self-reported
//     (GC_AGENT etc.), not authenticated, so quoting the text would let anyone
//     who can enqueue a nudge place arbitrary text in a named seat's turn.
//   - Recipient resolution is read-only: the sender must name a configured
//     named session, looked up in config alone. The mail CLI's recipient
//     resolver is not used, because it can materialize or claim a session
//     (ga-isa3j4); beadmail's Send stores the recipient string verbatim.

// nudgeDeadLetterBounceFrom is the From address of a dead-letter notice.
const nudgeDeadLetterBounceFrom = "gc"

// nudgeDeadLetterSubjectCauseMax bounds the cause quoted in the subject line.
const nudgeDeadLetterSubjectCauseMax = 80

// nudgeDeadLetterBouncer sends one dead-letter notice. It is a package var so
// tests can capture notices instead of writing mail beads.
var nudgeDeadLetterBouncer = sendNudgeDeadLetterMail

// reportDeadLetteredNudges mails each eligible item's sender one notice. It is
// best-effort: a failure writes one warning and never reaches the queue
// operation that dead-lettered the item. Callers must not hold the queue lock.
func reportDeadLetteredNudges(cityPath string, items []queuedNudge) {
	candidates := make([]queuedNudge, 0, len(items))
	for _, item := range items {
		if nudgeDeadLetterWorthBouncing(item) {
			candidates = append(candidates, item)
		}
	}
	if len(candidates) == 0 {
		return
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil || cfg == nil {
		// No readable config means no sender can be confirmed as a mailbox;
		// skip rather than mail an address nothing may ever read.
		return
	}
	cityName := loadedCityName(cfg, cityPath)
	configuredMailbox := func(name string) string {
		// An empty rig context keeps the lookup independent of this process's
		// cwd and GC_DIR: only city-scoped or fully qualified names resolve.
		spec, ok, err := session.FindNamedSessionSpecForTarget(cfg, cityName, name, "")
		if err != nil || !ok {
			return ""
		}
		return strings.TrimSpace(spec.Identity)
	}
	for _, item := range candidates {
		to := configuredMailbox(item.Sender)
		if to == "" {
			continue
		}
		if to == strings.TrimSpace(item.Agent) || to == configuredMailbox(item.Agent) {
			continue
		}
		subject, body := nudgeDeadLetterNotice(item)
		if err := nudgeDeadLetterBouncer(cityPath, to, subject, body); err != nil && nudgeWarningWriter != nil {
			fmt.Fprintf(nudgeWarningWriter, "gc nudge: warning: notifying %q that nudge %q dead-lettered: %v\n", to, item.ID, err) //nolint:errcheck // best-effort warning
		}
	}
}

// nudgeDeadLetterWorthBouncing applies the config-free filters: a sender to
// notify, one that is not a person reading a terminal, not the target itself,
// and a loss worth reporting.
func nudgeDeadLetterWorthBouncing(item queuedNudge) bool {
	sender := strings.TrimSpace(item.Sender)
	if sender == "" || sender == "human" {
		return false
	}
	if sender == strings.TrimSpace(item.Agent) {
		return false
	}
	if item.SenderSession != "" && item.SenderSession == item.SessionID {
		return false
	}
	// A mail --notify wake whose mail survives lost nothing: the mail itself
	// still reaches the inbox.
	if item.Source == "mail" {
		return false
	}
	// A superseded nudge was replaced by a newer one for the same reference.
	// Telling the sender to re-send it would only duplicate the newer one.
	if strings.TrimSpace(item.LastError) == "superseded" {
		return false
	}
	return true
}

// nudgeDeadLetterNotice renders the notice. The nudge's message text is
// deliberately absent (see the package comment above).
func nudgeDeadLetterNotice(item queuedNudge) (subject, body string) {
	cause := strings.Join(strings.Fields(deadReason(item)), " ")
	subject = fmt.Sprintf("[nudge dead-lettered] to %s: %s", item.Agent, truncateNudgeDeadLetterCause(cause, nudgeDeadLetterSubjectCauseMax))
	var b strings.Builder
	fmt.Fprintf(&b, "A nudge you queued was dead-lettered.\n\n")
	fmt.Fprintf(&b, "Nudge: %s\n", item.ID)
	fmt.Fprintf(&b, "Target: %s\n", item.Agent)
	fmt.Fprintf(&b, "Cause: %s\n", cause)
	fmt.Fprintf(&b, "Attempts: %d\n", item.Attempts)
	fmt.Fprintf(&b, "Created: %s\n", nudgeDeadLetterTime(item.CreatedAt))
	fmt.Fprintf(&b, "Dead: %s\n\n", nudgeDeadLetterTime(item.DeadAt))
	fmt.Fprintf(&b, "Read it: gc nudge status %s\n\n", item.Agent)
	fmt.Fprintf(&b, "The nudge was not delivered. If it mattered, re-send it or mail it.\n")
	return subject, b.String()
}

func nudgeDeadLetterTime(ts time.Time) string {
	if ts.IsZero() {
		return "unknown"
	}
	return ts.UTC().Format(time.RFC3339)
}

func truncateNudgeDeadLetterCause(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-3]) + "..."
}

// sendNudgeDeadLetterMail sends the notice through the city's mail provider,
// opened from cityPath rather than the process cwd. It mirrors
// openCityMailProvider's class-store routing: message beads through
// resolveMailMessagesStore, session reads through cliSessionStore. Send is
// called with an exact configured mailbox and no notification, so it neither
// resolves the recipient through a session-materializing path nor wakes one.
func sendNudgeDeadLetterMail(cityPath, to, subject, body string) error {
	name := os.Getenv("GC_MAIL")
	if name == "" {
		name = mailProviderNameForCity(cityPath)
	}
	if strings.HasPrefix(name, "exec:") || name == "fake" || name == "fail" {
		_, err := newMailProviderNamed(name, nil, false).Send(nudgeDeadLetterBounceFrom, to, subject, body)
		return err
	}
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return fmt.Errorf("opening the city store at %q: %w", cityPath, err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	msgStore := resolveMailMessagesStore(cliStorageRoutes(cityPath), store, cfg, cityPath, nil)
	sessStore := cliSessionStore(store, cfg, cityPath)
	mp := newMailProviderNamedWithSessionStore(name, msgStore, sessStore, false)
	_, err = mp.Send(nudgeDeadLetterBounceFrom, to, subject, body)
	return err
}
