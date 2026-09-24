package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
)

// mailCityRosterFor builds the cross-city mail roster for the loaded city.
// The zero roster (no [mail.crosscity] section, or no loadable config) is
// disabled and leaves every recipient resolving exactly as today.
func mailCityRosterFor(cfg *config.City, cityPath string) mail.CityRoster {
	local, peers := cfg.MailCityRoster(loadedCityName(cfg, cityPath))
	roster := mail.CityRoster{Local: local, Peers: peers, Towns: cfg.MailCrossCityTowns()}
	if roster.Enabled() {
		roster.RosterRoot, roster.RosterPin, roster.RosterSourceErr = cfg.MailCrossCityRosterSource(cityPath)
	}
	return roster
}

// crossCitySeatRefusal is the stderr line for a peer-city send the target
// town's rendered roster refuses (PROP-027 1.4): the seat is not listed, or
// the list itself could not be read. Nothing is stored either way.
func crossCitySeatRefusal(err error) string {
	return "gc mail: " + err.Error()
}

// crossCitySendGate is the one place a CLI send's FINAL recipient string —
// the exact form handed to the provider, after any local-city prefix has
// been stripped — meets the target town's rendered roster. It reports
// whether the send was refused and, if so, the exit code after printing the
// refusal (as JSON when asked).
func crossCitySendGate(roster mail.CityRoster, final string, jsonOut bool, stdout, stderr io.Writer) (refused bool, code int) {
	seatErr := roster.CheckForeignSeat(final)
	if seatErr == nil {
		return false, 0
	}
	msg := crossCitySeatRefusal(seatErr)
	if jsonOut {
		return true, writeJSONError(stdout, stderr, "cross_city_unknown_seat", msg, 1)
	}
	fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
	return true, 1
}

// crossCityReplyRosterWarning is printed when a reply into an existing
// foreign-origin thread disagrees with the roster. The reply is still
// written — the thread's peer id already resolved once — but the mismatch
// is named so a stale or missing roster gets fixed.
func crossCityReplyRosterWarning(err error) string {
	return "gc mail reply: warning: roster mismatch for the thread's peer: " + mail.RosterMismatchDetail(err) + "; the reply is written into the existing thread"
}

// crossCityBroadcastRecipients gates a broadcast's recipient set the way a
// direct send is gated: a live session's mailbox address is its free-form
// alias, and one shaped like a peer-city seat absent from that town's
// rendered roster is dropped from the set and named on stderr. The rest of
// the broadcast still goes out — a policy broadcast is not refused whole
// because one local session squats a foreign-shaped alias.
func crossCityBroadcastRecipients(roster mail.CityRoster, recipients map[string]bool, stderr io.Writer) map[string]bool {
	if !roster.Enabled() || len(recipients) == 0 {
		return recipients
	}
	for addr := range recipients {
		if err := roster.CheckForeignSeat(addr); err != nil {
			fmt.Fprintf(stderr, "gc mail send --all: skipping %s: %s\n", addr, err.Error()) //nolint:errcheck // best-effort stderr
			delete(recipients, addr)
		}
	}
	return recipients
}

// crossCityNotifyRefusal is the typed refusal for --notify on a recipient in
// another city. A cross-machine wake primitive does not exist; the
// recipient's wake is its own city's mail sweep, so the request is refused
// loudly before any write rather than reporting a nudge into a leg that is
// not there.
func crossCityNotifyRefusal(cmdName, recipient string) string {
	return fmt.Sprintf("%s: --notify does not cross cities: %q is in city %q; its wake is that city's own mail sweep. Retry without --notify.",
		cmdName, recipient, strings.SplitN(recipient, "/", 2)[0])
}

// defaultMailSendNotify is the notify-on-by-default decision for a direct
// local send once the caller has established that the send is local, not
// remote, and that no flag was given: a peer-city recipient never receives
// the default, because --notify does not cross cities and a defaulted flag
// must not turn a plain foreign send into a refusal the user never asked for.
func defaultMailSendNotify(foreignRecipient bool) bool {
	return !foreignRecipient
}

// mailSendRecipientIsForeign classifies the send's recipient (--to, else the
// first positional) against the ambient roster. With no [mail.crosscity]
// section every recipient is local and the default applies as before.
func mailSendRecipientIsForeign(args []string, to string) bool {
	recipient := strings.TrimSpace(to)
	if recipient == "" && len(args) > 0 {
		recipient = strings.TrimSpace(args[0])
	}
	if recipient == "" {
		return false
	}
	cityPath, cfg := ambientMailTargetConfig()
	roster := mailCityRosterFor(cfg, cityPath)
	if !roster.Enabled() {
		return false
	}
	kind, _ := roster.ResolveCityAddress(recipient)
	return kind == mail.CityAddressForeign
}

// localForeignSendWarning is printed after a peer-city send that was written
// to THIS city's store. Phase 1 of cross-city addressing moves no store: the
// message has no reader here unless a poller relays that mailbox, and the
// way to reach the peer is to write it on the hub with --context.
func localForeignSendWarning(recipient string) string {
	return fmt.Sprintf("gc mail send: stored locally for %s; nothing delivers it unless a poller relays this mailbox — send with --context <hub> to write it on the hub", recipient)
}

// cloudWakeGuardApplies says whether the cloud-wake --ref guard rail runs
// for a send. A foreign (peer-city) recipient is classified BEFORE the
// guard and never enters local wake resolution: its wake belongs to its own
// city's mail sweep, so a local alias that happens to share the slash form
// cannot turn a cross-city send into a cloud-wake refusal.
func cloudWakeGuardApplies(foreign bool, hasNudge bool, canonicalTo, ref string) bool {
	return !foreign && hasNudge && canonicalTo != "human" && strings.TrimSpace(ref) == ""
}
