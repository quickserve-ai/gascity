package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/liveness"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The tests in this file cover the key-sweep round (ga-lys454): the
// telemetry-class keys that were MISSED by the first moved set and kept hq
// commit churn at ~244/hr, because a patch carrying one unmoved key still
// writes versioned metadata and mints a Dolt commit.
//
// They deliberately drive the REAL write sites (the nudge-delivery stamp, the
// idle-claim and continuation-claim marker writers, the current-bead record)
// rather than the splitter, because "these exact call sites stop committing" is
// the claim the sweep makes. The assertion that matters everywhere is
// len(backing.batches)+len(backing.singles) == 0: a write the backing bead store
// never sees is a Dolt commit that never happens.

// TestNudgeDeliveryStampWritesNoVersionedMetadata is the acceptance test for the
// single biggest churn class the first deploy missed. Every successful nudge
// delivery (cmd_nudge.go direct + drain + ACP dispatcher, cmd_sling.go) calls
// stampLastNudgeDeliveredAt, which is a one-key SetMarker; with
// last_nudge_delivered_at in the moved set that write must reach the liveness
// table and never the versioned issues row.
func TestNudgeDeliveryStampWritesNoVersionedMetadata(t *testing.T) {
	store, backing, lv := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, map[string]string{"alias": "katya"})
	backing.batches = nil
	backing.singles = nil
	backing.updates = nil

	at := time.Date(2026, 9, 3, 18, 30, 0, 0, time.UTC)
	stampLastNudgeDeliveredAt(sessionFrontDoor(store), bead.ID, at)

	if n := len(backing.batches) + len(backing.singles) + len(backing.updates); n != 0 {
		t.Fatalf("nudge-delivery stamp made %d versioned bead writes, want 0 (batches=%v singles=%v)",
			n, backing.batches, backing.singles)
	}

	snap, err := lv.Get(context.Background(), bead.ID)
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	want := at.UTC().Format(time.RFC3339)
	if got := snap.Values[sessionpkg.MetadataLastNudgeDeliveredAt]; got != want {
		t.Fatalf("liveness %s = %q, want %q", sessionpkg.MetadataLastNudgeDeliveredAt, got, want)
	}
	// And the overlay must serve it back, so `gc session list` and the API still
	// render "last nudge N ago" off a bead read.
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata[sessionpkg.MetadataLastNudgeDeliveredAt] != want {
		t.Fatalf("overlaid %s = %q, want %q",
			sessionpkg.MetadataLastNudgeDeliveredAt,
			got.Metadata[sessionpkg.MetadataLastNudgeDeliveredAt], want)
	}
}

// TestIdleClaimMarkerWritesNoVersionedMetadata drives the stalled-claim backstop
// state machine. All three keys travel in ONE SetMetadataBatch, which is why
// idle_claim_nudge_count had to move alongside the trigger/at pair the census
// named: leaving the count versioned would have kept the batch committing on
// every observe, every reserve and every clear.
func TestIdleClaimMarkerWritesNoVersionedMetadata(t *testing.T) {
	store, backing, lv := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, map[string]string{"pool_managed": "true"})
	backing.batches = nil
	backing.singles = nil
	now := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)

	// observe → reserve → clear: the full cycle the reconcile tick can drive.
	if !writeIdleClaimMarker(store, &bead, "wb-A", 0, now, io.Discard) {
		t.Fatal("writeIdleClaimMarker(observe) reported failure")
	}
	if !writeIdleClaimMarker(store, &bead, "wb-A", 1, now.Add(time.Minute), io.Discard) {
		t.Fatal("writeIdleClaimMarker(reserve) reported failure")
	}
	if n := len(backing.batches) + len(backing.singles); n != 0 {
		t.Fatalf("idle-claim marker made %d versioned bead writes, want 0: %v", n, backing.batches)
	}
	snap, err := lv.Get(context.Background(), bead.ID)
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	if snap.Values[idleClaimNudgeTriggerKey] != "wb-A" || snap.Values[idleClaimNudgeCountKey] != "1" {
		t.Fatalf("liveness values = %v, want the whole marker diverted", snap.Values)
	}

	clearIdleClaimMarker(store, &bead, io.Discard)
	if n := len(backing.batches) + len(backing.singles); n != 0 {
		t.Fatalf("idle-claim clear made %d versioned bead writes, want 0: %v", n, backing.batches)
	}
	// A clear is a tombstone row, not a delete, so the overlay must serve the
	// cleared value over any stale committed one.
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v := got.Metadata[idleClaimNudgeTriggerKey]; v != "" {
		t.Fatalf("%s = %q after clear, want empty", idleClaimNudgeTriggerKey, v)
	}
}

// TestContinuationClaimMarkerWritesNoVersionedMetadata is the same guarantee for
// the post-step continuation backstop, whose six keys share the idle-claim
// engine and its single-batch write shape.
func TestContinuationClaimMarkerWritesNoVersionedMetadata(t *testing.T) {
	store, backing, _ := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, map[string]string{"pool_managed": "true"})
	backing.batches = nil
	backing.singles = nil

	target := backstopTarget{
		ID:         "wb-step-2",
		RootID:     "wb-root",
		StoreRef:   "city",
		Generation: "7",
	}
	now := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	if !writeContinuationClaimMarker(store, &bead, target, 1, now, io.Discard) {
		t.Fatal("writeContinuationClaimMarker reported failure")
	}
	clearContinuationClaimMarker(store, &bead, io.Discard)

	if n := len(backing.batches) + len(backing.singles); n != 0 {
		t.Fatalf("continuation-claim marker made %d versioned bead writes, want 0: %v", n, backing.batches)
	}
}

// TestSweptSessionTransitionsWriteNoVersionedMetadata drives the lifecycle
// builders whose only straggler was one of the swept keys. Each of these fires
// several times per session lifetime, so a single unmoved member re-mints the
// commit the rest of the patch avoids — that is exactly how state moved and
// state_reason did not.
func TestSweptSessionTransitionsWriteNoVersionedMetadata(t *testing.T) {
	now := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		patch sessionpkg.MetadataPatch
	}{
		{"sleep", sessionpkg.SleepPatch(now, "idle-timeout")},
		{"confirm-started", sessionpkg.ConfirmStartedPatch(now)},
		{"begin-drain", sessionpkg.BeginDrainPatch(now, "max-age")},
		{"drain-ack-stop-pending", sessionpkg.DrainAckStopPendingPatch(now)},
		{"request-wake", sessionpkg.RequestWakePatch("demand", now)},
		{"complete-drain", sessionpkg.CompleteDrainPatch(now, "drain_complete", false)},
		{"acknowledge-drain", sessionpkg.AcknowledgeDrainPatch(false)},
		{"quarantine", sessionpkg.QuarantinePatch(now.Add(time.Hour), 3)},
		{"pre-wake", sessionpkg.PreWakePatch(sessionpkg.PreWakePatchInput{
			Generation:        4,
			InstanceToken:     "tok-1",
			ContinuationEpoch: 2,
			Now:               now,
			SleepReason:       "",
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, backing, _ := newLivenessTestStore(t, liveness.ModeTable)
			bead := mustCreateSessionBead(t, store, nil)
			backing.batches = nil
			backing.singles = nil

			if err := sessionFrontDoor(store).ApplyPatch(bead.ID, tc.patch); err != nil {
				t.Fatalf("ApplyPatch: %v", err)
			}
			if n := len(backing.batches); n != 0 {
				t.Fatalf("%s made %d versioned bead writes, want 0; leftover keys: %v",
					tc.name, n, backing.batches)
			}
		})
	}
}

// TestRecordCurrentBeadWritesNoVersionedMetadata covers the
// currently_processing_bead_id stamp the reconciler makes whenever a session is
// woken onto a different work bead. It is a one-key SetMetadata, so after the
// sweep it must not touch the versioned row at all — and the overlay has to
// serve it back, because the reassign-divergence check reads it off the bead.
func TestRecordCurrentBeadWritesNoVersionedMetadata(t *testing.T) {
	store, backing, _ := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, map[string]string{
		sessionpkg.CurrentBeadIDKey: "wb-old",
	})
	backing.batches = nil
	backing.singles = nil

	if err := sessionFrontDoor(store).RecordCurrentBead(bead.ID, "wb-new"); err != nil {
		t.Fatalf("RecordCurrentBead: %v", err)
	}
	if n := len(backing.singles) + len(backing.batches); n != 0 {
		t.Fatalf("RecordCurrentBead made %d versioned bead writes, want 0: %v", n, backing.singles)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-new" {
		t.Fatalf("%s = %q, want the liveness value to shadow the stale committed wb-old",
			sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
}

// TestTriggerBeadIDStillCommits pins the deliberate NON-move. gc.trigger_bead_id
// is written as one member of the trigger/provenance cluster that
// session.Store.UpdateMetadataInfo commits in a SINGLE backend operation; a
// split would send the id to the liveness table and the store ref / pack /
// work dir through Update, leaving a bead bound to a new trigger with the old
// provenance. This test fails the moment someone adds it to the moved set.
func TestTriggerBeadIDStillCommits(t *testing.T) {
	store, backing, _ := newLivenessTestStore(t, liveness.ModeTable)
	bead := mustCreateSessionBead(t, store, nil)
	backing.updates = nil

	info, err := sessionFrontDoor(store).Get(bead.ID)
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	patch := sessionpkg.MetadataPatch{
		beadmeta.TriggerBeadIDMetadataKey:       "wb-A",
		beadmeta.TriggerBeadStoreRefMetadataKey: "city",
	}
	if _, err := sessionFrontDoor(store).UpdateMetadataInfo(info, patch); err != nil {
		t.Fatalf("UpdateMetadataInfo: %v", err)
	}
	if len(backing.updates) != 1 {
		t.Fatalf("backing saw %d Updates, want 1 — the provenance cluster must stay one operation", len(backing.updates))
	}
	for k, want := range patch {
		if got := backing.updates[0].Metadata[k]; got != want {
			t.Errorf("versioned Update[%q] = %q, want %q — the whole cluster must commit together", k, got, want)
		}
	}
	if liveness.IsKey(beadmeta.TriggerBeadIDMetadataKey) {
		t.Errorf("%s joined the moved set; see the LEFT VERSIONED note in internal/liveness/keys.go",
			beadmeta.TriggerBeadIDMetadataKey)
	}
}
