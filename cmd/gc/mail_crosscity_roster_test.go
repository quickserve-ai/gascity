package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	beadmail "github.com/gastownhall/gascity/internal/mail/beadmail"
)

// The rendered roster for town "alex" as the bridge ships it: crew seats
// carry their <rig>/<name> in "nudge"; town-level seats carry <town>/<name>
// in "address". Neutral seat names throughout.
const testAlexRosterJSON = `{
  "town": "alex",
  "agents": [
    {"address": "alex/steward", "nudge": "steward", "rig": "town", "role": "town-steward"},
    {"address": "tessa",  "nudge": "qcore/tessa",   "rig": "qcore",   "role": "assistant", "type": "crew"},
    {"address": "navani", "nudge": "gascity/navani", "rig": "gascity", "role": "engineer",  "type": "crew"}
  ]
}`

// writeCrossCityRosterCity is the hub's shape: local city westeros, peers
// qlandia (town alex, roster present) and gastown (town cherub, roster
// ABSENT), plus an unmapped peer "nowhere-city" that has no list at all.
func writeCrossCityRosterCity(t *testing.T) (cityPath, rosterRoot string) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_AGENT", "")
	cityPath = t.TempDir()
	rosterRoot = t.TempDir()
	alexDir := filepath.Join(rosterRoot, "cities", "alex")
	if err := os.MkdirAll(alexDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(alexDir, "agents.json"), []byte(testAlexRosterJSON), 0o644); err != nil {
		t.Fatalf("WriteFile(agents.json): %v", err)
	}
	cityTOML := `
[workspace]
name = "westeros"

[mail.crosscity]
city = "westeros"
cities = ["qlandia", "gastown", "nowhere-city"]
roster_root = '` + rosterRoot + `'

[mail.crosscity.towns]
qlandia = "alex"
gastown = "cherub"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	return cityPath, rosterRoot
}

// lastStderrLine returns the final non-empty stderr line: the refusal, past
// any harness warning (a test city imports no builtin packs) printed first.
func lastStderrLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

func countMessageBeads(t *testing.T, cityPath string) int {
	t.Helper()
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all, err := store.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List messages: %v", err)
	}
	n := 0
	for _, b := range all {
		if b.Type == "message" {
			n++
		}
	}
	return n
}

// (a) FAILS-IF: a foreign seat absent from its town's rendered roster is
// refused with the exact text, exit non-zero, and no row is stored.
func TestCmdMailSendCrossCityRosterRefusesAbsentSeat(t *testing.T) {
	cityPath, _ := writeCrossCityRosterCity(t)

	var stdout, stderr bytes.Buffer
	code := cmdMailSend(nil, false, false, "human", "qlandia/qcore/lyft", "x", "y", &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdMailSend = 0, want non-zero; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	want := "gc mail: unknown seat qlandia/qcore/lyft in town alex — not in cities/alex/agents.json @ unknown (3 known seats). Nothing sent."
	if got := lastStderrLine(stderr.String()); got != want {
		t.Errorf("last stderr line =\n  %q\nwant\n  %q\n(full stderr: %q)", got, want, stderr.String())
	}
	if n := countMessageBeads(t, cityPath); n != 0 {
		t.Errorf("message count = %d, want 0 (nothing sent)", n)
	}
}

// (b) A listed seat sends and stores exactly as today, under every spelling
// the roster admits.
func TestCmdMailSendCrossCityRosterAdmitsListedSeat(t *testing.T) {
	for _, to := range []string{"qlandia/gascity/navani", "qlandia/steward", "qlandia/qlandia.steward"} {
		t.Run(to, func(t *testing.T) {
			cityPath, _ := writeCrossCityRosterCity(t)
			var stdout, stderr bytes.Buffer
			code := cmdMailSend(nil, false, false, "human", to, "x", "y", &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cmdMailSend = %d, want 0; stderr=%s", code, stderr.String())
			}
			msg, found := findMessageBead(t, cityPath)
			if !found {
				t.Fatal("message bead not found")
			}
			if msg.Assignee != to {
				t.Errorf("Assignee = %q, want %q", msg.Assignee, to)
			}
			if msg.From != "westeros/human" {
				t.Errorf("From = %q, want city-qualified westeros/human", msg.From)
			}
			if strings.Contains(stderr.String(), "unknown seat") {
				t.Errorf("stderr = %q: a listed seat must not be refused", stderr.String())
			}
		})
	}
}

// (c) A mapped town whose roster file is missing fails closed with a text
// that names the town and the path — delivery is never widened silently.
func TestCmdMailSendCrossCityRosterMissingFileRefuses(t *testing.T) {
	cityPath, rosterRoot := writeCrossCityRosterCity(t)

	var stdout, stderr bytes.Buffer
	code := cmdMailSend(nil, false, false, "human", "gastown/qcore/tessa", "x", "y", &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdMailSend = 0, want non-zero; stderr=%s", stderr.String())
	}
	wantPath := filepath.Join(rosterRoot, "cities", "cherub", "agents.json")
	if got := lastStderrLine(stderr.String()); !strings.HasPrefix(got, "gc mail: roster for town cherub could not be read at "+wantPath) {
		t.Errorf("last stderr line = %q, want the missing-roster refusal naming town cherub at %s", got, wantPath)
	}
	if strings.Contains(stderr.String(), "unknown seat") {
		t.Errorf("stderr = %q: a missing roster must be distinct from an absent seat", stderr.String())
	}
	if n := countMessageBeads(t, cityPath); n != 0 {
		t.Errorf("message count = %d, want 0 (nothing sent)", n)
	}
}

// (d) A city not in the fleet roster keeps today's city-level refusal, and a
// peer city with no town mapping keeps today's plain foreign send.
func TestCmdMailSendCrossCityRosterUnknownAndUnmappedCityUnchanged(t *testing.T) {
	cityPath, _ := writeCrossCityRosterCity(t)

	var stdout, stderr bytes.Buffer
	code := cmdMailSend(nil, false, false, "human", "gastwn/qcore/tessa", "x", "y", &stdout, &stderr)
	if code == 0 {
		t.Fatalf("unknown city: cmdMailSend = 0, want non-zero; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown city") || !strings.Contains(stderr.String(), "gastwn") {
		t.Errorf("stderr = %q, want today's unknown-city refusal naming gastwn", stderr.String())
	}
	if strings.Contains(stderr.String(), "unknown seat") || strings.Contains(stderr.String(), "roster for town") {
		t.Errorf("stderr = %q: an unknown city must not reach the seat list", stderr.String())
	}
	if n := countMessageBeads(t, cityPath); n != 0 {
		t.Errorf("message count = %d, want 0", n)
	}

	stdout.Reset()
	stderr.Reset()
	code = cmdMailSend(nil, false, false, "human", "nowhere-city/any/seat", "x", "y", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("unmapped city: cmdMailSend = %d, want 0; stderr=%s", code, stderr.String())
	}
	if n := countMessageBeads(t, cityPath); n != 1 {
		t.Errorf("message count = %d, want 1 (an unmapped peer keeps today's send)", n)
	}
}

// (e) The one exception: a reply into an existing thread whose peer id
// resolves keeps working when the roster disagrees, with a warning that
// names the mismatch.
func TestCmdMailReplyCrossCityRosterMismatchWarnsAndSends(t *testing.T) {
	cityPath, _ := writeCrossCityRosterCity(t)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	seeded, err := beadmail.New(store).Send("qlandia/qcore/lyft", "human", "cutover", "leg is green")
	if err != nil {
		t.Fatalf("seed Send: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdMailReply([]string{seeded.ID, "received"}, "", "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailReply = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning") || !strings.Contains(stderr.String(), "unknown seat qlandia/qcore/lyft in town alex") {
		t.Errorf("stderr = %q, want a warning naming the roster mismatch", stderr.String())
	}
	if n := countMessageBeads(t, cityPath); n != 2 {
		t.Errorf("message count = %d, want 2 (the reply is written despite the stale roster)", n)
	}
}

// The storeless (exec:) send path applies the same list.
func TestCmdMailSendStorelessCrossCityRosterRefusesAbsentSeat(t *testing.T) {
	writeCrossCityRosterCity(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("PATH", t.TempDir())
	script, recordPath := writeExecSendScript(t)
	t.Setenv("GC_MAIL", "exec:"+script)

	var stdout, stderr bytes.Buffer
	code := cmdMailSend(nil, false, false, "human", "qlandia/qcore/lyft", "x", "y", &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdMailSend = 0, want non-zero; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc mail: unknown seat qlandia/qcore/lyft in town alex") {
		t.Errorf("stderr = %q, want the absent-seat refusal", stderr.String())
	}
	if _, err := os.Stat(recordPath); err == nil {
		t.Errorf("the exec provider received a send despite the refusal")
	}

	// Round 2: a local-city prefix is stripped ("westeros/qlandia/qcore/lyft"
	// -> "qlandia/qcore/lyft") and the FINAL string handed to the provider
	// is foreign; it meets the same gate.
	stdout.Reset()
	stderr.Reset()
	code = cmdMailSend(nil, false, false, "human", "westeros/qlandia/qcore/lyft", "x", "y", &stdout, &stderr)
	if code == 0 {
		t.Fatalf("local-prefixed: cmdMailSend = 0, want non-zero; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc mail: unknown seat qlandia/qcore/lyft in town alex") {
		t.Errorf("local-prefixed stderr = %q, want the stripped final string refused", stderr.String())
	}
	if _, err := os.Stat(recordPath); err == nil {
		t.Errorf("local-prefixed: the exec provider received a send despite the refusal")
	}

	// And a listed seat spelled with the local prefix still sends, as the
	// stripped canonical form, with the sender city-qualified.
	stdout.Reset()
	stderr.Reset()
	if code := cmdMailSend(nil, false, false, "human", "westeros/qlandia/gascity/navani", "x", "y", &stdout, &stderr); code != 0 {
		t.Fatalf("local-prefixed listed seat: cmdMailSend = %d, want 0; stderr=%s", code, stderr.String())
	}
	recorded, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("exec provider recorded nothing for the listed seat: %v", err)
	}
	if !strings.Contains(string(recorded), `"qlandia/gascity/navani"`) || !strings.Contains(string(recorded), `"westeros/human"`) {
		t.Errorf("exec payload = %s, want the stripped recipient and a city-qualified sender", recorded)
	}
}

// The store-backed CLI path strips the local prefix before resolution; the
// stripped foreign string must never be stored unlisted.
func TestCmdMailSendCrossCityRosterLocalPrefixedForeignRefused(t *testing.T) {
	cityPath, _ := writeCrossCityRosterCity(t)

	var stdout, stderr bytes.Buffer
	code := cmdMailSend(nil, false, false, "human", "westeros/qlandia/qcore/lyft", "x", "y", &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdMailSend = 0, want non-zero; stderr=%s", stderr.String())
	}
	if n := countMessageBeads(t, cityPath); n != 0 {
		t.Errorf("message count = %d, want 0 (nothing sent)", n)
	}
}

func seedRosterCitySessionAlias(t *testing.T, cityPath, alias string) {
	t.Helper()
	cfg, _ := loadCityConfig(cityPath, io.Discard)
	seedNamedOnDemandSession(t, cityPath, cfg, alias)
}

// Finding 2b: a broadcast writes to every live session mailbox, and a
// session's mailbox address is its free-form alias — one shaped like a
// peer-city seat is gated exactly like a direct send: skipped, named, and
// the rest of the broadcast still goes out.
func TestCmdMailSendAllCrossCityRosterSkipsAbsentForeignAlias(t *testing.T) {
	cityPath, _ := writeCrossCityRosterCity(t)
	seedRosterCitySessionAlias(t, cityPath, "qlandia/qcore/absent")
	seedRosterCitySessionAlias(t, cityPath, "tools/x")

	var stdout, stderr bytes.Buffer
	code := cmdMailSend(nil, false, true, "human", "", "s", "b", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailSend --all = %d, want 0; stderr=%s", code, stderr.String())
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all, err := store.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, b := range all {
		if b.Type == "message" && b.Assignee == "qlandia/qcore/absent" {
			t.Errorf("broadcast stored a row for the absent foreign seat %q", b.Assignee)
		}
	}
	if !strings.Contains(stdout.String(), "tools/x") {
		t.Errorf("stdout = %q, want the local recipient still sent to", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknown seat qlandia/qcore/absent in town alex") {
		t.Errorf("stderr = %q, want the skipped foreign alias named", stderr.String())
	}
}

// Finding 3: a recipient that still carries an empty segment after the
// entry point's one-trailing-slash trim is refused, not admitted by a
// second normalization while the slash form is stored.
func TestCmdMailSendCrossCityRosterRefusesDoubleSlash(t *testing.T) {
	cityPath, _ := writeCrossCityRosterCity(t)

	var stdout, stderr bytes.Buffer
	code := cmdMailSend(nil, false, false, "human", "qlandia/steward//", "x", "y", &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdMailSend = 0, want non-zero; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc mail: unknown seat qlandia/steward/ in town alex") {
		t.Errorf("stderr = %q, want the stored form qlandia/steward/ refused", stderr.String())
	}
	if n := countMessageBeads(t, cityPath); n != 0 {
		t.Errorf("message count = %d, want 0", n)
	}
}

// Finding 5: the reply warning must not claim "Nothing sent." — the reply
// is written.
func TestCmdMailReplyCrossCityRosterWarningWording(t *testing.T) {
	cityPath, _ := writeCrossCityRosterCity(t)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	seeded, err := beadmail.New(store).Send("qlandia/qcore/lyft", "human", "cutover", "leg is green")
	if err != nil {
		t.Fatalf("seed Send: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := cmdMailReply([]string{seeded.ID, "received"}, "", "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdMailReply = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "Nothing sent") {
		t.Errorf("stderr = %q: the reply warning must not claim nothing was sent", stderr.String())
	}
	if !strings.Contains(stderr.String(), "roster mismatch") || !strings.Contains(stderr.String(), "reply is written") {
		t.Errorf("stderr = %q, want a warning that names the mismatch and says the reply was written", stderr.String())
	}
}
