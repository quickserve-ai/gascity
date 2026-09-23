package main

import (
	"errors"
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

// errUnknownCityOrigin is the sentinel RefuseUnknownCity upgrades when an
// address names a city outside the roster; returned unchanged, it means the
// address is a known scope (roster city, local city, or rig) or carries no
// city segment at all.
var errUnknownCityOrigin = errors.New("origin city unknown")

// cloudWakeGuardApplies says whether the cloud-wake --ref guard rail runs
// for a send. A foreign (peer-city) recipient is classified BEFORE the
// guard and never enters local wake resolution: its wake belongs to its own
// city's mail sweep, so a local alias that happens to share the slash form
// cannot turn a cross-city send into a cloud-wake refusal.
func cloudWakeGuardApplies(foreign bool, hasNudge bool, canonicalTo, ref string) bool {
	return !foreign && hasNudge && canonicalTo != "human" && strings.TrimSpace(ref) == ""
}
