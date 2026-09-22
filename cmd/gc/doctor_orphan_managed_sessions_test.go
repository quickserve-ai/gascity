package main

import (
	"context"
	"errors"
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
	// Closed session bead: its runtime name must NOT be claimed.
	closed := create(beads.Bead{
		Title:  "retired",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "qcore--retired",
			"state":        "active",
		},
	})
	if err := store.Close(closed.ID); err != nil {
		t.Fatal(err)
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
	for _, unwanted := range []string{"qcore--retired", "work--not-a-session"} {
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
// an open session bead claims survives Fix, a seat whose bead is closed and a
// stray session are stopped.
func TestDoctorOrphanSessionsFixSparesSessionBeadClaimedSeats(t *testing.T) {
	store, _ := managedNamesTestStore(t)
	cityPath := t.TempDir()
	cfg := &config.City{Agents: []config.Agent{{Name: "mayor"}}}

	sp := runtime.NewFake()
	running := []string{"mayor", "qcore--archer", "platform--gastown__furiosa", "qcore--retired", "stray"}
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
		t.Fatalf("Run() = status %d details %v (%s), want a warning naming exactly qcore--retired and stray", r.Status, r.Details, r.Message)
	}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityPath}); err != nil {
		t.Fatalf("Fix() error = %v", err)
	}
	for _, name := range []string{"mayor", "qcore--archer", "platform--gastown__furiosa"} {
		if !sp.IsRunning(name) {
			t.Errorf("managed session %q was stopped by Fix", name)
		}
	}
	for _, name := range []string{"qcore--retired", "stray"} {
		if sp.IsRunning(name) {
			t.Errorf("orphan session %q still running after Fix", name)
		}
	}
}
