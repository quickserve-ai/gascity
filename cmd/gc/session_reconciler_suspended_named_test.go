package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ga-9qanni: an agent patched suspended=true kept running its on-demand named
// session. #6307 deliberately keeps a suspended agent's named-session BEAD
// (identity survives for resume) and relies on ComputeAwakeSet to decide it
// must not run; two wake causes skipped the suspended check: pending-create
// (a nudge-materialized bead was launched) and on-demand:running (a running
// one was kept awake forever). A suspended agent's named session must be
// drained and never started; the not-suspended controls pin both wakes.

func suspendedNamedTestCity(suspended bool) *config.City {
	return &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "refinery", StartCommand: "true", MaxActiveSessions: intPtr(1), Suspended: suspended}},
		NamedSessions: []config.NamedSession{{Template: "refinery", Mode: "on_demand"}},
	}
}

func TestReconcileSessionBeads_SuspendedAgentsRunningNamedSessionIsDrained(t *testing.T) {
	for _, suspended := range []bool{true, false} {
		env := newReconcilerTestEnv()
		env.cfg = suspendedNamedTestCity(suspended)
		name := "refinery"
		_ = env.sp.Start(context.Background(), name, runtime.Config{Command: "true"})
		sess := env.createSessionBead(name, "refinery")
		env.setSessionMetadata(&sess, map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "refinery",
			namedSessionModeMetadata:     "on_demand",
			"state":                      "active",
			"last_woke_at":               env.clk.Now().UTC().Format(time.RFC3339),
		})
		for tick := 0; tick < namedSuspendConfirmTicks+1; tick++ {
			env.reconcile([]beads.Bead{sess})
		}
		ds := env.dt.get(sess.ID)
		if suspended {
			// The bead is preserved (#6307), so the !desired "suspended" close
			// is never reached; the awake-set drain carries the "suspended"
			// reason instead, which neither work nor a wake reason can cancel.
			if ds == nil {
				t.Fatal("suspended agent's running on-demand named session was kept awake, want it drained")
			}
			if ds.reason != "suspended" {
				t.Fatalf("drain reason = %q, want \"suspended\" (not cancelable by the seat's own work)", ds.reason)
			}
		} else if ds != nil {
			t.Fatalf("control: not-suspended named session was drained (reason %q)", ds.reason)
		}
	}
}

func TestReconcileSessionBeads_SuspendedAgentsMaterializedNamedSessionIsNotStarted(t *testing.T) {
	for _, suspended := range []bool{true, false} {
		env := newReconcilerTestEnv()
		env.cfg = suspendedNamedTestCity(suspended)
		name := "refinery"
		sess := env.createSessionBead(name, "refinery")
		env.setSessionMetadata(&sess, map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "refinery",
			namedSessionModeMetadata:     "on_demand",
			"session_origin":             "named",
			"state":                      "start-pending",
			"pending_create_claim":       "true",
			"pending_create_started_at":  env.clk.Now().UTC().Format(time.RFC3339),
		})
		for tick := 0; tick < namedSuspendConfirmTicks+1; tick++ {
			env.reconcile([]beads.Bead{sess})
		}
		if running := env.sp.IsRunning(name); running == suspended {
			t.Fatalf("materialized named session running = %v with agent suspended=%v", running, suspended)
		}
	}
}

// The live refinery held its own in-progress patrol step. As a
// "no-wake-reason" drain its ack was canceled by that assigned work every
// other tick, so it was never stopped. With the real drain-ack path (non-nil
// drain ops), a suspended seat holding work must stop within a few ticks and
// keep its bead (#6307); the not-suspended control keeps running.
func TestReconcileSessionBeads_SuspendedAgentHoldingWorkIsStopped(t *testing.T) {
	for _, suspended := range []bool{true, false} {
		env := newReconcilerTestEnv()
		env.cfg = suspendedNamedTestCity(suspended)
		name := "refinery"
		if err := env.sp.Start(context.Background(), name, runtime.Config{Command: "true"}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		sess := env.createSessionBead(name, "refinery")
		env.setSessionMetadata(&sess, map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "refinery",
			namedSessionModeMetadata:     "on_demand",
			"state":                      "active",
			"last_woke_at":               env.clk.Now().UTC().Format(time.RFC3339),
		})
		if _, err := env.store.Create(beads.Bead{
			Title:    "patrol step",
			Type:     "task",
			Status:   "in_progress",
			Assignee: sess.ID,
		}); err != nil {
			t.Fatalf("Create(work): %v", err)
		}
		dops := newDrainOps(env.sp)
		tick := func() {
			got, err := env.store.Get(sess.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{got}, nil, dops)
			env.clk.Advance(30 * time.Second)
		}
		for i := 0; i < 6; i++ {
			tick()
		}
		if !suspended {
			if !env.sp.IsRunning(name) {
				t.Fatal("control: not-suspended seat holding work was stopped")
			}
			continue
		}
		waitForProviderStopped(t, env.sp, name)
		// Past the stop, finalize runs on the following ticks; it is the only
		// step that could close the bead, so assert #6307 after it.
		for i := 0; i < 3; i++ {
			tick()
		}
		if env.sp.IsRunning(name) {
			t.Fatal("suspended seat restarted after it was stopped")
		}
		final, err := env.store.Get(sess.ID)
		if err != nil {
			t.Fatalf("Get final: %v", err)
		}
		if final.Status == "closed" {
			t.Fatal("session bead closed after finalize, want it kept for resume (#6307)")
		}
		if st := final.Metadata["state"]; st != "asleep" && st != "drained" {
			t.Fatalf("session state after finalize = %q, want asleep or drained", st)
		}
	}
}
