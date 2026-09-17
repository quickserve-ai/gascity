package main

import (
	"strings"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// healReleasedWaitHoldReasonInfo is a Phase-0 heal for a wait hold that was
// released while its displayed reason stayed behind: an asleep session with
// sleep_reason="wait-hold" but neither the wait_hold marker nor a wait-hold
// sleep_intent. It clears the reason through the front door and returns info
// advanced by the clear (write-returns-Info), like healExpiredTimersInfo.
//
// Why it exists (fork PR #59 review, item 2): clearSessionWaitHold clears the
// wait_hold/sleep_intent pair unconditionally but drops sleep_reason only when
// a PersistedMarkers read confirms it is "wait-hold". Through the liveness
// overlay that read is fail-open — a degraded read returns committed metadata,
// where the reason is absent — so the release left the reason in the table.
// Once standing holds carry "wait-hold" into sleep_reason at drain completion,
// that stranded reason keeps an asleep pool seat out of
// isPoolSessionSlotFreeableInfo for good, and the release path is one-shot: the
// wait bead is already terminal, so nothing retries it. Healing the observed
// shape on every tick makes the release converge instead of depending on the
// one read.
//
// The heal never acts on a degraded read (the fields below would be committed
// values, and a clear written from them is fenced), and never while any half of
// the wait hold still stands. On a persist error info is returned unchanged
// and the next tick retries, matching healExpiredTimersInfo.
func healReleasedWaitHoldReasonInfo(info sessionpkg.Info, sessFront *sessionpkg.Store) sessionpkg.Info {
	if sessFront == nil || info.LivenessReadDegraded {
		return info
	}
	if sessionpkg.SleepReason(strings.TrimSpace(info.SleepReason)) != sessionpkg.SleepReasonWaitHold {
		return info
	}
	if strings.TrimSpace(info.WaitHold) != "" ||
		sessionpkg.StandingSleepIntent(info.SleepIntent) == sessionpkg.SleepReasonWaitHold {
		return info
	}
	if strings.TrimSpace(info.MetadataState) != string(sessionpkg.StateAsleep) {
		return info
	}
	batch := sessionpkg.MetadataPatch{"sleep_reason": ""}
	if err := sessFront.ApplyPatch(info.ID, batch); err != nil {
		return info
	}
	return info.ApplyPatch(batch)
}
