package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The fleet's resume-me helper cycles a session onto its own conversation:
// `gc session suspend S`, then `gc session resume S <S's current conversation>`
// until the seed is accepted, both addressed by session bead id. The suspend
// only records the hold; the runtime stays up while the reconciler drains it,
// and the reconciler rewrites the bead's state to awake for as long as it does.
// A session's own conversation is not "already live" on some other session:
// while S's runtime is still up the seed is refused as "currently running",
// which the helper retries, and once it is down the seed lands.

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
			switch {
			case tc.runtimeUp:
				// The retryable refusal the running-session gate gives, decided from
				// the bead's state as read after the lookup.
				if want := fx.sessionID + " is currently running"; code != 1 || !strings.Contains(stderr, want) || strings.Contains(stderr, "already live") {
					t.Fatalf("resume of the session's own conversation while its runtime is up (state %q) = exit %d, stderr %q; want exit 1 carrying %q, not \"already live\"\nstdout: %s", tc.meta["state"], code, stderr, want, stdout)
				}
			case code != 0:
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
			// refused seed writes nothing.
			if got.Metadata["held_until"] != heldUntil || got.Metadata["sleep_intent"] != "user-hold" {
				t.Fatalf("the seed disturbed the hold of a session still draining for it: held_until=%q sleep_intent=%q, want %q and %q",
					got.Metadata["held_until"], got.Metadata["sleep_intent"], heldUntil, "user-hold")
			}
			if req := got.Metadata["wake_request"]; req != "" {
				t.Fatalf("the seed requested a wake (%q) for a session still draining for its hold", req)
			}
			if seeded := got.Metadata[session.ResumeSeededKey()]; seeded != "" {
				t.Fatalf("the refused seed marked a seeded resume (%s=%q) on a session still draining for its hold", session.ResumeSeededKey(), seeded)
			}
		})
	}
}

// Whether the session is running is decided from its bead as it reads after
// the lookup, not as it read before: here the session is awake when resume
// starts and asleep by the time the (slow) task lookup returns, and the seed of
// its own conversation lands.
func TestSessionResumeOfItsOwnConversationReadsTheStateAfterTheLookup(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, map[string]string{"session_key": currentConvID, "state": "awake"}, false)
	writeNamedTestSession(t, fx.liveRoot, fx.agentDir, currentConvID+".jsonl",
		historyTranscriptLines(currentConvID, "lana", fx.agentDir, "lana's current conversation")...)
	bindHistoryTestRig(t, fx, "far")

	var once sync.Once
	var stopErr error
	prevOpener := historyRigStoreOpener
	t.Cleanup(func() { historyRigStoreOpener = prevOpener })
	historyRigStoreOpener = func(*config.City) rigStoreOpener {
		return func(string, string) (beads.Store, error) {
			once.Do(func() {
				stopErr = fx.store.SetMetadataBatch(fx.sessionID, map[string]string{"state": "asleep", "sleep_reason": "user-hold"})
			})
			return beads.NewMemStore(), nil
		}
	}

	// A prefix, so the id is resolved through the task lookup.
	var stdout, stderr bytes.Buffer
	code := cmdSessionResume([]string{fx.sessionID, currentConvID[:8]}, false, false, t.TempDir(), &stdout, &stderr)
	if stopErr != nil {
		t.Fatalf("stopping the session during the lookup: %v", stopErr)
	}
	if code != 0 {
		t.Fatalf("resume of the session's own conversation, asleep by the time the lookup returned = exit %d; stderr=%s", code, stderr.String())
	}
	got, err := fx.store.Get(fx.sessionID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	if req := got.Metadata["wake_request"]; req == "" {
		t.Fatalf("the accepted seed requested no wake; metadata=%v", got.Metadata)
	}
}

// Excusing the session's own record from the "already live" guard must not
// excuse another session live on the same conversation: here the session stops
// during the lookup, and another one in its work dir still holds the id.
func TestSessionResumeOfItsOwnConversationStillRefusesAnotherSessionLiveOnIt(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, map[string]string{"session_key": currentConvID, "state": "awake"}, false)
	writeNamedTestSession(t, fx.liveRoot, fx.agentDir, currentConvID+".jsonl",
		historyTranscriptLines(currentConvID, "lana", fx.agentDir, "lana's current conversation")...)
	bindHistoryTestRig(t, fx, "far")
	other, err := fx.store.Create(beads.Bead{
		Title:  "ray",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "ray-runtime",
			"alias":        "ray",
			"agent_name":   "ray",
			"template":     "ray",
			"provider":     "claude",
			"state":        "awake",
			"work_dir":     fx.agentDir,
			"session_key":  currentConvID,
		},
	})
	if err != nil {
		t.Fatalf("create the other session bead: %v", err)
	}

	var once sync.Once
	var stopErr error
	prevOpener := historyRigStoreOpener
	t.Cleanup(func() { historyRigStoreOpener = prevOpener })
	historyRigStoreOpener = func(*config.City) rigStoreOpener {
		return func(string, string) (beads.Store, error) {
			once.Do(func() {
				stopErr = fx.store.SetMetadataBatch(fx.sessionID, map[string]string{"state": "asleep", "sleep_reason": "user-hold"})
			})
			return beads.NewMemStore(), nil
		}
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionResume([]string{fx.sessionID, currentConvID[:8]}, false, false, t.TempDir(), &stdout, &stderr)
	if stopErr != nil {
		t.Fatalf("stopping the session during the lookup: %v", stopErr)
	}
	if code == 0 {
		t.Fatalf("resume seeded a conversation another session (%s) is live on; stdout=%s", other.ID, stdout.String())
	}
	if want := "is already live on " + other.ID; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want the refusal to carry %q", stderr.String(), want)
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

// Resolving an id on the bead without the task lookup must not drop the guard
// that lookup fed: a conversation another session is live on is refused as
// already live on that session, here one running in the seat's task worktree.
func TestSessionResumeOfAConversationOnTheBeadRefusesAnotherSessionLiveOnIt(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, map[string]string{"session_key": currentConvID}, true)
	writeNamedTestSession(t, fx.liveRoot, fx.agentDir, currentConvID+".jsonl",
		historyTranscriptLines(currentConvID, "lana", fx.agentDir, "lana's current conversation")...)
	other, err := fx.store.Create(beads.Bead{
		Title:  "ray",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "ray-runtime",
			"alias":        "ray",
			"agent_name":   "ray",
			"template":     "ray",
			"provider":     "claude",
			"state":        "awake",
			"work_dir":     fx.worktree,
			"session_key":  currentConvID,
		},
	})
	if err != nil {
		t.Fatalf("create the other session bead: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionResume([]string{fx.sessionID, currentConvID}, false, false, t.TempDir(), &stdout, &stderr); code == 0 {
		t.Fatalf("resume seeded a conversation another session (%s) is live on; stdout=%s", other.ID, stdout.String())
	}
	if want := "is already live on " + other.ID; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want the refusal to carry %q", stderr.String(), want)
	}
	got, err := fx.store.Get(fx.sessionID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	if req := got.Metadata["wake_request"]; req != "" {
		t.Fatalf("the refused resume requested a wake (%q)", req)
	}
}

// The "already live" guard asks the store, by session_key, whether a session
// other than the target is live on the conversation resume settled on. A
// mutating resume that cannot get that answer refuses, and every refusal comes
// before every write.

// errSessionKeyQuery is the failure sessionKeyQueryFailingStore injects.
var errSessionKeyQuery = errors.New("injected session_key query failure")

// sessionKeyQueryFailingStore fails every list that filters on session_key and
// delegates everything else to Store. The bead-policy layer turns ListByMetadata
// into List, so both are covered.
type sessionKeyQueryFailingStore struct {
	beads.Store
}

func (s sessionKeyQueryFailingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if _, ok := query.Metadata["session_key"]; ok {
		return nil, errSessionKeyQuery
	}
	return s.Store.List(query)
}

func (s sessionKeyQueryFailingStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if _, ok := filters["session_key"]; ok {
		return nil, errSessionKeyQuery
	}
	return s.Store.ListByMetadata(filters, limit, opts...)
}

// injectSessionKeyQueryError makes every store the command opens a
// sessionKeyQueryFailingStore. The fixture's own store, opened before, is not.
func injectSessionKeyQueryError(t *testing.T) {
	t.Helper()
	prev := openStoreFactoryForCity
	t.Cleanup(func() { openStoreFactoryForCity = prev })
	openStoreFactoryForCity = func(ctx context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		result, err := prev(ctx, opts)
		if err == nil {
			result.Store = sessionKeyQueryFailingStore{Store: result.Store}
		}
		return result, err
	}
}

// failCityStoreOpen leaves the command without a bead store.
func failCityStoreOpen(t *testing.T) {
	t.Helper()
	prev := openStoreFactoryForCity
	t.Cleanup(func() { openStoreFactoryForCity = prev })
	openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{}, errors.New("injected: city store unavailable")
	}
}

// createExternalLiveSession records an awake session that holds conversation
// key and runs in a work dir none of the target's lookups search.
func createExternalLiveSession(t *testing.T, fx historyWorktreeFixture, key string) beads.Bead {
	t.Helper()
	other, err := fx.store.Create(beads.Bead{
		Title:  "ray",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "ray-runtime",
			"alias":        "ray",
			"agent_name":   "ray",
			"template":     "ray",
			"provider":     "claude",
			"state":        "awake",
			"work_dir":     filepath.Join(t.TempDir(), "elsewhere"),
			"session_key":  key,
		},
	})
	if err != nil {
		t.Fatalf("create the external session bead: %v", err)
	}
	return other
}

// userHeldSessionMeta is a session the resume-me helper has just suspended,
// with extra on top.
func userHeldSessionMeta(extra map[string]string) map[string]string {
	meta := map[string]string{
		"state":        "suspended",
		"sleep_intent": "user-hold",
		"held_until":   time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}
	maps.Copy(meta, extra)
	return meta
}

// sessionMetadata is a copy of the fixture session bead's metadata as it reads now.
func sessionMetadata(t *testing.T, fx historyWorktreeFixture) map[string]string {
	t.Helper()
	b, err := fx.store.Get(fx.sessionID)
	if err != nil {
		t.Fatalf("store.Get(session): %v", err)
	}
	return maps.Clone(b.Metadata)
}

// assertResumeWroteNothing fails unless the session bead's metadata is exactly
// before: no seed, no resume-seeded mark, no wake request, any hold intact.
func assertResumeWroteNothing(t *testing.T, fx historyWorktreeFixture, before map[string]string) {
	t.Helper()
	after := sessionMetadata(t, fx)
	for _, key := range []string{"session_key", "held_until", "sleep_intent", "wake_request", session.ResumeSeededKey()} {
		if after[key] != before[key] {
			t.Errorf("the refused resume changed %s: %q -> %q", key, before[key], after[key])
		}
	}
	if !maps.Equal(before, after) {
		t.Fatalf("the refused resume wrote the session bead\nbefore: %v\nafter:  %v", before, after)
	}
}

// liveElsewhereCheckFailure is the refusal a mutating resume gives when the
// store cannot say whether another session is live on conversation id.
func liveElsewhereCheckFailure(id string) string {
	return "checking whether conversation " + id + " is live elsewhere: " + errSessionKeyQuery.Error()
}

func TestSessionResumeRefusesWhenItCannotTellWhetherTheConversationIsLiveElsewhere(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, userHeldSessionMeta(map[string]string{"session_key": currentConvID}), false)
	writeNamedTestSession(t, fx.liveRoot, fx.agentDir, currentConvID+".jsonl",
		historyTranscriptLines(currentConvID, "lana", fx.agentDir, "lana's current conversation")...)
	injectSessionKeyQueryError(t)
	before := sessionMetadata(t, fx)

	var stdout, stderr bytes.Buffer
	code := cmdSessionResume([]string{fx.sessionID, currentConvID}, false, false, t.TempDir(), &stdout, &stderr)
	if want := liveElsewhereCheckFailure(currentConvID); code != 1 || !strings.Contains(stderr.String(), want) {
		t.Fatalf("resume with the live-elsewhere check failing = exit %d, stderr %q; want exit 1 carrying %q\nstdout: %s", code, stderr.String(), want, stdout.String())
	}
	assertResumeWroteNothing(t, fx, before)
}

// The guard keys on the conversation the prefix resolved to, so a prefix is
// refused exactly as the full id is, wherever the session holding it runs.
func TestSessionResumeOfAPrefixRefusesASessionLiveOnItOutsideTheSearchedDirs(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, map[string]string{"session_key": currentConvID, "prior_session_key": priorConvID}, false)
	writeNamedTestSession(t, fx.liveRoot, fx.agentDir, priorConvID+".jsonl",
		historyTranscriptLines(priorConvID, "lana", fx.agentDir, "lana's prior conversation")...)
	other := createExternalLiveSession(t, fx, priorConvID)
	before := sessionMetadata(t, fx)

	var stdout, stderr bytes.Buffer
	code := cmdSessionResume([]string{fx.sessionID, priorConvID[:8]}, false, false, t.TempDir(), &stdout, &stderr)
	if want := "conversation " + priorConvID + " is already live on " + other.ID; code != 1 || !strings.Contains(stderr.String(), want) {
		t.Fatalf("resume of a prefix of a conversation %s is live on = exit %d, stderr %q; want exit 1 carrying %q\nstdout: %s", other.ID, code, stderr.String(), want, stdout.String())
	}
	assertResumeWroteNothing(t, fx, before)
}

func TestSessionResumeLastSkipsAConversationLiveOutsideTheSearchedDirs(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, nil, false)
	newest := writeNamedTestSession(t, fx.liveRoot, fx.agentDir, priorConvID+".jsonl",
		historyTranscriptLines(priorConvID, "lana", fx.agentDir, "lana's newest conversation")...)
	now := time.Now()
	for path, mtime := range map[string]time.Time{
		newest: now.Add(2 * time.Hour),
		filepath.Join(filepath.Dir(newest), agentDirConvID+".jsonl"): now.Add(time.Hour),
	} {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("Chtimes(%s): %v", path, err)
		}
	}
	createExternalLiveSession(t, fx, priorConvID)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionResume([]string{fx.sessionID}, true, false, t.TempDir(), &stdout, &stderr); code != 0 {
		t.Fatalf("resume --last = exit %d; stderr=%s", code, stderr.String())
	}
	if key := sessionMetadata(t, fx)["session_key"]; key != agentDirConvID {
		t.Fatalf("resume --last seeded %q, want %q: the newest, %s, is live on another session", key, agentDirConvID, priorConvID)
	}
}

func TestSessionResumeLastRefusesWhenItCannotTellWhetherTheConversationIsLiveElsewhere(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, userHeldSessionMeta(nil), false)
	injectSessionKeyQueryError(t)
	before := sessionMetadata(t, fx)

	var stdout, stderr bytes.Buffer
	code := cmdSessionResume([]string{fx.sessionID}, true, false, t.TempDir(), &stdout, &stderr)
	if want := liveElsewhereCheckFailure(agentDirConvID); code != 1 || !strings.Contains(stderr.String(), want) {
		t.Fatalf("resume --last with the live-elsewhere check failing = exit %d, stderr %q; want exit 1 carrying %q\nstdout: %s", code, stderr.String(), want, stdout.String())
	}
	assertResumeWroteNothing(t, fx, before)
}

// archivedConversationOfARunningSeat sets up an awake seat, held for a suspend
// the reconciler is draining, and an archived conversation that is not the
// seat's own. It returns the archive root, the archived transcript and the
// path a restore would write.
func archivedConversationOfARunningSeat(t *testing.T) (fx historyWorktreeFixture, archiveRoot, archived, livePath string) {
	t.Helper()
	fx = setupHistoryWorktreeFixture(t, userHeldSessionMeta(map[string]string{"session_key": currentConvID, "state": "awake"}), false)
	archiveRoot = t.TempDir()
	archived = writeNamedTestSession(t, archiveRoot, fx.agentDir, priorConvID+".jsonl",
		historyTranscriptLines(priorConvID, "lana", fx.agentDir, "lana's archived conversation")...)
	livePath = filepath.Join(fx.liveRoot, filepath.Base(filepath.Dir(archived)), filepath.Base(archived))
	return fx, archiveRoot, archived, livePath
}

func TestSessionResumeOfAnArchivedConversationRefusesARunningSeatBeforeRestoringIt(t *testing.T) {
	fx, archiveRoot, archived, livePath := archivedConversationOfARunningSeat(t)
	before := sessionMetadata(t, fx)

	var stdout, stderr bytes.Buffer
	code := cmdSessionResume([]string{fx.sessionID, priorConvID}, false, false, archiveRoot, &stdout, &stderr)
	if want := fx.sessionID + " is currently running"; code != 1 || !strings.Contains(stderr.String(), want) {
		t.Fatalf("resume of an archived conversation for a running seat = exit %d, stderr %q; want exit 1 carrying %q\nstdout: %s", code, stderr.String(), want, stdout.String())
	}
	if _, err := os.Stat(archived); err != nil {
		t.Fatalf("the archived transcript is gone from %s: %v", archived, err)
	}
	if _, err := os.Stat(livePath); !os.IsNotExist(err) {
		t.Fatalf("the refused resume restored the transcript to %s (stat: %v)", livePath, err)
	}
	assertResumeWroteNothing(t, fx, before)
}

// --print is the side-channel dive into a running seat's conversation: it still
// restores the archived transcript and prints the command.
func TestSessionResumePrintRestoresAnArchivedConversationOfARunningSeat(t *testing.T) {
	fx, archiveRoot, archived, livePath := archivedConversationOfARunningSeat(t)
	before := sessionMetadata(t, fx)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionResume([]string{fx.sessionID, priorConvID}, false, true, archiveRoot, &stdout, &stderr); code != 0 {
		t.Fatalf("resume --print of an archived conversation for a running seat = exit %d; stderr=%s", code, stderr.String())
	}
	if want := fmt.Sprintf("cd %s && claude --resume %s\n", fx.agentDir, priorConvID); stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Fatalf("resume --print did not restore %s to %s: %v", archived, livePath, err)
	}
	if want := "Restored archived transcript to " + livePath; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	assertResumeWroteNothing(t, fx, before)
}

// "bead store unavailable — use --print" sends the operator to --print, so
// --print --last needs no answer from the store about who else is live.
func TestSessionResumePrintLastNeedsNoLiveElsewhereAnswer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		breakStore func(*testing.T)
		// stderrNote is what stderr must say about the check that failed.
		stderrNote string
	}{
		{name: "store unavailable", breakStore: failCityStoreOpen},
		{
			name:       "session_key query fails",
			breakStore: injectSessionKeyQueryError,
			stderrNote: "could not check whether conversation " + agentDirConvID + " is live elsewhere",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := setupHistoryWorktreeFixture(t, nil, false)
			// A single-session agent, so "lana" resolves from config without a store.
			agentToml := filepath.Join(fx.cityDir, "agents", "lana", "agent.toml")
			data, err := os.ReadFile(agentToml)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(agentToml, append(data, []byte("max_active_sessions = 1\n")...), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.breakStore(t)

			var stdout, stderr bytes.Buffer
			if code := cmdSessionResume([]string{"lana"}, true, true, t.TempDir(), &stdout, &stderr); code != 0 {
				t.Fatalf("resume --print --last = exit %d; stderr=%s", code, stderr.String())
			}
			if want := fmt.Sprintf("cd %s && claude --resume %s\n", fx.agentDir, agentDirConvID); stdout.String() != want {
				t.Fatalf("stdout = %q, want %q", stdout.String(), want)
			}
			if !strings.Contains(stderr.String(), tc.stderrNote) {
				t.Fatalf("stderr = %q, want it to carry %q", stderr.String(), tc.stderrNote)
			}
		})
	}
}
