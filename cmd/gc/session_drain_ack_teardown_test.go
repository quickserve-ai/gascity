package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// drainAckTeardownFixture is the ga-x99xh0 replay shape: a max-2 pool whose two
// seats each hold one routed in_progress bead.
type drainAckTeardownFixture struct {
	now      time.Time
	cityDir  string
	cfg      *config.City
	store    beads.Store
	sp       *runtime.Fake
	rec      *events.Fake
	dops     *fakeDrainOps
	dt       *drainTracker
	clk      *clock.Fake
	sessions []beads.Bead
	work     []beads.Bead
}

const drainAckTeardownTemplate = "repo/worker"

func newDrainAckTeardownFixture(t *testing.T) *drainAckTeardownFixture {
	t.Helper()
	now := time.Date(2026, 9, 25, 23, 19, 33, 0, time.UTC)
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "worker")
	f := &drainAckTeardownFixture{
		now:     now,
		cityDir: cityDir,
		cfg: &config.City{
			Workspace: config.Workspace{Name: "trace-town"},
			Session:   config.SessionConfig{Provider: "fake"},
			Agents: []config.Agent{{
				Name:              "worker",
				Dir:               "repo",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(2),
			}},
		},
		store: beads.NewMemStore(),
		sp:    runtime.NewFake(),
		rec:   events.NewFake(),
		dops:  newFakeDrainOps(),
		dt:    newDrainTracker(),
		clk:   &clock.Fake{Time: now},
	}
	inProgress := "in_progress"
	for slot := 1; slot <= 2; slot++ {
		work := createRoutedReadyBeadForReplacement(t, f.store, drainAckTeardownTemplate, "platform leg")
		seat := createCanonicalPoolSession(t, f.store, &f.cfg.Agents[0], now, slot)
		setPoolSessionActive(t, f.store, seat.ID)
		seat = mustGetBead(t, f.store, seat.ID)
		name := seat.Metadata["session_name"]
		if err := f.sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("start seat %s runtime: %v", name, err)
		}
		assignee := seat.ID
		if err := f.store.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
			t.Fatalf("assign %s to %s: %v", work.ID, seat.ID, err)
		}
		f.sessions = append(f.sessions, seat)
		f.work = append(f.work, mustGetBead(t, f.store, work.ID))
	}
	return f
}

// tick runs one reconcile pass over the fixture's current session beads.
func (f *drainAckTeardownFixture) tick(t *testing.T, dops drainOps) {
	t.Helper()
	ds := buildDesiredState("trace-town", f.cityDir, f.clk.Now(), f.cfg, f.sp, f.store, io.Discard)
	current := make([]beads.Bead, 0, len(f.sessions))
	for _, s := range f.sessions {
		current = append(current, mustGetBead(t, f.store, s.ID))
	}
	reconcileSessionBeads(
		context.Background(), current, ds.State, map[string]bool{drainAckTeardownTemplate: true},
		f.cfg, f.sp, f.store, dops, nil, nil, f.dt, ds.PoolDesiredCounts, false, nil, "trace-town",
		nil, f.clk, f.rec, 0, 0, io.Discard, io.Discard,
	)
}

// openSeatCount is the template's open_count as the tick-summary trace reads it:
// open session beads whose normalized template is the pool's.
func (f *drainAckTeardownFixture) openSeatCount(t *testing.T) int {
	t.Helper()
	snap, err := loadSessionBeadSnapshot(f.store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}
	n := 0
	for _, info := range snap.OpenInfos() {
		if normalizedSessionTemplateInfo(info, f.cfg) == drainAckTeardownTemplate {
			n++
		}
	}
	return n
}

// TestDrainAckWithAssignedWork_PoolSeatsTornDownAndReplaced replays the Friday
// 2026-09-25 11:19 PM sequence behind ga-x99xh0: a max-2 pool, both seats
// assigned an in_progress bead, both told to drain (an account move) and both
// acknowledging while still assigned. Before the fix each seat was put to sleep
// holding its bead, the assigned-work wake resumed it on the old account, and
// the pool read open_count 2 / desired 2 for 178 ticks while serving nothing.
//
// The contract now: the ack tears the seat down AND releases its bead in the
// same act — assignee cleared, status back to open, gc.routed_to kept — so the
// next tick reads open_count 0 and desires two fresh seats for the two beads.
// The drain_acked_with_assigned_work event still fires: it is the observation,
// recorded before the release acts on it.
func TestDrainAckWithAssignedWork_PoolSeatsTornDownAndReplaced(t *testing.T) {
	f := newDrainAckTeardownFixture(t)
	if got := f.openSeatCount(t); got != 2 {
		t.Fatalf("precondition: open_count = %d, want 2", got)
	}
	for _, s := range f.sessions {
		if err := f.dops.setDrainAck(s.Metadata["session_name"]); err != nil {
			t.Fatalf("setDrainAck(%s): %v", s.Metadata["session_name"], err)
		}
	}

	// Tick 1: live runtimes + agent drain-acks -> stop-pending, async stop queued.
	f.tick(t, f.dops)
	for _, s := range f.sessions {
		waitForProviderStopped(t, f.sp, s.Metadata["session_name"])
	}
	// Tick 2: runtimes gone -> finalize the acks.
	f.tick(t, f.dops)

	acked := 0
	for _, ev := range f.rec.Events {
		if ev.Type == events.SessionDrainAckedWithAssignedWork {
			acked++
		}
	}
	if acked != 2 {
		t.Fatalf("%s events = %d, want 2 (the observation must still be recorded)", events.SessionDrainAckedWithAssignedWork, acked)
	}
	for _, s := range f.sessions {
		got := mustGetBead(t, f.store, s.ID)
		if got.Status != "closed" {
			t.Errorf("seat %s: status=%q state=%q sleep_reason=%q, want closed — a seat that acked a drain while assigned must be torn down, not retained",
				s.ID, got.Status, got.Metadata["state"], got.Metadata["sleep_reason"])
		}
	}
	for _, w := range f.work {
		got := mustGetBead(t, f.store, w.ID)
		if got.Assignee != "" {
			t.Errorf("work %s assignee = %q, want cleared so the pool can re-draw it", w.ID, got.Assignee)
		}
		if got.Status != "open" {
			t.Errorf("work %s status = %q, want open", w.ID, got.Status)
		}
		if route := got.Metadata[beadmeta.RoutedToMetadataKey]; route != drainAckTeardownTemplate {
			t.Errorf("work %s gc.routed_to = %q, want %q kept", w.ID, route, drainAckTeardownTemplate)
		}
	}

	// The next tick: nothing is retained, and the two released beads are demand
	// for two fresh seats.
	if got := f.openSeatCount(t); got != 0 {
		t.Fatalf("open_count after the drain acks = %d, want 0", got)
	}
	ds := buildDesiredState("trace-town", f.cityDir, f.clk.Now(), f.cfg, f.sp, f.store, io.Discard)
	desired := 0
	for _, tp := range ds.State {
		if tp.TemplateName == drainAckTeardownTemplate {
			desired++
		}
	}
	if desired != 2 {
		t.Fatalf("desired %s seats after the drain acks = %d, want 2 (a replacement per released bead)", drainAckTeardownTemplate, desired)
	}
}

// TestDrainAckTeardown_WorkingSeatStillCounted is the control: a seat that is
// genuinely working (runtime alive, bead in_progress, no drain) is capacity. A
// reconcile tick must leave it open, keep its assignment, and keep counting it.
func TestDrainAckTeardown_WorkingSeatStillCounted(t *testing.T) {
	f := newDrainAckTeardownFixture(t)
	f.tick(t, newFakeDrainOps())
	f.tick(t, newFakeDrainOps())

	for i, s := range f.sessions {
		got := mustGetBead(t, f.store, s.ID)
		if got.Status == "closed" {
			t.Errorf("working seat %s was closed; close_reason=%q", s.ID, got.Metadata["close_reason"])
		}
		w := mustGetBead(t, f.store, f.work[i].ID)
		if w.Assignee != s.ID || w.Status != "in_progress" {
			t.Errorf("working seat's bead %s: assignee=%q status=%q, want %q/in_progress", w.ID, w.Assignee, w.Status, s.ID)
		}
	}
	if got := f.openSeatCount(t); got != 2 {
		t.Fatalf("open_count with two working seats = %d, want 2", got)
	}
	for _, ev := range f.rec.Events {
		if ev.Type == events.SessionDrainAckedWithAssignedWork {
			t.Fatalf("unexpected %s for a seat that never drained", ev.Type)
		}
	}
}

// TestDrainAckTeardown_CertParkedWorkIsRetained pins the one assigned-work shape
// the teardown must not release: a bead parked on a certification wait
// (ga-mzovhi). Its owner is asleep by design and the cert-landing-patrol wakes
// it by the assignment, so the seat keeps the bead and stays open.
func TestDrainAckTeardown_CertParkedWorkIsRetained(t *testing.T) {
	f := newDrainAckTeardownFixture(t)
	f.sessions = f.sessions[:1]
	parked := f.work[0]
	if err := f.store.Update(parked.ID, beads.UpdateOpts{Labels: []string{beadmeta.CertWaitHoldLabel}}); err != nil {
		t.Fatalf("park %s: %v", parked.ID, err)
	}
	name := f.sessions[0].Metadata["session_name"]
	if err := f.dops.setDrainAck(name); err != nil {
		t.Fatalf("setDrainAck(%s): %v", name, err)
	}
	f.tick(t, f.dops)
	waitForProviderStopped(t, f.sp, name)
	f.tick(t, f.dops)

	got := mustGetBead(t, f.store, f.sessions[0].ID)
	if got.Status == "closed" {
		t.Fatalf("seat holding cert-parked work was torn down; close_reason=%q", got.Metadata["close_reason"])
	}
	w := mustGetBead(t, f.store, parked.ID)
	if w.Assignee != f.sessions[0].ID {
		t.Fatalf("cert-parked bead assignee = %q, want %q (the park's owner must be kept)", w.Assignee, f.sessions[0].ID)
	}
}
