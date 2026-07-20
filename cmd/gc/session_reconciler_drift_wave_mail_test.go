package main

// Tests for the drift-wave mail notice (ga-9n5hj residual, ruling
// ga-wisp-j23i1gj): when a config-drift wave exceeds the per-tick restart
// budget, the reconciler mails the [session] drift_wave_notify recipient
// once per wave — not once per tick — with a renotify after an hour if the
// same wave is still rolling.

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestConfigDriftWaveNotifierObserveTick(t *testing.T) {
	var n configDriftWaveNotifier
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)

	if !n.observeTick(true, now) {
		t.Fatal("first wave tick must mail")
	}
	if n.observeTick(true, now.Add(time.Minute)) {
		t.Fatal("continuing wave within renotify interval must not mail again")
	}
	if n.observeTick(false, now.Add(2*time.Minute)) {
		t.Fatal("quiet tick must not mail")
	}
	if !n.observeTick(true, now.Add(3*time.Minute)) {
		t.Fatal("a new wave after a quiet tick must mail")
	}
	if !n.observeTick(true, now.Add(3*time.Minute+configDriftWaveRenotifyInterval)) {
		t.Fatal("a wave still rolling past the renotify interval must mail a reminder")
	}
}

func driftWaveMailTestSessions(env *reconcilerTestEnv, count int) []beads.Bead {
	names := make([]string, 0, count)
	agents := make([]config.Agent, 0, count)
	for i := 0; i < count; i++ {
		name := "crew-" + string(rune('a'+i))
		names = append(names, name)
		agents = append(agents, config.Agent{Name: name})
	}
	env.cfg.Agents = agents
	sessions := make([]beads.Bead, 0, count)
	for _, n := range names {
		sessions = append(sessions, staggerTestNamedSession(env, n))
	}
	return sessions
}

func driftWaveMailCount(t *testing.T, env *reconcilerTestEnv, recipient string) int {
	t.Helper()
	// Mail beads are ephemeral — default (TierIssues) queries exclude them.
	mails, err := env.store.List(beads.ListQuery{Assignee: recipient, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List(assignee=%s): %v", recipient, err)
	}
	count := 0
	for _, b := range mails {
		if b.Type == "message" {
			count++
		}
	}
	return count
}

// TestReconcileSessionBeads_ConfigDriftWaveMailsOncePerWave verifies the
// wave notice goes out as ONE mail to the configured recipient when the
// budget is first exceeded, and that the still-rolling wave on the next
// tick does not mail again.
func TestReconcileSessionBeads_ConfigDriftWaveMailsOncePerWave(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Session: config.SessionConfig{DriftWaveNotify: "gastown.mayor"}}
	sessions := driftWaveMailTestSessions(env, 4)

	env.reconcile(sessions)

	if got := driftWaveMailCount(t, env, "gastown.mayor"); got != 1 {
		t.Fatalf("after first wave tick: %d mails, want 1 (stderr=%s)", got, env.stderr.String())
	}

	// Second tick: the wave is still rolling (deferred sessions remain).
	env.clk.Time = env.clk.Time.Add(time.Minute)
	reloaded := make([]beads.Bead, 0, len(sessions))
	for _, s := range sessions {
		got, err := env.store.Get(s.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", s.ID, err)
		}
		if got.Type == sessionBeadType {
			reloaded = append(reloaded, got)
		}
	}
	env.reconcile(reloaded)

	if got := driftWaveMailCount(t, env, "gastown.mayor"); got != 1 {
		t.Fatalf("after second wave tick: %d mails, want still 1 (no per-tick spam)", got)
	}
}

// TestReconcileSessionBeads_ConfigDriftWaveNoRecipientNoMail verifies the
// knob's zero value disables the mail while the wave stagger itself still
// works (event + stderr remain the record).
func TestReconcileSessionBeads_ConfigDriftWaveNoRecipientNoMail(t *testing.T) {
	env := newReconcilerTestEnv()
	sessions := driftWaveMailTestSessions(env, 4)

	env.reconcile(sessions)

	all, err := env.store.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, b := range all {
		if b.Type == "message" {
			t.Fatalf("no recipient configured, but found mail bead %s (%s)", b.ID, b.Title)
		}
	}
}
