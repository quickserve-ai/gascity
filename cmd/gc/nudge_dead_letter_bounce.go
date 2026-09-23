package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
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
//     (ga-isa3j4); beadmail's Send stores the recipient string verbatim, and
//     the provider is built with no session directory, so Send does not read
//     sessions at all.
//
// The report can run inside the controller loop (nudgeDispatchTick reaches
// runNudgeQueueMaintenanceSweep), so one call loads config once, opens the
// mail store once, and sends at most nudgeDeadLetterBouncesPerCall notices;
// the rest are named in one warning. It stays synchronous after the queue
// lock is released: a goroutine could be cut off by a CLI process exiting.

const (
	// nudgeDeadLetterBounceFrom is the From address of a dead-letter notice.
	nudgeDeadLetterBounceFrom = "gc"
	// nudgeDeadLetterSubjectCauseMax bounds the cause quoted in the subject.
	nudgeDeadLetterSubjectCauseMax = 80
	// nudgeDeadLetterCauseMax bounds the cause quoted anywhere in a notice.
	// Provider error text can echo the prompt, and the notice promises no
	// payload.
	nudgeDeadLetterCauseMax = 160
	// nudgeDeadLetterBouncesPerCall caps the notices one queue operation
	// sends, so a burst of deaths cannot stall the controller loop.
	nudgeDeadLetterBouncesPerCall = 10
	// nudgeManagedWakeRollbackCause prefixes the LastError that
	// rollbackQueuedNudge records when a managed wake fails. The enqueuing
	// command already exited 1 with that error, so the death is not bounced.
	nudgeManagedWakeRollbackCause = "managed wake failed: "
)

// nudgeDeadLetterSendFunc sends one notice through an already-open mailer.
type nudgeDeadLetterSendFunc func(to, subject, body string) error

// openNudgeDeadLetterMailer opens the city's mail sender once per report. It
// is a package var so tests can count opens and capture notices.
var openNudgeDeadLetterMailer = openCityNudgeDeadLetterMailer

type nudgeDeadLetterBounce struct {
	item queuedNudge
	to   string
}

// reportDeadLetteredNudges mails each eligible item's sender one notice. It is
// best-effort: failures write warnings and never reach the queue operation
// that dead-lettered the items. Callers must not hold the queue lock.
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
	var bounces []nudgeDeadLetterBounce
	for _, item := range candidates {
		to := configuredMailbox(item.Sender)
		if to == "" {
			continue
		}
		if to == strings.TrimSpace(item.Agent) || to == configuredMailbox(item.Agent) {
			continue
		}
		bounces = append(bounces, nudgeDeadLetterBounce{item: item, to: to})
	}
	if len(bounces) == 0 {
		return
	}
	var overflow []nudgeDeadLetterBounce
	if len(bounces) > nudgeDeadLetterBouncesPerCall {
		bounces, overflow = bounces[:nudgeDeadLetterBouncesPerCall], bounces[nudgeDeadLetterBouncesPerCall:]
	}
	defer warnUnbouncedDeadLetters(overflow, fmt.Sprintf("over the cap of %d per queue operation", nudgeDeadLetterBouncesPerCall))

	send, closeMailer, err := openNudgeDeadLetterMailer(cityPath, cfg)
	if err != nil {
		warnUnbouncedDeadLetters(bounces, fmt.Sprintf("the mail store did not open: %v", err))
		return
	}
	if closeMailer != nil {
		defer closeMailer()
	}
	for _, bounce := range bounces {
		subject, body := nudgeDeadLetterNotice(bounce.item)
		if err := send(bounce.to, subject, body); err != nil && nudgeWarningWriter != nil {
			fmt.Fprintf(nudgeWarningWriter, "gc nudge: warning: notifying %q that nudge %q dead-lettered: %v\n", bounce.to, bounce.item.ID, err) //nolint:errcheck // best-effort warning
		}
	}
}

// warnUnbouncedDeadLetters writes one warning naming dead-lettered nudges
// whose senders were not notified.
func warnUnbouncedDeadLetters(bounces []nudgeDeadLetterBounce, why string) {
	if len(bounces) == 0 || nudgeWarningWriter == nil {
		return
	}
	ids := make([]string, 0, len(bounces))
	for _, bounce := range bounces {
		ids = append(ids, bounce.item.ID)
	}
	fmt.Fprintf(nudgeWarningWriter, "gc nudge: warning: %d dead-lettered nudges not bounced to their senders (%s): %s\n", len(ids), why, strings.Join(ids, ", ")) //nolint:errcheck // best-effort warning
}

// nudgeDeadLetterWorthBouncing applies the config-free filters: a sender to
// notify, one that is not a person reading a terminal, not the target itself,
// and a loss the sender has not already seen.
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
	cause := strings.TrimSpace(item.LastError)
	// A superseded nudge was replaced by a newer one for the same reference.
	// Telling the sender to re-send it would only duplicate the newer one.
	if cause == "superseded" {
		return false
	}
	// A managed-wake rollback failed the enqueuing command with exit 1.
	if strings.HasPrefix(cause, nudgeManagedWakeRollbackCause) {
		return false
	}
	return true
}

// nudgeDeadLetterCause renders an item's cause on one line, with control
// characters removed, bounded to nudgeDeadLetterCauseMax runes.
func nudgeDeadLetterCause(item queuedNudge) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, deadReason(item))
	return truncateNudgeDeadLetterCause(strings.Join(strings.Fields(cleaned), " "), nudgeDeadLetterCauseMax)
}

// nudgeDeadLetterNotice renders the notice. The nudge's message text is
// deliberately absent (see the package comment above).
func nudgeDeadLetterNotice(item queuedNudge) (subject, body string) {
	cause := nudgeDeadLetterCause(item)
	subject = fmt.Sprintf("[nudge dead-lettered] to %s: %s", item.Agent, truncateNudgeDeadLetterCause(cause, nudgeDeadLetterSubjectCauseMax))
	var b strings.Builder
	fmt.Fprintf(&b, "A nudge you queued was dead-lettered.\n\n")
	fmt.Fprintf(&b, "Nudge: %s\n", item.ID)
	fmt.Fprintf(&b, "Target: %s\n", item.Agent)
	fmt.Fprintf(&b, "Cause: %s\n", cause)
	fmt.Fprintf(&b, "Attempts: %d\n", item.Attempts)
	fmt.Fprintf(&b, "Created: %s\n", nudgeDeadLetterTime(item.CreatedAt))
	fmt.Fprintf(&b, "Dead: %s\n\n", nudgeDeadLetterTime(item.DeadAt))
	// Point at the queue file, not `gc nudge status <agent>`: that command
	// resolves its target through a MATERIALIZING resolver, so following the
	// advice could create a session. Reading the file has no side effects.
	fmt.Fprintf(&b, "Read it: .gc/nudges/state.json, the \"dead\" list, id %s\n\n", item.ID)
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

// openCityNudgeDeadLetterMailer opens the city's mail sender from cityPath,
// reusing the caller's already-loaded cfg instead of loading config again.
// Message beads route through resolveMailMessagesStore, as in
// openCityMailProvider. The beadmail provider gets no session directory: Send
// then records the "gc" sender literally instead of resolving it against
// session beads, and it never resolves the recipient, so no session is read,
// created, or claimed. Send is plain mail; nothing is notified or nudged.
func openCityNudgeDeadLetterMailer(cityPath string, cfg *config.City) (nudgeDeadLetterSendFunc, func(), error) {
	name := os.Getenv("GC_MAIL")
	if name == "" && cfg != nil {
		name = cfg.Mail.Provider
	}
	if strings.HasPrefix(name, "exec:") || name == "fake" || name == "fail" {
		mp := newMailProviderNamed(name, nil, false)
		return func(to, subject, body string) error {
			_, err := mp.Send(nudgeDeadLetterBounceFrom, to, subject, body)
			return err
		}, nil, nil
	}
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the city store at %q: %w", cityPath, err)
	}
	msgStore := resolveMailMessagesStore(cliStorageRoutes(cityPath), store, cfg, cityPath, nil)
	mp := beadmail.NewWithSessionDirectory(msgStore, nil)
	send := func(to, subject, body string) error {
		_, err := mp.Send(nudgeDeadLetterBounceFrom, to, subject, body)
		return err
	}
	return send, func() { _ = closeBeadStoreHandle(store) }, nil
}
