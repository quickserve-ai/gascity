package beads

import (
	"strings"
	"testing"
)

func envValues(env []string, key string) []string {
	var out []string
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			out = append(out, strings.TrimPrefix(e, prefix))
		}
	}
	return out
}

func TestExecEnvForBd_InjectsAutoBackupOptOut(t *testing.T) {
	// Every bd subprocess spawned through the runner must carry the
	// auto-backup opt-out (ga-yfbs28): bd's PersistentPostRun backup_export
	// sync has no retention and stuck-loops on broken remote state (the
	// 2026-06-08 town-wide wedge, ga-0eq). The projected-env opt-out only
	// covers gc env projections; this is the choke point for everything
	// else (hook claim, store bridge, t3bridge, libstore, provider
	// lifecycle).
	base := []string{"PATH=/usr/bin"}
	got := execEnvFor("bd", base, nil)
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "false" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want exactly [false]", vals)
	}
}

func TestExecEnvForBd_OverridesInheritedEnable(t *testing.T) {
	// A BD_BACKUP_ENABLED=true inherited from the parent process must not
	// leak through: gc policy forces the opt-out on gc-managed bd calls,
	// matching applyBdAutoBackupOptOut's unconditional projection.
	base := []string{"PATH=/usr/bin", "BD_BACKUP_ENABLED=true"}
	got := execEnvFor("bd", base, nil)
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "false" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want exactly [false] (inherited true must be replaced)", vals)
	}
}

func TestExecEnvForBd_ExplicitCallerOverrideWins(t *testing.T) {
	// An explicit per-call override is a deliberate caller decision (e.g. a
	// backup-focused test fixture) and must beat the injected baseline.
	base := []string{"PATH=/usr/bin"}
	got := execEnvFor("bd", base, map[string]string{"BD_BACKUP_ENABLED": "true"})
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "true" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want exactly [true] (explicit override wins)", vals)
	}
}

func TestExecEnvForBd_MergesOtherOverrides(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/u"}
	got := execEnvFor("bd", base, map[string]string{"BEADS_DIR": "/x/.beads"})
	if vals := envValues(got, "BEADS_DIR"); len(vals) != 1 || vals[0] != "/x/.beads" {
		t.Errorf("BEADS_DIR values = %v, want [/x/.beads]", vals)
	}
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "false" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want [false] alongside other overrides", vals)
	}
	if vals := envValues(got, "HOME"); len(vals) != 1 || vals[0] != "/home/u" {
		t.Errorf("HOME values = %v, want [/home/u] preserved", vals)
	}
}

// ga-bb7rzy REGRESSION. The field splice, at the exact call that produced it.
//
// A qcore-scoped `gc hook --claim` federates onto the CITY store. The city
// projection resolves a managed-local target: no host (loopback needs none),
// port 51361. The parent process is the witness session, whose environment
// carries the qcore rig endpoint 100.71.23.94:3307.
//
// execEnvFor overlays the projected map onto that parent environment, and an
// overlay replaces ONLY the keys the map contains. So the map must carry the
// host key EXPLICITLY EMPTY. When it does, the hub host is overwritten and bd
// falls back to 127.0.0.1 — the city's own server. When the key is merely
// absent (the pre-fix projection), the hub host survives beside the city's
// port and bd dials 100.71.23.94:51361, an endpoint no scope declares.
func TestExecEnvForBd_ProjectedEmptyHostOverwritesInheritedEndpoint(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"BEADS_DOLT_SERVER_HOST=100.71.23.94",
		"BEADS_DOLT_SERVER_PORT=3307",
		"GC_DOLT_HOST=100.71.23.94",
		"GC_DOLT_PORT=3307",
		"GC_DOLT_MANAGED_LOCAL=0",
	}
	// The managed-city projection as cmd/gc builds it after the fix.
	cityProjection := map[string]string{
		"BEADS_DOLT_SERVER_HOST": "",
		"BEADS_DOLT_SERVER_PORT": "51361",
		"GC_DOLT_HOST":           "",
		"GC_DOLT_PORT":           "51361",
		"GC_DOLT_MANAGED_LOCAL":  "",
	}

	got := execEnvFor("bd", parent, cityProjection)

	for _, tc := range []struct{ key, want string }{
		{"BEADS_DOLT_SERVER_HOST", ""},
		{"GC_DOLT_HOST", ""},
		{"GC_DOLT_MANAGED_LOCAL", ""},
		{"BEADS_DOLT_SERVER_PORT", "51361"},
		{"GC_DOLT_PORT", "51361"},
	} {
		vals := envValues(got, tc.key)
		if len(vals) != 1 || vals[0] != tc.want {
			t.Errorf("%s values = %v, want exactly [%q] — the inherited rig value must not survive the overlay", tc.key, vals, tc.want)
		}
	}
}

// TestExecEnvForBd_AbsentHostKeyIsResurrected pins the MECHANISM the fix above
// depends on, so nobody "tidies" a projection back to deleting the key: with the
// host key absent from the map, the overlay leaves the parent's host standing
// next to the projected port — reconstituting the exact field chimera. If this
// ever stops holding, execEnvFor has started stripping keys and the
// present-and-empty projections may be relaxed.
func TestExecEnvForBd_AbsentHostKeyIsResurrected(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "BEADS_DOLT_SERVER_HOST=100.71.23.94"}
	preFixProjection := map[string]string{"BEADS_DOLT_SERVER_PORT": "51361"}

	got := execEnvFor("bd", parent, preFixProjection)

	host := envValues(got, "BEADS_DOLT_SERVER_HOST")
	port := envValues(got, "BEADS_DOLT_SERVER_PORT")
	if len(host) != 1 || host[0] != "100.71.23.94" || len(port) != 1 || port[0] != "51361" {
		t.Fatalf("host = %v port = %v; expected the overlay to leave the inherited host beside the projected port (the ga-bb7rzy mechanism)", host, port)
	}
}

func TestExecEnvForNonBd_LeavesEnvAlone(t *testing.T) {
	// The runner also execs dolt directly; non-bd commands keep the
	// caller-visible environment untouched.
	base := []string{"PATH=/usr/bin", "BD_BACKUP_ENABLED=true"}
	got := execEnvFor("dolt", base, nil)
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "true" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want [true] untouched for non-bd commands", vals)
	}
}
