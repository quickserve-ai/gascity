package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// barryOnDemandCityAndRigConfig is the ga-elylrw shape: an on-demand city seat
// "barry" with no session bead yet, and a rig seat qcore/barry sharing its leaf.
func barryOnDemandCityAndRigConfig(rigDir string) *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "qcore", Path: rigDir}},
		Agents: []config.Agent{
			{Name: "barry", StartCommand: "true"},
			{Name: "barry", Dir: "qcore", StartCommand: "true"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "barry"},
			{Template: "barry", Dir: "qcore"},
		},
	}
}

// ga-elylrw (F1): "/barry" from a rig cwd, when the city seat has no bead and
// must be materialized. The materialize step re-resolved the BARE identity in
// rig context and came back ambiguous, so "gc mail send /barry" stored the
// mail and then failed its nudge, and "gc nudge /barry" failed outright.
func TestResolveSessionIDMaterializingNamed_RootedCitySeatMaterializesFromARigCwd(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")
	rigDir := t.TempDir()
	cityPath := t.TempDir()
	cfg := barryOnDemandCityAndRigConfig(rigDir)

	// Control: the same seat materialized from outside any rig. Its alias and
	// session name are what the rooted materialize must also produce.
	t.Setenv("GC_DIR", cityPath)
	if got := currentRigContext(cfg); got != "" {
		t.Fatalf("control currentRigContext = %q, want none", got)
	}
	controlStore := beads.NewMemStore()
	controlID, err := resolveSessionIDMaterializingNamed(cityPath, cfg, controlStore, "barry")
	if err != nil {
		t.Fatalf("control materialize(barry) from the city: %v", err)
	}
	control, err := controlStore.Get(controlID)
	if err != nil {
		t.Fatalf("control Get: %v", err)
	}

	t.Setenv("GC_DIR", rigDir)
	if got := currentRigContext(cfg); got != "qcore" {
		t.Fatalf("currentRigContext = %q, want qcore (the test must run from inside the rig)", got)
	}
	store := beads.NewMemStore()
	id, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, "/barry")
	if err != nil {
		t.Fatalf("materialize(/barry) from the qcore rig: %v", err)
	}
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	for _, key := range []string{"alias", "session_name", "configured_named_identity", "template"} {
		if got.Metadata[key] != control.Metadata[key] {
			t.Fatalf("%s = %q, want %q (the same seat as the bare form from the city)", key, got.Metadata[key], control.Metadata[key])
		}
	}
	if got.Metadata["configured_named_identity"] != "barry" {
		t.Fatalf("configured_named_identity = %q, want the city seat barry", got.Metadata["configured_named_identity"])
	}
}

// ga-elylrw (F1, sling leg): gc sling's direct-session resolver resolves the
// target, then hands spec.Identity to the materializing resolver, which
// re-resolved the bare city identity in rig context.
func TestCliDirectSessionResolver_RootedCitySeatFromARigCwd(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")
	rigDir := t.TempDir()
	cityPath := t.TempDir()
	t.Setenv("GC_DIR", rigDir)
	cfg := barryOnDemandCityAndRigConfig(rigDir)
	store := beads.NewMemStore()

	id, ok, err := cliDirectSessionResolver(store, "test-city", cityPath, cfg, "/barry", "qcore")
	if err != nil || !ok {
		t.Fatalf("cliDirectSessionResolver(/barry, qcore) = %q ok=%v err=%v, want the city seat", id, ok, err)
	}
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if b.Metadata["configured_named_identity"] != "barry" {
		t.Fatalf("configured_named_identity = %q, want barry", b.Metadata["configured_named_identity"])
	}
}

// ga-elylrw (N1): two open rig sessions share the leaf "foo" and no city seat
// has it, so bare "foo" is ambiguous at the basename step and the rooted retry
// finds nothing. The wake must report the original ambiguity, which lists the
// sessions, not the retry's "session not found". The resolver is the real
// session-ID chain resolveNudgeTarget runs.
func TestResolveMailWakeTarget_KeepsTheAmbiguityWhenNoCitySeatExists(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")
	cityPath := t.TempDir()
	t.Setenv("GC_DIR", cityPath)
	store := beads.NewMemStore()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	for _, alias := range []string{"rigA/foo", "rigB/foo"} {
		if _, err := store.Create(beads.Bead{
			Type:     session.BeadType,
			Labels:   []string{session.LabelSession},
			Metadata: map[string]string{"alias": alias, "session_name": strings.ReplaceAll(alias, "/", "--")},
		}); err != nil {
			t.Fatalf("Create(%s): %v", alias, err)
		}
	}
	resolve := func(identifier string) (nudgeTarget, error) {
		id, err := resolveSessionIDWithConfig(cityPath, cfg, store, identifier)
		return nudgeTarget{sessionID: id}, err
	}
	// Preflight: the rooted retry really does come back not-found, so the
	// assertion below is about which error is kept.
	if _, err := resolve("/foo"); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("resolve(/foo) = %v, want ErrSessionNotFound", err)
	}
	_, err := resolveMailWakeTarget("foo", resolve)
	if !errors.Is(err, session.ErrAmbiguous) || !strings.Contains(err.Error(), "rigA/foo") || !strings.Contains(err.Error(), "rigB/foo") {
		t.Fatalf("resolveMailWakeTarget(foo) = %v, want the original ambiguity naming rigA/foo and rigB/foo", err)
	}
}
