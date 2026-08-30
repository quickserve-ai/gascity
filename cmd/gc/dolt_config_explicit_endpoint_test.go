package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// FIELD INCIDENT, ga-uurd84 / ga-tmhxnd (2026-08-27 22:50:16Z).
//
// Rig qcore's .beads/config.yaml stated gc.endpoint_origin: explicit with the
// westeros hub endpoint (100.71.23.94:3307) after the hub flip. city.toml
// carried no endpoint keys for the rig. A controller reload ran rig init; the
// bd lifecycle script succeeded for the first time since the flip, so
// initBeadsForDirWithExecutor reached finalizeCanonicalBdScopeInit, and
// forcedScopeDoltConfigStateForInit — which reads city.toml only — inferred
// inherited_city. The write stripped dolt.host/dolt.port/dolt.user and stamped
// the result verified without probing anything.
//
// Reproduced end to end before the fix: the resulting file was byte-identical
// to the mayor's backup of the damaged file
// (.gc/agents/mayor/flip-recipe/backup/config.yaml.reverted-by-reload-20260827-155732).
//
// Consequence: every resolution without an ambient GC_DOLT_HOST/PORT read the
// frozen local archive with NO error — silent stale reads, not visible
// breakage.
//
// The tests below pin the two defects separately.

// TestForcedScopeInitKeepsOperatorSetExplicitRigEndpoint is the reproduction of
// the damaging write, at the exact pair of calls finalizeCanonicalBdScopeInit
// makes: derive the forced state, then write it.
func TestForcedScopeInitKeepsOperatorSetExplicitRigEndpoint(t *testing.T) {
	cityPath, rigPath := writeExplicitHubRigFixture(t)

	// The supervisor exec env that drove the field reload: a managed-local port
	// with no host beside it.
	t.Setenv("GC_DOLT_PORT", "51361")
	t.Setenv("GC_DOLT_HOST", "")

	state, ok, err := forcedScopeDoltConfigStateForInit(cityPath, rigPath, "qc")
	if err != nil {
		t.Fatalf("forcedScopeDoltConfigStateForInit: %v", err)
	}
	if !ok {
		t.Fatalf("forcedScopeDoltConfigStateForInit returned no state for %s", rigPath)
	}
	if state.EndpointOrigin != contract.EndpointOriginExplicit {
		t.Fatalf("forced init state downgraded an operator-set explicit endpoint to %q (host %q port %q)", state.EndpointOrigin, state.DoltHost, state.DoltPort)
	}
	if err := ensureCanonicalScopeConfigState(fsys.OSFS{}, rigPath, state); err != nil {
		t.Fatalf("ensureCanonicalScopeConfigState: %v", err)
	}
	assertRigKeptExplicitHubEndpoint(t, rigPath)
}

// TestForcedScopeInitStillHonorsCityTomlEndpoint guards the other direction:
// preserving a declared scope endpoint must not make city.toml inert. An
// operator moving a rig's endpoint in city.toml still wins, because that forced
// state DECLARES an endpoint rather than inferring one.
func TestForcedScopeInitStillHonorsCityTomlEndpoint(t *testing.T) {
	cityPath, rigPath := writeExplicitHubRigFixture(t)
	appendRigEndpointToCityToml(t, cityPath, "10.9.9.9", "3399")

	state, ok, err := forcedScopeDoltConfigStateForInit(cityPath, rigPath, "qc")
	if err != nil {
		t.Fatalf("forcedScopeDoltConfigStateForInit: %v", err)
	}
	if !ok {
		t.Fatalf("forcedScopeDoltConfigStateForInit returned no state for %s", rigPath)
	}
	if state.EndpointOrigin != contract.EndpointOriginExplicit {
		t.Fatalf("origin = %q, want explicit", state.EndpointOrigin)
	}
	if state.DoltHost != "10.9.9.9" || state.DoltPort != "3399" {
		t.Fatalf("city.toml endpoint did not win: got %s:%s, want 10.9.9.9:3399", state.DoltHost, state.DoltPort)
	}
}

// TestInheritedCityStatusIsNotAssertedWithoutAProbe pins the second defect: an
// origin inferred from a bare port claimed "verified", the word the
// endpoint-setting commands write only after verifyExternalDoltEndpoint
// connected. With no authoritative city state there is nothing to inherit and
// nothing measured, so the honest value is unverified.
func TestInheritedCityStatusIsNotAssertedWithoutAProbe(t *testing.T) {
	root := t.TempDir()
	cityPath := filepath.Join(root, "gascity")
	rigPath := filepath.Join(root, "rig")
	for _, dir := range []string{filepath.Join(cityPath, ".gc"), filepath.Join(rigPath, ".beads")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	// No city .beads/config.yaml at all: the city state is not authoritative.
	// The rig carries a bare port with no host and no stated origin.
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("issue_prefix: qc\ndolt.port: 51361\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(rig config.yaml): %v", err)
	}

	state, ok, err := contract.ResolveAuthoritativeConfigState(fsys.OSFS{}, cityPath, rigPath, "qc")
	if err != nil {
		t.Fatalf("ResolveAuthoritativeConfigState: %v", err)
	}
	if !ok {
		t.Fatalf("ResolveAuthoritativeConfigState returned no state")
	}
	if state.EndpointOrigin != contract.EndpointOriginInheritedCity {
		t.Fatalf("origin = %q, want inherited_city", state.EndpointOrigin)
	}
	if state.EndpointStatus == contract.EndpointStatusVerified {
		t.Fatalf("endpoint_status = verified with no authoritative city state and no probe; want unverified")
	}
	if state.EndpointStatus != contract.EndpointStatusUnverified {
		t.Fatalf("endpoint_status = %q, want unverified", state.EndpointStatus)
	}
}

// TestNormalizeScopeKeepsOperatorSetExplicitRigEndpoint covers the other
// normalization entry point — the gc dolt-config normalize-scope verb the bd
// lifecycle script calls. This path already preferred the scope file (it goes
// through resolveDesiredRigEndpointState, not the forced one); the test keeps it
// that way, and records that the verb was NOT the field emitter.
func TestNormalizeScopeKeepsOperatorSetExplicitRigEndpoint(t *testing.T) {
	cityPath, rigPath := writeExplicitHubRigFixture(t)
	t.Setenv("GC_DOLT_PORT", "51361")
	t.Setenv("GC_DOLT_HOST", "")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"dolt-config", "normalize-scope",
		"--city", cityPath,
		"--dir", rigPath,
		"--prefix", "qc",
		"--dolt-database", "qc",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	assertRigKeptExplicitHubEndpoint(t, rigPath)
}

// writeExplicitHubRigFixture builds a faithful copy of the field scope state at
// 2026-08-27 22:50Z: a managed-local city, and a rig outside the city tree
// whose .beads/config.yaml states the operator's explicit hub endpoint.
//
// city.toml deliberately carries NO dolt_host/dolt_port for the rig — that was
// the field state at 22:50Z; the keys were added at 23:07Z as a durable
// operator declaration. The scope file alone must be enough.
func writeExplicitHubRigFixture(t *testing.T) (cityPath, rigPath string) {
	t.Helper()
	// The field rig lives outside the city tree, like /Users/cherub/gt/qcore.
	root := t.TempDir()
	cityPath = filepath.Join(root, "gascity")
	rigPath = filepath.Join(root, "gt", "qcore", "mayor", "rig")
	for _, dir := range []string{
		filepath.Join(cityPath, ".gc"),
		filepath.Join(cityPath, ".beads"),
		filepath.Join(rigPath, ".beads"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}

	cityToml := "[workspace]\nname = \"gascity\"\nprefix = \"ga\"\n\n[beads]\nprovider = \"bd\"\n\n[[rigs]]\nname = \"qcore\"\npath = \"" + rigPath + "\"\nprefix = \"qc\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	// City scope: managed-local Dolt, as the live city is.
	cityConfig := "issue_prefix: ga\nissue-prefix: ga\ndolt.auto-start: false\nexport.auto: false\nbackup.enabled: false\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.mode: server\n"
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cityConfig), 0o644); err != nil {
		t.Fatalf("WriteFile(city config.yaml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(city metadata.json): %v", err)
	}

	// Rig scope: the operator's explicit hub endpoint, faithful to the file the
	// flip wrote and the mayor restored.
	rigConfig := "issue_prefix: qc\nissue-prefix: qc\ndolt.auto-start: false\nexport.auto: false\ngc.endpoint_origin: explicit\ngc.endpoint_status: verified\ntypes.custom: molecule,convoy,message,event,gate,merge-request,agent,role,rig,session,spec,convergence,step\nbackup.enabled: false\ndolt:\n  disable-event-flush: true\ndolt.mode: server\ndolt.host: 100.71.23.94\ndolt.port: 3307\ndolt.user: cherub\n"
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(rigConfig), 0o644); err != nil {
		t.Fatalf("WriteFile(rig config.yaml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"qc"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(rig metadata.json): %v", err)
	}
	return cityPath, rigPath
}

func appendRigEndpointToCityToml(t *testing.T, cityPath, host, port string) {
	t.Helper()
	path := filepath.Join(cityPath, "city.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(city.toml): %v", err)
	}
	updated := strings.Replace(string(data), "prefix = \"qc\"\n", "prefix = \"qc\"\ndolt_host = \""+host+"\"\ndolt_port = \""+port+"\"\n", 1)
	if updated == string(data) {
		t.Fatalf("could not add endpoint keys to the qcore rig block:\n%s", data)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

func assertRigKeptExplicitHubEndpoint(t *testing.T, rigPath string) {
	t.Helper()
	cfgData, err := os.ReadFile(filepath.Join(rigPath, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(rig config.yaml): %v", err)
	}
	cfgText := string(cfgData)
	for _, want := range []string{
		"gc.endpoint_origin: explicit",
		"dolt.host: 100.71.23.94",
		"dolt.port: 3307",
		"dolt.user: cherub",
	} {
		if !strings.Contains(cfgText, want) {
			t.Fatalf("normalization dropped %q from an operator-set explicit endpoint:\n%s", want, cfgText)
		}
	}
	if strings.Contains(cfgText, "inherited_city") {
		t.Fatalf("normalization downgraded an explicit endpoint to inherited_city:\n%s", cfgText)
	}
	// The managed-local port mirror belongs only to scopes that inherit the
	// city endpoint. A scope with its own endpoint must never get one.
	if _, err := os.Stat(filepath.Join(rigPath, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		port, _ := os.ReadFile(filepath.Join(rigPath, ".beads", "dolt-server.port"))
		t.Fatalf("normalization wrote a managed port mirror (%q) into an explicit-endpoint scope, stat err = %v", strings.TrimSpace(string(port)), err)
	}
}
