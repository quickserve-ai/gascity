package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// gateTestRig is the single rig gateTestCity declares. The store refs the tests
// pass alongside the city ("qcore", "astro", "") are matched against it, so the
// name is fixed here rather than threaded through every call.
const gateTestRig = "qcore"

// gateTestCity builds a city with one pool-capable agent and one rig named
// gateTestRig, with that rig's orphan_release knob set to orphanRelease (nil
// leaves it unset).
func gateTestCity(orphanRelease *bool) *config.City {
	return &config.City{
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		Rigs: []config.Rig{{
			Name:          gateTestRig,
			Path:          "/tmp/" + gateTestRig,
			OrphanRelease: orphanRelease,
		}},
	}
}

// seedGateWork creates an in_progress pool-routed bead with a dead assignee —
// the shape releaseOrphanedPoolAssignments exists to reopen.
func seedGateWork(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{
		Title:    "orphaned pool work",
		Assignee: "worker-dead",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	if err := store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("Set work status: %v", err)
	}
	reloaded, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Reload work bead: %v", err)
	}
	return reloaded
}

// assertStillClaimed proves the hold actually preserved the claim rather than
// merely returning an empty released slice. A gate that returns "released
// nothing" while some other path cleared the assignee would pass a
// release-count-only assertion, which is the failure this check exists to
// catch.
func assertStillClaimed(t *testing.T, store beads.Store, id string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress (claim must survive the hold)", got.Status)
	}
	if got.Assignee != "worker-dead" {
		t.Fatalf("assignee = %q, want worker-dead (claim must survive the hold)", got.Assignee)
	}
}

func TestPoolOrphanReleaseGate_UnsetKnobStillReleases(t *testing.T) {
	store := beads.NewMemStore()
	work := seedGateWork(t, store)

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store, gateTestCity(nil), "", nil,
		[]beads.Bead{work}, nil, []string{"qcore"}, nil,
	)
	if len(released) != 1 || released[0].ID != work.ID {
		t.Fatalf("released = %v, want [%s] — an unset knob must not change behavior", released, work.ID)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get work bead: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("status=%q assignee=%q, want open/empty", got.Status, got.Assignee)
	}
}

func TestPoolOrphanReleaseGate_DisabledRigHoldsClaim(t *testing.T) {
	store := beads.NewMemStore()
	work := seedGateWork(t, store)

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store, gateTestCity(boolPtr(false)), "", nil,
		[]beads.Bead{work}, nil, []string{"qcore"}, nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none — orphan_release=false must stop the sweeper", released)
	}
	assertStillClaimed(t, store, work.ID)
}

func TestPoolOrphanReleaseGate_ExplicitTrueStillReleases(t *testing.T) {
	store := beads.NewMemStore()
	work := seedGateWork(t, store)

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store, gateTestCity(boolPtr(true)), "", nil,
		[]beads.Bead{work}, nil, []string{"qcore"}, nil,
	)
	if len(released) != 1 {
		t.Fatalf("released = %v, want 1 — orphan_release=true is the default posture", released)
	}
}

// A disabled rig must not quiesce OTHER rigs. This is the blast-radius check:
// the cutover disables one shared rig while the rest of the city keeps working.
func TestPoolOrphanReleaseGate_DisabledRigDoesNotAffectOtherRig(t *testing.T) {
	store := beads.NewMemStore()
	work := seedGateWork(t, store)

	cfg := gateTestCity(boolPtr(false))
	cfg.Rigs = append(cfg.Rigs, config.Rig{Name: "astro", Path: "/tmp/astro"})

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store, cfg, "", nil,
		[]beads.Bead{work}, nil, []string{"astro"}, nil,
	)
	if len(released) != 1 {
		t.Fatalf("released = %v, want 1 — disabling qcore must not stop astro", released)
	}
}

// City-owned work carries an empty store ref and has no rig knob to consult,
// so it keeps releasing even while a rig is disabled.
func TestPoolOrphanReleaseGate_CityStoreWorkUnaffected(t *testing.T) {
	store := beads.NewMemStore()
	work := seedGateWork(t, store)

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store, gateTestCity(boolPtr(false)), "", nil,
		[]beads.Bead{work}, nil, []string{""}, nil,
	)
	if len(released) != 1 {
		t.Fatalf("released = %v, want 1 — city-store work has no rig knob", released)
	}
}

// Without store refs the rig cannot be read off the index, so the gate must
// fall back to the bead and FAIL CLOSED while any rig is disabled. A kill
// switch that releases whenever provenance is murky is not a kill switch.
func TestPoolOrphanReleaseGate_NoStoreRefsFailsClosedWhenAnyRigDisabled(t *testing.T) {
	store := beads.NewMemStore()
	work := seedGateWork(t, store)

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store, gateTestCity(boolPtr(false)), "", nil,
		[]beads.Bead{work}, nil, nil, nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none — unresolvable rig must fail closed while a rig is disabled", released)
	}
	assertStillClaimed(t, store, work.ID)
}

// The same missing-refs path must be bit-for-bit unchanged on the overwhelming
// majority of cities, where nobody sets the knob at all.
func TestPoolOrphanReleaseGate_NoStoreRefsUnchangedWhenNoRigDisabled(t *testing.T) {
	store := beads.NewMemStore()
	work := seedGateWork(t, store)

	released := releaseOrphanedPoolAssignmentsFromBeads(
		store, gateTestCity(nil), "", nil,
		[]beads.Bead{work}, nil, nil, nil,
	)
	if len(released) != 1 {
		t.Fatalf("released = %v, want 1 — no knob set anywhere must behave exactly as before", released)
	}
}

// The hold must be VISIBLE. A silent kill switch reproduces the class of bug
// the sweeper's own foreign-identity gate was written to avoid, and the cutover
// runbook reads this line as evidence the lever took effect.
func TestHeldByOrphanReleaseGate_SummaryNamesRigCountAndIDs(t *testing.T) {
	var held heldByOrphanReleaseGate
	if held.summary() != "" {
		t.Fatalf("summary on an empty gate = %q, want empty", held.summary())
	}
	for _, id := range []string{"qc-1", "qc-2", "qc-3", "qc-4", "qc-5", "qc-6", "qc-7"} {
		held.add("qcore", id)
	}
	held.add("", "qc-orphan")

	summary := held.summary()
	if !strings.Contains(summary, "held 8 claims across 2 rig(s)") {
		t.Fatalf("summary = %q, want an exact claim and rig count", summary)
	}
	if !strings.Contains(summary, `"qcore" (7: qc-1, qc-2, qc-3, qc-4, qc-5 +2 more)`) {
		t.Fatalf("summary = %q, want an exact count with a sampled ID list", summary)
	}
	if !strings.Contains(summary, `"<unresolved>" (1: qc-orphan)`) {
		t.Fatalf("summary = %q, want the unresolved bucket named, not an empty rig", summary)
	}
	if held.claims() != 8 {
		t.Fatalf("claims() = %d, want 8", held.claims())
	}
}

func TestRigOrphanReleaseDisabled_UnknownRigAndNilConfig(t *testing.T) {
	cfg := gateTestCity(boolPtr(false))
	if rigOrphanReleaseDisabled(cfg, "nosuchrig") {
		t.Fatal("an unknown rig must not be treated as disabled")
	}
	if rigOrphanReleaseDisabled(nil, "qcore") {
		t.Fatal("a nil config must not be treated as disabled")
	}
	if !rigOrphanReleaseDisabled(cfg, "QCORE") {
		t.Fatal("rig name match must be case-insensitive, matching the rest of rig resolution")
	}
	if !poolOrphanReleaseAllowed(cfg, "") {
		t.Fatal("empty store ref means the city store and must stay allowed")
	}
}
