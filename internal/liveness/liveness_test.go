package liveness

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestIsKeyCoversTheMovedFieldSet(t *testing.T) {
	moved := []string{
		"state", "awake_started_at", "last_woke_at", "slept_at", "sleep_reason",
		"synced_at", "generation", "held_until", "drain_at", "quarantined_until",
		"churn_count", "continuation_epoch", "continuation_reset_pending",
		"pending_create_claim", "pending_create_started_at", "primed_at",
		"priming_attempted_at", "instance_token", "prior_session_key",
		"creation_complete_at", "detached_at", "usage_compute_emitted_at",
		"gc.last_heartbeat_at",
	}
	for _, k := range moved {
		if !IsKey(k) {
			t.Errorf("IsKey(%q) = false, want true", k)
		}
	}
	if got, want := len(Keys()), len(moved); got != want {
		t.Errorf("len(Keys()) = %d, want %d — the moved set changed without updating this test", got, want)
	}
	// Stable identity/config must stay in versioned metadata.
	for _, k := range []string{
		"agent_name", "alias", "command", "provider", "gc.session_name",
		"gc.work_dir", "state_reason", "session_name", "template",
		"suspended_at", "wait_hold", "sleep_intent",
	} {
		if IsKey(k) {
			t.Errorf("IsKey(%q) = true, want false — versioned metadata must not move", k)
		}
	}
}

func TestSplitPartitionsAndPreservesClears(t *testing.T) {
	live, rest := Split(map[string]string{
		"state":                "asleep",
		"slept_at":             "2026-09-03T00:00:00Z",
		"pending_create_claim": "", // a clear must reach the liveness half, not be dropped
		"state_reason":         "idle timeout",
		"alias":                "katya",
	})
	wantLive := map[string]string{
		"state":                "asleep",
		"slept_at":             "2026-09-03T00:00:00Z",
		"pending_create_claim": "",
	}
	wantRest := map[string]string{
		"state_reason": "idle timeout",
		"alias":        "katya",
	}
	if !reflect.DeepEqual(live, wantLive) {
		t.Errorf("live = %v, want %v", live, wantLive)
	}
	if !reflect.DeepEqual(rest, wantRest) {
		t.Errorf("rest = %v, want %v", rest, wantRest)
	}
}

func TestPlanWriteTableModeLeavesNothingVersionedForAnAllLivenessPatch(t *testing.T) {
	plan := PlanWrite(ModeTable, map[string]string{
		"state":    "active",
		"slept_at": "",
	})
	if len(plan.Versioned) != 0 {
		t.Fatalf("Versioned = %v, want empty — an all-liveness patch must skip the bead write entirely", plan.Versioned)
	}
	if len(plan.Liveness) != 2 {
		t.Fatalf("Liveness = %v, want both keys", plan.Liveness)
	}
}

func TestPlanWriteMetadataModeSendsTheFullPatchVersionedAndMirrors(t *testing.T) {
	patch := map[string]string{"state": "active", "alias": "katya"}
	plan := PlanWrite(ModeMetadata, patch)
	if !reflect.DeepEqual(plan.Versioned, patch) {
		t.Errorf("Versioned = %v, want the full patch %v", plan.Versioned, patch)
	}
	if !reflect.DeepEqual(plan.Liveness, map[string]string{"state": "active"}) {
		t.Errorf("Liveness = %v, want the liveness half mirrored", plan.Liveness)
	}
	// The returned Versioned map must be a copy: mutating it must not corrupt
	// the caller's patch.
	plan.Versioned["state"] = "mutated"
	if patch["state"] != "active" {
		t.Errorf("PlanWrite aliased the caller's patch")
	}
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want Mode
	}{
		{"", ModeTable},
		{"table", ModeTable},
		{"TABLE", ModeTable},
		{"metadata", ModeMetadata},
		{" Metadata ", ModeMetadata},
		{"sidecar", ModeTable}, // an unrecognized value must not silently disable the fix
	} {
		if got := ParseMode(tc.raw); got != tc.want {
			t.Errorf("ParseMode(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestOverlayPrefersLivenessAndFallsBackToCommitted(t *testing.T) {
	written := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	got := Overlay(
		map[string]string{
			"state":    "active",      // shadowed by the liveness row
			"alias":    "katya",       // no liveness row: carried through
			"slept_at": "2026-01-01Z", // stale committed value...
		},
		Snapshot{
			Values:    map[string]string{"state": "asleep", "slept_at": ""},
			WrittenAt: written,
		},
	)
	if got["state"] != "asleep" {
		t.Errorf("state = %q, want the liveness value", got["state"])
	}
	if got["alias"] != "katya" {
		t.Errorf("alias = %q, want the committed value carried through", got["alias"])
	}
	// ...and a CLEAR must project as empty, not fall back to the stale committed
	// value. This is why a clear is a tombstone row and never a DELETE.
	if got["slept_at"] != "" {
		t.Errorf("slept_at = %q, want the clear to win over the stale committed value", got["slept_at"])
	}
	if got[WrittenAtKey] != written.Format(time.RFC3339Nano) {
		t.Errorf("%s = %q, want %q", WrittenAtKey, got[WrittenAtKey], written.Format(time.RFC3339Nano))
	}
}

func TestOverlayWithNoRowsIsIdentity(t *testing.T) {
	meta := map[string]string{"state": "active"}
	got := Overlay(meta, Snapshot{})
	if len(got) != 1 || got["state"] != "active" {
		t.Fatalf("Overlay = %v, want the committed metadata unchanged", got)
	}
	if _, stamped := got[WrittenAtKey]; stamped {
		t.Errorf("Overlay stamped %s with no liveness rows", WrittenAtKey)
	}
}

func TestSetBatchRejectsTheDerivedClockKey(t *testing.T) {
	m := NewMemStore()
	err := m.SetBatch(context.Background(), "gc-1", map[string]string{WrittenAtKey: "now"})
	if err == nil {
		t.Fatalf("SetBatch(%s) = nil, want an error: the clock is derived, never written", WrittenAtKey)
	}
}

func TestMemStoreRoundTripAndTombstone(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	if err := m.SetBatch(ctx, "gc-1", map[string]string{"state": "active", "generation": "7"}); err != nil {
		t.Fatalf("SetBatch: %v", err)
	}
	if err := m.SetBatch(ctx, "gc-1", map[string]string{"state": ""}); err != nil {
		t.Fatalf("SetBatch clear: %v", err)
	}
	snap, err := m.Get(ctx, "gc-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v, ok := snap.Values["state"]; !ok || v != "" {
		t.Errorf("state = (%q,%v), want a present empty tombstone", v, ok)
	}
	if snap.Values["generation"] != "7" {
		t.Errorf("generation = %q, want 7 — an unrelated key must survive a clear", snap.Values["generation"])
	}
	if snap.WrittenAt.IsZero() {
		t.Errorf("WrittenAt is zero, want the write clock")
	}
	many, err := m.GetMany(ctx, []string{"gc-1", "gc-absent", "gc-1"})
	if err != nil {
		t.Fatalf("GetMany: %v", err)
	}
	if len(many) != 1 {
		t.Errorf("GetMany returned %d snapshots, want 1 (absent beads are omitted, duplicates collapsed)", len(many))
	}
}

// TestMemStoreConcurrentDisjointKeysLoseNothing is the in-process half of the
// concurrency contract: two writers hammering DIFFERENT keys on the same bead
// must both survive. The cross-CONNECTION half — which is the property the
// retired JSON-sidecar design could not provide — is
// TestSQLStoreConcurrentWritersAcrossSeparateConnections.
func TestMemStoreConcurrentDisjointKeysLoseNothing(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := []string{"state", "generation"}[i]
			for n := 0; n < 50; n++ {
				if err := m.SetBatch(ctx, "gc-1", map[string]string{key: "v"}); err != nil {
					t.Errorf("SetBatch: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	snap, err := m.Get(ctx, "gc-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if snap.Values["state"] != "v" || snap.Values["generation"] != "v" {
		t.Fatalf("Values = %v, want both writers' keys present", snap.Values)
	}
}
