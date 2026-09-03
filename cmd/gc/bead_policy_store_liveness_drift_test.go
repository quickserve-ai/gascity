package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/liveness"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// The tests in this file cover the SECOND key-sweep round (ga-lys454): the
// config-drift deferral stamps, measured on the live hq store 2026-09-03
// 21:40-22:40Z. In that hour 146 of 156 hq commits touched a session bead, and
// gastown.mayor alone committed 27 times — 23 of those changing
// attached_config_drift_deferred_at and nothing else, on a session doing no
// work, which is exactly what this bead's idle-steady-state acceptance forbids.
//
// The round's two other measured classes (invocation_usage_cursor, the
// session_circuit_* cluster) are deliberately NOT moved; the reasoning lives in
// the LEFT VERSIONED note in internal/liveness/keys.go and is pinned by
// TestCircuitAndUsageCursorStayVersioned.
//
// Like the first round's tests these drive the REAL write sites, and the
// assertion that matters is len(batches)+len(singles)+len(updates) == 0: a write
// the backing bead store never sees is a Dolt commit that never happens.

// TestAttachedConfigDriftDeferralWritesNoVersionedMetadata pins the largest
// measured class of the round. recordSessionAttachedConfigDriftDeferral runs on
// every reconciler tick for every attached session with unapplied config drift;
// its own 2-minute throttle still left one commit per session per 2 min, which
// is ~30/hr for a session that is doing nothing at all.
func TestAttachedConfigDriftDeferralWritesNoVersionedMetadata(t *testing.T) {
	store, backing, lv := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, map[string]string{"alias": "katya"})
	backing.batches, backing.singles, backing.updates = nil, nil, nil

	clk := &clock.Fake{Time: time.Date(2026, 9, 3, 22, 41, 0, 0, time.UTC)}
	const driftKey = "v5:3b75c42980c76676"
	info := sessionpkg.Info{ID: bead.ID}
	if err := recordSessionAttachedConfigDriftDeferral(info, sessionFrontDoor(store), clk, driftKey); err != nil {
		t.Fatalf("recordSessionAttachedConfigDriftDeferral: %v", err)
	}

	if n := len(backing.batches) + len(backing.singles) + len(backing.updates); n != 0 {
		t.Fatalf("attached config-drift deferral made %d versioned bead writes, want 0 (batches=%v singles=%v)",
			n, backing.batches, backing.singles)
	}

	snap, err := lv.Get(context.Background(), bead.ID)
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	if got := snap.Values["attached_config_drift_deferred_key"]; got != driftKey {
		t.Fatalf("liveness attached_config_drift_deferred_key = %q, want %q", got, driftKey)
	}

	// The throttle is only correct if the reconciler reads its own stamp back:
	// the deferral it just recorded must suppress the next tick's write, and it
	// can only do that through the overlay now that the row is off the bead.
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	back := sessiontest.InfoFromMeta(t, got.Metadata)
	if !recentlyDeferredSessionAttachedConfigDrift(back, clk, driftKey) {
		t.Fatalf("deferral did not read back through the overlay: at=%q key=%q",
			back.AttachedConfigDriftDeferredAt, back.AttachedConfigDriftDeferredKey)
	}
	clk.Advance(time.Minute)
	backing.batches, backing.singles, backing.updates = nil, nil, nil
	if err := recordSessionAttachedConfigDriftDeferral(back, sessionFrontDoor(store), clk, driftKey); err != nil {
		t.Fatalf("second deferral: %v", err)
	}
	if n := len(backing.batches) + len(backing.singles) + len(backing.updates); n != 0 {
		t.Fatalf("throttled re-deferral made %d versioned bead writes, want 0", n)
	}
}

// TestNamedConfigDriftDeferralWritesNoVersionedMetadata covers the non-attached
// half of the same pair; it shares the batch, so leaving it versioned would keep
// the commit its sibling avoids.
func TestNamedConfigDriftDeferralWritesNoVersionedMetadata(t *testing.T) {
	store, backing, lv := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, map[string]string{"alias": "katya"})
	backing.batches, backing.singles, backing.updates = nil, nil, nil

	at := time.Date(2026, 9, 3, 22, 41, 0, 0, time.UTC)
	const driftKey = "v5:a14ee547b6661135"
	info := sessionpkg.Info{ID: bead.ID}
	if err := recordNamedSessionConfigDriftDeferredAt(info, sessionFrontDoor(store), at, driftKey); err != nil {
		t.Fatalf("recordNamedSessionConfigDriftDeferredAt: %v", err)
	}
	if n := len(backing.batches) + len(backing.singles) + len(backing.updates); n != 0 {
		t.Fatalf("named config-drift deferral made %d versioned bead writes, want 0 (batches=%v)", n, backing.batches)
	}
	snap, err := lv.Get(context.Background(), bead.ID)
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	if got := snap.Values["config_drift_deferred_key"]; got != driftKey {
		t.Fatalf("liveness config_drift_deferred_key = %q, want %q", got, driftKey)
	}
}

// TestConfigDriftDeferralClusterIsCompleteInTheMovedSet does the same for the
// four drift-deferral keys, which are written as two pairs and cleared as one
// batch of four.
func TestConfigDriftDeferralClusterIsCompleteInTheMovedSet(t *testing.T) {
	for _, k := range []string{
		namedSessionConfigDriftDeferredAtMetadata,
		namedSessionConfigDriftDeferredKeyMetadata,
		sessionAttachedConfigDriftDeferredAtMetadata,
		sessionAttachedConfigDriftDeferredKeyMetadata,
	} {
		if !liveness.IsKey(k) {
			t.Errorf("config-drift deferral key %q is not in the liveness moved set", k)
		}
	}
}
