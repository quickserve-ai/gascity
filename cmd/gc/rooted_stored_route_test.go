package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// barryCityAndRigWithBeads is the ga-pml0rv shape: a city seat "barry" and a
// rig seat "qcore/barry" sharing a leaf, each with an open session bead.
func barryCityAndRigWithBeads(t *testing.T, rigDir string) (*config.City, beads.Store, map[string]string) {
	t.Helper()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "qcore", Path: rigDir}},
		Agents: []config.Agent{
			{Name: "barry", StartCommand: "true"},
			{Name: "barry", Dir: "qcore", StartCommand: "true"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "barry", Mode: "always"},
			{Template: "barry", Dir: "qcore", Mode: "always"},
		},
	}
	store := beads.NewMemStore()
	ids := map[string]string{}
	for identity, state := range map[string]string{"barry": "city-seat-state", "qcore/barry": "rig-seat-state"} {
		b, err := store.Create(beads.Bead{
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"alias":                     identity,
				"session_name":              strings.ReplaceAll(identity, "/", "--"),
				"state":                     state,
				"configured_named_session":  "true",
				"configured_named_identity": identity,
				"configured_named_mode":     "always",
			},
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", identity, err)
		}
		ids[identity] = b.ID
	}
	return cfg, store, ids
}

// ga-pml0rv (1): the status lookup without a snapshot passed the city seat's
// bare identity to a resolver that re-resolves it in the cwd's rig, so from
// inside qcore the city seat read "lookup error: ... ambiguous".
func TestNamedSessionStatusForCity_CitySeatFromARigCwdWithoutSnapshot(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")
	rigDir := t.TempDir()
	t.Setenv("GC_DIR", rigDir)
	cfg, store, _ := barryCityAndRigWithBeads(t, rigDir)
	if got := currentRigContext(cfg); got != "qcore" {
		t.Fatalf("currentRigContext = %q, want qcore", got)
	}
	cityPath := t.TempDir()
	if got := namedSessionStatusForCity(cityPath, cfg, store, nil, "test-city", "barry", "always", suspensionstate.State{}, nil); got != "city-seat-state" {
		t.Fatalf("status(barry) from the qcore rig = %q, want city-seat-state", got)
	}
	if got := namedSessionStatusForCity(cityPath, cfg, store, nil, "test-city", "qcore/barry", "always", suspensionstate.State{}, nil); got != "rig-seat-state" {
		t.Fatalf("status(qcore/barry) = %q, want rig-seat-state", got)
	}
}

// ga-pml0rv (2): a workflow routed to the city seat stores the bare identity
// "barry" in routed_to. The dispatcher re-resolves it in the workflow's rig
// context (qcore), where it is ambiguous with qcore/barry, so the workflow
// could not be bound. A stored qualified route must still bind the rig seat.
func TestGraphFallbackBindingForBead_StoredBareCitySeatRouteInARigContext(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")
	rigDir := t.TempDir()
	t.Setenv("GC_DIR", t.TempDir())
	cfg, store, ids := barryCityAndRigWithBeads(t, rigDir)
	cityPath := t.TempDir()

	for routedTo, want := range map[string]string{"barry": ids["barry"], "qcore/barry": ids["qcore/barry"]} {
		source := beads.Bead{ID: "src-" + strings.ReplaceAll(routedTo, "/", "-"), Metadata: map[string]string{
			graphroute.GraphExecutionRouteMetaKey:      routedTo,
			graphroute.GraphExecutionRigContextMetaKey: "qcore",
		}}
		binding, err := graphFallbackBindingForBead(source, store, "test-city", cityPath, cfg)
		if err != nil {
			t.Fatalf("graphFallbackBindingForBead(routed_to=%q, rig qcore): %v", routedTo, err)
		}
		if binding.DirectSessionID != want {
			t.Fatalf("routed_to=%q bound %q, want %q", routedTo, binding.DirectSessionID, want)
		}
	}
}

func TestIsCityNamedSessionIdentity(t *testing.T) {
	cfg, _, _ := barryCityAndRigWithBeads(t, t.TempDir())
	for identity, want := range map[string]bool{"barry": true, "qcore/barry": false, "/barry": false, "nobody": false, "": false} {
		if got := isCityNamedSessionIdentity(cfg, identity); got != want {
			t.Fatalf("isCityNamedSessionIdentity(%q) = %v, want %v", identity, got, want)
		}
	}
}
