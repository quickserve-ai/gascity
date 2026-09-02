package scripts_test

import (
	"regexp"
	"testing"
)

// TestBeadsModulePin anchors go.mod's beads requirement, the way
// TestDoltVersionPins anchors Dolt's. The native store IS the beads library
// linked into gc, so this one line decides the highest Dolt schema version a gc
// binary can open. Our city databases are migrated by bd built from this same
// revision; a gc pinned below it trips beads' schema-skew gate and falls back
// to the exec store rather than failing, so the town runs degraded with nothing
// red anywhere. A gc pinned above it is the same hazard mirrored — it migrates
// a city DB past what every other machine's bd knows.
//
// Neither direction is a go.mod edit. Moving this pin means redeploying bd
// across the fleet in the same window, so the pin moves here, in CARRY.md's
// "Beads pin" table, and on every machine, together.
//
// Note this is a different axis from deps.env's BD_VERSION, which pins the bd
// *release tarball* CI and Docker install and can only name a published tag.
// The two are deliberately independent; TestBDVersionPins owns that one.
func TestBeadsModulePin(t *testing.T) {
	// v1.1.1-0.20260805093327-bf97b73749ac is commit bf97b73749ac on
	// gastownhall/beads main (2026-08-05), schema v59 — the revision upstream
	// gastownhall/gascity main pins, and therefore the one every bd in the
	// fleet is built from since the 2026-09-01 window (the fleet pins what
	// upstream gascity main pins at window time, never ahead of it). Carry
	// builds of bd keep this exact label in main.Version — gc's version_compat
	// gate requires it — and carry their identity in main.Build/Commit/Branch.
	const beadsFleetPin = "v1.1.1-0.20260805093327-bf97b73749ac"

	gomod := readFile(t, repoRoot(t), "go.mod")

	// Match every version this go.mod associates with the beads module —
	// require and replace alike — so a stale requirement cannot hide beside a
	// correct one, and a substring match cannot be satisfied by a comment.
	re := regexp.MustCompile(`(?m)^\s*(?:replace\s+|require\s+)?github\.com/steveyegge/beads\s+(v\S+)`)
	matches := re.FindAllStringSubmatch(gomod, -1)
	if len(matches) == 0 {
		t.Fatal("go.mod names no version for github.com/steveyegge/beads")
	}
	for _, m := range matches {
		if m[1] != beadsFleetPin {
			t.Errorf("go.mod pins github.com/steveyegge/beads %s; this town's bd is built from %s. If the pin is meant to move, move beadsFleetPin, CARRY.md's beads pin table, and every machine's bd together.",
				m[1], beadsFleetPin)
		}
	}
}
