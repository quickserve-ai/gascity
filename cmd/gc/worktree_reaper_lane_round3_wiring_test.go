package main

import (
	"context"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func laneFenceState(cr *CityRuntime) (gen uint64, inflight int) {
	f := cr.sessionStartFenceOf()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen, f.inflight
}

// TestCityRuntimeRun_RegistersSessionStartFenceOnItsProvider pins the wiring
// the round-3 coverage rests on: once run() is under way, every
// internal/session Manager start through the city's provider is bracketed by
// the lane's fence (observed through session.BracketProviderStart, the same
// registry lookup Manager.startRuntime does), and the registration is retired
// when run() returns. A future start path that builds a Manager on this
// provider cannot bypass it.
func TestCityRuntimeRun_RegistersSessionStartFenceOnItsProvider(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cr *CityRuntime
	var sawBracket atomic.Bool
	cr, runtimeErr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops: newDrainOps(sp),
		Rec:  events.Discard,
		OnStarted: func() {
			gen0, _ := laneFenceState(cr)
			end := sessionpkg.BracketProviderStart(sp)
			gen1, inflight := laneFenceState(cr)
			end()
			gen2, after := laneFenceState(cr)
			sawBracket.Store(gen1 == gen0+1 && inflight >= 1 && gen2 == gen0+2 && after == inflight-1)
			cancel()
		},
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if runtimeErr != nil {
		t.Fatalf("building the city runtime: %v", runtimeErr)
	}
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	cr.run(ctx)

	if !sawBracket.Load() {
		t.Fatal("a start through the city's provider did not move the lane's session-start fence while run() was active")
	}
	gen0, _ := laneFenceState(cr)
	sessionpkg.BracketProviderStart(sp)()
	if gen1, _ := laneFenceState(cr); gen1 != gen0 {
		t.Fatalf("fence still registered after run() returned (gen %d -> %d)", gen0, gen1)
	}
}
