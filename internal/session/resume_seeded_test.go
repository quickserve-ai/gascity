package session

import (
	"testing"
	"time"
)

func TestStampPriorSessionKey(t *testing.T) {
	t.Run("preserves cleared key", func(t *testing.T) {
		patch := MetadataPatch{"session_key": ""}
		StampPriorSessionKey(patch, map[string]string{"session_key": "old-key"})
		if patch[PriorSessionKeyMetadata] != "old-key" {
			t.Fatalf("prior_session_key = %q, want old-key", patch[PriorSessionKeyMetadata])
		}
	})
	t.Run("preserves rotated key", func(t *testing.T) {
		patch := MetadataPatch{"session_key": "new-key"}
		StampPriorSessionKey(patch, map[string]string{"session_key": "old-key"})
		if patch[PriorSessionKeyMetadata] != "old-key" {
			t.Fatalf("prior_session_key = %q, want old-key", patch[PriorSessionKeyMetadata])
		}
	})
	t.Run("no-op when patch keeps the key", func(t *testing.T) {
		patch := MetadataPatch{"state": "asleep"}
		StampPriorSessionKey(patch, map[string]string{"session_key": "old-key"})
		if _, ok := patch[PriorSessionKeyMetadata]; ok {
			t.Fatal("stamped prior_session_key without a session_key change")
		}
	})
	t.Run("no-op when prior key empty or unchanged", func(t *testing.T) {
		patch := MetadataPatch{"session_key": ""}
		StampPriorSessionKey(patch, map[string]string{"session_key": ""})
		if _, ok := patch[PriorSessionKeyMetadata]; ok {
			t.Fatal("stamped prior_session_key from empty prior")
		}
		patch = MetadataPatch{"session_key": "same"}
		StampPriorSessionKey(patch, map[string]string{"session_key": "same"})
		if _, ok := patch[PriorSessionKeyMetadata]; ok {
			t.Fatal("stamped prior_session_key for unchanged key")
		}
	})
	t.Run("nil-safe", func(t *testing.T) {
		StampPriorSessionKey(nil, map[string]string{"session_key": "x"})
		StampPriorSessionKey(MetadataPatch{"session_key": ""}, nil)
	})
}

func TestResumeSeededClearedByResetPaths(t *testing.T) {
	now := time.Now()
	cases := map[string]MetadataPatch{
		"ConversationResetPatch":     ConversationResetPatch(true),
		"RestartRequestPatch":        RestartRequestPatch("rotated", now),
		"ContinuationResetWakePatch": ContinuationResetWakePatch(now),
		"ConfigDriftResetPatch":      ConfigDriftResetPatch(StateAsleep, "rotated", now),
		"AcknowledgeDrainPatchFresh": AcknowledgeDrainPatch(true),
		"CompleteDrainPatchFresh":    CompleteDrainPatch(now, "idle", true),
		"PreWakePatchFresh": PreWakePatch(PreWakePatchInput{
			Generation: 1, InstanceToken: "t", ContinuationEpoch: 1, Now: now, FreshWake: true,
		}),
		"CommitStartedPatch": CommitStartedPatch(CommitStartedPatchInput{CoreHash: "h", LiveHash: "l", Now: now}),
	}
	for name, patch := range cases {
		if got, ok := patch[resumeSeededKey]; !ok || got != "" {
			t.Errorf("%s: resume_seeded = (%q, present=%v), want cleared", name, got, ok)
		}
	}
}

func TestResumeSeededSurvivesNonFreshPaths(t *testing.T) {
	now := time.Now()
	cases := map[string]MetadataPatch{
		"AcknowledgeDrainPatch": AcknowledgeDrainPatch(false),
		"CompleteDrainPatch":    CompleteDrainPatch(now, "idle", false),
		"PreWakePatch": PreWakePatch(PreWakePatchInput{
			Generation: 1, InstanceToken: "t", ContinuationEpoch: 1, Now: now, FreshWake: false,
		}),
	}
	for name, patch := range cases {
		if _, ok := patch[resumeSeededKey]; ok {
			t.Errorf("%s: resume_seeded cleared on a non-fresh transition", name)
		}
	}
}

func TestIsResumeSeeded(t *testing.T) {
	if IsResumeSeeded(nil) {
		t.Error("nil metadata reported seeded")
	}
	if IsResumeSeeded(map[string]string{resumeSeededKey: ""}) {
		t.Error("empty flag reported seeded")
	}
	if !IsResumeSeeded(map[string]string{resumeSeededKey: "true"}) {
		t.Error("seeded metadata not detected")
	}
}
