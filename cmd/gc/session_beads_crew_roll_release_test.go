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
		"",
		crewRollConfig(false),
		cityStore,
		map[string]beads.Store{"qcore": rigStore},
		crewSessionBead(),
		"", // the close path's runTargetFallback, verbatim from cmdSessionClose
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
		"", crewRollConfig(false), cityStore, nil, crewSessionBead(), "", &stderr,
	)

	got, err := cityStore.Get(work.ID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if got.Assignee != crewRuntimeIdentity {
		t.Fatalf("city work Assignee = %q, want %q retained", got.Assignee, crewRuntimeIdentity)
	}
}

// assertProposedNotReleased is fence #1's signature (ga-9n8hjv): a named agent's
// bead keeps its assignee AND its status across a session teardown, and carries
// the release PROPOSAL (label + first-sight metadata naming the release path)
// for a judge instead.
func assertProposedNotReleased(t *testing.T, got beads.Bead, wantAssignee, wantStatus, wantPath, what string) {
	t.Helper()
	if got.Assignee != wantAssignee {
		t.Fatalf("%s Assignee = %q, want %q kept — session teardown never unassigns a named agent's work (ga-9n8hjv)", what, got.Assignee, wantAssignee)
	}
	if got.Status != wantStatus {
		t.Fatalf("%s Status = %q, want %q kept", what, got.Status, wantStatus)
	}
	hasLabel := false
	for _, l := range got.Labels {
		if l == beadmeta.ReleaseProposedLabel {
			hasLabel = true
		}
	}
	if !hasLabel {
		t.Fatalf("%s labels = %v, want %q: a withheld release must leave a proposal a judge can find", what, got.Labels, beadmeta.ReleaseProposedLabel)
	}
	if got.Metadata[beadmeta.ReleaseProposedAtMetadataKey] == "" {
		t.Fatalf("%s has no %s stamp", what, beadmeta.ReleaseProposedAtMetadataKey)
	}
	if got.Metadata[beadmeta.ReleaseProposedPathMetadataKey] != wantPath {
		t.Fatalf("%s %s = %q, want %q", what, beadmeta.ReleaseProposedPathMetadataKey, got.Metadata[beadmeta.ReleaseProposedPathMetadataKey], wantPath)
	}
	if got.Metadata[beadmeta.ReleaseProposedAssigneeMetadataKey] != wantAssignee {
		t.Fatalf("%s %s = %q, want %q", what, beadmeta.ReleaseProposedAssigneeMetadataKey, got.Metadata[beadmeta.ReleaseProposedAssigneeMetadataKey], wantAssignee)
	}
}

// TestSessionCloseProposesRetiredNamedSessionWork: an identity that is NOT in
// the config any more still belonged to a named agent. Before ga-9n8hjv its work
// was released here; under fence #1 (Cherub's typed Q12 re-ruling, 2026-09-27)
// teardown keeps it and proposes the release for a judge.
func TestSessionCloseProposesRetiredNamedSessionWork(t *testing.T) {
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
		"",
		&config.City{}, // qcore/ray is no longer configured
		cityStore,
		map[string]beads.Store{"qcore": rigStore},
		crewSessionBead(),
		"qcore/fallback-route",
		&stderr,
	)

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	assertProposedNotReleased(t, got, crewRuntimeIdentity, "in_progress", "retired-session-unclaim", "retired-agent work")
	if got.Metadata[beadmeta.RunTargetMetadataKey] != "" {
		t.Fatalf("retired-agent work run_target = %q, want untouched: a proposal re-routes nothing", got.Metadata[beadmeta.RunTargetMetadataKey])
	}
}

// TestSessionCloseProposesSuspendedAgentWork: a suspended agent's tier never
// claims, which is why this used to release. It is still a named agent's work,
// so under fence #1 the release is proposed, not made (ga-9n8hjv); the judge
// decides whether to wait for the agent or hand the work on.
func TestSessionCloseProposesSuspendedAgentWork(t *testing.T) {
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
		"",
		crewRollConfig(true), // backing agent suspended
		cityStore,
		map[string]beads.Store{"qcore": rigStore},
		crewSessionBead(),
		"qcore/fallback-route",
		&stderr,
	)

	got, err := rigStore.Get(work.ID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	assertProposedNotReleased(t, got, crewRuntimeIdentity, "open", "retired-session-unclaim", "suspended-agent work")
}

// TestSessionCloseProposesEphemeralIdentifierWork pins the other edge: work a
// named session holds under an identifier that DIES with it (the session bead
// ID, the "rig--agent" session_name form) is still the named agent's work. It is
// not released (ga-9n8hjv); it is proposed, and the label is what keeps it from
// stranding silently on an address nothing answers to.
func TestSessionCloseProposesEphemeralIdentifierWork(t *testing.T) {
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
		"",
		crewRollConfig(false),
		cityStore,
		map[string]beads.Store{"qcore": rigStore},
		sessionBead,
		"qcore/fallback-route",
		&stderr,
	)

	gotByID, err := rigStore.Get(byBeadID.ID)
	if err != nil {
		t.Fatalf("get bead-ID work: %v", err)
	}
	assertProposedNotReleased(t, gotByID, sessionBead.ID, "open", "retired-session-unclaim", "bead-ID work")

	gotByName, err := rigStore.Get(bySessionName.ID)
	if err != nil {
		t.Fatalf("get session-name work: %v", err)
	}
	assertProposedNotReleased(t, gotByName, crewSessionName, "open", "retired-session-unclaim", "session-name work")
}
