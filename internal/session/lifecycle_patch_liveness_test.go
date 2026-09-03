package session

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/liveness"
)

// TestHotPatchBuildersEmitOnlyLivenessKeys is the batch-completeness guard,
// driven through the REAL builders: the splitter skips the versioned bead write
// only when EVERY key in a patch is a liveness key, so a single unmoved key in a
// hot builder re-mints the Dolt commit its siblings avoid — the exact way
// post-deploy churn stayed at ~244/hr when state moved but state_reason did
// not. A key added to any builder below fails here until it is classified:
// either into liveness's moved set, or by moving the builder into the
// deliberate exceptions at the bottom with the reasoning written down.
//
// (An earlier guard restated each builder's key set by hand in
// internal/liveness's tests; it could not see builder drift, which its review
// called out. liveness cannot import this package — session imports liveness —
// so the real-builder guard lives here.)
func TestHotPatchBuildersEmitOnlyLivenessKeys(t *testing.T) {
	now := time.Date(2026, 9, 3, 19, 0, 0, 0, time.UTC)
	builders := map[string]MetadataPatch{
		"SleepPatch":                  SleepPatch(now, "idle-timeout"),
		"ConfirmStartedPatch":         ConfirmStartedPatch(now),
		"BeginDrainPatch":             BeginDrainPatch(now, "max-age"),
		"DrainAckStopPendingPatch":    DrainAckStopPendingPatch(now),
		"AcknowledgeDrainPatch":       AcknowledgeDrainPatch(false),
		"CompleteDrainPatch":          CompleteDrainPatch(now, "drain_complete", false),
		"RequestWakePatch":            RequestWakePatch("demand", now),
		"RequestExplicitWakePatch":    RequestExplicitWakePatch("operator", now),
		"ClearWakeBlockersPatch":      ClearWakeBlockersPatch(StateSuspended, string(SleepReasonUserHold)),
		"ClearExpiredHoldPatch":       ClearExpiredHoldPatch(string(SleepReasonUserHold)),
		"ClearExpiredQuarantinePatch": ClearExpiredQuarantinePatch(string(SleepReasonQuarantine)),
		"QuarantinePatch":             QuarantinePatch(now.Add(time.Hour), 3),
		"PreWakePatch": PreWakePatch(PreWakePatchInput{
			Generation:        4,
			InstanceToken:     "tok-1",
			ContinuationEpoch: 2,
			Now:               now,
			SleepReason:       string(SleepReasonDrained),
		}),
	}
	for name, patch := range builders {
		if len(patch) == 0 {
			t.Errorf("%s emitted an empty patch; the guard is not exercising it", name)
			continue
		}
		for k := range patch {
			if !liveness.IsKey(k) {
				t.Errorf("%s emits %q, which is NOT in the liveness moved set — either move it (internal/liveness/keys.go) or move this builder into this test's deliberate exceptions with the reasoning; as it stands every %s write mints a Dolt commit for this one key", name, k, name)
			}
		}
	}

	// Deliberate exceptions — builders that are ALLOWED to write versioned
	// metadata, asserted so their exemption is a recorded decision rather than
	// an omission:
	//
	//   - CommitStartedPatch / RestartRequestPatch carry the session-restart
	//     identity cluster (started_* hashes, session_key, ...). One commit per
	//     restart is genuine lifecycle history, kept deliberately.
	//   - AcknowledgeDrainPatch(freshWake=true) and ContinuationResetWakePatch
	//     are restart-CLASS events: a fresh wake and a continuation reset both
	//     re-seed the identity cluster, so they commit once per fresh start —
	//     same rationale as CommitStartedPatch. (Their FREQUENCY on always-on
	//     fresh-cycling templates is the ga-ysqkmi cadence problem, not a
	//     storage classification problem.) The freshWake=false ack stays in the
	//     all-liveness set above.
	//   - ClosePatch / ArchivePatch apply inside a status-changing Tx, which
	//     commits regardless; inside a Tx nothing splits (all keys go
	//     versioned, fenced — see beadPolicyStore.Tx), so batch completeness
	//     buys nothing there.
	for name, patch := range map[string]MetadataPatch{
		"CommitStartedPatch":               CommitStartedPatch(CommitStartedPatchInput{}),
		"RestartRequestPatch":              RestartRequestPatch("sess-key", now),
		"AcknowledgeDrainPatch/fresh-wake": AcknowledgeDrainPatch(true),
		"ContinuationResetWakePatch":       ContinuationResetWakePatch(now),
	} {
		versioned := 0
		for k := range patch {
			if !liveness.IsKey(k) {
				versioned++
			}
		}
		if versioned == 0 {
			t.Errorf("%s no longer writes any versioned key; it belongs in the all-liveness set above, not the exceptions", name)
		}
	}
}
