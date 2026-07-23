package main

// Tests for the 2026-07-18 town-crash policy fixes:
//   - ga-9n5hj: prompt-only (FPExtra) config drift applies lazily — never
//     restarts or drains a live session; genuine drift restarts are bounded
//     per tick (stagger) with a wave event as advance notice.
//   - ga-2aq43: a continuation-reset marker whose startup ack never arrived
//     is cleared once the runtime is demonstrably healthy, instead of staying
//     armed and hard-resetting the session hours later.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestConfigDriftLazyApplicable(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		want   bool
	}{
		{"empty is not classifiable", nil, false},
		{"fpextra only is lazy", []string{"FPExtra"}, true},
		{"command is eager", []string{"Command"}, false},
		{"fpextra plus env is eager", []string{"Env", "FPExtra"}, false},
		{"env only is eager", []string{"Env"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := configDriftLazyApplicable(tc.fields); got != tc.want {
				t.Fatalf("configDriftLazyApplicable(%v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}

// resetMarkerTestSetup creates a session bead carrying a committed
// continuation-reset marker aged `age` before the env clock's now.
func resetMarkerTestSetup(env *reconcilerTestEnv, age time.Duration) beads.Bead {
	session := env.createSessionBead("worker", "worker")
	committedAt := env.clk.Now().UTC().Add(-age)
	env.setSessionMetadata(&session, map[string]string{
		"continuation_reset_pending":   "true",
		sessionpkg.ResetCommittedAtKey: committedAt.UTC().Format(time.RFC3339),
	})
	return session
}

func TestClearStaleResetMarkerIfHealthy_ClearsForAttachedSession(t *testing.T) {
	env := newReconcilerTestEnv()
	session := resetMarkerTestSetup(env, time.Hour)
	env.sp.SetAttached("worker", true)

	clearStaleResetMarkerIfHealthy(seedSessionInfo(session), env.store, env.sp, "worker", "worker",
		true, time.Minute, env.clk.Now().UTC(), env.dt, env.rec, &env.stderr, nil)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want cleared (stderr=%s)",
			got.Metadata["continuation_reset_pending"], env.stderr.String())
	}
	if got.Metadata[sessionpkg.ResetCommittedAtKey] != "" {
		t.Fatalf("reset_committed_at = %q, want cleared", got.Metadata[sessionpkg.ResetCommittedAtKey])
	}
	if !strings.Contains(env.stderr.String(), "cleared stale reset marker") {
		t.Fatalf("expected stderr note about the clear, got: %s", env.stderr.String())
	}
}

func TestClearStaleResetMarkerIfHealthy_ClearsForActivityAfterReset(t *testing.T) {
	env := newReconcilerTestEnv()
	session := resetMarkerTestSetup(env, time.Hour)
	// Runtime not attached, but provider observed activity well after the
	// reset commit + startup window — the conversation running now began
	// after the reset, so the reset evidently happened.
	env.sp.SetActivity("worker", env.clk.Now().UTC().Add(-5*time.Minute))

	clearStaleResetMarkerIfHealthy(seedSessionInfo(session), env.store, env.sp, "worker", "worker",
		true, time.Minute, env.clk.Now().UTC(), env.dt, env.rec, &env.stderr, nil)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want cleared (stderr=%s)",
			got.Metadata["continuation_reset_pending"], env.stderr.String())
	}
}

func TestClearStaleResetMarkerIfHealthy_KeepsMarker(t *testing.T) {
	cases := []struct {
		name  string
		age   time.Duration
		alive bool
		setup func(env *reconcilerTestEnv)
	}{
		{
			// Within 2x startupTimeout (floored at 2m) the tracked start
			// path still owns the marker.
			name:  "fresh marker",
			age:   30 * time.Second,
			alive: true,
			setup: func(env *reconcilerTestEnv) { env.sp.SetAttached("worker", true) },
		},
		{
			// A dead runtime is recordResetStallIfDue's territory, never
			// the healthy-clear's.
			name:  "dead runtime",
			age:   time.Hour,
			alive: false,
			setup: func(env *reconcilerTestEnv) { env.sp.SetAttached("worker", true) },
		},
		{
			name:  "no evidence",
			age:   time.Hour,
			alive: true,
			setup: func(_ *reconcilerTestEnv) {},
		},
		{
			// Activity from before the reset commit proves nothing about
			// the reset having happened — could be the stale runtime the
			// reset wants replaced.
			name:  "activity predates reset",
			age:   time.Hour,
			alive: true,
			setup: func(env *reconcilerTestEnv) {
				env.sp.SetActivity("worker", env.clk.Now().UTC().Add(-2*time.Hour))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			session := resetMarkerTestSetup(env, tc.age)
			tc.setup(env)

			clearStaleResetMarkerIfHealthy(seedSessionInfo(session), env.store, env.sp, "worker", "worker",
				tc.alive, time.Minute, env.clk.Now().UTC(), env.dt, env.rec, &env.stderr, nil)

			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Metadata["continuation_reset_pending"] != "true" {
				t.Fatalf("continuation_reset_pending = %q, want kept (stderr=%s)",
					got.Metadata["continuation_reset_pending"], env.stderr.String())
			}
		})
	}
}

// TestReconcileSessionBeads_ConfigDriftFPExtraOnlyIsLazy verifies that drift
// confined to FPExtra (prompt fragments) neither drains nor restarts a live
// session. Regression for the 2026-07-18 crash: a city-wide append_fragments
// change drifted every agent's FPExtra, the guards held for ~2.5h, and on
// guard expiry the reconciler rolled the entire roster mid-work (ga-9n5hj).
func TestReconcileSessionBeads_ConfigDriftFPExtraOnlyIsLazy(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Desired/current config has NO FingerprintExtra; the session started
	// with one — the only drifted field is FPExtra.
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	startedCfg := runtime.Config{Command: "test-cmd", FingerprintExtra: map[string]string{"fragment": "old-prompt"}}
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(startedCfg))
	if err != nil {
		t.Fatalf("marshal breakdown: %v", err)
	}
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(startedCfg),
		"core_hash_breakdown": string(breakdown),
	})

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("FPExtra-only drift must not drain, got drain=%+v stderr=%s", ds, env.stderr.String())
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// State healing may rename active → awake for an alive session; either
	// way it must not be reset toward a restart (start-pending/creating).
	if state := got.Metadata["state"]; state != "active" && state != "awake" {
		t.Fatalf("state = %q, want active/awake (session must keep running)", state)
	}
	if got.Metadata["continuation_reset_pending"] == "true" {
		t.Fatal("lazy drift must not commit a reset")
	}
	if strings.Contains(env.stderr.String(), "config-drift worker") {
		t.Fatalf("lazy drift must not log the eager drift diagnostic, stderr=%s", env.stderr.String())
	}
}

// TestReconcileSessionBeads_ConfigDriftCommandStaysEager guards the lazy
// check's scope: drift that includes a non-lazy field (Command) keeps the
// existing drain behavior even when FPExtra also drifted.
func TestReconcileSessionBeads_ConfigDriftCommandStaysEager(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig() // current Command: new-cmd
	session := env.createSessionBead("worker", "worker")
	startedCfg := runtime.Config{Command: "test-cmd", FingerprintExtra: map[string]string{"fragment": "old-prompt"}}
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(startedCfg))
	if err != nil {
		t.Fatalf("marshal breakdown: %v", err)
	}
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(startedCfg),
		"core_hash_breakdown": string(breakdown),
	})

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("Command drift must still drain (stderr=%s)", env.stderr.String())
	}
	if ds.reason != "config-drift" {
		t.Fatalf("drain reason = %q, want config-drift", ds.reason)
	}
}

// staggerTestNamedSession builds an alive named session whose stored config
// drifted in Command (eager) and whose deferral guards are already exhausted:
// not attached, activity old enough to be outside the recent-activity window.
func staggerTestNamedSession(env *reconcilerTestEnv, name string) beads.Bead {
	tp := TemplateParams{
		Command:      "new-cmd",
		SessionName:  name,
		TemplateName: name,
	}
	env.desiredState[name] = tp
	_ = env.sp.Start(context.Background(), name, runtime.Config{Command: "new-cmd"})
	env.sp.SetActivity(name, env.clk.Now().UTC().Add(-time.Hour))

	session := env.createSessionBead(name, name)
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash":                   runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
		sessionpkg.NamedSessionMetadataKey:      "true",
		sessionpkg.NamedSessionIdentityMetadata: name,
	})
	return session
}

// TestReconcileSessionBeads_ConfigDriftRestartsStaggered verifies the
// per-tick drift-restart budget: with more drifted named sessions than the
// budget allows, one tick restarts at most maxConfigDriftRestartsPerTick and
// defers the rest to later ticks. The 2026-07-18 crash rolled ~17 sessions
// in one sweep, which also cycled the tmux server (ga-9n5hj).
func TestReconcileSessionBeads_ConfigDriftRestartsStaggered(t *testing.T) {
	env := newReconcilerTestEnv()
	names := []string{"crew-a", "crew-b", "crew-c", "crew-d"}
	agents := make([]config.Agent, 0, len(names))
	for _, n := range names {
		agents = append(agents, config.Agent{Name: n})
	}
	env.cfg = &config.City{Agents: agents}
	sessions := make([]beads.Bead, 0, len(names))
	for _, n := range names {
		sessions = append(sessions, staggerTestNamedSession(env, n))
	}

	env.reconcile(sessions)

	// A restarted session commits a NEW started_config_hash (the same tick's
	// start machinery completes the fresh spawn and rewrites it); a deferred
	// session still carries the old stored hash.
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})
	countRestarted := func() int {
		restarted := 0
		for _, s := range sessions {
			got, err := env.store.Get(s.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", s.ID, err)
			}
			if got.Metadata["started_config_hash"] != oldHash {
				restarted++
			}
		}
		return restarted
	}
	if restarted := countRestarted(); restarted != maxConfigDriftRestartsPerTick {
		t.Fatalf("restarted %d sessions in one tick, want exactly %d (stderr=%s)",
			restarted, maxConfigDriftRestartsPerTick, env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "config-drift wave") {
		t.Fatalf("expected wave announcement once budget exceeded, stderr=%s", env.stderr.String())
	}

	// Subsequent ticks pick up the deferred sessions: reload beads and
	// reconcile again — the remaining drifted sessions restart within the
	// next tick's budget.
	env.clk.Time = env.clk.Time.Add(time.Minute)
	reloaded := make([]beads.Bead, 0, len(sessions))
	for _, s := range sessions {
		got, err := env.store.Get(s.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", s.ID, err)
		}
		reloaded = append(reloaded, got)
	}
	env.reconcile(reloaded)

	if restarted := countRestarted(); restarted != len(names) {
		t.Fatalf("after second tick %d/%d sessions restarted, want all (stderr=%s)",
			restarted, len(names), env.stderr.String())
	}
}
