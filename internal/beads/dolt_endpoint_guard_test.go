package beads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeManagedCityFixture builds a city whose managed-local Dolt is published as
// running on port, the way .gc/runtime/packs/dolt/dolt-state.json records it.
func writeManagedCityFixture(t *testing.T, port string) string {
	t.Helper()
	cityPath := t.TempDir()
	stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", stateDir, err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.beads): %v", err)
	}
	state := `{"running":true,"pid":67792,"port":` + port +
		`,"data_dir":"` + filepath.Join(cityPath, ".beads", "dolt") + `"}`
	if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), []byte(state), 0o644); err != nil {
		t.Fatalf("WriteFile(dolt-state.json): %v", err)
	}
	// The city scope is managed_city: it declares no host at all.
	cityCfg := "issue_prefix: ga\ndolt.mode: server\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\n"
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(cityCfg), 0o644); err != nil {
		t.Fatalf("WriteFile(city config.yaml): %v", err)
	}
	return cityPath
}

// TestGuardRefusesFieldChimera is the ga-bb7rzy incident endpoint, verbatim:
// the westeros hub host carried in from a rig-scoped session, paired with the
// city's own managed-local port, addressing the CITY store — which declares no
// host at all.
func TestGuardRefusesFieldChimera(t *testing.T) {
	cityPath := writeManagedCityFixture(t, "51361")
	environ := []string{
		"GC_CITY_PATH=" + cityPath,
		"BEADS_DIR=" + filepath.Join(cityPath, ".beads"),
		"GC_STORE_ROOT=" + cityPath,
		"BEADS_DOLT_SERVER_HOST=100.71.23.94",
		"BEADS_DOLT_SERVER_PORT=51361",
	}

	err := guardSynthesizedDoltEndpoint(environ, cityPath)
	if err == nil {
		t.Fatal("guard allowed 100.71.23.94:51361 — a hub host on the city's managed-local port that no scope declares")
	}
	msg := err.Error()
	for _, want := range []string{"100.71.23.94", "51361", "ga-bb7rzy", "BEADS_DOLT_SERVER_HOST", "dolt-state.json"} {
		if !strings.Contains(msg, want) {
			t.Errorf("guard error does not name %q, so an operator cannot see which half came from where:\n%s", want, msg)
		}
	}
}

// TestGuardAllowsDeclaredRigEndpointOnCollidingPort is the false-positive
// defence, and the reason the predicate is not just "non-loopback host + managed
// port". A rig that genuinely points at a remote host DECLARES it; the guard
// must allow that even when the remote port number happens to equal the port the
// local managed server was allocated.
func TestGuardAllowsDeclaredRigEndpointOnCollidingPort(t *testing.T) {
	cityPath := writeManagedCityFixture(t, "3307")
	rigPath := filepath.Join(t.TempDir(), "rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(rig .beads): %v", err)
	}
	rigCfg := "issue_prefix: qc\ndolt.mode: server\ngc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.host: 100.71.23.94\ndolt.port: 3307\n"
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(rigCfg), 0o644); err != nil {
		t.Fatalf("WriteFile(rig config.yaml): %v", err)
	}

	environ := []string{
		"GC_CITY_PATH=" + cityPath,
		"BEADS_DIR=" + filepath.Join(rigPath, ".beads"),
		"GC_STORE_ROOT=" + rigPath,
		"BEADS_DOLT_SERVER_HOST=100.71.23.94",
		"BEADS_DOLT_SERVER_PORT=3307",
	}

	if err := guardSynthesizedDoltEndpoint(environ, rigPath); err != nil {
		t.Fatalf("guard refused a rig endpoint its own config.yaml declares: %v", err)
	}
}

// TestGuardFailsOpen covers every uncertainty: the guard may only reject a pair
// it can prove impossible.
func TestGuardFailsOpen(t *testing.T) {
	cityPath := writeManagedCityFixture(t, "51361")
	base := func(extra ...string) []string {
		return append([]string{"GC_CITY_PATH=" + cityPath, "GC_STORE_ROOT=" + cityPath}, extra...)
	}
	for _, tc := range []struct {
		name    string
		environ []string
	}{
		{"loopback host on the managed port", base("BEADS_DOLT_SERVER_HOST=127.0.0.1", "BEADS_DOLT_SERVER_PORT=51361")},
		{"no host at all", base("BEADS_DOLT_SERVER_PORT=51361")},
		{"remote host on its own port", base("BEADS_DOLT_SERVER_HOST=100.71.23.94", "BEADS_DOLT_SERVER_PORT=3307")},
		{"no port", base("BEADS_DOLT_SERVER_HOST=100.71.23.94")},
		{"no city in the environment", []string{"BEADS_DOLT_SERVER_HOST=100.71.23.94", "BEADS_DOLT_SERVER_PORT=51361"}},
		{"empty environment", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := guardSynthesizedDoltEndpoint(tc.environ, cityPath); err != nil {
				t.Fatalf("guard refused a pair it cannot prove impossible: %v", err)
			}
		})
	}
}

// TestGuardIgnoresAnotherCitysManagedState keeps the guard from borrowing a
// port number out of a state file that does not describe this city's server.
func TestGuardIgnoresAnotherCitysManagedState(t *testing.T) {
	cityPath := writeManagedCityFixture(t, "51361")
	stateFile := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	foreign := `{"running":true,"pid":1,"port":51361,"data_dir":"/somewhere/else/.beads/dolt"}`
	if err := os.WriteFile(stateFile, []byte(foreign), 0o644); err != nil {
		t.Fatalf("WriteFile(dolt-state.json): %v", err)
	}

	environ := []string{
		"GC_CITY_PATH=" + cityPath,
		"GC_STORE_ROOT=" + cityPath,
		"BEADS_DOLT_SERVER_HOST=100.71.23.94",
		"BEADS_DOLT_SERVER_PORT=51361",
	}
	if err := guardSynthesizedDoltEndpoint(environ, cityPath); err != nil {
		t.Fatalf("guard used a state file describing another city's server: %v", err)
	}
}

func TestGuardOptOut(t *testing.T) {
	t.Setenv("GC_DOLT_ENDPOINT_GUARD", "0")
	if guardSynthesizedDoltEndpointEnabled() {
		t.Fatal("GC_DOLT_ENDPOINT_GUARD=0 did not disable the guard")
	}
	t.Setenv("GC_DOLT_ENDPOINT_GUARD", "")
	if !guardSynthesizedDoltEndpointEnabled() {
		t.Fatal("guard must be on by default — the failure it prevents is silent")
	}
}
