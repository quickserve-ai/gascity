package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// ga-sdynmb: `gc session close <agent>` is the DOCUMENTED move for a provider
// flip or config-drift roll (city.toml: "reset does not re-resolve provider"),
// and cmdSessionClose handed the closing session's whole backlog to
// unclaimWorkAssignedToRetiredSessionBead with runTargetFallback="". That
// cleared the assignee on every open/in_progress bead the agent held, across
// the city AND rig stores, and — the fallback being empty — stamped no
// gc.run_target either. The result on 2026-08-17/18 was 91 crew beads left
// open+unassigned+unrouted: invisible to the pool demand probe (keys on
// gc.routed_to), skipped by releaseOrphanedPoolAssignments (skips empty-routed
// beads), and deliberately out of scope for witness orphan recovery (which is
// scoped to POOL/EPHEMERAL identities so crew work is never dumped into the
// polecat pool). A restart is not a retirement.
//
// These tests are the falsifiable floor: crewRollConfig's named session is
// STILL in the config, so the identity outlives the close, and each assertion
// below fails on unpatched source (assignee comes back "").
//
// NAMESPACE NOTE, and the reason the fixtures look redundant: an agent lives
// under TWO address forms — the CONFIG form carried in the session bead's
// "template" metadata (here "qcore/cherub-law.ray") and the RUNTIME form that
// is its actual assignee ("qcore/ray"). Only the runtime form is what the
// agent's own hook matches on (work_query Tier 1 is an assignee match), so
// every assertion here is written against the RUNTIME form. Asserting the
// config form instead is the known false pass this bead called out.

const (
	crewRuntimeIdentity = "qcore/ray"            // assignee; what ray's own hook matches
	crewConfigTemplate  = "qcore/cherub-law.ray" // session bead "template" metadata
	crewSessionName     = "qcore--ray"           // ephemeral session_name form
)

// crewRollConfig is a city whose [[named_session]] qcore/ray is backed by a
// live (unsuspended) agent — i.e. the agent survives the roll.
func crewRollConfig(suspended bool) *config.City {
	// Deliberately the PRODUCTION shape, not the convenient one: a real
	// [[named_session]] declares name="ray" alongside template="cherub-law.ray",
	// so the agent template ("qcore/cherub-law.ray") and the session identity
	// ("qcore/ray") are DIFFERENT strings — QualifiedName is Dir+"/"+IdentityName
	// and IdentityName prefers Name over Template. Collapsing the two here would
	// let a guard that keys on the template form pass this suite and still be a
	// no-op against the live city.
	return &config.City{
		Agents: []config.Agent{
			{Name: "cherub-law.ray", Dir: "qcore", Suspended: suspended, MaxActiveSessions: intPtr(1)},
		},
		NamedSessions: []config.NamedSession{
			{Name: "ray", Template: "cherub-law.ray", Dir: "qcore", Mode: "always"},
		},
	}
}

// crewSessionBead is the closing session bead for the named crew agent, shaped
// like the real ones: template metadata in the CONFIG form, identity in the
// RUNTIME form.
func crewSessionBead() beads.Bead {
	return beads.Bead{
		ID:     "ga-oldsession",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"configured_named_session":  "true",
			"configured_named_identity": crewRuntimeIdentity,
			"agent_name":                crewRuntimeIdentity,
			"alias":                     crewRuntimeIdentity,
			"session_name":              crewSessionName,
			"template":                  crewConfigTemplate,
			"state":                     "active",
		},
	}
}

// assertNotStranded fails with the exact defect signature from the bead:
// open + no assignee + no gc.routed_to + no gc.run_target is the triple that
// no subsystem will ever pick up.
func assertNotStranded(t *testing.T, got beads.Bead, what string) {
	t.Helper()
	if got.Assignee == "" &&
		got.Metadata[beadmeta.RoutedToMetadataKey] == "" &&
		got.Metadata[beadmeta.RunTargetMetadataKey] == "" {
		t.Fatalf("%s (%s) is STRANDED: open+unassigned+unrouted — invisible to the demand probe and to orphan recovery", what, got.ID)
	}
}

// TestSessionCloseKeepsConfiguredCrewWorkAcrossRoll is the core regression: a
// named crew agent that is still configured keeps BOTH an open and an
// in_progress bead, in the rig store, across the close.
func TestSessionCloseKeepsConfiguredCrewWorkAcrossRoll(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	openWork, err := rigStore.Create(beads.Bead{
		Title: "open crew work", Status: "open", Assignee: crewRuntimeIdentity,
	})
	if err != nil {
		t.Fatalf("create open work: %v", err)
	}
	inProgressWork, err := rigStore.Create(beads.Bead{
		Title: "in-flight crew epic", Assignee: crewRuntimeIdentity,
	})
	if err != nil {
		t.Fatalf("create in_progress work: %v", err)
	}
	// MemStore.Create forces Status="open", so drive the bead to in_progress
	// explicitly — otherwise this case silently degrades into a second copy of
	// the open-work case and stops covering the status-reset half of the bug.
	inProgress := "in_progress"
	if err := rigStore.Update(inProgressWork.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}

	var stderr bytes.Buffer
	unclaimWorkAssignedToRetiredSessionBead(
		cityStore,
		map[string]beads.Store{"qcore": rigStore},
		crewSessionBead(),
		"", // the close path's runTargetFallback, verbatim from cmdSessionClose
		crewRollConfig(false),
		&stderr,
	)

	gotOpen, err := rigStore.Get(openWork.ID)
	if err != nil {
		t.Fatalf("get open work: %v", err)
	}
	if gotOpen.Assignee != crewRuntimeIdentity {
		t.Fatalf("open work Assignee = %q, want %q retained — a restart is not a retirement", gotOpen.Assignee, crewRuntimeIdentity)
	}
	assertNotStranded(t, gotOpen, "open crew work")

	gotInProgress, err := rigStore.Get(inProgressWork.ID)
	if err != nil {
		t.Fatalf("get in_progress work: %v", err)
	}
	if gotInProgress.Assignee != crewRuntimeIdentity {
		t.Fatalf("in_progress work Assignee = %q, want %q retained", gotInProgress.Assignee, crewRuntimeIdentity)
	}
	if gotInProgress.Status != "in_progress" {
		t.Fatalf("in_progress work Status = %q, want %q — a roll must not silently reset an in-flight epic to open", gotInProgress.Status, "in_progress")
	}
	assertNotStranded(t, gotInProgress, "in_progress crew work")
}

// TestSessionCloseKeepsConfiguredCrewWorkInCityStore covers the city-scoped
// half of the same sweep: a city-level named agent's own ga- beads.
func TestSessionCloseKeepsConfiguredCrewWorkInCityStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	work, err := cityStore.Create(beads.Bead{
		Title: "city crew work", Status: "open", Assignee: crewRuntimeIdentity,
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}

	var stderr bytes.Buffer
	unclaimWorkAssignedToRetiredSessionBead(
		cityStore, nil, crewSessionBead(), "", crewRollConfig(false), &stderr,
	)

	got, err := cityStore.Get(work.ID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if got.Assignee != crewRuntimeIdentity {
		t.Fatalf("city work Assignee = %q, want %q retained", got.Assignee, crewRuntimeIdentity)
	}
}

// TestSessionCloseStillReleasesRetiredNamedSessionWork is the companion that
// keeps the guard honest: an identity that is NOT in the config is genuinely
// retired, so its work must still be released.
func TestSessionCloseStillReleasesRetiredNamedSessionWork(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	work, err := rigStore.Create(beads.Bead{
		Title: "retired agent work", Assignee: crewRuntimeIdentity,
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}

	var stderr bytes.Buffer
	unclaimWorkAssignedToRetiredSessionBead(
		cityStore,
		map[string]beads.Store{"qcore": rigStore},
		crewSessionBead(),
		"qcore/fallback-route",
		&config.City{}, // qcore/ray is no longer configured
		&stderr,
	)

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if got.Assignee != "" {
		t.Fatalf("retired-agent work Assignee = %q, want cleared — a real retirement must still release its work", got.Assignee)
	}
	if got.Status != "open" {
		t.Fatalf("retired-agent work Status = %q, want %q", got.Status, "open")
	}
	if got.Metadata[beadmeta.RunTargetMetadataKey] != "qcore/fallback-route" {
		t.Fatalf("retired-agent work run_target = %q, want the fallback route stamped", got.Metadata[beadmeta.RunTargetMetadataKey])
	}
}

// TestSessionCloseStillReleasesSuspendedAgentWork pins the suspension carve-out
// that isConfiguredNamedSessionIdentity already encodes: a suspended agent's
// tier never claims, so keeping its assignee would orphan the bead with neither
// side picking it up.
func TestSessionCloseStillReleasesSuspendedAgentWork(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	work, err := rigStore.Create(beads.Bead{
		Title: "suspended agent work", Status: "open", Assignee: crewRuntimeIdentity,
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}

	var stderr bytes.Buffer
	unclaimWorkAssignedToRetiredSessionBead(
		cityStore,
		map[string]beads.Store{"qcore": rigStore},
		crewSessionBead(),
		"qcore/fallback-route",
		crewRollConfig(true), // backing agent suspended
		&stderr,
	)

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if got.Assignee != "" {
		t.Fatalf("suspended-agent work Assignee = %q, want cleared", got.Assignee)
	}
}

// TestSessionCloseStillReleasesEphemeralIdentifierWork pins the other edge: even
// for a still-configured crew agent, work pinned to an identifier that DIES with
// this session (the session bead ID, the "rig--agent" session_name form) must
// still be released, or it strands on an address nothing will ever answer to.
func TestSessionCloseStillReleasesEphemeralIdentifierWork(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	sessionBead := crewSessionBead()
	byBeadID, err := rigStore.Create(beads.Bead{
		Title: "work pinned to the dying session bead", Status: "open", Assignee: sessionBead.ID,
	})
	if err != nil {
		t.Fatalf("create bead-ID work: %v", err)
	}
	bySessionName, err := rigStore.Create(beads.Bead{
		Title: "work pinned to the session_name form", Status: "open", Assignee: crewSessionName,
	})
	if err != nil {
		t.Fatalf("create session-name work: %v", err)
	}

	var stderr bytes.Buffer
	unclaimWorkAssignedToRetiredSessionBead(
		cityStore,
		map[string]beads.Store{"qcore": rigStore},
		sessionBead,
		"qcore/fallback-route",
		crewRollConfig(false),
		&stderr,
	)

	gotByID, err := rigStore.Get(byBeadID.ID)
	if err != nil {
		t.Fatalf("get bead-ID work: %v", err)
	}
	if gotByID.Assignee != "" {
		t.Fatalf("bead-ID work Assignee = %q, want cleared — that identifier dies with the session", gotByID.Assignee)
	}
	assertNotStranded(t, gotByID, "bead-ID work")

	gotByName, err := rigStore.Get(bySessionName.ID)
	if err != nil {
		t.Fatalf("get session-name work: %v", err)
	}
	if gotByName.Assignee != "" {
		t.Fatalf("session-name work Assignee = %q, want cleared", gotByName.Assignee)
	}
	assertNotStranded(t, gotByName, "session-name work")
}
