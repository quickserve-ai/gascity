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

// TestCacheHeartbeatWatchedScopesAlwaysIncludesCity pins that the city store —
// the scope that actually stalled in ga-yc0chj — is watched even when the
// config is unavailable.
func TestCacheHeartbeatWatchedScopesAlwaysIncludesCity(t *testing.T) {
	got := cacheHeartbeatWatchedScopes(t.TempDir(), nil)
	if !reflect.DeepEqual(got, []string{cacheHeartbeatCityScope}) {
		t.Fatalf("watched scopes = %v, want [%s]", got, cacheHeartbeatCityScope)
	}
}
