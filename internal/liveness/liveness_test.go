package liveness

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIsKeyCoversTheMovedFieldSet(t *testing.T) {
	moved := []string{
		"state", "state_reason", "awake_started_at", "last_woke_at", "slept_at",
		"sleep_reason", "sleep_intent",
		"synced_at", "generation", "held_until", "drain_at", "quarantined_until",
		"quarantine_cycle", "churn_count", "wake_attempts", "wait_hold",
		"wake_request", "wake_requested_at",
		"continuation_epoch", "continuation_reset_pending",
		"pending_create_claim", "pending_create_started_at", "primed_at",
		"priming_attempted_at", "instance_token", "prior_session_key",
		"creation_complete_at", "detached_at",
		"currently_processing_bead_id",
		"usage_compute_emitted_at", "usage_model_swept_at",
		"last_nudge_delivered_at",
		"idle_claim_nudge_trigger", "idle_claim_nudge_count", "idle_claim_nudge_at",
		"continuation_claim_nudge_work", "continuation_claim_nudge_root",
		"continuation_claim_nudge_store_ref", "continuation_claim_nudge_generation",
		"continuation_claim_nudge_count", "continuation_claim_nudge_at",
		"stranded_event_emitted_at",
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
		"gc.work_dir", "session_name", "template",
		"suspended_at", "session_key", "started_config_hash", "started_live_hash",
		"prompt_hash", "resume_seeded", "continuity_eligible", "archived_at",
		"close_reason", "closed_at", "pin_awake",
	} {
		if IsKey(k) {
			t.Errorf("IsKey(%q) = true, want false — versioned metadata must not move", k)
		}
	}
}

// TestTriggerBeadIDStaysVersioned pins the one measured churn driver the sweep
// deliberately did NOT move. gc.trigger_bead_id is the pool slot's binding to
// its dispatched work, not telemetry, and it is written as one member of the
// trigger/provenance cluster that session.Store.UpdateMetadataInfo commits in a
// single backend operation — splitting it out would send the trigger id to the
// table and the store ref / pack / work dir through Update, which is exactly the
// partial provenance row that contract forbids. See the LEFT VERSIONED note in
// keys.go.
func TestTriggerBeadIDStaysVersioned(t *testing.T) {
	for _, k := range []string{
		"gc.trigger_bead_id", "gc.trigger_bead_store_ref", "gc.pack",
		"gc.pack_workspace", "gc.brain_parent_sid",
	} {
		if IsKey(k) {
			t.Errorf("IsKey(%q) = true; the trigger/provenance cluster must commit atomically in versioned metadata", k)
		}
	}
}

func TestEveryMovedKeyFitsTheColumn(t *testing.T) {
	for _, k := range Keys() {
		if len(k) > maxKeyLen {
			t.Errorf("key %q is %d bytes; the k column holds %d", k, len(k), maxKeyLen)
		}
	}
	long := strings.Repeat("x", maxKeyLen+1)
	if err := NewMemStore().SetBatch(context.Background(), "gc-1", map[string]string{long: "v"}); err == nil {
		t.Errorf("SetBatch accepted a %d-byte key; the server would truncate it into a collision", len(long))
	}
}

func TestSplitPartitionsAndPreservesClears(t *testing.T) {
	live, rest := Split(map[string]string{
		"state":                "asleep",
		"state_reason":         "idle timeout",
		"slept_at":             "2026-09-03T00:00:00Z",
		"pending_create_claim": "", // a clear must reach the liveness half, not be dropped
		"session_key":          "conv-1",
		"alias":                "katya",
	})
	wantLive := map[string]string{
		"state":                "asleep",
		"state_reason":         "idle timeout",
		"slept_at":             "2026-09-03T00:00:00Z",
		"pending_create_claim": "",
	}
	wantRest := map[string]string{
		"session_key": "conv-1",
		"alias":       "katya",
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
	}, time.Now())
	if len(plan.Versioned) != 0 {
		t.Fatalf("Versioned = %v, want empty — an all-liveness patch must skip the bead write entirely", plan.Versioned)
	}
	if len(plan.Liveness) != 2 {
		t.Fatalf("Liveness = %v, want both keys", plan.Liveness)
	}
}

func TestPlanWriteMetadataModeSendsTheFullPatchVersionedAndMirrors(t *testing.T) {
	patch := map[string]string{"state": "active", "alias": "katya"}
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	plan := PlanWrite(ModeMetadata, patch, now)
	for k, v := range patch {
		if plan.Versioned[k] != v {
			t.Errorf("Versioned[%q] = %q, want %q — the full patch must go versioned", k, plan.Versioned[k], v)
		}
	}
	// The fence is what makes "versioned is authoritative" true in a mode the
	// overlay cannot see, and it is stamped PER liveness key.
	if got := plan.Versioned[FenceKeyFor("state")]; got != FenceStamp(now) {
		t.Errorf("Versioned[%s] = %q, want the fence stamp %q", FenceKeyFor("state"), got, FenceStamp(now))
	}
	if _, stamped := plan.Versioned[FenceKeyFor("alias")]; stamped {
		t.Errorf("Versioned fenced the non-liveness key alias; only liveness keys have table rows to fence")
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

func TestSetBatchRejectsEveryOverlayMarker(t *testing.T) {
	m := NewMemStore()
	for _, k := range []string{WrittenAtKey, FenceKeyFor("state"), FencePrefix + "anything"} {
		if err := m.SetBatch(context.Background(), "gc-1", map[string]string{k: "now"}); err == nil {
			t.Errorf("SetBatch(%s) = nil, want an error: a marker is never a table row", k)
		}
	}
}

// TestOverlayIgnoresAMarkerKeyThatSomehowBecameARow is the other end of the
// forgery guard: a row carrying a marker key must never reach merged metadata,
// where it would forge the bead's own fence or freshness clock out of table data.
func TestOverlayIgnoresAMarkerKeyThatSomehowBecameARow(t *testing.T) {
	fence := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	committed := map[string]string{
		"state":                       "active",
		FenceKeyFor("state"):          FenceStamp(fence),
		"instance_token":              "committed",
		FenceKeyFor("instance_token"): FenceStamp(fence),
	}
	snap := Snapshot{
		Values: map[string]string{
			// A forged fence that would un-fence instance_token, and a forged
			// freshness clock.
			FenceKeyFor("instance_token"): FenceStamp(fence.Add(-time.Hour)),
			WrittenAtKey:                  FenceStamp(fence.Add(time.Hour)),
			"instance_token":              "forged",
			"state":                       "asleep",
		},
		Times: map[string]time.Time{
			FenceKeyFor("instance_token"): fence.Add(time.Minute),
			WrittenAtKey:                  fence.Add(time.Minute),
			"instance_token":              fence.Add(-time.Minute),
			"state":                       fence.Add(time.Minute),
		},
		WrittenAt: fence.Add(time.Minute),
	}
	got := Overlay(committed, snap)
	if got[FenceKeyFor("instance_token")] != FenceStamp(fence) {
		t.Errorf("%s = %q, want the COMMITTED stamp; a table row forged the fence",
			FenceKeyFor("instance_token"), got[FenceKeyFor("instance_token")])
	}
	if got["instance_token"] != "committed" {
		t.Errorf("instance_token = %q, want the committed value", got["instance_token"])
	}
	if got[WrittenAtKey] != FenceStamp(fence.Add(time.Minute)) {
		t.Errorf("%s = %q, want the surviving rows' max, not the forged value", WrittenAtKey, got[WrittenAtKey])
	}
	if got["state"] != "asleep" {
		t.Errorf("state = %q, want the genuine post-fence row to still win", got["state"])
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

// --- fence (review blocker 1) -------------------------------------------------

// TestOverlayFencesRowsOlderThanTheFallbackStamp is the unit half of the
// stale-shadow blocker: after a degraded write commits liveness values plus a
// fence marker per key, a PRE-outage row must not come back and win once the
// pool recovers. Without the fence the overlay is unconditional across arbitrary
// time, and wake fencing reads a resurrected instance_token.
func TestOverlayFencesRowsOlderThanTheFallbackStamp(t *testing.T) {
	fence := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	committed := map[string]string{
		"instance_token":                    "post-outage",
		"generation":                        "9",
		FenceKeyFor("instance_token"):       FenceStamp(fence),
		FenceKeyFor("generation"):           FenceStamp(fence),
		FenceKeyFor("state"):                FenceStamp(fence),
		FenceKeyFor("pending_create_claim"): FenceStamp(fence),
	}
	snap := Snapshot{
		Values: map[string]string{
			"instance_token": "pre-outage", // written before its fence: dropped
			"generation":     "4",          // written before its fence: dropped
			"state":          "asleep",     // written AFTER its fence: still wins
		},
		Times: map[string]time.Time{
			"instance_token": fence.Add(-time.Hour),
			"generation":     fence.Add(-time.Minute),
			"state":          fence.Add(time.Second),
		},
		WrittenAt: fence.Add(time.Second),
	}
	got := Overlay(committed, snap)
	if got["instance_token"] != "post-outage" {
		t.Errorf("instance_token = %q, want the committed post-outage value; a stale row won the fence", got["instance_token"])
	}
	if got["generation"] != "9" {
		t.Errorf("generation = %q, want the committed 9", got["generation"])
	}
	if got["state"] != "asleep" {
		t.Errorf("state = %q, want the row written after the fence to still win", got["state"])
	}
	if got[WrittenAtKey] != FenceStamp(fence.Add(time.Second)) {
		t.Errorf("%s = %q, want the SURVIVING max, not the dropped rows'", WrittenAtKey, got[WrittenAtKey])
	}
}

// TestOverlayLeavesUnfencedKeysAlone is the other half of the per-key rule. A
// key with no marker never had a newer committed value, so its row is the
// freshest thing anyone has — fencing it would swap live telemetry for whatever
// ancient value the bead was created with.
func TestOverlayLeavesUnfencedKeysAlone(t *testing.T) {
	fence := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	committed := map[string]string{
		"instance_token":              "post-outage",
		"state":                       "created-long-ago",
		FenceKeyFor("instance_token"): FenceStamp(fence),
	}
	snap := Snapshot{
		Values: map[string]string{
			"instance_token": "pre-outage",
			"state":          "asleep",
		},
		Times: map[string]time.Time{
			"instance_token": fence.Add(-time.Hour),
			"state":          fence.Add(-time.Hour), // older than the OTHER key's fence
		},
		WrittenAt: fence.Add(-time.Hour),
	}
	got := Overlay(committed, snap)
	if got["instance_token"] != "post-outage" {
		t.Errorf("instance_token = %q, want the fenced committed value", got["instance_token"])
	}
	if got["state"] != "asleep" {
		t.Errorf("state = %q, want the unfenced row to win; it has no marker of its own", got["state"])
	}
}

func TestOverlayDropsEverythingWhenNoRowPostdatesTheFence(t *testing.T) {
	fence := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	committed := map[string]string{"state": "active", FenceKeyFor("state"): FenceStamp(fence)}
	snap := Snapshot{
		Values:    map[string]string{"state": "asleep"},
		Times:     map[string]time.Time{"state": fence}, // exactly AT the fence: dropped
		WrittenAt: fence,
	}
	got := Overlay(committed, snap)
	if got["state"] != "active" {
		t.Errorf("state = %q, want the committed value", got["state"])
	}
	if _, stamped := got[WrittenAtKey]; stamped {
		t.Errorf("stamped %s with no surviving rows", WrittenAtKey)
	}
}

// TestOverlayFencesAKeyWhoseRowHasNoTimestamp pins the fail-closed rule: a row
// that cannot prove it postdates its fence is dropped, because the committed
// value is the one the fallback write just recorded.
func TestOverlayFencesAKeyWhoseRowHasNoTimestamp(t *testing.T) {
	fence := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	committed := map[string]string{"state": "active", FenceKeyFor("state"): FenceStamp(fence)}
	snap := Snapshot{
		Values: map[string]string{"state": "asleep"},
		Times:  map[string]time.Time{}, // no clock at all
	}
	if got := Overlay(committed, snap)["state"]; got != "active" {
		t.Errorf("state = %q, want the committed value; a row with no timestamp cannot clear its fence", got)
	}
}

func TestOverlayWithAnUnparseableFenceKeepsTelemetry(t *testing.T) {
	// A corrupt marker must not silently discard live telemetry — fencing
	// nothing is the conservative direction.
	committed := map[string]string{"state": "active", FenceKeyFor("state"): "garbage"}
	snap := Snapshot{
		Values:    map[string]string{"state": "asleep"},
		Times:     map[string]time.Time{"state": time.Now().UTC()},
		WrittenAt: time.Now().UTC(),
	}
	if got := Overlay(committed, snap)["state"]; got != "asleep" {
		t.Errorf("state = %q, want the liveness value; an unparseable fence must fence nothing", got)
	}
}

func TestFallbackPlanFencesAndCarriesEverythingVersioned(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	got := FallbackPlan(map[string]string{
		"state":       "asleep",
		"session_key": "conv-1",
	}, now)
	if got["state"] != "asleep" || got["session_key"] != "conv-1" {
		t.Errorf("FallbackPlan = %v, want both halves versioned", got)
	}
	if got[FenceKeyFor("state")] != FenceStamp(now) {
		t.Errorf("FallbackPlan did not stamp %s", FenceKeyFor("state"))
	}
	if _, stamped := got[FenceKeyFor("session_key")]; stamped {
		t.Errorf("FallbackPlan fenced the versioned key session_key; it has no table row to fence")
	}
	// No liveness keys means nothing to fence, so no marker is committed.
	plain := FallbackPlan(map[string]string{"alias": "katya"}, now)
	for k := range plain {
		if IsMarkerKey(k) {
			t.Errorf("FallbackPlan stamped %q on a patch with no liveness keys: %v", k, plain)
		}
	}
}

// TestFallbackPlanFencesAreIndependentPerKey is the round-2 blocker in unit
// form. Two fallbacks over DIFFERENT key sets merge into committed metadata the
// way the store applies them — key by key — and the second must not un-fence the
// first. The scalar stamp + key-list this replaced was last-write-wins on both
// halves, so the second fallback resurrected the first one's rows.
func TestFallbackPlanFencesAreIndependentPerKey(t *testing.T) {
	first := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)

	committed := map[string]string{}
	for k, v := range FallbackPlan(map[string]string{
		"generation":           "5",
		"instance_token":       "post-outage",
		"pending_create_claim": "",
	}, first) {
		committed[k] = v
	}
	// A LATER fallback, different keys. Metadata writes merge per key.
	for k, v := range FallbackPlan(map[string]string{
		"state":     "asleep",
		"synced_at": "2026-09-03T12:01:00Z",
	}, second) {
		committed[k] = v
	}

	for _, k := range []string{"generation", "instance_token", "pending_create_claim"} {
		if got := committed[FenceKeyFor(k)]; got != FenceStamp(first) {
			t.Errorf("%s = %q, want the FIRST fallback's stamp %q — the second fallback un-fenced it",
				FenceKeyFor(k), got, FenceStamp(first))
		}
	}
	for _, k := range []string{"state", "synced_at"} {
		if got := committed[FenceKeyFor(k)]; got != FenceStamp(second) {
			t.Errorf("%s = %q, want the second fallback's stamp %q", FenceKeyFor(k), got, FenceStamp(second))
		}
	}

	// And the overlay honors all five: every pre-outage row is dropped.
	preOutage := first.Add(-time.Hour)
	snap := Snapshot{
		Values: map[string]string{
			"generation": "4", "instance_token": "pre-outage", "pending_create_claim": "true",
			"state": "active", "synced_at": "2026-09-03T11:00:00Z",
		},
		Times: map[string]time.Time{
			"generation": preOutage, "instance_token": preOutage, "pending_create_claim": preOutage,
			"state": preOutage, "synced_at": preOutage,
		},
		WrittenAt: preOutage,
	}
	got := Overlay(committed, snap)
	for k, want := range map[string]string{
		"generation": "5", "instance_token": "post-outage", "pending_create_claim": "",
		"state": "asleep", "synced_at": "2026-09-03T12:01:00Z",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want the committed %q; a stale row survived", k, got[k], want)
		}
	}
}

func TestSplitDropsMarkerKeysFromBothHalves(t *testing.T) {
	live, rest := Split(map[string]string{
		"state":                 "active",
		"alias":                 "katya",
		WrittenAtKey:            "forged",
		FenceKeyFor("state"):    "forged",
		FencePrefix + "made_up": "forged",
	})
	if _, ok := live[WrittenAtKey]; ok {
		t.Errorf("marker leaked into the liveness half: %v", live)
	}
	for _, k := range []string{WrittenAtKey, FenceKeyFor("state"), FencePrefix + "made_up"} {
		if _, ok := rest[k]; ok {
			t.Errorf("marker %q leaked into the versioned remainder; a caller could forge the freshness clock", k)
		}
		if _, ok := live[k]; ok {
			t.Errorf("marker %q leaked into the liveness half", k)
		}
	}
	if live["state"] != "active" || rest["alias"] != "katya" {
		t.Errorf("Split lost real keys: live=%v rest=%v", live, rest)
	}
}

func TestMemStoreDeleteKeysRemovesRowsRatherThanTombstoningThem(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	if err := m.SetBatch(ctx, "gc-1", map[string]string{"state": "active", "generation": "7"}); err != nil {
		t.Fatalf("SetBatch: %v", err)
	}
	if err := m.DeleteKeys(ctx, "gc-1", []string{"state", "never-written"}); err != nil {
		t.Fatalf("DeleteKeys: %v", err)
	}
	snap, err := m.Get(ctx, "gc-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Absent, not an empty tombstone: the overlay must fall THROUGH to the
	// committed metadata, not project an empty value over it.
	if _, present := snap.Values["state"]; present {
		t.Errorf("state is still present after DeleteKeys: %v", snap.Values)
	}
	if snap.Values["generation"] != "7" {
		t.Errorf("generation = %q, want 7 — DeleteKeys must not touch unnamed keys", snap.Values["generation"])
	}
}

func TestIsConnectionError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "bad conn", err: driver.ErrBadConn, want: true},
		{name: "invalid connection text", err: errors.New("invalid connection"), want: true},
		{name: "refused", err: errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), want: true},
		{name: "closed pool", err: errors.New("sql: database is closed"), want: true},
		{name: "wrapped", err: fmt.Errorf("liveness: upsert: %w", driver.ErrBadConn), want: true},
		{name: "statement error", err: errors.New("Error 1054: Unknown column 'nope'"), want: false},
		{name: "not found", err: errors.New("no rows in result set"), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsConnectionError(tc.err); got != tc.want {
				t.Errorf("IsConnectionError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
