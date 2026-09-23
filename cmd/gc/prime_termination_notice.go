package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// THE FALLBACK HANDOFF (ga-ksac39 S2, half (a) of Cherub's 2026-09-18 decision).
//
// A seat whose session was ended FOR it boots today into a conversation that
// looks exactly like a fresh start. Nothing tells it that a predecessor existed,
// that the predecessor was killed rather than finished, or that no handoff note
// was written. That is the cold-amnesiac gap: the seat cannot even know it
// should be suspicious of its own emptiness.
//
// This reads the termination record S1 writes to the seat's own session bead and
// renders it into the SessionStart hook context, once.
//
// WHY THIS IS A BOOT-TIME READ AND NOT A MAIL SENT AT DEATH (design revision 4;
// v1 specified a mail-to-self and this replaces it):
//
//   - A mail wisp cannot carry a record. Wisps skip DOLT_COMMIT, so a note that
//     goes missing leaves nothing to recover it from; archive-after-inject DELETES
//     the row on a stdout write that may never have been read (ga-vsxs3s); and the
//     retention TTL reaps it. The bead record is versioned history. This is katya's
//     rider R1 — bead history is the source of record, mail only carries — applied
//     one layer out from the ratio to the note.
//   - Writing it at death means writing it during the stop, in exactly the
//     sick-store windows where force-exits cluster and the write is likeliest to
//     fail or hang. Reading it at boot costs nothing: the seat is already doing
//     store reads, and a boot that cannot reach the store is not a boot.
//   - The record it renders is the one the ratio already counts, so the notice and
//     katya's ga-fbzz9u read can never disagree about what happened.
//
// WHAT IT DELIBERATELY DOES NOT CARRY, so the omissions are decisions:
//
//   - No pane tail, now or later. argv on this box has carried live provider keys
//     (ga-icxer4) and a redactor is a denylist that fails open.
//   - No list of the seat's in-progress beads, YET. Getting that right means
//     reading the seat's OWN work store: a city-store read silently under-reports
//     for a rig seat, and a partial list of "your beads" in a recovery note reads
//     as authoritative and is worse than none. The seat's own resume check already
//     enumerates them correctly, per identity and per store. Adding the list is a
//     follow-up that must resolve the work store, not a line to sneak in here.
//   - No instructions about how to recover. Every instruction in the first version
//     of the config-drift note was wrong (ga-68f9qa): the controller does not know
//     which startup protocol the seat's pack defines. Facts, then defer.

// terminationNoticeInjection renders the once-only "your last session did not
// hand off" block for the booting seat, plus the stamp that stops it repeating.
//
// The stamp is returned as an afterDelivery callback rather than written inline,
// mirroring sessionStartAutoHandoffInjection: a `--json` PREVIEW renders the
// exact text the hook would emit but must not consume the notice out from under
// the real SessionStart invocation.
//
// Every failure degrades to "" and never blocks a prime. A seat that cannot read
// its own bead has bigger problems than a missing notice, and a prime that
// refused to run over one would turn a cosmetic gap into an outage.
func terminationNoticeInjection(stderr io.Writer) primeHookContextInjection {
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	if sessionID == "" {
		return primeHookContextInjection{}
	}
	store, cityPath, code := openCityStoreWithPath(io.Discard, "gc prime")
	if store == nil || code != 0 {
		return primeHookContextInjection{}
	}
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	sessStore := cliSessionStore(store, cfg, cityPath)
	if sessStore == nil {
		return primeHookContextInjection{}
	}
	bead, err := sessStore.Get(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: reading termination record: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
		return primeHookContextInjection{}
	}
	// THE MARK IS STAMPED WHETHER OR NOT A NOTICE RENDERS, and that is enforced
	// by there being ONE return path rather than by a test. The mark doubles as
	// this seat's last-boot marker; a marker that only advanced when a notice
	// fired would leave the freshness window frozen at the last notice, which is
	// the exact hole it exists to close (katya, S2 review). Two return paths here
	// would make forgetting it a one-line edit.
	return primeHookContextInjection{
		text:          terminationNoticeText(bead.Metadata, time.Now().UTC()),
		afterDelivery: func() { stampTerminationNoticeChecked(sessStore, sessionID, stderr) },
	}
}

// terminationNoticeText is the whole decision, pure: metadata in, notice or ""
// out. Separated from the store reads so both arms are testable without a city,
// and so the injector above has nothing left to branch on.
func terminationNoticeText(meta map[string]string, now time.Time) string {
	rec, ok := sessionpkg.ReadTerminationRecord(meta)
	if !ok || !rec.NoticeOwed() {
		return ""
	}
	return renderTerminationNotice(rec, now)
}

// stampTerminationNoticeChecked records that this seat has now looked. It closes
// the freshness window: the next boot serves a notice only for an ending that
// happened after this instant.
func stampTerminationNoticeChecked(sessStore beads.Store, sessionID string, stderr io.Writer) {
	patch := sessionpkg.TerminationNoticeCheckedPatch(time.Now().UTC())
	if err := sessionpkg.NewStore(beads.SessionStore{Store: sessStore}).ApplyPatch(sessionID, patch); err != nil {
		// Worth one line. A failed stamp degrades in the SAFE direction — the
		// window stays where it was, so the next boot may re-render a notice it
		// has already shown, which is noise rather than loss or a false claim.
		// But a seat seeing the same ending three times should be able to find
		// out why.
		fmt.Fprintf(stderr, "gc prime: stamping the termination notice check: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
	}
}

// renderTerminationNotice is the text itself. Separated from the store reads so
// the wording is testable without a city.
//
// ACTOR AND REASON ARE SANITIZED. Reason carries an operator's free-text
// `--reason` verbatim by design, so it is attacker-reachable in exactly the way
// mail bodies are: without this, a crafted reason could close the
// <system-reminder> block and speak to the seat in its own voice
// (gastownhall/gascity#2195, the same defense formatInjectOutput applies).
func renderTerminationNotice(rec sessionpkg.TerminationRecord, now time.Time) string {
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	b.WriteString("YOUR PREVIOUS SESSION DID NOT HAND OFF. It was ended for you, and no\n")
	b.WriteString("handoff note was composed, so nothing was carried forward from it.\n\n")
	fmt.Fprintf(&b, "  kind:      %s\n", extmsg.SanitizeForSystemReminder(string(rec.Kind)))
	if actor := extmsg.SanitizeForSystemReminder(rec.Actor); actor != "" {
		fmt.Fprintf(&b, "  by:        %s\n", actor)
	}
	if reason := extmsg.SanitizeForSystemReminder(rec.Reason); reason != "" {
		fmt.Fprintf(&b, "  reason:    %s\n", reason)
	}
	if !rec.At.IsZero() {
		fmt.Fprintf(&b, "  ended:     %s (%s ago)\n", rec.At.UTC().Format(time.RFC3339), terminationNoticeAge(now.Sub(rec.At)))
	}
	if !rec.RequestedAt.IsZero() && !rec.At.IsZero() {
		fmt.Fprintf(&b, "  requested: %s (%s before the stop)\n",
			rec.RequestedAt.UTC().Format(time.RFC3339), terminationNoticeAge(rec.At.Sub(rec.RequestedAt)))
	}
	b.WriteString("\nThis notice is MECHANICAL: gc read it off the termination record on your\n")
	b.WriteString("own session bead. It carries no reasoning, no plan and no summary of what\n")
	b.WriteString("the previous session was doing, because none of that was ever recorded.\n\n")
	b.WriteString("Treat your recall of that session as ABSENT rather than lossy. It\n")
	b.WriteString("deliberately does not tell you how to recover: your own role prompt\n")
	b.WriteString("defines your startup protocol, and a confident wrong instruction from\n")
	b.WriteString("here is worse than none.\n\n")
	b.WriteString("It also does NOT list the work you had in progress. That omission is\n")
	b.WriteString("deliberate, and it is stated here so that you do not read this notice\n")
	b.WriteString("as a complete account of what you were doing.\n")
	b.WriteString("</system-reminder>\n")
	return b.String()
}

// terminationNoticeAge renders a duration at one significant unit. Deliberately coarse:
// the seat needs to know whether this happened minutes or days ago, and a
// spurious "56m32.4s" invites reading precision the clock does not have.
func terminationNoticeAge(d time.Duration) string {
	if d < 0 {
		// A future timestamp is a clock disagreement, not a negative age.
		// Saying so is better than rendering "-3m", which reads as a bug in the
		// record rather than in the clocks.
		return "clock skew"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
