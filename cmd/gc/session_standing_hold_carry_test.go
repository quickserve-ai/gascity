package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/liveness"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Carry-only coverage for the standing-hold pick (upstream #6335) where it
// meets behavior upstream does not have: the fresh-wake drain cooldown
// (ga-kaei) and the session-liveness read overlay (ga-lys454).

// carryAlwaysFreshNamedEnv builds a live always-mode, wake_mode=fresh named
// session — the ga-kaei heartbeat shape — and returns the env, its bead and its
// runtime name.
func carryAlwaysFreshNamedEnv(t *testing.T) (*reconcilerTestEnv, beads.Bead, string) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", SleepAfterIdle: config.SessionSleepOff}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "test-cmd",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(%s): %v", sessionName, err)
	}
	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"wake_mode":                  "fresh",
		// Woke well before this tick: past the rapid-crash and churn windows.
		"last_woke_at": env.clk.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
	})
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	return env, session, sessionName
}

// carryDrainAckToTerminal drives an agent drain-ack on a live session through the
// stop-pending finalize, with no assigned work, and returns the settled bead.
func carryDrainAckToTerminal(t *testing.T, env *reconcilerTestEnv, session beads.Bead, sessionName string) beads.Bead {
	t.Helper()
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	dops := newFakeDrainOps()
	if err := dops.setDrainAck(sessionName); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	if woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, dops, nil, nil, env.dt,
		nil, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
	); woken != 0 {
		t.Fatalf("woken on the drain-ack tick = %d, want 0; stderr=%s", woken, env.stderr.String())
	}
	return env.reconcileStopPendingToTerminal(t, env.sp, session, dops, cfgNames)
}

// carryReconcileOnce runs one plain tick (no drain ops, no assigned work) over the
// session's current persisted bead and returns the wake count.
func carryReconcileOnce(t *testing.T, env *reconcilerTestEnv, id string) int {
	t.Helper()
	cur, err := env.store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return reconcileSessionBeads(
		context.Background(), []beads.Bead{cur}, env.desiredState,
		configuredSessionNames(env.cfg, "", env.store), env.cfg, env.sp, env.store,
		nil, nil, nil, env.dt, nil, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
	)
}

// TestReconcileSessionBeads_SuspendHoldOutlivesFreshWakeCooldown pins woodhouse's
// review item 3 on fork PR #59. A suspended always-mode fresh-wake named session
// with no assigned work takes the drain-ack branch, which keeps its user-hold
// intent. The ga-kaei cooldown then wrote held_until = now+5m over the suspend's
// indefinite sentinel. Once that expired nothing blocked the named-always pass,
// which respawned the seat — the operator's suspend ended five minutes later
// with no operator act — and the pick's ClearExpiredHoldPatch released the
// user-hold intent with the timer. The respawn predates the pick (the same test
// fails on the carry without it); the stomp is the defect. The cooldown may
// extend a hold but never shorten one.
func TestReconcileSessionBeads_SuspendHoldOutlivesFreshWakeCooldown(t *testing.T) {
	env, session, sessionName := carryAlwaysFreshNamedEnv(t)
	sentinel := env.clk.Now().Add(indefiniteHoldDuration).UTC().Format(time.RFC3339)
	// Exactly what cmdSessionSuspend writes on the managed path.
	env.setSessionMetadata(&session, map[string]string{
		"held_until":   sentinel,
		"sleep_intent": string(sessionpkg.SleepReasonUserHold),
		"state":        "suspended",
	})

	got := carryDrainAckToTerminal(t, env, session, sessionName)
	if held := got.Metadata["held_until"]; held != sentinel {
		t.Errorf("held_until after drain-ack = %q, want the suspend sentinel %q (the fresh-wake cooldown must not shorten an operator hold)", held, sentinel)
	}
	if intent := got.Metadata["sleep_intent"]; intent != string(sessionpkg.SleepReasonUserHold) {
		t.Fatalf("sleep_intent after drain-ack = %q, want %q", intent, sessionpkg.SleepReasonUserHold)
	}

	// Cross the cooldown window. The seat must still be parked.
	env.clk.Time = env.clk.Now().Add(freshWakeHeartbeatCooldown + time.Minute)
	if woken := carryReconcileOnce(t, env, session.ID); woken != 0 {
		t.Errorf("woken %s after the drain-ack = %d, want 0 (the suspend was released with no operator act); stderr=%s",
			freshWakeHeartbeatCooldown+time.Minute, woken, env.stderr.String())
	}
	if env.sp.IsRunning(sessionName) {
		t.Errorf("suspended seat %q was respawned once the fresh-wake cooldown elapsed", sessionName)
	}
	final := env.sessionInfo(session.ID)
	if final.SleepIntent != string(sessionpkg.SleepReasonUserHold) {
		t.Errorf("sleep_intent after the cooldown window = %q, want %q", final.SleepIntent, sessionpkg.SleepReasonUserHold)
	}
	if final.HeldUntil != sentinel {
		t.Errorf("held_until after the cooldown window = %q, want %q", final.HeldUntil, sentinel)
	}

	// The park still ends on the operator's act: `gc session wake` clears the
	// hold and the named-always pass brings the seat back.
	if _, err := sessionFrontDoor(env.store).WakeSession(session.ID, env.clk.Now().UTC(), sessionpkg.WakeOpts{}); err != nil {
		t.Fatalf("WakeSession: %v", err)
	}
	if woken := carryReconcileOnce(t, env, session.ID); woken != 1 {
		t.Errorf("woken after `gc session wake` = %d, want 1; stderr=%s", woken, env.stderr.String())
	}
}

// TestReconcileSessionBeads_FreshWakeCooldownExtendsButNeverShortensAHold pins
// both arms of the ga-kaei stamp once it respects an existing hold: a live hold
// that ends before the cooldown is extended to it, and one that ends after it —
// here an agent keep-alive with no intent — is left exactly as written.
func TestReconcileSessionBeads_FreshWakeCooldownExtendsButNeverShortensAHold(t *testing.T) {
	for _, tt := range []struct {
		name       string
		holdFor    time.Duration
		wantCooled bool
	}{
		{name: "shorter hold is extended to the cooldown", holdFor: time.Minute, wantCooled: true},
		{name: "longer keep-alive hold is kept", holdFor: 30 * time.Minute, wantCooled: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env, session, sessionName := carryAlwaysFreshNamedEnv(t)
			hold := env.clk.Now().Add(tt.holdFor).UTC().Format(time.RFC3339)
			env.setSessionMetadata(&session, map[string]string{"held_until": hold})
			cooled := env.clk.Now().Add(freshWakeHeartbeatCooldown).UTC().Format(time.RFC3339)

			got := carryDrainAckToTerminal(t, env, session, sessionName)
			want := hold
			if tt.wantCooled {
				want = cooled
			}
			if held := got.Metadata["held_until"]; held != want {
				t.Errorf("held_until after drain-ack = %q, want %q (hold was %q, cooldown ends %q)", held, want, hold, cooled)
			}
		})
	}
}

// unreachableLivenessStore fails every read and write with a connection-class
// error: the liveness pool is gone. The first failure retires the binding's
// pool, so later operations through the same binding take the no-store path —
// both shapes of a degraded overlay are exercised in one tick.
type unreachableLivenessStore struct {
	*liveness.MemStore
	reads int
}

var errLivenessUnreachable = errors.New("invalid connection")

func (s *unreachableLivenessStore) Get(context.Context, string) (liveness.Snapshot, error) {
	s.reads++
	return liveness.Snapshot{}, errLivenessUnreachable
}

func (s *unreachableLivenessStore) GetMany(context.Context, []string) (map[string]liveness.Snapshot, error) {
	s.reads++
	return nil, errLivenessUnreachable
}

func (s *unreachableLivenessStore) SetBatch(context.Context, string, map[string]string) error {
	return errLivenessUnreachable
}

// TestReconcileSessionBeads_DegradedLivenessReadDefersStandingHoldDrainAck runs
// the drain-ack of a suspended seat with assigned work through the fork's
// liveness overlay, with the overlay down for exactly the tick that finalizes
// it — woodhouse's review item 1 on fork PR #59, and the first lifecycle test
// here that does not run on a bare MemStore.
//
// suspend writes an all-liveness batch, so the hold lives only in the liveness
// table. A failed overlay read serves committed metadata, where sleep_intent
// and held_until are absent. Finalizing on that read relabels the parked seat
// idle and writes sleep_intent="" through FallbackPlan; the fence then drops
// the pre-outage user-hold row once the pool recovers, while the unfenced
// held_until row survives, so the crash-recovery override reads an operator
// suspend as an agent keep-alive and respawns the seat. The degraded tick must
// defer the finalize instead, and the recovered tick must finalize with the
// hold intact.
func TestReconcileSessionBeads_DegradedLivenessReadDefersStandingHoldDrainAck(t *testing.T) {
	backing := beads.NewMemStore()
	lv := liveness.NewMemStore()
	lvNow := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	lv.Clock = func() time.Time { return lvNow }
	overlay := func(store liveness.Store) beads.Store {
		return wrapStoreWithBeadPolicies(backing, &config.City{}, newLivenessBindingForTest(store, liveness.ModeTable))
	}

	env := newReconcilerTestEnv()
	env.store = overlay(lv)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	heldUntil := env.clk.Now().Add(indefiniteHoldDuration).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		"held_until":   heldUntil,
		"sleep_intent": string(sessionpkg.SleepReasonUserHold),
		"state":        "suspended",
		"last_woke_at": env.clk.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
	})
	if committed, err := backing.Get(session.ID); err != nil {
		t.Fatalf("backing Get: %v", err)
	} else if committed.Metadata["sleep_intent"] != "" || committed.Metadata["held_until"] != "" {
		t.Fatalf("precondition: the suspend reached versioned metadata (%v); the hold must live only in the liveness table", committed.Metadata)
	}

	task, err := env.store.Create(beads.Bead{Title: "assigned task", Type: "task"})
	if err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	status, assignee := "in_progress", session.ID
	if err := env.store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(task): %v", err)
	}
	if task, err = env.store.Get(task.ID); err != nil {
		t.Fatalf("Get(task): %v", err)
	}

	cfgNames := map[string]bool{"worker": true}
	tick := func(dops drainOps) int {
		cur, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", session.ID, err)
		}
		return reconcileSessionBeads(
			context.Background(), []beads.Bead{cur}, env.desiredState, cfgNames,
			env.cfg, env.sp, env.store, dops, []beads.Bead{task}, nil, env.dt,
			map[string]int{"worker": 1}, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
		)
	}

	// Healthy tick: the ack moves the seat to stop-pending and stops it.
	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	if woken := tick(dops); woken != 0 {
		t.Fatalf("woken on the drain-ack tick = %d, want 0; stderr=%s", woken, env.stderr.String())
	}
	waitForProviderStopped(t, env.sp, "worker")

	// Degraded tick: the finalize would run now, on a fail-open read.
	lvNow = lvNow.Add(time.Minute)
	down := &unreachableLivenessStore{MemStore: lv}
	env.store = overlay(down)
	degradedRead, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("degraded Get: %v", err)
	}
	if degradedRead.Metadata[liveness.ReadDegradedKey] != "true" || degradedRead.Metadata["sleep_intent"] != "" {
		t.Fatalf("precondition: the degraded read = %v, want committed metadata marked degraded with no sleep_intent", degradedRead.Metadata)
	}
	if woken := tick(dops); woken != 0 {
		t.Errorf("woken on the degraded tick = %d, want 0; stderr=%s", woken, env.stderr.String())
	}
	if down.reads == 0 {
		t.Fatalf("the degraded tick never read through the unreachable store; the test proves nothing")
	}

	// Recovery: the same rows, read through a healthy pool.
	lvNow = lvNow.Add(time.Minute)
	env.store = overlay(lv)
	recovered := env.sessionInfo(session.ID)
	if recovered.SleepIntent != string(sessionpkg.SleepReasonUserHold) {
		t.Errorf("sleep_intent after the degraded tick = %q, want %q (a fenced degraded write buried the suspend)",
			recovered.SleepIntent, sessionpkg.SleepReasonUserHold)
	}
	if recovered.SleepReason == string(sessionpkg.SleepReasonIdle) {
		t.Errorf("sleep_reason after the degraded tick = %q: the parked seat was relabeled idle on a fail-open read", recovered.SleepReason)
	}
	if recovered.HeldUntil != heldUntil {
		t.Errorf("held_until after the degraded tick = %q, want %q", recovered.HeldUntil, heldUntil)
	}

	// The recovered tick finalizes on a real read, carrying the hold.
	if woken := tick(dops); woken != 0 {
		t.Errorf("woken on the recovered drain-ack tick = %d, want 0; stderr=%s", woken, env.stderr.String())
	}
	final := env.sessionInfo(session.ID)
	if final.SleepIntent != string(sessionpkg.SleepReasonUserHold) || final.SleepReason != string(sessionpkg.SleepReasonUserHold) {
		t.Errorf("after the recovered finalize: sleep_intent=%q sleep_reason=%q, want both %q (state=%q)",
			final.SleepIntent, final.SleepReason, sessionpkg.SleepReasonUserHold, final.MetadataState)
	}

	// And the following ticks leave the suspended seat down. Two plain ticks:
	// the crash-recovery override is what respawns a buried suspend, and it
	// need not fire on the first tick after the ack is consumed.
	for i := 1; i <= 2; i++ {
		if woken := tick(nil); woken != 0 || env.sp.IsRunning("worker") {
			t.Errorf("plain tick %d after the degraded tick respawned the suspended seat (woken=%d); stderr=%s", i, woken, env.stderr.String())
			break
		}
	}
}
