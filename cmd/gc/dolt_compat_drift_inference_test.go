package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// ga-298g8t. validateCanonicalCompatDoltDrift is the FIRST statement of
// startBeadsLifecycle, which runs on `gc start` and on every controller config
// reload. So anything it refuses does not merely go red in doctor — it rejects
// reloads and can stop the city coming back, which is the ga-tmhxnd symptom
// class ("a supervisor restart may not come back").
//
// It used to refuse this combination:
//
//	rig  .beads/config.yaml  gc.endpoint_origin: inherited_city
//	city .beads/config.yaml  gc.endpoint_origin: managed_city
//	city.toml [[rigs]]       dolt_host / dolt_port present
//
// That made the two defenses protecting rig qcore's hub endpoint on 2026-08-27
// mutually exclusive: the operator's city.toml declaration, and the rig file
// being explicit rather than inherited_city. They never co-existed only because
// of the order the repair happened in (revert 22:50Z, restore 22:57Z, keys
// 23:07Z). Keys first would have refused to start.
//
// inherited_city in a rig file is DERIVED — it is what the absence of city.toml
// endpoint keys computes to. city.toml keys are an operator statement. Same
// class as ga-uurd84 one layer over: there an inference OVERWROTE a declaration,
// here it BLOCKED one.

func writeCompatDriftFixture(t *testing.T, rigOrigin string, rigHost, rigPort string) (cityPath string, cfg *config.City) {
	t.Helper()
	cityPath = t.TempDir()
	rigPath := filepath.Join(cityPath, "repo")
	for _, dir := range []string{filepath.Join(cityPath, ".gc"), filepath.Join(cityPath, ".beads"), filepath.Join(rigPath, ".beads")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"),
		[]byte("issue_prefix: dc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.mode: server\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city config.yaml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(city metadata.json): %v", err)
	}

	rigConfig := "issue_prefix: rp\ngc.endpoint_origin: " + rigOrigin + "\ngc.endpoint_status: verified\ndolt.mode: server\n"
	if rigOrigin == "explicit" {
		rigConfig += "dolt.host: 100.71.23.94\ndolt.port: 3307\n"
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(rigConfig), 0o644); err != nil {
		t.Fatalf("WriteFile(rig config.yaml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"rp"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(rig metadata.json): %v", err)
	}

	cfg = &config.City{
		Rigs: []config.Rig{{
			Name:     "repo",
			Path:     rigPath,
			Prefix:   "rp",
			DoltHost: rigHost,
			DoltPort: rigPort,
		}},
	}
	return cityPath, cfg
}

// TestCompatDriftDoesNotLetAnInferredRigOriginRefuseStart is the ga-298g8t pin.
// It calls startBeadsLifecycle, not just the predicate, because the claim being
// made is about STARTING THE CITY: the gate returns before any provider work, so
// no Dolt server is touched.
func TestCompatDriftDoesNotLetAnInferredRigOriginRefuseStart(t *testing.T) {
	cityPath, cfg := writeCompatDriftFixture(t, "inherited_city", "100.71.23.94", "3307")

	if err := validateCanonicalCompatDoltDrift(cityPath, cfg); err != nil {
		t.Fatalf("an operator's city.toml endpoint declaration must not be refused because the rig file carries the DERIVED inherited_city: %v", err)
	}

	// The whole point is the start path, so assert there too. Any error here
	// must not be the drift refusal; later stages legitimately fail in a
	// fixture with no Dolt server, and that is not what this test measures.
	err := startBeadsLifecycle(cityPath, "", cfg, io.Discard)
	if err != nil && strings.Contains(err.Error(), "Dolt drift") {
		t.Fatalf("startBeadsLifecycle still refuses on compat drift, so a reload and a city start would be rejected: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "deprecated rig dolt_host/dolt_port") {
		t.Fatalf("startBeadsLifecycle still refuses on the deprecated-keys conflict: %v", err)
	}
}

// TestCompatDriftStillRefusesTwoContradictingDeclarations is the non-vacuity
// half. Loosening the inference case must not loosen the case that matters: a
// rig file that DECLARES an endpoint disagreeing with city.toml is a genuine
// operator contradiction, and failing closed on it is correct. Without this, the
// fix above is satisfiable by deleting the check.
func TestCompatDriftStillRefusesTwoContradictingDeclarations(t *testing.T) {
	// Rig file declares 100.71.23.94:3307; city.toml declares a different host.
	cityPath, cfg := writeCompatDriftFixture(t, "explicit", "10.0.0.9", "3307")

	err := validateCanonicalCompatDoltDrift(cityPath, cfg)
	if err == nil {
		t.Fatal("two contradicting endpoint DECLARATIONS must still fail closed; got no error")
	}
	if !strings.Contains(err.Error(), "drift") {
		t.Fatalf("error should name the drift, got: %v", err)
	}

	// And an agreeing pair must be accepted, so the check is not simply
	// rejecting every explicit rig that has city.toml keys.
	cityPath, cfg = writeCompatDriftFixture(t, "explicit", "100.71.23.94", "3307")
	if err := validateCanonicalCompatDoltDrift(cityPath, cfg); err != nil {
		t.Fatalf("a rig file and city.toml that AGREE must be accepted: %v", err)
	}
}

// TestCompatDriftInferredRigStillRejectsAnEndpointItCannotHonor keeps the one
// genuinely broken shape rejected: city.toml declares an endpoint for a rig
// whose file inherits, AND the resolved city endpoint is external and different.
// Then following the inheritance really does land somewhere else, and the
// operator has written two things that cannot both be true.
func TestCompatDriftInferredRigUnderExternalCityStillChecksTheMirror(t *testing.T) {
	cityPath, cfg := writeCompatDriftFixture(t, "inherited_city", "10.0.0.9", "3399")
	// Make the city externally canonical and mirrored by the rig, so the
	// inherited rig has a real endpoint of its own to disagree with.
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"),
		[]byte("issue_prefix: dc\ngc.endpoint_origin: city_canonical\ngc.endpoint_status: verified\ndolt.mode: server\ndolt.host: 100.71.23.94\ndolt.port: 3307\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city config.yaml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "repo", ".beads", "config.yaml"),
		[]byte("issue_prefix: rp\ngc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\ndolt.mode: server\ndolt.host: 100.71.23.94\ndolt.port: 3307\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(rig config.yaml): %v", err)
	}
	if err := validateCanonicalCompatDoltDrift(cityPath, cfg); err == nil {
		t.Fatal("city.toml declaring 10.0.0.9:3399 for a rig that inherits an external 100.71.23.94:3307 endpoint is a real contradiction; want an error")
	}
	_ = fsys.OSFS{}
}

// TestCompatDriftDemotionStaysVisible is the non-vacuity check on the DEMOTION
// itself. Turning a hard error into a pass is trivially achievable by deleting
// the check, which would trade a startup landmine for a silent one — the exact
// failure mode this family of bugs is made of. So the disagreement must still be
// reported: as an advisory from compatDoltDriftAdvisories, and on the start
// path's stderr.
func TestCompatDriftDemotionStaysVisible(t *testing.T) {
	cityPath, cfg := writeCompatDriftFixture(t, "inherited_city", "100.71.23.94", "3307")

	advisories := compatDoltDriftAdvisories(cityPath, cfg)
	if len(advisories) != 1 {
		t.Fatalf("want exactly 1 advisory for the stale inherited_city rig, got %d: %v", len(advisories), advisories)
	}
	for _, want := range []string{`rig "repo"`, "100.71.23.94:3307", "inherited_city"} {
		if !strings.Contains(advisories[0], want) {
			t.Fatalf("advisory does not name %q, so an operator cannot act on it: %s", want, advisories[0])
		}
	}

	// The start path must SAY it, not just tolerate it.
	var stderr strings.Builder
	_ = startBeadsLifecycle(cityPath, "", cfg, &stderr)
	if !strings.Contains(stderr.String(), "canonical/compat Dolt drift") {
		t.Fatalf("startBeadsLifecycle tolerated the drift silently; stderr was:\n%s", stderr.String())
	}

	// And a city with no disagreement must produce NO advisory, or the signal is
	// noise and gets ignored.
	cityPath, cfg = writeCompatDriftFixture(t, "explicit", "100.71.23.94", "3307")
	if advisories := compatDoltDriftAdvisories(cityPath, cfg); len(advisories) != 0 {
		t.Fatalf("an agreeing city must produce no advisory, got: %v", advisories)
	}
}
