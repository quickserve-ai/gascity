package mail

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/session"
)

// ErrUnresolvedCityProbe is the error a caller passes to RefuseUnknownCity
// when it has no resolution failure of its own to upgrade — a reply's thread
// origin, or a storeless send — and only wants the address classified.
// Returned unchanged, it means the address names a known scope or carries no
// city segment.
var ErrUnresolvedCityProbe = errors.New("origin city unknown")

// CityRoster names the local city and the peer cities addressable with
// city-qualified mail addresses of the form <city>/<address>. The first path
// segment is the city; everything after the first "/" is that city's own
// address form and is opaque to every other city. City names match exactly,
// never case-folded.
type CityRoster struct {
	Local string
	Peers []string
	// Towns maps a peer city name to the town whose rendered roster
	// (cities/<town>/agents.json) lists its seats. A peer city with no
	// mapping has no list and keeps the city-level rules only.
	Towns map[string]string
	// RosterRoot is the directory holding cities/<town>/agents.json: the
	// pack-cache clone of the imported repository that ships them at its
	// pinned commit, or the configured roster_root. Empty means no list.
	RosterRoot string
	// RosterPin is the commit RosterRoot was rendered at, printed in a
	// refusal so the reader knows which roster said no; RosterPinUnknown
	// when the root was hand-pointed.
	RosterPin string
	// RosterSourceErr records a failure to resolve RosterRoot (an ambiguous
	// or unreadable pack pin). A send to a mapped town then fails closed
	// with that reason rather than proceeding without a list.
	RosterSourceErr error
}

// Enabled reports whether cross-city addressing is configured: a roster needs
// both a local city name and at least one peer. The zero value is disabled and
// leaves every recipient exactly as it resolves today.
func (r CityRoster) Enabled() bool {
	return strings.TrimSpace(r.Local) != "" && len(r.Peers) > 0
}

// CityAddressKind classifies a recipient under cross-city addressing rules.
type CityAddressKind int

const (
	// CityAddressNone means the recipient carries no cross-city semantics and
	// resolves exactly as it does without a roster.
	CityAddressNone CityAddressKind = iota
	// CityAddressLocal means the recipient named the local city; the returned
	// address is the remainder, resolved locally. <local>/<addr> and <addr>
	// are one mailbox.
	CityAddressLocal
	// CityAddressForeign means the recipient named a peer city; the returned
	// address is canonical as written and must not be session-resolved.
	CityAddressForeign
)

// ResolveCityAddress classifies recipient by its first path segment. A
// recipient with no "/", an empty remainder, or a first segment naming no
// roster city is CityAddressNone with the recipient returned unchanged, so
// bare names and rig-qualified names keep today's behavior.
//
// The remainder gets the same minimal normalization local resolution applies
// (surrounding whitespace and one trailing "/" trimmed), so a foreign send
// cannot mint a mailbox the destination's own reads would never serve. The
// split is single-level: <local>/<local>/<addr> is not recursively stripped.
func (r CityRoster) ResolveCityAddress(recipient string) (CityAddressKind, string) {
	if !r.Enabled() {
		return CityAddressNone, recipient
	}
	city, rest, found := strings.Cut(strings.TrimSpace(recipient), "/")
	if !found {
		return CityAddressNone, recipient
	}
	rest = strings.TrimSuffix(strings.TrimSpace(rest), "/")
	if rest == "" {
		return CityAddressNone, recipient
	}
	if city == r.Local {
		return CityAddressLocal, rest
	}
	if slices.Contains(r.Peers, city) {
		return CityAddressForeign, city + "/" + rest
	}
	return CityAddressNone, recipient
}

// UnknownCityError refuses an address whose first segment names no known
// city. It deliberately replaces the resolution failure it upgrades rather
// than wrapping it: a stale roster must refuse with the cities it knows,
// never fall through to a session lookup spelled "session not found".
type UnknownCityError struct {
	City  string
	Known []string
}

func (e *UnknownCityError) Error() string {
	return fmt.Sprintf("unknown city %q (known cities: %s)", e.City, strings.Join(e.Known, ", "))
}

// RefuseUnknownCity upgrades a NOT-FOUND resolution failure into an
// *UnknownCityError when the failed recipient's first segment names neither a
// known city nor a local scope: the local city, a rig or agent directory
// (config.City.LocalAddressPrefixes), or the session-target forms
// "template:<rig>/<name>", "controller" and "human". Every other case returns
// err unchanged: no roster, no "/", a known scope — and, above all, an error
// that is not a not-found (a store timeout, a name-squat refusal), which must
// keep its own text rather than be re-spelled as "unknown city". Only
// session.ErrSessionNotFound and ErrUnresolvedCityProbe are upgraded.
func RefuseUnknownCity(err error, recipient string, roster CityRoster, localScopes []string) error {
	if err == nil || !roster.Enabled() {
		return err
	}
	if !errors.Is(err, session.ErrSessionNotFound) && !errors.Is(err, ErrUnresolvedCityProbe) {
		return err
	}
	segment, rest, found := strings.Cut(strings.TrimSpace(recipient), "/")
	if !found || rest == "" || segment == roster.Local {
		return err
	}
	if strings.HasPrefix(segment, "template:") || segment == "controller" || segment == "human" {
		return err
	}
	if slices.Contains(roster.Peers, segment) {
		return err
	}
	for _, scope := range localScopes {
		// A scope may be a nested agent dir ("projects/backend"); the
		// address starts with its leading segment, which is what the
		// config-side collision check compares too.
		leading, _, _ := strings.Cut(strings.TrimSpace(scope), "/")
		if leading == segment {
			return err
		}
	}
	known := make([]string, 0, 1+len(roster.Peers))
	known = append(known, roster.Local)
	known = append(known, roster.Peers...)
	return &UnknownCityError{City: segment, Known: known}
}

// ExpandLocalRecipients appends the city-qualified alias of each local
// recipient address, so a read serves mail addressed either way: delivery to
// <local>/<addr> is a read on <addr>'s inbox. Recipients already carrying a
// roster city segment are left alone; duplicates collapse.
func (r CityRoster) ExpandLocalRecipients(recipients []string) []string {
	if !r.Enabled() {
		return recipients
	}
	seen := make(map[string]bool, 2*len(recipients))
	out := make([]string, 0, 2*len(recipients))
	add := func(addr string) {
		if addr == "" || seen[addr] {
			return
		}
		seen[addr] = true
		out = append(out, addr)
	}
	for _, addr := range recipients {
		add(addr)
	}
	for _, addr := range recipients {
		if kind, _ := r.ResolveCityAddress(addr); kind != CityAddressNone {
			continue
		}
		add(r.Local + "/" + addr)
	}
	return out
}

// QualifySender returns the sender's city-qualified form for a message that
// crosses cities, so the recipient's plain reply resolves back here. A sender
// already carrying a roster city segment is returned unchanged.
func (r CityRoster) QualifySender(sender string) string {
	if !r.Enabled() || sender == "" {
		return sender
	}
	if kind, _ := r.ResolveCityAddress(sender); kind != CityAddressNone {
		return sender
	}
	return r.Local + "/" + sender
}
