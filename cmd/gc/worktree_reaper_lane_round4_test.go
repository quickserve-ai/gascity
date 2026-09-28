package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// Review round 4 (ga-yuiof4 item 3; Astra r4 1-2): a reload never drops a
// provider's session-start fence registration while a consumer may still hold
// a handle on it, and a failed reload drops the registration it added for a
// provider it never published.

// newRound4ReloadRuntime builds a runtime on sp with controller state, with
// the session-start fence armed the way run() arms it.
func newRound4ReloadRuntime(t *testing.T, sp runtime.Provider) (*CityRuntime, *controllerState, string, string) {
	t.Helper()
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()
	// What run() does before anything can start a session.
	cr.startFenceActive = true
	cr.registerSessionStartFence(cr.sp)
	t.Cleanup(func() { cr.retireSessionStartFences() })
	return cr, cs, cityPath, tomlPath
}

// TestSessionStartFence_HandleOnReplacedProviderStaysFenced (Astra r4 1): an
// in-process API wake resolved its worker handle on the provider in place
// before a reload; the reload replaces the provider; the wake then starts
// through the old provider. That start must still move the lane's fence — the
// signal that makes an in-flight reaper removal protect — because the reload
// must not have unregistered the old provider.
func TestSessionStartFence_HandleOnReplacedProviderStaysFenced(t *testing.T) {
	sp := runtime.NewFake()
	cr, cs, cityPath, tomlPath := newRound4ReloadRuntime(t, sp)

	sessionStore := beads.NewMemStore()
	info, err := sessionpkg.NewManagerWithOptions(sessionStore, sp).CreateSession(context.Background(), sessionpkg.CreateOptions{BeadOnly: true, Template: "worker", Title: "Worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// The API server's construction: a worker factory on cs.SessionProvider().
	factory, err := worker.NewFactory(worker.FactoryConfig{Store: sessionStore, Provider: cs.SessionProvider(), CityPath: cityPath})
	if err != nil {
		t.Fatalf("worker factory: %v", err)
	}
	handle, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatalf("worker handle: %v", err)
	}

	writeCityRuntimeConfig(t, tomlPath, "fail")
	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)
	if lastProviderName != "fail" || sameRuntimeProvider(cr.sp, sp) {
		t.Fatalf("reload did not replace the provider (lastProviderName=%q)", lastProviderName)
	}
	if got := len(cr.startFenceRegs); got != 2 {
		t.Fatalf("fence registrations after a provider-swapping reload = %d, want 2 (old kept, new added)", got)
	}

	gen0, _ := laneFenceState(cr)
	if startErr := handle.Start(context.Background()); startErr != nil {
		t.Logf("wake returned %v (only the fence is under test)", startErr)
	}
	if !anyStartCall(sp) {
		t.Fatal("the wake never reached the pre-reload provider's Start; the test did not exercise a start")
	}
	gen1, inflight := laneFenceState(cr)
	if gen1 < gen0+2 || inflight != 0 {
		t.Fatalf("lane fence after a wake on the replaced provider: gen %d -> %d, inflight %d; want gen advanced by at least 2 and inflight 0", gen0, gen1, inflight)
	}
}

// TestSessionStartFence_FailedReloadDropsUnpublishedRegistration (Astra r4 2):
// a reload that builds and registers a replacement provider but then fails
// before publishing it (here: listing the old provider's sessions fails) must
// leave only the original registration behind.
func TestSessionStartFence_FailedReloadDropsUnpublishedRegistration(t *testing.T) {
	sp := &partialListPoolProvider{Fake: runtime.NewFake(), listErr: errors.New("backend unavailable")}
	cr, _, cityPath, tomlPath := newRound4ReloadRuntime(t, sp)

	writeCityRuntimeConfig(t, tomlPath, "fail")
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)
	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
	}
	if len(cr.startFenceRegs) != 1 || !sameRuntimeProvider(cr.startFenceRegs[0].sp, sp) {
		providers := make([]string, 0, len(cr.startFenceRegs))
		for _, reg := range cr.startFenceRegs {
			providers = append(providers, fmt.Sprintf("%T", reg.sp))
		}
		t.Fatalf("fence registrations after a failed reload = %v, want only the original provider", providers)
	}
}
