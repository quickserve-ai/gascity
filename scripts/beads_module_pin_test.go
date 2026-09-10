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
// "Beads pin" table, in deps.env, and on every machine, together.
//
// Two lines carry it. The require line stays equal to upstream gascity main's
// pin. The replace line is what the fleet actually builds: the beads fork's
// carry build of bd (github.com/quickserve-ai/beads at the fleet revision),
// which gc links, every machine runs, and CI's lockstep install compiles.
//
// Note this is a different axis from deps.env's BD_VERSION, which pins the bd
// *release tarball* the container image and the minimum-supported contract cell
// install, and can only name a published tag; TestBDVersionPins owns that one.
// The general CI path is no longer independent of this pin: since ga-yl326d the
// setup-gascity-* actions build bd from the revision below
// (.github/scripts/install-bd-lockstep.sh), so the CLI on PATH and the linked
// library are the same revision by construction.
func TestBeadsModulePin(t *testing.T) {
	// v1.1.1-0.20260805093327-bf97b73749ac is commit bf97b73749ac on
	// gastownhall/beads main (2026-08-05), schema v59 — the revision upstream
	// gastownhall/gascity main pins. The require line stays equal to upstream's
	// at every rebase (CARRY.md "Beads pin") so the module graph keeps
	// upstream's shape; what the fleet actually BUILDS is the replace below.
	const beadsFleetPin = "v1.1.1-0.20260805093327-bf97b73749ac"

	// The fleet runs the beads fork's carry build of bd, so go.mod replaces the
	// upstream module with that fork at the exact fleet revision: gc's linked
	// library, every machine's bd and CI's lockstep bd
	// (.github/scripts/install-bd-lockstep.sh) are all built from it, and gc's
	// version_compat preflight compares bd's main.Version label against
	// dep.Replace.Version byte-for-byte. deps.env's BD_CURRENT_VERSION /
	// BD_CURRENT_REF name the same revision for the contract matrix. Moving it
	// means redeploying bd across the fleet in the same window, so all of those
	// move together (CARRY.md "Beads pin").
	const (
		beadsFleetForkPath    = "github.com/quickserve-ai/beads"
		beadsFleetForkVersion = "v1.1.1-fleet.20260910"
	)

	gomod := readFile(t, repoRoot(t), "go.mod")

	// Match every version this go.mod associates with the upstream module path
	// — require and a versioned replace left-hand side alike — so a stale
	// requirement cannot hide beside a correct one, and a substring match
	// cannot be satisfied by a comment.
	re := regexp.MustCompile(`(?m)^\s*(?:replace\s+|require\s+)?github\.com/steveyegge/beads\s+(v\S+)`)
	matches := re.FindAllStringSubmatch(gomod, -1)
	if len(matches) == 0 {
		t.Fatal("go.mod names no version for github.com/steveyegge/beads")
	}
	for _, m := range matches {
		if m[1] != beadsFleetPin {
			t.Errorf("go.mod pins github.com/steveyegge/beads %s; upstream gascity main pins %s and the require line must stay equal to it. If the pin is meant to move, move beadsFleetPin, CARRY.md's beads pin table, and every machine's bd together.",
				m[1], beadsFleetPin)
		}
	}

	// The replace: exactly one, to the fork, at the fleet revision. Capture the
	// path and version after "=>" so a replace to a local directory (no
	// version), to another module, or to another fork revision all fail here —
	// and so does a missing replace, which the require check above would pass
	// while shipping a gc linked against upstream's beads, a library the
	// fleet's bd is not built from.
	replaceRe := regexp.MustCompile(`(?m)^\s*(?:replace\s+)?github\.com/steveyegge/beads(?:\s+v\S+)?\s*=>\s*(\S+)(?:\s+(v\S+))?\s*(?://.*)?$`)
	replaces := replaceRe.FindAllStringSubmatch(gomod, -1)
	if len(replaces) != 1 {
		t.Fatalf("go.mod has %d replace directives for github.com/steveyegge/beads, want exactly one: => %s %s (the fleet's bd is built from that revision)",
			len(replaces), beadsFleetForkPath, beadsFleetForkVersion)
	}
	if got, want := replaces[0][1]+" "+replaces[0][2], beadsFleetForkPath+" "+beadsFleetForkVersion; got != want {
		t.Errorf("go.mod replaces github.com/steveyegge/beads => %s; this town's bd is built from %s. If the fork build is meant to move, move beadsFleetForkVersion, deps.env BD_CURRENT_VERSION/BD_CURRENT_REF, CARRY.md's beads pin table, and every machine's bd together.",
			got, want)
	}
}
