package beadmeta

import "strings"

// HoldLabelPrefix marks a bead as parked under the wait-class contract
// (bridge-wait-classes.md): hold:<class> means owned-and-waiting — the park
// has a watcher, and the patrol's label flip restores dispatchability. The
// class set is open by design, so consumers match this PREFIX rather than
// enumerating classes (an enumeration silently misses the next class added
// to the contract).
//
// Contract for consumers (ga-uica16; upstream analog: the hold-label
// conventions doc's serve/exist split): a hold:*-parked bead must not be
// SERVED as work or counted as UNASSIGNED pool demand — but assigned-work
// paths (crash recovery, the owner's own queue) stay hold-transparent, or a
// parked bead's owner goes invisible.
//
// This file is the single definition of the rule. The jq fragment and the Go
// matcher below must express the same predicate; consumers import these
// rather than restating the prefix.
const HoldLabelPrefix = "hold:"

// HoldParkExcludeSelectJQ is the jq select that keeps only beads carrying no
// hold:* label, for shell/jq consumers of bd --json output.
const HoldParkExcludeSelectJQ = `select(([.labels[]? | select(startswith("` + HoldLabelPrefix + `"))] | length) == 0)`

// HasHoldLabel reports whether any of the labels marks a hold:* park.
func HasHoldLabel(labels []string) bool {
	for _, label := range labels {
		if strings.HasPrefix(strings.TrimSpace(label), HoldLabelPrefix) {
			return true
		}
	}
	return false
}
