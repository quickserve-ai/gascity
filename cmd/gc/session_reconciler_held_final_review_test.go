package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Regression tests for the final strict review of gastownhall/gascity#6178
// (the Cherub-town gc owner's probes, 2026-09-09). The probe shapes are his;
// the assertions pin the fixed behavior.

// An operator's explicit `gc runtime drain` (GC_DRAIN) issued while the
// reconciler's own orphan drain is acked must survive the heartbeat hold's
// cancel: the hold withdraws only what the reconciler owns — its tracker
// entry and its own ack keys — and GC_DRAIN is written by the explicit path
// alone. Tracked and recovered forms of the reconciler's ack.
func TestReconcileSessionBeads_HeartbeatHoldCancelKeepsExplicitDrain(t *testing.T) {
	for _, tracked := range []bool{true, false} {
		name := "recovered"
		if tracked {
			name = "tracked"
		}
		t.Run(name, func(t *testing.T) {
			env, seat, _ := newUndesiredPoolSeat(t, 45*time.Minute)
			ds := &drainState{startedAt: env.clk.Now(), deadline: env.clk.Now().Add(defaultDrainTimeout), reason: "orphaned", generation: 1, ackSet: true}
			if tracked {
				env.dt.set(seat.ID, ds)
			}
			if err := setReconcilerDrainAckMetadata(env.sp, "worker", ds); err != nil {
				t.Fatal(err)
			}
			dops := newDrainOps(env.sp)
			if code := doRuntimeDrain(dops, env.sp, env.rec, "worker", "worker", false, &env.stdout, &env.stderr); code != 0 {
				t.Fatalf("explicit drain: exit %d, stderr=%s", code, env.stderr.String())
			}
			if draining, err := dops.isDraining("worker"); err != nil || !draining {
				t.Fatalf("explicit drain not visible before the tick: draining=%v err=%v", draining, err)
			}
			got, err := env.store.Get(seat.ID)
			if err != nil {
				t.Fatal(err)
			}
			env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{got}, nil, dops)
			if !strings.Contains(env.stdout.String(), "Canceled drain-acked session 'worker' (heartbeat hold)") {
				t.Fatalf("hold did not cancel the reconciler's own ack: stdout=%s stderr=%s", env.stdout.String(), env.stderr.String())
			}
			if draining, err := dops.isDraining("worker"); err != nil || !draining {
				t.Fatalf("explicit drain withdrawn by the hold cancel: draining=%v err=%v running=%v stdout=%s", draining, err, env.sp.IsRunning("worker"), env.stdout.String())
			}
			if acked, _ := dops.isDrainAcked("worker"); acked {
				t.Fatal("reconciler's own ack survived its cancel")
			}
			if !env.sp.IsRunning("worker") {
				t.Fatal("seat stopped inside the cancel")
			}
		})
	}
}

// The producer-side case (a concrete SessionBeadID request keeps a held seat
// reusable) targets upstream's request-aware reusablePoolSessionInfosForRequest,
// which this branch does not carry; carry's reusablePoolSessionInfos has no
// concrete-request path, so that test stays upstream-only.

// A held pool seat that is desired again must not be queued to stop on the
// reconciler's own orphan or no-wake-reason ack: the desired ack arm applies
// the hold lens the not-desired arm has, cancels the reconciler's ack, and
// leaves the seat running. Both cancelable reasons, tracked and recovered.
func TestReconcileSessionBeads_DesiredHeldSeatReconcilerAckIsCanceledNotStopped(t *testing.T) {
	for _, reason := range []string{"orphaned", "no-wake-reason"} {
		for _, tracked := range []bool{true, false} {
			shape := "recovered"
			if tracked {
				shape = "tracked"
			}
			t.Run(reason+"/"+shape, func(t *testing.T) {
				env, seat, _ := newUndesiredPoolSeat(t, 45*time.Minute)
				env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
				env.addDesired("worker", "worker", false)
				ds := &drainState{startedAt: env.clk.Now(), deadline: env.clk.Now().Add(defaultDrainTimeout), reason: reason, generation: 1, ackSet: true}
				if tracked {
					env.dt.set(seat.ID, ds)
				}
				if err := setReconcilerDrainAckMetadata(env.sp, "worker", ds); err != nil {
					t.Fatal(err)
				}
				got, err := env.store.Get(seat.ID)
				if err != nil {
					t.Fatal(err)
				}
				dops := newDrainOps(env.sp)
				env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{got}, map[string]int{"worker": 1}, dops)
				got, err = env.store.Get(seat.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Metadata["state_reason"] == "drain-ack-stop-pending" || !env.sp.IsRunning("worker") {
					t.Fatalf("held desired seat stopped or queued to stop: state=%q reason=%q running=%v stdout=%s stderr=%s", got.Metadata["state"], got.Metadata["state_reason"], env.sp.IsRunning("worker"), env.stdout.String(), env.stderr.String())
				}
				if !strings.Contains(env.stdout.String(), "Canceled drain-acked session 'worker' (heartbeat hold)") {
					t.Fatalf("desired arm did not cancel the reconciler's ack for the hold: stdout=%s", env.stdout.String())
				}
				if acked, _ := dops.isDrainAcked("worker"); acked {
					t.Fatal("reconciler's own ack survived the cancel")
				}
				if tracked && env.dt.get(seat.ID) != nil {
					t.Fatal("tracker entry survived the cancel")
				}
			})
		}
	}
}

// An agent ack that lands inside the hold's cancel keeps its provenance —
// the own-only clear re-asserts source=agent from the ack key it crossed —
// and a seat whose agent then exits before the next desired-state
// observation is finalized, not left open as a legacy ack. The
// source-restored subtest is the control the fix must match.
func TestReconcileSessionBeads_AgentAckInsideHoldCancelKeepsProvenanceAndFinalizes(t *testing.T) {
	for _, restore := range []bool{true, false} {
		name := "source-crossed-by-cancel"
		if restore {
			name = "source-restored-control"
		}
		t.Run(name, func(t *testing.T) {
			env, seat, _ := newUndesiredPoolSeat(t, 45*time.Minute)
			ds := &drainState{startedAt: env.clk.Now(), deadline: env.clk.Now().Add(defaultDrainTimeout), reason: "orphaned", generation: 1, ackSet: true}
			env.dt.set(seat.ID, ds)
			if err := setReconcilerDrainAckMetadata(env.sp, "worker", ds); err != nil {
				t.Fatal(err)
			}
			provider := &ackAtFirstRemovalProvider{Provider: env.sp}
			got, err := env.store.Get(seat.ID)
			if err != nil {
				t.Fatal(err)
			}
			reconcileSessionBeadsAtPath(context.Background(), "", []beads.Bead{got}, nil, nil, env.cfg, provider, env.store,
				newDrainOps(provider), nil, nil, nil, env.dt, map[string]int{}, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr)
			if !provider.fired || provider.ackErr != nil {
				t.Fatalf("agent ack did not complete inside the cancel: fired=%v err=%v", provider.fired, provider.ackErr)
			}
			if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "1" {
				t.Fatalf("agent ack did not survive the cancel: %q", ack)
			}
			if source, _ := env.sp.GetMeta("worker", reconcilerDrainAckSourceKey); source != drainAckSourceAgentValue {
				t.Fatalf("agent ack provenance crossed by the cancel and not restored: %s=%q", reconcilerDrainAckSourceKey, source)
			}
			if restore {
				if err := env.sp.SetMeta("worker", reconcilerDrainAckSourceKey, drainAckSourceAgentValue); err != nil {
					t.Fatal(err)
				}
			}
			if err := env.sp.Stop("worker"); err != nil {
				t.Fatal(err)
			}
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", false)
			got, err = env.store.Get(seat.ID)
			if err != nil {
				t.Fatal(err)
			}
			env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{got}, map[string]int{"worker": 1}, newDrainOps(env.sp))
			got, err = env.store.Get(seat.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "closed" {
				t.Fatalf("completed agent ack not finalized: status=%q state=%q reason=%q running=%v stdout=%s stderr=%s", got.Status, got.Metadata["state"], got.Metadata["state_reason"], env.sp.IsRunning("worker"), env.stdout.String(), env.stderr.String())
			}
		})
	}
}
