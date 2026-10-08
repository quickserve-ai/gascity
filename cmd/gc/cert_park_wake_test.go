package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// ga-mzovhi: a bead parked for certification (hold:cert-wait, ga-5zosxs) stays
// in_progress under its owner. The assigned-work wake arm treated it as live
// demand, so a pool seat that drained because it had nothing actionable was
// respawned on the next tick — measured every 1-2 minutes on qc-btft3q. These
// tests pin both halves of the fix: a parked assignment does not wake its
// sleeping owner, and the patrol's durable flip (cert:landed / cert:action)
// wakes it again WITHOUT depending on the nudge that follows the flip.

// certParkGetCountingStore counts live Gets and can fail them, so a test can
// prove which beads the probe read and what a failed read does.
type certParkGetCountingStore struct {
	beads.Store
	gets atomic.Int32
	fail bool
	// stall, when non-nil, blocks every Get until it is closed: a store that
	// has stopped answering.
	stall chan struct{}
}

func (s *certParkGetCountingStore) Get(id string) (beads.Bead, error) {
	s.gets.Add(1)
	if s.stall != nil {
		<-s.stall
	}
	if s.fail {
		return beads.Bead{}, errors.New("bd show: connection refused")
	}
	return s.Store.Get(id)
}

// waitCertParkReadSlotsFree waits for abandoned reads to return their slots so
// one test's stalled reads cannot starve the next test's.
func waitCertParkReadSlotsFree(t *testing.T) {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for len(certParkReadSlots) > 0 {
		select {
		case <-timeout.C:
			t.Fatalf("cert park read slots still held: %d", len(certParkReadSlots))
		case <-tick.C:
		}
	}
}

// certParkSkipGate opens gate the first time the probe logs a skipped read.
// With every Get blocked on gate, that moment is one where each launched read
// still holds its slot, so a slot freed by a finished read cannot hand the
// owner past the cap a read it should not get. Without the gate a fast store
// frees slots mid-loop and the cap reads as cap+1 (ga-isk41m).
type certParkSkipGate struct {
	once sync.Once
	gate chan struct{}
}

func (g *certParkSkipGate) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "skipped") {
		g.open()
	}
	return len(p), nil
}

func (g *certParkSkipGate) open() { g.once.Do(func() { close(g.gate) }) }

// certParkMarkInProgress moves a freshly created bead to in_progress (stores
// create beads open) and returns the stored row.
func certParkMarkInProgress(t *testing.T, store beads.Store, work beads.Bead) beads.Bead {
	t.Helper()
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update(work in_progress): %v", err)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work): %v", err)
	}
	if got.Status != "in_progress" {
		t.Fatalf("work status = %q, want in_progress", got.Status)
	}
	return got
}

// certParkAwakeFixture builds the controller's view of one pool seat holding
// one in_progress bead whose LIVE labels are liveLabels. The row handed to the
// awake bridge deliberately carries NO labels: the controller's cached
// in_progress rows come from a reconcile scan that passes `bd list
// --skip-labels`, so a fix that trusted the cached row's labels would be blind
// in production.
func certParkAwakeFixture(t *testing.T, liveLabels []string, running bool) (AwakeInput, []int, certParkedWorkProbe, *certParkGetCountingStore) {
	t.Helper()
	store := &certParkGetCountingStore{Store: beads.NewMemStore()}
	work, err := store.Create(beads.Bead{
		Title:    "PR parked for certification",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "mc-p1",
		Labels:   liveLabels,
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	work = certParkMarkInProgress(t, store, work)
	store.gets.Store(0)
	cachedRow := work
	cachedRow.Labels = nil
	input := AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "mc-p1", SessionName: "polecat-mc-p1", Template: "hello-world/polecat", State: "asleep"},
		},
		WorkBeads:        []AwakeWorkBead{{ID: work.ID, Assignee: "mc-p1", Status: "in_progress"}},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		RunningSessions:  map[string]bool{},
		Now:              now,
	}
	if running {
		input.SessionBeads[0].State = "active"
		input.RunningSessions["polecat-mc-p1"] = true
	}
	probe := newLiveCertParkedWorkProbe([]beads.Bead{cachedRow}, []beads.Store{store}, io.Discard)
	if probe == nil {
		t.Fatal("newLiveCertParkedWorkProbe returned nil for an aligned row/store pair")
	}
	return input, []int{0}, probe, store
}

func TestCertParkedInProgressWorkDoesNotWakeSleepingOwner(t *testing.T) {
	input, sources, probe, store := certParkAwakeFixture(t, []string{"hold:cert-wait", "ready:needs-grooming"}, false)

	result := computeAwakeSetWithCertParks(&input, sources, probe)

	assertAsleep(t, result, "polecat-mc-p1")
	d := result["polecat-mc-p1"]
	if d.HasAssignedWork {
		t.Error("HasAssignedWork = true, want false: a parked assignment is not wake demand")
	}
	if !d.HasCertParkedWork {
		t.Error("HasCertParkedWork = false, want true: the reconciler keys the stranded-repair guard on it")
	}
	if got := store.gets.Load(); got != 1 {
		t.Errorf("live label reads = %d, want 1", got)
	}
}

func TestCertParkResumeFlipWakesOwner(t *testing.T) {
	for name, labels := range map[string][]string{
		"patrol flipped to cert:landed":            {"cert:landed", "ready:needs-grooming"},
		"patrol flipped to cert:action":            {"cert:action"},
		"resume label added, hold not yet removed": {"hold:cert-wait", "cert:landed"},
	} {
		t.Run(name, func(t *testing.T) {
			input, sources, probe, _ := certParkAwakeFixture(t, labels, false)

			result := computeAwakeSetWithCertParks(&input, sources, probe)

			assertAwake(t, result, "polecat-mc-p1")
			assertReason(t, result, "polecat-mc-p1", "assigned-work")
			if result["polecat-mc-p1"].HasCertParkedWork {
				t.Error("HasCertParkedWork = true after a resume flip, want false")
			}
		})
	}
}

func TestCertParkUnparkedInProgressWorkStillWakes(t *testing.T) {
	input, sources, probe, _ := certParkAwakeFixture(t, []string{"ready:ready"}, false)

	result := computeAwakeSetWithCertParks(&input, sources, probe)

	assertAwake(t, result, "polecat-mc-p1")
	assertReason(t, result, "polecat-mc-p1", "assigned-work")
}

// A live seat decides for itself what to do with a parked bead (it drain-acks),
// so the controller spends no label read on it and leaves its decision alone.
func TestCertParkLiveOwnerIsNotProbed(t *testing.T) {
	input, sources, probe, store := certParkAwakeFixture(t, []string{"hold:cert-wait"}, true)

	result := computeAwakeSetWithCertParks(&input, sources, probe)

	assertAwake(t, result, "polecat-mc-p1")
	if got := store.gets.Load(); got != 0 {
		t.Fatalf("live label reads = %d, want 0 for a running owner", got)
	}
}

// A label read that fails is evidence of nothing, so it must fail toward the
// pre-fix behavior (wake), never toward a stall.
func TestCertParkUnreadableLabelsFailTowardWaking(t *testing.T) {
	input, sources, probe, store := certParkAwakeFixture(t, []string{"hold:cert-wait"}, false)
	store.fail = true

	result := computeAwakeSetWithCertParks(&input, sources, probe)

	assertAwake(t, result, "polecat-mc-p1")
	assertReason(t, result, "polecat-mc-p1", "assigned-work")
}

// certParkPoolOwnerEnv is the production shape of ga-mzovhi through the real
// reconcile call site: a pool seat that drain-acked (asleep, sleep_reason=idle)
// while it still owns one in_progress bead.
func certParkPoolOwnerEnv(t *testing.T, labels []string) (*reconcilerTestEnv, beads.Bead, beads.Bead, *capturingRecorder) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "asleep",
		"sleep_reason":         "idle",
		poolManagedMetadataKey: boolMetadata(true),
	})
	work, err := env.store.Create(beads.Bead{
		Title:    "PR parked for certification",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
		Labels:   labels,
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	work = certParkMarkInProgress(t, env.store, work)
	rec := &capturingRecorder{}
	env.rec = rec
	return env, session, work, rec
}

func runCertParkReconcileTick(t *testing.T, env *reconcilerTestEnv, session, work beads.Bead) int {
	t.Helper()
	// The cached in_progress row the controller hands the reconciler carries no
	// labels (the reconcile scan skips them); the park must be read live.
	row := work
	row.Labels = nil
	opts := append([]startExecutionOption{withAssignedWorkStores([]beads.Store{env.store})}, env.startOptions...)
	return reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, nil, env.cfg, env.sp,
		env.store, nil, []beads.Bead{row}, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		opts...,
	)
}

// TestReconcileSessionBeads_CertParkedPoolOwnerStaysAsleepAndKeepsItsBead is the
// end-to-end guard. Suppressing the wake alone is not enough: an asleep, idle
// pool seat with no wake reason and assigned work is exactly what the stranded
// repair reaps two minutes later — it would unassign the parked bead and close
// the seat, so the patrol's flip would find no owner to wake. The wake arm was
// the only thing keeping that repair from firing.
func TestReconcileSessionBeads_CertParkedPoolOwnerStaysAsleepAndKeepsItsBead(t *testing.T) {
	env, session, work, rec := certParkPoolOwnerEnv(t, []string{beadmeta.CertWaitHoldLabel})

	if woken := runCertParkReconcileTick(t, env, session, work); woken != 0 {
		t.Fatalf("tick 1 woken = %d, want 0; stderr=%s", woken, env.stderr.String())
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("tick 1 started the parked seat; a hold:cert-wait assignment must not wake its owner")
	}

	env.clk.Time = env.clk.Time.Add(strandedRepairConfirmGrace + time.Minute)
	updated, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if woken := runCertParkReconcileTick(t, env, updated, work); woken != 0 {
		t.Fatalf("tick 2 woken = %d, want 0; stderr=%s", woken, env.stderr.String())
	}

	if got := len(rec.strandedEvents()); got != 0 {
		t.Errorf("session.stranded events = %d, want 0: a parked owner is not a stranded worker", got)
	}
	gotWork, err := env.store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work): %v", err)
	}
	if gotWork.Status != "in_progress" || gotWork.Assignee != session.ID {
		t.Fatalf("parked work = status %q assignee %q, want in_progress assigned to %s (ownership must survive the park)", gotWork.Status, gotWork.Assignee, session.ID)
	}
	gotSession, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if gotSession.Status == "closed" {
		t.Fatal("parked owner's session bead was closed; the patrol's flip would have no seat to wake")
	}
}

// TestReconcileSessionBeads_CertParkFlipWakesOwnerWithoutNudge is the wake half:
// the patrol writes the flip durably and nudges second, and a nudge can be lost
// to a restart. The flip alone must wake the seat on the next tick.
func TestReconcileSessionBeads_CertParkFlipWakesOwnerWithoutNudge(t *testing.T) {
	for name, labels := range map[string][]string{
		"cert:landed":  {beadmeta.CertLandedLabel},
		"cert:action":  {beadmeta.CertActionLabel},
		"never parked": nil,
	} {
		t.Run(name, func(t *testing.T) {
			env, session, work, _ := certParkPoolOwnerEnv(t, labels)

			if woken := runCertParkReconcileTick(t, env, session, work); woken != 1 {
				t.Fatalf("woken = %d, want 1; stderr=%s", woken, env.stderr.String())
			}
			if !env.sp.IsRunning("worker") {
				t.Fatal("owner of an unparked in_progress bead was not started")
			}
		})
	}
}

// TestCertParkStalledReadWakesWithinBudget: the live read is a `bd show` with a
// long subprocess timeout, run inside the reconcile tick. A store that stops
// answering must cost the tick at most the read budget, and the unanswered
// read must fail toward waking the owner.
func TestCertParkStalledReadWakesWithinBudget(t *testing.T) {
	input, sources, _, store := certParkAwakeFixture(t, []string{"hold:cert-wait"}, false)
	store.stall = make(chan struct{})
	defer waitCertParkReadSlotsFree(t)
	defer close(store.stall)
	const budget = 100 * time.Millisecond
	cached := []beads.Bead{{ID: input.WorkBeads[0].ID, Assignee: "mc-p1", Status: "in_progress"}}
	probe := newCertParkedWorkProbeWithBudget(cached, []beads.Store{store}, io.Discard, budget)

	done := make(chan map[string]AwakeDecision, 1)
	started := time.Now()
	go func() { done <- computeAwakeSetWithCertParks(&input, sources, probe) }()
	var result map[string]AwakeDecision
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("computeAwakeSetWithCertParks blocked on a stalled label read past 5s; the tick must not wait beyond the read budget")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("tick waited %s on a stalled read, want about the %s budget", elapsed, budget)
	}
	assertAwake(t, result, "polecat-mc-p1")
	assertReason(t, result, "polecat-mc-p1", "assigned-work")
}

// TestCertParkReadsAreCapped: at most cap(certParkReadSlots) live reads run per
// tick; an owner beyond the cap is not read and wakes as before.
func TestCertParkReadsAreCapped(t *testing.T) {
	waitCertParkReadSlotsFree(t)
	limit := cap(certParkReadSlots)
	owners := limit + 1
	store := &certParkGetCountingStore{Store: beads.NewMemStore()}
	input := AwakeInput{
		Agents:           []AwakeAgent{{QualifiedName: "hello-world/polecat"}},
		ScaleCheckCounts: map[string]int{"hello-world/polecat": 0},
		RunningSessions:  map[string]bool{},
		Now:              now,
	}
	var rows []beads.Bead
	var stores []beads.Store
	var sources []int
	for n := 1; n <= owners; n++ {
		sessionID := fmt.Sprintf("mc-p%d", n)
		work, err := store.Create(beads.Bead{Title: "parked", Type: "task", Assignee: sessionID, Labels: []string{"hold:cert-wait"}})
		if err != nil {
			t.Fatalf("Create(work): %v", err)
		}
		work = certParkMarkInProgress(t, store, work)
		input.SessionBeads = append(input.SessionBeads, AwakeSessionBead{
			ID: sessionID, SessionName: "polecat-" + sessionID, Template: "hello-world/polecat", State: "asleep",
		})
		input.WorkBeads = append(input.WorkBeads, AwakeWorkBead{ID: work.ID, Assignee: sessionID, Status: "in_progress"})
		rows = append(rows, beads.Bead{ID: work.ID, Assignee: sessionID, Status: "in_progress"})
		stores = append(stores, store)
		sources = append(sources, n-1)
	}
	store.gets.Store(0)
	skip := &certParkSkipGate{gate: make(chan struct{})}
	store.stall = skip.gate
	// A regression that gives the owner past the cap a read never logs a skip;
	// open the gate anyway so the reads return and the count fails the test.
	t.Cleanup(skip.open)

	result := computeAwakeSetWithCertParks(&input, sources, newLiveCertParkedWorkProbe(rows, stores, skip))
	skip.open()

	if got := int(store.gets.Load()); got != limit {
		t.Fatalf("live label reads = %d, want the cap %d", got, limit)
	}
	awake := 0
	for _, d := range result {
		if d.ShouldWake {
			awake++
		}
	}
	if awake != owners-limit {
		t.Fatalf("awake owners = %d, want %d (every owner beyond the read cap wakes)", awake, owners-limit)
	}
	waitCertParkReadSlotsFree(t)
}

// TestReconcileSessionBeads_CertParkLifecycle drives one parked pool seat
// through the whole park: parked across three stranded-repair confirmation
// windows, then un-parked by a label change alone. The seat must never be
// started or reaped while parked, and must be started on the tick after the
// hold goes away — whether the patrol flipped it or a human removed it.
func TestReconcileSessionBeads_CertParkLifecycle(t *testing.T) {
	cases := map[string]beads.UpdateOpts{
		"patrol flip to cert:landed": {RemoveLabels: []string{beadmeta.CertWaitHoldLabel}, Labels: []string{beadmeta.CertLandedLabel}},
		"patrol flip to cert:action": {RemoveLabels: []string{beadmeta.CertWaitHoldLabel}, Labels: []string{beadmeta.CertActionLabel}},
		"manual un-park":             {RemoveLabels: []string{beadmeta.CertWaitHoldLabel}},
	}
	for name, unpark := range cases {
		t.Run(name, func(t *testing.T) {
			env, session, work, rec := certParkPoolOwnerEnv(t, []string{beadmeta.CertWaitHoldLabel, "ready:needs-grooming"})
			parkedAt := env.clk.Time

			for tick := 0; tick < 4; tick++ {
				current, err := env.store.Get(session.ID)
				if err != nil {
					t.Fatalf("tick %d Get(session): %v", tick, err)
				}
				if woken := runCertParkReconcileTick(t, env, current, work); woken != 0 {
					t.Fatalf("parked tick %d woken = %d, want 0; stderr=%s", tick, woken, env.stderr.String())
				}
				if env.sp.IsRunning("worker") {
					t.Fatalf("parked tick %d started the seat", tick)
				}
				env.clk.Time = env.clk.Time.Add(strandedRepairConfirmGrace)
			}
			if parked, window := env.clk.Time.Sub(parkedAt), 3*strandedRepairConfirmGrace; parked <= window {
				t.Fatalf("seat parked for %s, want more than 3x the stranded confirm window (%s)", parked, window)
			}

			if err := env.store.Update(work.ID, unpark); err != nil {
				t.Fatalf("un-park labels: %v", err)
			}
			current, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("Get(session): %v", err)
			}
			if woken := runCertParkReconcileTick(t, env, current, work); woken != 1 {
				t.Fatalf("tick after un-park woken = %d, want 1; stderr=%s", woken, env.stderr.String())
			}
			if !env.sp.IsRunning("worker") {
				t.Fatal("seat was not started after its bead was un-parked")
			}

			if got := len(rec.strandedEvents()); got != 0 {
				t.Errorf("session.stranded events = %d, want 0 across the whole park", got)
			}
			gotSession, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("Get(session): %v", err)
			}
			if gotSession.Status == "closed" {
				t.Error("session bead closed during the park")
			}
			gotWork, err := env.store.Get(work.ID)
			if err != nil {
				t.Fatalf("Get(work): %v", err)
			}
			if gotWork.Status != "in_progress" || gotWork.Assignee != session.ID {
				t.Errorf("work = status %q assignee %q, want in_progress assigned to %s", gotWork.Status, gotWork.Assignee, session.ID)
			}
		})
	}
}
