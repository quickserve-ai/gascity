package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// Live-owner invariant (ga-pzop1c): gc hook --claim must never take over a
// bead whose recorded executing session is still alive. On 2026-09-03 a claim
// reassigned qc-53bmzg from a live session to a fresh one — the bead read as
// unassigned because a reaper had cleared assignee/status, but its
// gc.session_id and gc.work_dir still named the live prior worker, and both
// sessions then edited one worktree for ~15 minutes. The guard classifies the
// candidate's recorded session before claiming:
//
//   - recorded session is LIVE            -> decline, loudly (never silent);
//   - liveness cannot be determined       -> decline, loudly (fail closed —
//     claiming on an unreadable session store reproduces the incident, while
//     a loud decline is diagnosable and the work is retried next tick);
//   - recorded session is closed/terminal -> claim proceeds, and the claim-
//     time identity stamp preserves the displaced id as gc.prev_session_id
//     so recovery can detect the takeover instead of finding it erased.
//
// A candidate whose gc.session_id is empty or already names THIS session is
// out of scope — adoption of one's own work is the normal path.

// hookOwnerSessionVerdict classifies the liveness of a candidate's recorded
// executing session for the live-owner claim guard.
type hookOwnerSessionVerdict int

const (
	// hookOwnerSessionGone: the recorded session is closed, terminal, absent,
	// or resolves to a non-session bead — the takeover may proceed.
	hookOwnerSessionGone hookOwnerSessionVerdict = iota
	// hookOwnerSessionLive: the recorded session is a live incarnation — the
	// claim must be declined.
	hookOwnerSessionLive
	// hookOwnerSessionUnknown: the session store could not answer — fail
	// closed (decline) with a distinct reason.
	hookOwnerSessionUnknown
)

// hookOwnerSessionProbe answers whether the session named on a candidate is
// still live. Injected through hookClaimOps so tests control the verdict; the
// production probe is built at the hook command root where cityPath and cfg
// are in hand (newHookOwnerSessionProbe).
type hookOwnerSessionProbe func(ownerSessionID string) (hookOwnerSessionVerdict, string)

// declinedLiveOwnerSampleLimit mirrors declinedForeignSampleLimit for the
// never-silent summary line.
const declinedLiveOwnerSampleLimit = 5

// newHookOwnerSessionProbe builds the production owner-liveness probe over the
// same relocation-safe session front door the stale-session fence uses
// (classifyHookClaimSession), minus the instance-token arm: the question here
// is "is that session alive at all", not "is it the current incarnation".
func newHookOwnerSessionProbe(cityPath string, cfg *config.City) hookOwnerSessionProbe {
	return func(ownerSessionID string) (hookOwnerSessionVerdict, string) {
		store, err := openCityStoreAt(cityPath)
		if err != nil {
			return hookOwnerSessionUnknown, fmt.Sprintf("opening session store: %v", err)
		}
		info, err := cliSessionFrontDoor(store, cfg, cityPath).Get(ownerSessionID)
		if err != nil {
			switch {
			case errors.Is(err, beads.ErrNotFound):
				return hookOwnerSessionGone, "session bead not found"
			case errors.Is(err, session.ErrSessionNotFound):
				return hookOwnerSessionGone, "id resolves to a non-session bead"
			default:
				return hookOwnerSessionUnknown, fmt.Sprintf("loading session bead: %v", err)
			}
		}
		return hookOwnerSessionInfoVerdict(info)
	}
}

// hookOwnerSessionInfoVerdict is the pure liveness decision over a session
// Info snapshot. The live states deliberately match the claim-eligibility set
// in hookClaimSessionEligibility (active/awake/creating/start-pending, plus
// the canonicalized-empty legacy state): a session in any of those states may
// be mid-work on the bead's worktree. Every dormant or terminal state means
// the owner is gone and the takeover is safe.
func hookOwnerSessionInfoVerdict(info session.Info) (hookOwnerSessionVerdict, string) {
	if info.Closed {
		return hookOwnerSessionGone, "session bead is closed"
	}
	switch state := session.State(strings.TrimSpace(info.MetadataState)); state {
	case session.StateNone, session.StateActive, session.StateAwake, session.StateCreating, session.StateStartPending:
		return hookOwnerSessionLive, fmt.Sprintf("session state %q", state)
	default:
		return hookOwnerSessionGone, fmt.Sprintf("session state %q is not live", state)
	}
}

// hookCandidateRecordedSession returns the session id recorded on a candidate
// when it names a session OTHER than the claimer's own. Empty when the
// candidate carries no session back-reference or carries the claimer's.
func hookCandidateRecordedSession(candidate beads.Bead, ownSessionID string) string {
	recorded := strings.TrimSpace(candidate.Metadata[beadmeta.SessionIDMetadataKey])
	if recorded == "" {
		recorded = strings.TrimSpace(candidate.Metadata[beadmeta.SessionIDCamelMetadataKey])
	}
	if recorded == "" || recorded == strings.TrimSpace(ownSessionID) {
		return ""
	}
	return recorded
}

// reportDeclinedLiveOwner writes the never-silent summary for candidates
// declined because their recorded session is (or may be) live.
func reportDeclinedLiveOwner(stderr io.Writer, declined []string) {
	if len(declined) == 0 {
		return
	}
	sample := declined
	suffix := ""
	if len(sample) > declinedLiveOwnerSampleLimit {
		suffix = fmt.Sprintf(" (+%d more)", len(sample)-declinedLiveOwnerSampleLimit)
		sample = sample[:declinedLiveOwnerSampleLimit]
	}
	fmt.Fprintf(stderr, "gc hook --claim: declined-live-owner: %d candidate(s) still record another session as their executor and were not claimed (ga-pzop1c): %s%s\n", //nolint:errcheck
		len(declined), strings.Join(sample, ", "), suffix)
}
