package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
)

// managedNamesTestStore seeds a store with the session-bead shapes the
// orphan-sessions lister must tell apart (ga-n2f1ph). It returns the store and
// the ID of the open session bead that carries no session_name metadata.
func managedNamesTestStore(t *testing.T) (*beads.MemStore, string) {
	t.Helper()
	store := beads.NewMemStore()
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	// Open named session whose runtime name is not template-derived.
	create(beads.Bead{
		Title:  "archer",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "qcore--archer",
			"alias":        "qcore/archer",
			"template":     "qcore/cherub-law.archer",
			"state":        "active",
		},
	})
	// Open namepool-themed pool instance.
	create(beads.Bead{
		Title:  "furiosa",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "platform--gastown__furiosa",
			"state":        "asleep",
		},
	})
	// Closed session beads: their runtime names must NOT be claimed. One is a
	// retired pool instance; the other is a configured named seat whose bead
	// closed, which config (not the bead) must keep managed.
	for _, closedName := range []string{"platform--gastown__nux", "qcore--warden"} {
		closed := create(beads.Bead{
			Title:  closedName,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name": closedName,
				"state":        "active",
			},
		})
		if err := store.Close(closed.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Open session bead with no session_name: the manager runs it as s-<id>.
	unnamed := create(beads.Bead{
		Title:    "unnamed",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: map[string]string{"state": "active"},
	})
	// Non-session work bead carrying a session_name-shaped key: not a session.
	create(beads.Bead{
		Title:    "work",
		Type:     "task",
		Metadata: map[string]string{"session_name": "work--not-a-session"},
	})
	return store, unnamed.ID
}

func TestDoctorManagedSessionNamesCollectsOpenSessionBeadsOnly(t *testing.T) {
	store, unnamedID := managedNamesTestStore(t)
	cityPath := t.TempDir()
	opens := 0
	lister := doctorManagedSessionNames(cityPath, &config.City{}, func(path string) (beads.Store, error) {
		opens++
		if path != cityPath {
			t.Errorf("opened store at %q, want city path %q", path, cityPath)
		}
		return store, nil
	})
	if opens != 0 {
		t.Fatalf("store opened %d times before the lister was called, want 0 (lazy)", opens)
	}

	names, err := lister()
	if err != nil {
		t.Fatalf("lister() error = %v", err)
	}
	if opens != 1 {
		t.Errorf("store opened %d times, want 1", opens)
	}
	for _, want := range []string{"qcore--archer", "platform--gastown__furiosa", "s-" + unnamedID} {
		if _, ok := names[want]; !ok {
			t.Errorf("managed names missing %q; got %v", want, names)
		}
	}
	for _, unwanted := range []string{"platform--gastown__nux", "qcore--warden", "work--not-a-session"} {
		if _, ok := names[unwanted]; ok {
			t.Errorf("managed names include %q, want it excluded; got %v", unwanted, names)
		}
	}
	if len(names) != 3 {
		t.Errorf("managed names = %v, want exactly 3", names)
	}
}

func TestDoctorManagedSessionNamesOpenErrorIsReturned(t *testing.T) {
	lister := doctorManagedSessionNames(t.TempDir(), &config.City{}, func(string) (beads.Store, error) {
		return nil, errors.New("dolt unreachable")
	})
	names, err := lister()
	if err == nil {
		t.Fatalf("lister() error = nil, names = %v; want the open error", names)
	}
	if !strings.Contains(err.Error(), "dolt unreachable") {
		t.Errorf("lister() error = %q, want it to carry the open error", err.Error())
	}
}

// TestDoctorOrphanSessionsFixSparesSessionBeadClaimedSeats wires the cmd/gc
// lister into the doctor check end to end: a live seat whose runtime name only
// an open session bead claims survives Fix, and so does a configured named
// seat whose bead is closed (config alone manages it); a pool instance whose
// bead is closed and a stray session are stopped.
func TestDoctorOrphanSessionsFixSparesSessionBeadClaimedSeats(t *testing.T) {
	store, _ := managedNamesTestStore(t)
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents:        []config.Agent{{Name: "mayor"}},
		NamedSessions: []config.NamedSession{{Name: "warden", Template: "cherub-law.warden", Dir: "qcore"}},
	}

	sp := runtime.NewFake()
	running := []string{"mayor", "qcore--archer", "platform--gastown__furiosa", "qcore--warden", "platform--gastown__nux", "stray"}
	for _, name := range running {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	check := doctor.NewOrphanSessionsCheck(cfg, "test", "", sp).
		WithManagedSessionNames(doctorManagedSessionNames(cityPath, cfg, func(string) (beads.Store, error) {
			return store, nil
		}))

	if r := check.Run(&doctor.CheckContext{CityPath: cityPath}); r.Status != doctor.StatusWarning || len(r.Details) != 2 {
		t.Fatalf("Run() = status %d details %v (%s), want a warning naming exactly platform--gastown__nux and stray", r.Status, r.Details, r.Message)
	}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityPath}); err != nil {
		t.Fatalf("Fix() error = %v", err)
	}
	for _, name := range []string{"mayor", "qcore--archer", "platform--gastown__furiosa", "qcore--warden"} {
		if !sp.IsRunning(name) {
			t.Errorf("managed session %q was stopped by Fix", name)
		}
	}
	for _, name := range []string{"platform--gastown__nux", "stray"} {
		if sp.IsRunning(name) {
			t.Errorf("orphan session %q still running after Fix", name)
		}
	}
}

// orphanFixAgainst runs the orphan-sessions check, wired through the cmd/gc
// lister over store, against a city running a template seat, a bead-claimed
// seat and a stray. It returns the runtime, so a caller can assert what
// survived, and the Fix error.
func orphanFixAgainst(t *testing.T, cityPath string, cfg *config.City, store beads.Store) (*runtime.Fake, error) {
	t.Helper()
	sp := runtime.NewFake()
	for _, name := range []string{"mayor", "qcore--archer", "stray"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	check := doctor.NewOrphanSessionsCheck(cfg, "test", "", sp).
		WithManagedSessionNames(doctorManagedSessionNames(cityPath, cfg, func(string) (beads.Store, error) {
			return store, nil
		}))
	return sp, check.Fix(&doctor.CheckContext{CityPath: cityPath})
}

// TestDoctorManagedSessionNamesRefusesUnroutedSessionsRelocation: a city whose
// config binds the sessions class off the work binding, but whose one-shot
// storage routes came back nil (cliStorageRoutes answers nil when it cannot
// load the city's config, as here: no city.toml), must not read the work
// store. That read succeeds EMPTY in a relocated city, so the lister errors
// and Fix refuses.
func TestDoctorManagedSessionNamesRefusesUnroutedSessionsRelocation(t *testing.T) {
	resetCLIStorageRoutes(t)
	store, _ := managedNamesTestStore(t)
	cityPath := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
		Storage: &config.StorageConfig{Classes: config.StorageClasses{
			Work:      config.StorageWorkBinding,
			Graph:     "infra",
			Sessions:  "infra",
			Messaging: "infra",
			Orders:    "infra",
			Nudges:    "infra",
		}},
	}
	if cliSessionsRelocated(cityPath) {
		t.Fatal("precondition: routes relocate sessions for a city with no city.toml; want nil routes")
	}

	names, err := doctorManagedSessionNames(cityPath, cfg, func(string) (beads.Store, error) {
		return store, nil
	})()
	if err == nil {
		t.Fatalf("lister() error = nil, names = %v; want a refusal to read the work store for a relocated sessions class", names)
	}
	if !strings.Contains(err.Error(), "do not relocate") {
		t.Errorf("lister() error = %q, want it to name the unrouted relocation", err.Error())
	}

	sp, fixErr := orphanFixAgainst(t, cityPath, cfg, store)
	if fixErr == nil || !strings.Contains(fixErr.Error(), "refusing to stop") {
		t.Fatalf("Fix() error = %v, want a refusal", fixErr)
	}
	for _, name := range []string{"mayor", "qcore--archer", "stray"} {
		if !sp.IsRunning(name) {
			t.Errorf("session %q was stopped by a Fix that should have refused", name)
		}
	}
}

// TestDoctorOrphanSessionsFixRefusesOnPartialSessionBeadListing: a session-bead
// listing that only partially succeeded may be missing the very bead that
// claims a live seat, so the lister errors and Fix stops nothing.
func TestDoctorOrphanSessionsFixRefusesOnPartialSessionBeadListing(t *testing.T) {
	mem, _ := managedNamesTestStore(t)
	cfg := &config.City{Agents: []config.Agent{{Name: "mayor"}}}

	sp, fixErr := orphanFixAgainst(t, t.TempDir(), cfg, &partialSessionListStore{MemStore: mem})
	if fixErr == nil {
		t.Fatal("Fix() error = nil, want a refusal on a partial session-bead listing")
	}
	for _, want := range []string{"refusing to stop", "listing managed sessions failed", "skipped corrupt session bead"} {
		if !strings.Contains(fixErr.Error(), want) {
			t.Errorf("Fix() error = %q, want it to contain %q", fixErr.Error(), want)
		}
	}
	for _, name := range []string{"mayor", "qcore--archer", "stray"} {
		if !sp.IsRunning(name) {
			t.Errorf("session %q was stopped by a Fix that should have refused", name)
		}
	}
}

// TestBuildDoctorChecksWiresOrphanSessionsManagedNames pins the production
// wiring: the orphan-sessions check buildDoctorChecks registers must carry the
// managed-name lister. Without it the check is template-only, and every named
// seat whose alias differs from its template reads as an orphan Fix stops.
func TestBuildDoctorChecksWiresOrphanSessionsManagedNames(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck: true, SkipManagedDoltCheck: true,
	})
	for _, check := range checks {
		if check.Name() != "orphan-sessions" {
			continue
		}
		orphan, ok := check.(*doctor.OrphanSessionsCheck)
		if !ok {
			t.Fatalf("orphan-sessions check is %T, want *doctor.OrphanSessionsCheck", check)
		}
		if !orphan.HasManagedSessionNames() {
			t.Fatal("orphan-sessions check registered without a managed-name lister; Fix would stop live named and pool seats")
		}
		return
	}
	t.Fatalf("orphan-sessions check not registered; names=%v", doctorCheckNames(checks))
}
