package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

// TestCacheHeartbeatWatchedScopesExcludesSuspendedRigs is the false-alarm
// guard. The controller does not arm a reconciler for a suspended rig
// (rigStoreBackgroundRefresh), so that rig's leftover heartbeat record goes
// stale by design. Watching it would produce a standing failure on every
// `gc doctor` run — which is how a watchdog gets ignored.
func TestCacheHeartbeatWatchedScopesExcludesSuspendedRigs(t *testing.T) {
	cityDir := t.TempDir()
	runtimeDir := citylayout.RuntimePath(cityDir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	state := `{"rigs":{"astro":{"suspended":true},"qcore":{"suspended":false}}}`
	if err := os.WriteFile(filepath.Join(runtimeDir, "suspension-state.json"), []byte(state), 0o644); err != nil {
		t.Fatalf("write suspension state: %v", err)
	}

	cfg := &config.City{Rigs: []config.Rig{
		{Name: "qcore", Path: filepath.Join(cityDir, "qcore")},
		{Name: "astro", Path: filepath.Join(cityDir, "astro")},
		{Name: "unbound"}, // declared but never bound to a path — no store, no reconciler
	}}

	got := cacheHeartbeatWatchedScopes(cityDir, cfg)
	want := []string{cacheHeartbeatCityScope, "qcore"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("watched scopes = %v, want %v", got, want)
	}
}

// TestCacheHeartbeatScopeForRigRefusesTheCityLabel is the aliasing guard.
// "city" is NOT a reserved rig name — config.ValidateRigs reserves only the
// [[orders.overrides]] wildcard — so a rig named "city" would publish its
// reconcile heartbeat into the CITY store's record: one file, two live
// reconcilers, last writer wins. A healthy rig would then keep that record
// fresh while the city cache sat stalled, masking the exact ga-yc0chj shape
// behind a green tick. Such a rig publishes nothing instead.
func TestCacheHeartbeatScopeForRigRefusesTheCityLabel(t *testing.T) {
	if got := cacheHeartbeatScopeForRig(cacheHeartbeatCityScope); got != "" {
		t.Errorf("cacheHeartbeatScopeForRig(%q) = %q, want \"\" — a rig must never alias the city record",
			cacheHeartbeatCityScope, got)
	}
	if got := cacheHeartbeatScopeForRig("  "); got != "" {
		t.Errorf("cacheHeartbeatScopeForRig(blank) = %q, want \"\"", got)
	}
	if got := cacheHeartbeatScopeForRig(" qcore "); got != "qcore" {
		t.Errorf("cacheHeartbeatScopeForRig(\" qcore \") = %q, want \"qcore\"", got)
	}
}

// TestCacheHeartbeatWatchedScopesDropsRigNamedCity pins the same guard at the
// watch-list seam: the city scope must appear exactly once, never twice, so a
// duplicated label cannot double-count a single record as two healthy caches.
func TestCacheHeartbeatWatchedScopesDropsRigNamedCity(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "city", Path: filepath.Join(t.TempDir(), "city-rig")},
		{Name: "qcore", Path: filepath.Join(t.TempDir(), "qcore")},
	}}

	got := cacheHeartbeatWatchedScopes(t.TempDir(), cfg)
	want := []string{cacheHeartbeatCityScope, "qcore"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("watched scopes = %v, want %v (the rig named \"city\" must not alias the city record)", got, want)
	}
}

// TestCacheHeartbeatWatchedScopesAlwaysIncludesCity pins that the city store —
// the scope that actually stalled in ga-yc0chj — is watched even when the
// config is unavailable.
func TestCacheHeartbeatWatchedScopesAlwaysIncludesCity(t *testing.T) {
	got := cacheHeartbeatWatchedScopes(t.TempDir(), nil)
	if !reflect.DeepEqual(got, []string{cacheHeartbeatCityScope}) {
		t.Fatalf("watched scopes = %v, want [%s]", got, cacheHeartbeatCityScope)
	}
}
