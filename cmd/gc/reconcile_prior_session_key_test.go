package main

import (
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// The carry's conversation history (ga-oe8h) keeps a pointer to every
// conversation a lifecycle transition resets away: whatever rotates or
// clears session_key stamps the old key into prior_session_key. Legacy's
// resets stamp it (recordWakeFailure, recordChurn, the drain-ack finalize);
// these pin that v2's do too, from the row as it stood before the reset.

// priorKeyRow is an automatic conversation of a provider no probe can find
// a transcript for, so a crash resets it (no resume backstop involved).
func priorKeyRow(lastWoke time.Duration, now time.Time) map[string]string {
	return map[string]string{
		"session_name": "worker", "template": "worker", "session_origin": "ephemeral", "provider": "codex", "work_dir": "/work",
		"session_key": "k-1", "started_config_hash": "h-1", "wake_attempts": "0",
		"last_woke_at": now.Add(-lastWoke).Format(time.RFC3339),
	}
}

// Kills a v2 failed start (a rapid crash) or churn that resets the
// conversation without stamping the key it reset away, where legacy's
// checkStability stamps it.
func TestV2ExitResetStampsPriorSessionKey(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name     string
		lastWoke time.Duration
	}{
		{"failed start (rapid crash)", 10 * time.Second},
		{"churn", 2 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			meta := priorKeyRow(c.lastWoke, now)
			store := newTestStore()
			legacyRow := makeBead("b1", maps.Clone(meta))
			store.persistRow(legacyRow)
			clk := &clock.Fake{Time: now}
			if info, _ := checkStability(seedSessionInfo(legacyRow), nil, false, newDrainTracker(), sessionFrontDoor(store), clk, nil, nil, events.NewFake()); c.lastWoke >= stabilityThreshold {
				checkChurn(info, nil, false, newDrainTracker(), sessionFrontDoor(store), clk)
			}
			syncBeadFromStore(&legacyRow, store)
			if legacyRow.Metadata["session_key"] != "" || legacyRow.Metadata[session.PriorSessionKeyMetadata] != "k-1" {
				t.Fatalf("legacy: row %v, want the key reset away and stamped as prior", legacyRow.Metadata)
			}

			row := sessionRow("gc-1", append([]string{"state", "awake", "session_name", "s-gc-1"}, flatten(meta)...)...)
			patch, _, accrued, _ := deadRuntimePatch(&rowFacts{w: &World{Now: now}, row: censusRowOf(t, row), entry: &selectionEntry{Liveness: livenessGone, Desired: desireSleep}}, false)
			if !accrued {
				t.Fatalf("v2: no accrual for %s", c.name)
			}
			if got := patch.Apply(maps.Clone(row.Metadata)); got["session_key"] != "" || got[session.PriorSessionKeyMetadata] != "k-1" {
				t.Fatalf("v2: patch %v, want the key reset away and stamped as prior, as legacy's", patch)
			}
		})
	}
}

// Kills a v2 restart handoff that rotates a live row's key without stamping
// the one it replaces, where legacy's drain-ack finalize stamps it.
func TestSimRestartHandoffStampsPriorSessionKey(t *testing.T) {
	s := converged(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1", "started_config_hash", "h-1", "last_woke_at", rowAt(-time.Hour)), gc1Work())
	s.ask(restartAsked...)
	s.settle()
	m := s.gc1()
	if m[session.ResetCommittedAtKey] == "" || m["session_key"] == "k-1" {
		t.Fatalf("row %v, want the restart handed off with its key rotated or cleared", m)
	}
	if m[session.PriorSessionKeyMetadata] != "k-1" {
		t.Fatalf("prior_session_key = %q, want the rotated-away key k-1 (row %v)", m[session.PriorSessionKeyMetadata], m)
	}
	s.noViolations(t)
}
