package mail

import "strings"

// ForeignTownPrefix reports the leading segment of a slash-qualified recipient
// when that segment names neither this city nor one of its rigs, i.e. when the
// address plausibly names a seat in ANOTHER town (ga-5toacq).
//
// It is advisory only. Callers use it to explain a recipient that has ALREADY
// failed to resolve, never to decide whether a recipient resolves: a hub can
// legitimately accept a foreign-prefixed mailbox (gastown/woodhouse delivered 24
// times through the cross-town poller by 2026-09-16), so gating resolution on
// this would break a working leg. A typo in a local rig name also returns true;
// the wording at the call sites is conditional for that reason.
//
// Without it, a cross-town send fails as "session not found", which cannot be
// told apart from a typo, a retired agent, or a town that does not exist. The
// sender concludes the address was wrong and stops, and the message is lost with
// no record on either side.
func ForeignTownPrefix(recipient, cityName string, rigNames []string) (string, bool) {
	recipient = strings.TrimSpace(recipient)
	slash := strings.Index(recipient, "/")
	if slash <= 0 || slash == len(recipient)-1 {
		return "", false
	}
	prefix := recipient[:slash]
	if prefix == "controller" || prefix == "human" {
		return "", false
	}
	if cityName = strings.TrimSpace(cityName); cityName != "" && prefix == cityName {
		return "", false
	}
	for _, rig := range rigNames {
		if strings.TrimSpace(rig) == prefix {
			return "", false
		}
	}
	return prefix, true
}
