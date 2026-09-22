package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

const (
	// beadsRequirePath is the module path go.mod requires for beads. It is
	// upstream's, and stays upstream's: a fleet build is consumed through a
	// replace, so the fork keeps declaring this same module path and gc's
	// imports never mention the fork.
	beadsRequirePath = "github.com/steveyegge/beads"
	// beadsFleetPath is the fork the fleet cuts its beads builds from.
	beadsFleetPath = "github.com/quickserve-ai/beads"
	// beadsFleetTag is the fleet build gc is pinned to (gc-1c2b).
	beadsFleetTag = "v1.3.0-rc.2-fleet.20260922.1"
	// beadsFleetTagMarker separates the upstream release a fleet tag was cut
	// from (vX.Y.Z[-rc.N]) from its fleet date and serial.
	beadsFleetTagMarker = "-fleet."
)

// TestGoModPinsTheBeadsFleetBuild is the checked shape of the beads pin
// (gc-1c2b). The fleet runs a quickserve-ai/beads build — upstream's release
// plus the carries the fleet needs — and gc consumes it through a single
// go.mod replace rather than by requiring the fork, so every import path,
// every error string and every other pin in this repository keeps naming the
// upstream module.
//
// Three things have to hold together, and each has failed somewhere before:
// the replace is the only one in the file (a second one is a different
// argument that has to be made on its own terms), it points at the fork at
// the tag the beads owner actually published, and the upstream release that
// tag was cut from is the one the require line names. The third is the
// invariant that makes the replace a redirect instead of a silent version
// bump — the module graph, deps.env's BD_CURRENT_VERSION and the bd
// compatibility floors are all still reasoning about the require line.
func TestGoModPinsTheBeadsFleetBuild(t *testing.T) {
	path := filepath.Join(repoRoot(t), "go.mod")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	parsed, err := modfile.Parse(path, data, nil)
	if err != nil {
		t.Fatalf("parse go.mod: %v", err)
	}

	if len(parsed.Replace) != 1 {
		got := make([]string, 0, len(parsed.Replace))
		for _, replace := range parsed.Replace {
			got = append(got, replace.Old.Path+" => "+replace.New.Path+" "+replace.New.Version)
		}
		t.Fatalf("go.mod has %d replace directives (%v); the beads fleet pin is meant to be the only one", len(parsed.Replace), got)
	}

	replace := parsed.Replace[0]
	if replace.Old.Path != beadsRequirePath {
		t.Errorf("replace redirects %q, want %q", replace.Old.Path, beadsRequirePath)
	}
	if replace.Old.Version != "" {
		t.Errorf("replace names left-hand version %q; the pin redirects every version of %s, not one", replace.Old.Version, beadsRequirePath)
	}
	if replace.New.Path != beadsFleetPath {
		t.Errorf("replace targets %q, want %q", replace.New.Path, beadsFleetPath)
	}
	if replace.New.Version != beadsFleetTag {
		t.Errorf("replace targets version %q, want the pinned fleet tag %q", replace.New.Version, beadsFleetTag)
	}

	required := ""
	for _, require := range parsed.Require {
		if require.Mod.Path == beadsRequirePath {
			required = require.Mod.Version
			break
		}
	}
	if required == "" {
		t.Fatalf("go.mod has no require line for %s", beadsRequirePath)
	}

	base, _, found := strings.Cut(beadsFleetTag, beadsFleetTagMarker)
	if !found {
		t.Fatalf("fleet tag %q does not carry a %q marker, so the release it was cut from cannot be read", beadsFleetTag, beadsFleetTagMarker)
	}
	if base != required {
		t.Errorf("fleet tag %q was cut from %s, but go.mod requires %s; the replace must redirect the required release, not substitute another one", beadsFleetTag, base, required)
	}
}
