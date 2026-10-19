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
	beadsFleetTag = "v1.3.1-fleet.20261005.1"
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

// TestBazelFleetBDFollowsTheBeadsFleetTag keeps Bazel's fleet bd on the tag
// go.mod's replace names (ga-l0b72a.1). The targets that test gc against the
// bd the fleet runs (cmd/gc's integration build, :acceptance_test) take
// MODULE.bazel's fleet http_file; a pin advance that moves the replace and
// leaves this file behind would test gc's library against the previous
// fleet bd, the same split that kept
// TestBuildPinnedBDBinaryForTestsUsesGoModSource and the BeadsProxied
// acceptance rows red while Bazel handed them the upstream release.
func TestBazelFleetBDFollowsTheBeadsFleetTag(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "MODULE.bazel"))
	if err != nil {
		t.Fatalf("read MODULE.bazel: %v", err)
	}
	module := string(data)
	repo := "bd_bin_" + strings.NewReplacer(".", "_", "-", "_").Replace(beadsFleetTag)
	start := strings.Index(module, "http_file(\n    name = \""+repo+"\",\n")
	if start < 0 {
		t.Fatalf("MODULE.bazel has no http_file %s for the fleet bd %s", repo, beadsFleetTag)
	}
	end := strings.Index(module[start:], "\n)\n")
	if end < 0 {
		t.Fatalf("MODULE.bazel http_file %s is not closed", repo)
	}
	block := module[start : start+end]
	url := "https://github.com/quickserve-ai/beads/releases/download/" + beadsFleetTag + "/bd-" + beadsFleetTag + "-linux-amd64"
	for _, want := range []string{`"` + url + `"`, `downloaded_file_path = "bd",`, `executable = True,`} {
		if !strings.Contains(block, want) {
			t.Errorf("http_file %s lacks %s:\n%s", repo, want, block)
		}
	}
	label := "@" + repo + "//file"
	for _, rel := range []string{"cmd/gc/BUILD.bazel", "test/acceptance/BUILD.bazel"} {
		build, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(build), "$(rootpath "+label+")") {
			t.Errorf("%s does not hand its tests %s", rel, label)
		}
	}
}

// TestMacAcceptanceFleetBDFollowsTheBeadsFleetTag is the Mac lane's twin of the
// Bazel test above (ga-tkfdsb). Tier A acceptance on the Mac runs plain go test,
// and setup-gascity-macos installs the upstream BD_VERSION release, which kept
// TestBeadsProxiedDefaultNativeLane/status-zero-forks red on the Mac nightly
// after ga-l0b72a.2 fixed only Bazel. The acceptance step takes the fleet bd
// named by FLEET_BD_TAG through GC_ACCEPTANCE_BD_BIN; this keeps that tag on
// the replace's and the step wired to it.
func TestMacAcceptanceFleetBDFollowsTheBeadsFleetTag(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "mac-regression.yml"))
	if err != nil {
		t.Fatalf("read mac-regression.yml: %v", err)
	}
	workflow := string(data)
	for _, want := range []string{
		`FLEET_BD_TAG: "` + beadsFleetTag + `"`,
		`GC_ACCEPTANCE_BD_BIN: ${{ steps.fleet-bd.outputs.path }}`,
		`https://github.com/quickserve-ai/beads/releases/download/${TAG}`,
		`shasum -a 256 -c -`,
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("mac-regression.yml lacks %s: the Mac acceptance step must test gc against the fleet bd %s", want, beadsFleetTag)
		}
	}
}

// TestNightlyPerfLaneFleetBDFollowsTheBeadsFleetTag is the Linux nightly's twin
// of the Mac test above (ga-tkfdsb). The beads-proxied-perf lane built bd from
// upstream's BD_CURRENT_REF, which kept
// TestBeadsProxiedDefaultNativeLane/status-zero-forks red every night (4 forks,
// gc-xol7); it now installs the fleet release named by FLEET_BD_TAG, checked
// against its SHA256SUMS. This keeps that tag on the replace's.
func TestNightlyPerfLaneFleetBDFollowsTheBeadsFleetTag(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "nightly.yml"))
	if err != nil {
		t.Fatalf("read nightly.yml: %v", err)
	}
	workflow := string(data)
	for _, want := range []string{
		`FLEET_BD_TAG: "` + beadsFleetTag + `"`,
		`https://github.com/quickserve-ai/beads/releases/download/${TAG}`,
		`sha256sum -c -`,
		`echo "GC_ACCEPTANCE_BD_BIN=$dir/bd" >> "$GITHUB_ENV"`,
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("nightly.yml lacks %s: the proxied-native perf lane must test gc against the fleet bd %s", want, beadsFleetTag)
		}
	}
}
