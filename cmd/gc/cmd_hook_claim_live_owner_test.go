package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// Pins the live-owner claim invariant (ga-pzop1c): on 2026-09-03 gc hook
// --claim reassigned qc-53bmzg to a fresh session while the session recorded
// in its gc.session_id was live and editing the bead's worktree — the bead
// read as unassigned because a reaper had cleared assignee/status. The claim
// path must decline such a candidate loudly while the recorded session is
// live (or unreadable — fail closed), and a legitimate takeover from a dead
// session must preserve the displaced id as gc.prev_session_id.

type liveOwnerClaimSpy struct{ ids []string }

func (s *liveOwnerClaimSpy) fn(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
	s.ids = append(s.ids, id)
	return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{}}, true, nil
}

func liveOwnerGuardOpsOpts(workQueryJSON string, spy *liveOwnerClaimSpy, probe hookOwnerSessionProbe) (hookClaimOps, hookClaimOptions) {
	ops := hookClaimOps{
		Runner:            func(string, string) (string, error) { return workQueryJSON, nil },
		Claim:             spy.fn,
		ResolveWorkBranch: func(string) string { return "" },
		StampWorkMeta:     noopStampWorkMeta,
		PublishRunMap:     func(string, string, ...string) error { return nil },
		OwnerSessionLive:  probe,
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env:                []string{"GC_SESSION_ID=ga-me"},
		JSON:               true,
	}
	return ops, opts
}

func TestHookClaimDeclinesCandidateWithLiveRecordedSession(t *testing.T) {
	spy := &liveOwnerClaimSpy{}
	probe := func(owner string) (hookOwnerSessionVerdict, string) {
		if owner != "ga-other" {
			t.Errorf("probe asked about %q, want ga-other", owner)
		}
		return hookOwnerSessionLive, `session state "active"`
	}
	ops, opts := liveOwnerGuardOpsOpts(
		`[{"id":"qc-race","status":"open","metadata":{"gc.routed_to":"worker","gc.session_id":"ga-other","gc.work_dir":"/w/slit"}}]`,
		spy, probe,
	)

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 0 {
		t.Fatalf("claim mutations = %v, want none — a live-owned candidate must never be claimed", spy.ids)
	}
	if !strings.Contains(stderr.String(), "declined-live-owner: 1") {
		t.Errorf("stderr = %q, want a loud declined-live-owner summary", stderr.String())
	}
}

func TestHookClaimFailsClosedWhenOwnerLivenessUnknown(t *testing.T) {
	spy := &liveOwnerClaimSpy{}
	probe := func(string) (hookOwnerSessionVerdict, string) {
		return hookOwnerSessionUnknown, "opening session store: boom"
	}
	ops, opts := liveOwnerGuardOpsOpts(
		`[{"id":"qc-murky","status":"open","metadata":{"gc.routed_to":"worker","gc.session_id":"ga-other"}}]`,
		spy, probe,
	)

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 0 {
		t.Fatalf("claim mutations = %v, want none — unknown owner liveness fails closed", spy.ids)
	}
	if !strings.Contains(stderr.String(), "declined-live-owner: 1") {
		t.Errorf("stderr = %q, want the fail-closed decline reported loudly", stderr.String())
	}
}

func TestHookClaimTakesOverFromGoneSessionAndLaterCandidatePastLiveOne(t *testing.T) {
	spy := &liveOwnerClaimSpy{}
	probe := func(owner string) (hookOwnerSessionVerdict, string) {
		if owner == "ga-live" {
			return hookOwnerSessionLive, `session state "active"`
		}
		return hookOwnerSessionGone, "session bead is closed"
	}
	ops, opts := liveOwnerGuardOpsOpts(
		`[{"id":"qc-blocked","status":"open","metadata":{"gc.routed_to":"worker","gc.session_id":"ga-live"}},`+
			`{"id":"qc-free","status":"open","metadata":{"gc.routed_to":"worker","gc.session_id":"ga-dead"}}]`,
		spy, probe,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(spy.ids) != 1 || spy.ids[0] != "qc-free" {
		t.Fatalf("claim mutations = %v, want only qc-free (dead owner) claimed", spy.ids)
	}
	if !strings.Contains(stderr.String(), "declined-live-owner: 1") {
		t.Errorf("stderr = %q, want the skipped live-owned candidate reported", stderr.String())
	}
}

func TestHookClaimOwnSessionAndNilProbeUnaffected(t *testing.T) {
	// A candidate recording the claimer's own session is adoption, not a
	// takeover; a nil probe (non-session invocation) disables the guard.
	for name, probe := range map[string]hookOwnerSessionProbe{
		"own session id": func(string) (hookOwnerSessionVerdict, string) {
			t.Error("probe must not be consulted for the claimer's own session")
			return hookOwnerSessionLive, ""
		},
		"nil probe": nil,
	} {
		spy := &liveOwnerClaimSpy{}
		meta := `{"gc.routed_to":"worker","gc.session_id":"ga-me"}`
		if probe == nil {
			meta = `{"gc.routed_to":"worker","gc.session_id":"ga-anyone"}`
		}
		ops, opts := liveOwnerGuardOpsOpts(`[{"id":"qc-mine","status":"open","metadata":`+meta+`}]`, spy, probe)

		var stdout, stderr bytes.Buffer
		doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

		if len(spy.ids) != 1 {
			t.Errorf("%s: claim mutations = %v, want the candidate claimed", name, spy.ids)
		}
		if strings.Contains(stderr.String(), "declined-live-owner") {
			t.Errorf("%s: stderr = %q, want no live-owner decline", name, stderr.String())
		}
	}
}

func TestHookOwnerSessionInfoVerdict(t *testing.T) {
	cases := []struct {
		name string
		info session.Info
		want hookOwnerSessionVerdict
	}{
		{"closed bead", session.Info{Closed: true}, hookOwnerSessionGone},
		{"active", session.Info{MetadataState: "active"}, hookOwnerSessionLive},
		{"creating", session.Info{MetadataState: "creating"}, hookOwnerSessionLive},
		{"legacy empty state", session.Info{}, hookOwnerSessionLive},
		{"drained", session.Info{MetadataState: "drained"}, hookOwnerSessionGone},
		{"asleep", session.Info{MetadataState: "asleep"}, hookOwnerSessionGone},
	}
	for _, tc := range cases {
		if got, _ := hookOwnerSessionInfoVerdict(tc.info); got != tc.want {
			t.Errorf("%s: verdict = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHookClaimIdentityPatchPreservesDisplacedSession(t *testing.T) {
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=ga-me"}}
	ops := hookClaimOps{ResolveWorkBranch: func(string) string { return "" }}

	patch := hookClaimIdentityPatch(beads.Bead{
		ID:       "qc-x",
		Metadata: map[string]string{"gc.session_id": "ga-dead"},
	}, opts, ops, "/tmp/work")
	if patch["gc.session_id"] != "ga-me" {
		t.Errorf("session id patch = %q, want ga-me", patch["gc.session_id"])
	}
	if patch["gc.prev_session_id"] != "ga-dead" {
		t.Errorf("prev session id = %q, want the displaced ga-dead preserved", patch["gc.prev_session_id"])
	}

	patch = hookClaimIdentityPatch(beads.Bead{ID: "qc-y", Metadata: map[string]string{}}, opts, ops, "/tmp/work")
	if _, ok := patch["gc.prev_session_id"]; ok {
		t.Errorf("prev session id stamped with no prior owner: %v", patch)
	}

	patch = hookClaimIdentityPatch(beads.Bead{
		ID:       "qc-z",
		Metadata: map[string]string{"gc.session_id": "ga-me"},
	}, opts, ops, "/tmp/work")
	if len(patch) != 0 {
		t.Errorf("re-adoption of own bead should patch nothing, got %v", patch)
	}
}
