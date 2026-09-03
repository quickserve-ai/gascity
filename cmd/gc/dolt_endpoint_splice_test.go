package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// FIELD INCIDENT, ga-bb7rzy (2026-09-02 22:28Z).
//
// qcore/gastown.witness ran `gc hook --claim --json`. Being rig-scoped, its hook
// store set includes the CITY store as a federated tertiary
// (cmd_hook.go appendCityHookStore). The city projection resolved a MANAGED-LOCAL
// target — loopback needs no host — and the pre-fix code expressed that by
// DELETING the host keys while setting the port to the city's managed 51361.
//
// The claim mutation then rebuilt the child environment by overlaying that map
// onto the parent process environment (hookClaimEnvMap -> beads.NewBdStore ->
// ExecCommandRunnerWithEnvContext -> execEnvFor -> mergeEnv). An overlay replaces
// only the keys the map CONTAINS, so the deleted host was refilled from the
// witness session's own rig projection — the westeros hub, 100.71.23.94 — and bd
// dialed 100.71.23.94:51361: the hub host on the city's managed-local port. The
// endpoint existed in no config file and in no session environment; it was
// manufactured inside the process, after the merge, which is why the live env
// projection inspected during the incident read clean.
//
// The fix is that a scope with no host of its own records the key PRESENT AND
// EMPTY, so the overlay overwrites the inherited value instead of leaving it.
// This test pins the projection side at the three sites that were changed; the
// overlay side is pinned by
// TestExecEnvForBd_ProjectedEmptyHostOverwritesInheritedEndpoint in
// internal/beads.

// ambientRigProjection is the witness session's environment: projected for the
// qcore rig store on the westeros hub. A city-scoped projection built in this
// process inherits it, and every key the city projection does not overwrite
// survives into the child.
func ambientRigProjection() map[string]string {
	return map[string]string{
		"GC_DOLT_HOST":           "100.71.23.94",
		"GC_DOLT_PORT":           "3307",
		"BEADS_DOLT_SERVER_HOST": "100.71.23.94",
		"BEADS_DOLT_SERVER_PORT": "3307",
		"GC_DOLT_MANAGED_LOCAL":  "0",
	}
}

func TestManagedCityProjectionEmptiesHostAgainstAmbientRigEndpoint(t *testing.T) {
	env := ambientRigProjection()

	// The city's own target: managed-local Dolt on the ephemeral 51361. Loopback
	// needs no host, and the scope is not another store.
	managed := contract.DoltConnectionTarget{Host: "127.0.0.1", Port: "51361"}
	applyCanonicalDoltTargetEnv(env, managed)
	mirrorBeadsDoltServerEnv(env, false)

	for _, tc := range []struct{ key, want string }{
		// The three sites the fix changed: a scope with no host of its own, and
		// a scope that is not another store, must SAY SO rather than stay silent.
		{"GC_DOLT_HOST", ""},
		{"BEADS_DOLT_SERVER_HOST", ""},
		{"GC_DOLT_MANAGED_LOCAL", ""},
		// The port half was always written, which is exactly why the deleted
		// host half spliced onto it.
		{"GC_DOLT_PORT", "51361"},
		{"BEADS_DOLT_SERVER_PORT", "51361"},
	} {
		got, ok := env[tc.key]
		if !ok {
			t.Errorf("%s absent from the managed-city projection: an overlay onto the parent environ would resurrect the ambient rig value (ga-bb7rzy)", tc.key)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q — the ambient hub endpoint must not survive a city-scoped projection", tc.key, got, tc.want)
		}
	}
}

// TestManagedCityProjectionSurvivesGcOwnMerge carries the same projection through
// gc's own env merge, so the child receives an explicit empty entry rather than a
// silently dropped key. mergeRuntimeEnv strips these keys from the base before
// applying overrides, so it was never the leaking merge — but the projection must
// still emit the empty entry here for the bd-side overlay to have something to
// overwrite with.
func TestManagedCityProjectionSurvivesGcOwnMerge(t *testing.T) {
	env := ambientRigProjection()
	applyCanonicalDoltTargetEnv(env, contract.DoltConnectionTarget{Host: "127.0.0.1", Port: "51361"})
	mirrorBeadsDoltServerEnv(env, false)

	parent := []string{
		"PATH=/usr/bin",
		"BEADS_DOLT_SERVER_HOST=100.71.23.94",
		"BEADS_DOLT_SERVER_PORT=3307",
		"GC_DOLT_HOST=100.71.23.94",
	}
	merged := mergeRuntimeEnv(parent, env)

	for _, key := range []string{"GC_DOLT_HOST", "BEADS_DOLT_SERVER_HOST"} {
		var vals []string
		prefix := key + "="
		for _, entry := range merged {
			if strings.HasPrefix(entry, prefix) {
				vals = append(vals, strings.TrimPrefix(entry, prefix))
			}
		}
		if len(vals) != 1 || vals[0] != "" {
			t.Errorf("merged %s values = %v, want exactly [\"\"] so no child can read the hub host beside the city's port", key, vals)
		}
	}
}

// TestExternalRigProjectionStillCarriesItsEndpoint is the other direction: the
// fix must not blunt a real external endpoint. A rig pointing at the hub still
// projects the hub host, port and the "another store" record.
func TestExternalRigProjectionStillCarriesItsEndpoint(t *testing.T) {
	env := map[string]string{}
	external := contract.DoltConnectionTarget{Host: "100.71.23.94", Port: "3307", External: true}
	applyCanonicalDoltTargetEnv(env, external)
	mirrorBeadsDoltServerEnv(env, false)

	for _, tc := range []struct{ key, want string }{
		{"GC_DOLT_HOST", "100.71.23.94"},
		{"BEADS_DOLT_SERVER_HOST", "100.71.23.94"},
		{"GC_DOLT_PORT", "3307"},
		{"BEADS_DOLT_SERVER_PORT", "3307"},
		{"GC_DOLT_MANAGED_LOCAL", "0"},
	} {
		if got := env[tc.key]; got != tc.want {
			t.Errorf("%s = %q, want %q for a declared external endpoint", tc.key, got, tc.want)
		}
	}
}
