package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// A polecat that pushes its branch but dies before completing the refinery
// handoff can leave its work bead with gc.routed_to cleared. When the
// reconciler reaps the dead session bead, releaseWorkFromClosedSessionBead
// clears the assignee and reopens the work — but if it does not also restore a
// route, the bead is stranded open+unassigned+unrouted: invisible to BOTH the
// pool demand probe (which keys on gc.routed_to) and releaseOrphanedPoolAssignments
// (which skips empty-routed beads). The fix passes the owning pool route,
// recovered from the closing session's own template metadata, as ReleaseWorkBead's
// run_target fallback; restoreCarriedWorkRoutes (#3421) then backfills gc.routed_to
// from that run_target so a fresh worker re-claims it.
func TestReleaseWorkFromClosedSessionBeadRestoresPoolRouteForUnroutedWork(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "gastown__polecat-th-87n",
			"template":     "gascity/gastown.polecat",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	// Completed-and-pushed work whose routing was lost mid-handoff: a branch
	// on origin, no gc.routed_to, no gc.run_target, assigned to the dead session.
	work, err := store.Create(beads.Bead{
		Title:    "handoff-orphan work",
		Status:   "open",
		Assignee: sessionBead.ID,
		Metadata: map[string]string{"branch": "polecat/ga-n2d.2"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}

	var stderr bytes.Buffer
	releaseWorkFromClosedSessionBead(store, sessionBead, &stderr)

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", got.Assignee)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Metadata[beadmeta.RunTargetMetadataKey] != "gascity/gastown.polecat" {
		t.Fatalf("gc.run_target = %q, want gascity/gastown.polecat (restored pool route; restoreCarriedWorkRoutes re-stamps gc.routed_to from it so the pool demand probe re-discovers the work)", got.Metadata[beadmeta.RunTargetMetadataKey])
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("gc.routed_to = %q, want empty immediately after release (restoreCarriedWorkRoutes backfills it from gc.run_target on the next tick)", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// Workflow-kind beads recover their route via the same ReleaseWorkBead
// run_target fallback as plain work; they re-claim through the legacy
// gc.run_target queue (see #2860), which restoreCarriedWorkRoutes (#3421)
// recognizes for pre-eld2x workflow roots.
func TestReleaseWorkFromClosedSessionBeadRestoresRunTargetForWorkflowKind(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"template":     "graph/worker",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:    "graph step",
		Status:   "in_progress",
		Assignee: sessionBead.ID,
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	var stderr bytes.Buffer
	releaseWorkFromClosedSessionBead(store, sessionBead, &stderr)

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RunTargetMetadataKey] != "graph/worker" {
		t.Fatalf("gc.run_target = %q, want graph/worker (workflow-kind work routes via gc.run_target)", got.Metadata[beadmeta.RunTargetMetadataKey])
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("gc.routed_to = %q, want empty for workflow-kind work", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// Work that still carries a route must be left untouched — only truly unrouted
// orphans get a restored route. Guards against clobbering an in-flight route.
func TestReleaseWorkFromClosedSessionBeadLeavesExistingRouteUntouched(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"template":     "gascity/gastown.polecat",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:    "still-routed work",
		Status:   "in_progress",
		Assignee: sessionBead.ID,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "gascity/other-pool"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	var stderr bytes.Buffer
	releaseWorkFromClosedSessionBead(store, sessionBead, &stderr)

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", got.Assignee)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/other-pool" {
		t.Fatalf("gc.routed_to = %q, want unchanged gascity/other-pool", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// When the closing session bead carries no template/agent_name, there is no
// route to recover — the work is still released, just without a restored route.
func TestReleaseWorkFromClosedSessionBeadWithoutTemplateStillReleases(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:    "worker",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: map[string]string{"session_name": "worker-1"},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:    "unrouted work, unknown pool",
		Status:   "in_progress",
		Assignee: sessionBead.ID,
		Metadata: map[string]string{"branch": "polecat/ga-n2d.2"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	var stderr bytes.Buffer
	releaseWorkFromClosedSessionBead(store, sessionBead, &stderr)

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want empty", got.Assignee)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("gc.routed_to = %q, want empty (no template to recover a route from)", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// A NAMED agent's entire portfolio was detached every time its session closed.
// releaseWorkFromClosedSessionBead built its target set from
// sessionBeadAssigneeIdentities, which includes session_name and
// configured_named_identity — durable identities, not session handles — so a seat
// that cycled at a clean boundary (which handoff doctrine REQUIRES) had every open
// and in-progress bead cleared to an empty assignee, with in_progress reset to open.
//
// Measured on hq 2026-09-11T05:08:17Z: 50 beads in one second, 9s after the session
// bead closed; 11 went in_progress -> open. The seat's next session ran the
// mechanical resume check (List{assignee, in_progress}) and was told "no work" while
// holding 11 in-progress items, one of them a P1 durability bead 6h into what became
// a 95-hour outage. Good behavior was the trigger (ga-9n8hjv).
func TestReleaseWorkFromClosedSessionBeadKeepsNamedSeatPortfolio(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "katya",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":              "katya",
			"configured_named_identity": "katya",
			"template":                  "katya",
			"state":                     "active",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	// The seat's own work, claimed under its DURABLE identity — the normal shape for
	// a named agent, and the shape the 09-11 wave destroyed.
	work, err := store.Create(beads.Bead{
		Title:    "data-plane P1",
		Status:   "open",
		Assignee: "katya",
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}

	var stderr bytes.Buffer
	releaseWorkFromClosedSessionBead(store, sessionBead, &stderr)

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	// Both halves matter. The assignee is what the resume check queries BY; the
	// status is what it filters ON. Losing either one makes the work invisible to
	// the owner, which is the actual harm.
	if got.Assignee != "katya" {
		t.Fatalf("named seat lost its assignee: got %q, want \"katya\"", got.Assignee)
	}
	if got.Status != "in_progress" {
		t.Fatalf("named seat's work was reopened: got status %q, want in_progress", got.Status)
	}
}

// The other half of the cut: a closing session's HANDLES are still released, so
// ordinary pool orphan reclamation is untouched. The bead ID and the pool alias name
// nothing once the session is gone, so work still claimed under one is genuinely
// orphaned and must not be held forever (ga-9n8hjv acceptance item 3).
func TestReleaseWorkFromClosedSessionBeadStillReleasesSessionHandles(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "gastown__polecat-th-91z",
			"alias":        "nux",
			"template":     "gascity/gastown.polecat",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	byAlias, err := store.Create(beads.Bead{Title: "claimed by alias", Status: "open", Assignee: "nux"})
	if err != nil {
		t.Fatalf("create alias work: %v", err)
	}
	byID, err := store.Create(beads.Bead{Title: "claimed by bead id", Status: "open", Assignee: sessionBead.ID})
	if err != nil {
		t.Fatalf("create id work: %v", err)
	}

	var stderr bytes.Buffer
	releaseWorkFromClosedSessionBead(store, sessionBead, &stderr)

	for _, tc := range []struct {
		name string
		id   string
	}{{"alias", byAlias.ID}, {"session bead id", byID.ID}} {
		got, err := store.Get(tc.id)
		if err != nil {
			t.Fatalf("get %s work: %v", tc.name, err)
		}
		if got.Assignee != "" {
			t.Fatalf("work claimed by %s was not released: assignee %q", tc.name, got.Assignee)
		}
	}
}
