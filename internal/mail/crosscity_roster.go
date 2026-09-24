package mail

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// townSeatRig is the "rig" value a rendered roster gives a town-level seat:
// one addressed as <town>/<name> rather than through a rig.
const townSeatRig = "town"

// TownSeat is one entry of a town's rendered roster (cities/<town>/agents.json
// in the bridge): Address is the sender-facing alias (bare for a crew seat,
// <town>/<name> for a town-level seat), Nudge the seat's own <rig>/<name>
// form, Rig the rig it lives in or "town". Other fields are ignored.
type TownSeat struct {
	Address string `json:"address"`
	Nudge   string `json:"nudge"`
	Rig     string `json:"rig"`
}

// TownRoster is a town's rendered address list, read at send time from the
// local pack cache: never fetched live.
type TownRoster struct {
	// Town is the roster's own town name as the file states it.
	Town string `json:"town"`
	// Seats lists every seat the town renders.
	Seats []TownSeat `json:"agents"`
}

// TownRosterPath returns where a town's rendered roster lives under a roster
// root: <root>/cities/<town>/agents.json, the layout the bridge repository
// ships and the pack cache mirrors at the pinned commit.
func TownRosterPath(root, town string) string {
	return filepath.Join(root, "cities", town, "agents.json")
}

// LoadTownRoster reads and parses a rendered roster file. Any read or parse
// failure is returned with the path in context; the caller decides whether
// that fails closed (a send) or degrades to a warning (a reply).
func LoadTownRoster(path string) (TownRoster, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TownRoster{}, fmt.Errorf("reading roster %s: %w", path, err)
	}
	var roster TownRoster
	if err := json.Unmarshal(data, &roster); err != nil {
		return TownRoster{}, fmt.Errorf("parsing roster %s: %w", path, err)
	}
	return roster, nil
}

// Len returns the number of seats the roster lists.
func (r TownRoster) Len() int {
	return len(r.Seats)
}

// Knows reports whether local — the address remainder after the city
// segment, for a recipient addressed as <city>/<local> — names a seat the
// roster lists. The match is exact and case-sensitive, with no prefix or
// fuzzy resolution: a crew seat matches on its <rig>/<name> nudge form, a
// town-level seat on the <name> of its <town>/<name> address, and the
// <city>.<name> spelling of a town-level seat (the way a city's own
// city-level seat stamps its sends) is that same seat.
func (r TownRoster) Knows(city, local string) bool {
	if local == "" || strings.HasPrefix(local, "/") || strings.HasSuffix(local, "/") || strings.Contains(local, "//") {
		// An empty segment can never be a listed seat; refusing it here
		// keeps the gate exact on the string that will be stored.
		return false
	}
	// The literal forms first: a crew seat's nudge, a town-level seat's
	// <town>/<local> address. Only when neither lists it is <city>.<name>
	// read as the alias of the town-level seat <name>.
	if r.knowsLiteral(local) {
		return true
	}
	if alias, ok := strings.CutPrefix(local, city+"."); ok && alias != "" {
		return r.knowsTownSeat(alias)
	}
	return false
}

// knowsLiteral matches local exactly: a crew seat on its nudge, a
// town-level seat on its <town>/<local> address.
func (r TownRoster) knowsLiteral(local string) bool {
	for _, seat := range r.Seats {
		if seat.Rig == townSeatRig {
			if seat.Address == r.Town+"/"+local {
				return true
			}
			continue
		}
		if seat.Nudge == local {
			return true
		}
	}
	return false
}

// knowsTownSeat matches name against town-level seats only.
func (r TownRoster) knowsTownSeat(name string) bool {
	for _, seat := range r.Seats {
		if seat.Rig == townSeatRig && seat.Address == r.Town+"/"+name {
			return true
		}
	}
	return false
}

// UnknownSeatError refuses a send to a peer-city seat that the peer town's
// rendered roster does not list. Nothing is stored: a row addressed to a seat
// nobody polls is the failure this refusal exists for.
type UnknownSeatError struct {
	// Address is the canonical <city>/<local> recipient as sent.
	Address string
	// Town is the peer town whose roster was consulted.
	Town string
	// Pin is the commit the roster was rendered at, or RosterPinUnknown.
	Pin string
	// Known is how many seats the roster lists.
	Known int
}

// Detail names the mismatch without the send-refusal tail: the part a reply
// warning can print truthfully, since the reply is written.
func (e *UnknownSeatError) Detail() string {
	return fmt.Sprintf("unknown seat %s in town %s — not in cities/%s/agents.json @ %s (%d known seats)",
		e.Address, e.Town, e.Town, e.Pin, e.Known)
}

// Error is the send refusal: the mismatch, then the fact that nothing was
// stored.
func (e *UnknownSeatError) Error() string {
	return e.Detail() + ". Nothing sent."
}

// RosterUnreadableError refuses a send to a mapped peer town whose rendered
// roster could not be read at send time. It fails closed on purpose: a
// missing list must never widen delivery to whatever address was typed.
type RosterUnreadableError struct {
	// Town is the peer town whose roster was wanted.
	Town string
	// Path is where the roster was looked for.
	Path string
	// Err is the read or parse failure.
	Err error
}

// Detail names the unreadable roster without the send-refusal tail.
func (e *RosterUnreadableError) Detail() string {
	if e.Path == "" {
		return fmt.Sprintf("roster for town %s could not be located: %v", e.Town, e.Err)
	}
	return fmt.Sprintf("roster for town %s could not be read at %s: %v", e.Town, e.Path, e.Err)
}

// Error is the send refusal: the unreadable roster, then the fact that
// nothing was stored. It fails closed on purpose.
func (e *RosterUnreadableError) Error() string {
	return e.Detail() + ". Nothing sent."
}

// RosterMismatchDetail returns the mismatch a CheckForeignSeat refusal
// describes, without its "Nothing sent." tail, for the one place the
// refusal is downgraded to a warning: a reply into an existing thread,
// which IS written. Any other error is returned as its own text.
func RosterMismatchDetail(err error) string {
	var unknown *UnknownSeatError
	if errors.As(err, &unknown) {
		return unknown.Detail()
	}
	var unreadable *RosterUnreadableError
	if errors.As(err, &unreadable) {
		return unreadable.Detail()
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// Unwrap exposes the underlying read failure.
func (e *RosterUnreadableError) Unwrap() error {
	return e.Err
}

// CheckForeignSeat applies the peer town's address list to recipient, which
// must be the FINAL string the send will store: the gate does no
// normalization of its own, so what it admits is exactly what is written
// (a remainder still carrying an empty segment is refused). It is a no-op
// (nil) unless the recipient's first segment names a peer city, that city
// maps to a town in Towns, and a RosterRoot is known: an unmapped peer city
// and a city with no list source keep today's behavior exactly. For a
// mapped town it returns *RosterUnreadableError when the roster source
// could not be resolved (RosterSourceErr) or the roster file cannot be read
// or parsed, and *UnknownSeatError when the seat is not listed.
func (r CityRoster) CheckForeignSeat(recipient string) error {
	if !r.Enabled() {
		return nil
	}
	city, local, found := strings.Cut(recipient, "/")
	if !found || city == r.Local || !slices.Contains(r.Peers, city) {
		return nil
	}
	canonical := recipient
	town := r.Towns[city]
	if town == "" {
		return nil
	}
	if r.RosterSourceErr != nil {
		return &RosterUnreadableError{Town: town, Err: r.RosterSourceErr}
	}
	if strings.TrimSpace(r.RosterRoot) == "" {
		return nil
	}
	path := TownRosterPath(r.RosterRoot, town)
	roster, err := LoadTownRoster(path)
	if err != nil {
		return &RosterUnreadableError{Town: town, Path: path, Err: err}
	}
	if roster.Knows(city, local) {
		return nil
	}
	pin := strings.TrimSpace(r.RosterPin)
	if pin == "" {
		pin = config.RosterPinUnknown
	}
	return &UnknownSeatError{Address: canonical, Town: town, Pin: pin, Known: roster.Len()}
}
