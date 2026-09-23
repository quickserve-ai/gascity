package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
)

// mailCityRosterFor builds the cross-city mail roster for the loaded city.
// The zero roster (no [mail.crosscity] section, or no loadable config) is
// disabled and leaves every recipient resolving exactly as today.
func mailCityRosterFor(cfg *config.City, cityPath string) mail.CityRoster {
	local, peers := cfg.MailCityRoster(loadedCityName(cfg, cityPath))
	return mail.CityRoster{Local: local, Peers: peers}
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
