package main

import (
	"bytes"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The fleet's resume-me helper cycles a session onto its own conversation:
// `gc session suspend S`, then `gc session resume S <S's current conversation>`
// until the seed is accepted, both addressed by session bead id. The suspend
// only records the hold; the runtime stays up while the reconciler drains it,
// and the reconciler rewrites the bead's state to awake for as long as it does.
// A session's own conversation is not "already live" on some other session, so
// the seed has to land in every one of those states.

// seedOwnConversation runs the helper's seed against a fixture session that
// records currentConvID as its conversation and carries sessionMeta on top.
func seedOwnConversation(t *testing.T, sessionMeta map[string]string) (fx historyWorktreeFixture, code int, stdout, stderr string) {
	t.Helper()
	meta := map[string]string{"session_key": currentConvID}
	for k, v := range sessionMeta {
		meta[k] = v
	}
	fx = setupHistoryWorktreeFixture(t, meta, false)
	writeNamedTestSession(t, fx.liveRoot, fx.agentDir, currentConvID+".jsonl",
		historyTranscriptLines(currentConvID, "lana", fx.agentDir, "lana's current conversation")...)

	var out, errOut bytes.Buffer
	code = cmdSessionResume([]string{fx.sessionID, currentConvID}, false, false, t.TempDir(), &out, &errOut)
	return fx, code, out.String(), errOut.String()
}

func TestSessionResumeAcceptsItsOwnConversationWhileHeld(t *testing.T) {
	heldUntil := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		meta map[string]string
		// runtimeUp marks the states in which the session's process is still
		// running and the reconciler is draining it for the hold.
		runtimeUp bool
	}{
		{
			name: "suspended, before the reconciler has acted on the hold",
			meta: map[string]string{"state": "suspended"},
		},
		{
			name:      "awake, as the reconciler rewrites it while it drains for the hold",
			meta:      map[string]string{"state": "awake"},
			runtimeUp: true,
		},
		{
			name:      "draining, the drain acknowledged and the stop pending",
			meta:      map[string]string{"state": "draining", "state_reason": "drain-ack-stop-pending"},
			runtimeUp: true,
		},
		{
			name: "asleep, the drain complete and the hold still standing",
			meta: map[string]string{"state": "asleep", "sleep_reason": "user-hold"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := map[string]string{"sleep_intent": "user-hold", "held_until": heldUntil}
			for k, v := range tc.meta {
				meta[k] = v
			}
			fx, code, stdout, stderr := seedOwnConversation(t, meta)
			if code != 0 {
				t.Fatalf("resume of the session's own conversation was refused (state %q, user hold): exit %d\nstderr: %sstdout: %s", tc.meta["state"], code, stderr, stdout)
			}
			got, err := fx.store.Get(fx.sessionID)
			if err != nil {
				t.Fatalf("store.Get(session): %v", err)
			}
			if key := got.Metadata["session_key"]; key != currentConvID {
				t.Fatalf("session_key = %q, want the session's own conversation %q", key, currentConvID)
			}
			if !tc.runtimeUp {
				return
			}
			// The process is still up and being drained for the hold. A seed that
			// clears the hold or requests a wake cancels that drain (user-hold is a
			// cancelable reason) and leaves the seat on its old process, so here the
			// seed records the conversation and nothing else.
			if got.Metadata["held_until"] != heldUntil || got.Metadata["sleep_intent"] != "user-hold" {
				t.Fatalf("the seed disturbed the hold of a session still draining for it: held_until=%q sleep_intent=%q, want %q and %q",
					got.Metadata["held_until"], got.Metadata["sleep_intent"], heldUntil, "user-hold")
			}
			if req := got.Metadata["wake_request"]; req != "" {
				t.Fatalf("the seed requested a wake (%q) for a session still draining for its hold", req)
			}
		})
	}
}

// A conversation id recorded on the session's own bead is found by id, wherever
// its transcript lies. Resolving it needs no in-progress task lookup, and that
// lookup opens every rig store of the city, one of which can be a remote hub.
func TestSessionResumeOfAConversationOnTheBeadOpensNoRigStores(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, map[string]string{"session_key": currentConvID}, false)
	writeNamedTestSession(t, fx.liveRoot, fx.agentDir, currentConvID+".jsonl",
		historyTranscriptLines(currentConvID, "lana", fx.agentDir, "lana's current conversation")...)
	bindHistoryTestRig(t, fx, "near")
	bindHistoryTestRig(t, fx, "far")

	var mu sync.Mutex
	var opened []string
	prevOpener := historyRigStoreOpener
	t.Cleanup(func() { historyRigStoreOpener = prevOpener })
	historyRigStoreOpener = func(*config.City) rigStoreOpener {
		return func(rigPath, _ string) (beads.Store, error) {
			mu.Lock()
			opened = append(opened, filepath.Base(rigPath))
			mu.Unlock()
			return beads.NewMemStore(), nil
		}
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionResume([]string{fx.sessionID, currentConvID}, false, false, t.TempDir(), &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionResume = %d; stderr=%s", code, stderr.String())
	}
	got, err := fx.store.Get(fx.sessionID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	if key := got.Metadata["session_key"]; key != currentConvID {
		t.Fatalf("session_key = %q, want seeded %q", key, currentConvID)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(opened) != 0 {
		t.Fatalf("resume with the explicit conversation id %s, recorded on the session's bead, opened %d rig store(s) %q; want none",
			currentConvID, len(opened), opened)
	}
}
