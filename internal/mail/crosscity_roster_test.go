package mail

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testTownRosterJSON = `{
  "town": "alex",
  "agents": [
    {"address": "alex/steward", "nudge": "steward", "rig": "town", "role": "town-steward"},
    {"address": "alex/human",   "nudge": "qcore/tessa", "rig": "town", "role": "overseer-via-tessa"},
    {"address": "tessa",  "nudge": "qcore/tessa",  "rig": "qcore",   "role": "assistant", "type": "crew"},
    {"address": "navani", "nudge": "gascity/navani", "rig": "gascity", "role": "engineer", "type": "crew"}
  ]
}`

func writeTestTownRoster(t *testing.T, root, town, body string) string {
	t.Helper()
	path := TownRosterPath(root, town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestTownRosterPathShape(t *testing.T) {
	got := TownRosterPath("/roster", "alex")
	want := filepath.Join("/roster", "cities", "alex", "agents.json")
	if got != want {
		t.Errorf("TownRosterPath = %q, want %q", got, want)
	}
}

// The seat match is exact and case-sensitive: a crew seat matches on its
// <rig>/<name> nudge form, a town-level seat on its <town>/<name> address,
// and the <city>.<name> spelling of a town-level seat is that seat.
func TestTownRosterKnows(t *testing.T) {
	root := t.TempDir()
	writeTestTownRoster(t, root, "alex", testTownRosterJSON)
	roster, err := LoadTownRoster(TownRosterPath(root, "alex"))
	if err != nil {
		t.Fatalf("LoadTownRoster: %v", err)
	}
	if roster.Len() != 4 {
		t.Errorf("Len = %d, want 4", roster.Len())
	}
	cases := []struct {
		city, local string
		want        bool
	}{
		{"qlandia", "qcore/tessa", true},
		{"qlandia", "gascity/navani", true},
		{"qlandia", "steward", true},
		{"qlandia", "qlandia.steward", true},
		{"qlandia", "human", true},
		{"qlandia", "qcore/lyft", false},
		{"qlandia", "navani", false},          // the bare alias is not the canonical seat
		{"qlandia", "QCORE/tessa", false},     // case-sensitive
		{"qlandia", "gascity/navani/", false}, // no normalization here
		{"qlandia", "alex/steward", false},    // the roster's own town prefix is not an address form
		{"qlandia", "", false},
	}
	for _, tc := range cases {
		if got := roster.Knows(tc.city, tc.local); got != tc.want {
			t.Errorf("Knows(%q, %q) = %v, want %v", tc.city, tc.local, got, tc.want)
		}
	}
}

func TestLoadTownRosterRejectsMalformed(t *testing.T) {
	root := t.TempDir()
	path := writeTestTownRoster(t, root, "alex", `{"town": "alex", "agents": [`)
	if _, err := LoadTownRoster(path); err == nil {
		t.Fatal("LoadTownRoster accepted malformed JSON")
	}
}

func rosterWithTowns(root string) CityRoster {
	return CityRoster{
		Local:      "westeros",
		Peers:      []string{"qlandia", "gastown"},
		Towns:      map[string]string{"qlandia": "alex", "gastown": "cherub"},
		RosterRoot: root,
		RosterPin:  "4e05dae7bf6e1f5be09ab2846c858f3b40762fd5",
	}
}

func TestCheckForeignSeatRefusesAbsentSeatVerbatim(t *testing.T) {
	root := t.TempDir()
	writeTestTownRoster(t, root, "alex", testTownRosterJSON)
	err := rosterWithTowns(root).CheckForeignSeat("qlandia/qcore/lyft")
	var unknown *UnknownSeatError
	if !errors.As(err, &unknown) {
		t.Fatalf("CheckForeignSeat = %v, want *UnknownSeatError", err)
	}
	want := "unknown seat qlandia/qcore/lyft in town alex — not in cities/alex/agents.json @ 4e05dae7bf6e1f5be09ab2846c858f3b40762fd5 (4 known seats). Nothing sent."
	if err.Error() != want {
		t.Errorf("Error() =\n  %q\nwant\n  %q", err.Error(), want)
	}
}

func TestCheckForeignSeatAdmitsListedSeat(t *testing.T) {
	root := t.TempDir()
	writeTestTownRoster(t, root, "alex", testTownRosterJSON)
	r := rosterWithTowns(root)
	for _, addr := range []string{"qlandia/gascity/navani", "qlandia/steward", "qlandia/qlandia.steward"} {
		if err := r.CheckForeignSeat(addr); err != nil {
			t.Errorf("CheckForeignSeat(%q) = %v, want nil", addr, err)
		}
	}
}

// A mapped town whose roster file is absent or unreadable fails CLOSED with
// its own text: delivery is never widened by a missing list.
func TestCheckForeignSeatFailsClosedWithoutRosterFile(t *testing.T) {
	root := t.TempDir()
	err := rosterWithTowns(root).CheckForeignSeat("gastown/qcore/tessa")
	var unreadable *RosterUnreadableError
	if !errors.As(err, &unreadable) {
		t.Fatalf("CheckForeignSeat = %v, want *RosterUnreadableError", err)
	}
	wantPath := TownRosterPath(root, "cherub")
	if unreadable.Town != "cherub" || unreadable.Path != wantPath {
		t.Errorf("RosterUnreadableError = %+v, want town cherub at %s", unreadable, wantPath)
	}
	if got := err.Error(); got == "" || !containsAll(got, "roster for town cherub could not be read at "+wantPath, "Nothing sent.") {
		t.Errorf("Error() = %q, want the town and path named", got)
	}
}

// An unmapped peer city, or a roster with no list source, keeps today's
// behavior: no list, no refusal beyond the city-level one — even when a
// roster for that town happens to be on disk, since only the mapping
// makes it apply.
func TestCheckForeignSeatNoListIsNoOp(t *testing.T) {
	root := t.TempDir()
	writeTestTownRoster(t, root, "cherub", `{"town":"cherub","agents":[{"address":"cherub/steward","nudge":"steward","rig":"town"}]}`)
	r := rosterWithTowns(root)
	r.Towns = map[string]string{"qlandia": "alex"}
	if err := r.CheckForeignSeat("gastown/anything/at/all"); err != nil {
		t.Errorf("unmapped city: CheckForeignSeat = %v, want nil", err)
	}
	r.RosterRoot = ""
	if err := r.CheckForeignSeat("qlandia/qcore/lyft"); err != nil {
		t.Errorf("no roster root: CheckForeignSeat = %v, want nil", err)
	}
	var zero CityRoster
	if err := zero.CheckForeignSeat("qlandia/qcore/lyft"); err != nil {
		t.Errorf("zero roster: CheckForeignSeat = %v, want nil", err)
	}
}

// A non-foreign address is never checked: the list is about peer seats only.
func TestCheckForeignSeatSkipsLocalAndBare(t *testing.T) {
	root := t.TempDir()
	r := rosterWithTowns(root)
	for _, addr := range []string{"westeros/qcore/lyft", "qcore/lyft", "human"} {
		if err := r.CheckForeignSeat(addr); err != nil {
			t.Errorf("CheckForeignSeat(%q) = %v, want nil", addr, err)
		}
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// Finding 3: the gate validates exactly the string that will be stored. A
// recipient still carrying an empty segment after entry normalization
// ("qlandia/steward/") must be refused, never re-normalized into a listed
// seat while the slash form is what gets written.
func TestCheckForeignSeatRefusesEmptySegmentVerbatim(t *testing.T) {
	root := t.TempDir()
	writeTestTownRoster(t, root, "alex", testTownRosterJSON)
	r := rosterWithTowns(root)
	for _, addr := range []string{"qlandia/steward/", "qlandia/gascity/navani/", "qlandia//steward", "qlandia/gascity//navani"} {
		err := r.CheckForeignSeat(addr)
		var unknown *UnknownSeatError
		if !errors.As(err, &unknown) {
			t.Errorf("CheckForeignSeat(%q) = %v, want *UnknownSeatError", addr, err)
			continue
		}
		if unknown.Address != addr {
			t.Errorf("CheckForeignSeat(%q) refused %q, want the string as given", addr, unknown.Address)
		}
	}
}

// Finding 4: a town-level seat whose literal name carries the <city>.
// spelling ("alex/qlandia.worker") is matched literally FIRST; the
// <city>.<name> alias rule applies only when the literal form is absent.
func TestTownRosterKnowsLiteralTownAddressBeforeAlias(t *testing.T) {
	root := t.TempDir()
	writeTestTownRoster(t, root, "alex", `{"town":"alex","agents":[
	  {"address":"alex/qlandia.worker","nudge":"worker","rig":"town"},
	  {"address":"alex/steward","nudge":"steward","rig":"town"}]}`)
	roster, err := LoadTownRoster(TownRosterPath(root, "alex"))
	if err != nil {
		t.Fatalf("LoadTownRoster: %v", err)
	}
	if !roster.Knows("qlandia", "qlandia.worker") {
		t.Error(`Knows("qlandia", "qlandia.worker") = false, want the literal alex/qlandia.worker matched`)
	}
	if !roster.Knows("qlandia", "qlandia.steward") {
		t.Error(`Knows("qlandia", "qlandia.steward") = false, want the alias spelling of alex/steward admitted`)
	}
	if roster.Knows("qlandia", "worker") {
		t.Error(`Knows("qlandia", "worker") = true, want false: no alex/worker is listed`)
	}
}

// Finding 5: the reply warning names the mismatch without claiming
// "Nothing sent." — the reply IS written.
func TestRosterMismatchDetailOmitsNothingSent(t *testing.T) {
	root := t.TempDir()
	writeTestTownRoster(t, root, "alex", testTownRosterJSON)
	r := rosterWithTowns(root)
	for _, addr := range []string{"qlandia/qcore/lyft", "gastown/qcore/tessa"} {
		err := r.CheckForeignSeat(addr)
		if err == nil {
			t.Fatalf("CheckForeignSeat(%q) = nil, want a refusal to describe", addr)
		}
		detail := RosterMismatchDetail(err)
		if detail == "" || strings.Contains(detail, "Nothing sent") {
			t.Errorf("RosterMismatchDetail(%v) = %q, want the mismatch named without \"Nothing sent.\"", err, detail)
		}
		if !strings.HasSuffix(err.Error(), " Nothing sent.") {
			t.Errorf("Error() = %q, want the refusal to end with \"Nothing sent.\"", err.Error())
		}
	}
}
