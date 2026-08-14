package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// ---------------------------------------------------------------------------
// ga-2otk73 iteration 3. Two residual holes found by a cross-family reviewer on
// the (otherwise cleared) iteration 2, plus one self-contradiction.
//
// HOLE 1. The confirmation window required N "stopped" readings spanning a
// wall-clock span, which proves the readings were taken APART IN TIME and
// nothing else. It never proved any reading came from a probe that SUCCEEDED.
// tmux's StateCache serves an empty "everything is stopped" snapshot once >30s
// have passed since its last successful refresh and keeps serving it while the
// fetch stays broken, and IsRunning and ProcessAlive both read that one
// snapshot — so a ~2.5 minute tmux wedge produced three identical degraded
// readings AND a degraded final re-check, and a LIVE session's bead was closed.
// All ~18 agents share one provider cache, and this city has a documented
// history of the tmux server wedging (ga-03ixvj, ga-0ecw5).
//
// HOLE 2. The pre-close fingerprint check compared two independently fetched
// snapshots while the real start-commit path (commitStartResult →
// sessFront.ApplyPatch) held no lock at all, so a commit could land after the
// re-read, after the compare, or inside the close.
//
// F1. The commit dropped the sweep's named carve-out arguing that an asleep
// named session is authoritatively stopped and unprotectable by any runtime
// gate — while the retained collision path still admitted StateAsleep through
// pendingCreateRollbackState.
// ---------------------------------------------------------------------------

// unattestedProvider reports the fleet as stopped and CANNOT attest that it
// looked — the tmux-cache-past-staleTTL shape, expressed at the provider seam.
// It is a real runtime.Provider (the fake's session map is empty, so the
// readings really are "stopped"); only the attestation is degraded.
type unattestedProvider struct {
	*runtime.Fake
}

func (p *unattestedProvider) AttestLiveness(name string, processNames []string) runtime.AttestedLiveness {
	return runtime.AttestedLiveness{
		Liveness: runtime.ObserveLiveness(p.Fake, name, processNames),
		Fresh:    false,
	}
}

// flakyAttestProvider attests according to a per-call script, so a chain can be
// built out of alternating good and degraded probes.
type flakyAttestProvider struct {
	*runtime.Fake
	mu    sync.Mutex
	fresh []bool
	calls int
}

func (p *flakyAttestProvider) AttestLiveness(name string, processNames []string) runtime.AttestedLiveness {
	p.mu.Lock()
	fresh := false
	if p.calls < len(p.fresh) {
		fresh = p.fresh[p.calls]
	}
	p.calls++
	p.mu.Unlock()
	return runtime.AttestedLiveness{
		Liveness: runtime.ObserveLiveness(p.Fake, name, processNames),
		Fresh:    fresh,
	}
}

// silentProvider implements runtime.Provider and nothing else. Embedding the
// INTERFACE (not the fake struct) keeps runtime.LivenessAttester out of its
// method set, which is how every provider that has not been taught to attest —
// acp, k8s, ssh, exec, subprocess, herdr, t3bridge, any pack runtime — presents
// itself.
type silentProvider struct {
	runtime.Provider
}

// HOLE 1, the decisive case. The provider reports stopped on every tick and
// cannot attest any of those readings. However many ticks elapse — here three
// full confirmation windows — the holder must keep its name.
//
// RED before the fix: the readings are indistinguishable from real deaths, the
// chain reaches namedNameReleaseConfirmTicks over namedNameReleaseConfirmWindow,
// and the bead is closed on tick 3 while the real session may be alive.
func TestSyncSessionBeads_NeverReleasesWhenProbeCannotAttestFreshness(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, map[string]string{
		"session_key": "sk-live-conversation",
	})
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	sp := &unattestedProvider{Fake: runtime.NewFake()}
	out := runSquatSyncTicks(t, namedNameReleaseConfirmTicks*3, store, cfg, sessionName, sp)

	if strings.Contains(out, "released session_name") {
		t.Fatalf("released the name off a probe that could not attest a fresh reading — a wedged tmux fetch reads exactly like a dead fleet: %q", out)
	}
	if !strings.Contains(out, "cannot attest") {
		t.Fatalf("stderr = %q, want the UNKNOWN-liveness diagnostic; a fleet-wide probe outage must not hide behind 'awaiting confirmation'", out)
	}
	// It must also not have banked ANY progress: a degraded tick is UNKNOWN, so
	// the chain resets rather than sitting one attested tick away from a close.
	got, err := mem.Get(holder.ID)
	if err != nil {
		t.Fatalf("Get(holder): %v", err)
	}
	if v := strings.TrimSpace(got.Metadata[namedNameReleaseProbeCountKey]); v != "" {
		t.Fatalf("%s = %q after only unattested ticks, want no banked progress", namedNameReleaseProbeCountKey, v)
	}
	assertBeadOpen(t, mem, holder.ID, "holder observed only through unattested probes")
}

// HOLE 1, the intermittent case — the one a wall-clock window is worst at. The
// probe alternates attested/degraded, so no three CONSECUTIVE attested readings
// ever occur, even though eight ticks and eight minutes pass and every reading
// says "stopped".
func TestSyncSessionBeads_UnattestedTickBreaksTheConfirmationChain(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	sp := &flakyAttestProvider{
		Fake:  runtime.NewFake(),
		fresh: []bool{true, true, false, true, true, false, true, true},
	}
	out := runSquatSyncTicks(t, len(sp.fresh), store, cfg, sessionName, sp)
	if strings.Contains(out, "released session_name") {
		t.Fatalf("released on a chain assembled from the good moments between degraded probes: %q", out)
	}
	assertBeadOpen(t, mem, holder.ID, "holder under an intermittently degraded probe")

	// Liveness check on the test itself: the same provider attesting every tick
	// MUST release, or this test would pass for the trivial reason that nothing
	// can ever release.
	sp.mu.Lock()
	sp.fresh = []bool{true, true, true}
	sp.calls = 0
	sp.mu.Unlock()
	out = runSquatSyncTicks(t, namedNameReleaseConfirmTicks, store, cfg, sessionName, sp)
	if !strings.Contains(out, "released session_name") {
		t.Fatalf("three consecutive ATTESTED stopped readings did not release the name: %q", out)
	}
}

// HOLE 1, the provider-capability case. A provider that cannot attest at all is
// UNKNOWN forever, so the recovery never fires for it. That is the deliberate
// trade — the wedge stays until a human intervenes, which is where it was before
// this fix — and it is pinned here so nobody "fixes" it by defaulting an
// unattestable provider to trusted.
func TestSyncSessionBeads_NeverReleasesWhenProviderCannotAttestAtAll(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	sp := &silentProvider{Provider: runtime.NewFake()}
	if _, ok := interface{}(sp).(runtime.LivenessAttester); ok {
		t.Fatal("precondition: silentProvider must NOT implement runtime.LivenessAttester")
	}
	out := runSquatSyncTicks(t, namedNameReleaseConfirmTicks*2, store, cfg, sessionName, sp)
	// Assert on the DIAGNOSTIC, not only on the bead: a released bead is reopened
	// by the very next tick, so "open at the end" alone would pass vacuously.
	if strings.Contains(out, "released session_name") {
		t.Fatalf("released the name off a provider that cannot attest anything: %q", out)
	}
	if !strings.Contains(out, "cannot attest") {
		t.Fatalf("stderr = %q, want the UNKNOWN-liveness diagnostic", out)
	}
	assertBeadOpen(t, mem, holder.ID, "holder behind a provider that cannot attest")
}

// hookedWriteStore fires onWrite before every SetMetadataBatch. It exists to
// land a concurrent start-commit at an exact point in the release sequence —
// after the gates have passed, before the guarded re-read.
type hookedWriteStore struct {
	beads.Store
	mu      sync.Mutex
	onWrite func(id string, kvs map[string]string)
}

func (s *hookedWriteStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.mu.Lock()
	hook := s.onWrite
	s.mu.Unlock()
	if hook != nil {
		hook(id, kvs)
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *hookedWriteStore) arm(fn func(id string, kvs map[string]string)) {
	s.mu.Lock()
	s.onWrite = fn
	s.mu.Unlock()
}

// HOLE 2, the abort. A start-commit taking the same per-bead start lock lands
// MID-SEQUENCE — after the tick's gates passed, before the guarded update
// re-reads — and the close must abort.
//
// This is the shape the reviewer named: commitStartResult writes
// CommitStartedPatch/ClearPendingCreateClaim through sessFront.ApplyPatch, which
// holds no name or identifier lock, so "the creator finished while we were
// deciding" is a real interleaving and not a theoretical one.
func TestSyncSessionBeads_AbortsReleaseWhenLockedStartCommitLandsMidSequence(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	split := &cacheSplitStore{Store: mem, hiddenID: holder.ID}
	hooked := &hookedWriteStore{Store: split}

	// Confirming ticks with the hook disarmed.
	runSquatSyncTicks(t, namedNameReleaseConfirmTicks-1, hooked, cfg, sessionName, runtime.NewFake())

	var committed bool
	hooked.arm(func(id string, kvs map[string]string) {
		// The releasing tick's confirmation write: gates have passed, the
		// guarded update has not started. Commit the create HERE, through the
		// same lock the start lane takes.
		if committed || id != holder.ID || strings.TrimSpace(kvs[namedNameReleaseProbeCountKey]) == "" {
			return
		}
		committed = true
		if err := session.WithSessionBeadStartLock("", holder.ID, func() error {
			return mem.SetMetadataBatch(holder.ID, map[string]string{
				"state":                string(session.StateActive),
				"pending_create_claim": "",
				"creation_complete_at": time.Now().UTC().Format(time.RFC3339),
			})
		}); err != nil {
			t.Errorf("committing the create mid-sequence: %v", err)
		}
	})

	out := runSquatSync(t, hooked, cfg, sessionName, runtime.NewFake())
	if !committed {
		t.Fatal("the concurrent start-commit never fired; the test proved nothing")
	}
	if strings.Contains(out, "released session_name") {
		t.Fatalf("closed a bead whose create had already committed: %q", out)
	}
	if !strings.Contains(out, "changed under us") {
		t.Fatalf("stderr = %q, want the guarded update to reject the changed pending-create attempt", out)
	}
	assertBeadOpen(t, mem, holder.ID, "holder whose create committed mid-sequence")
}

// HOLE 2, the exclusion. The compare above is only a compare-AND-swap if no
// participating writer can run between the guarded re-read and the close. This
// pins that: a start-lane writer that takes the per-bead start lock must NOT be
// able to acquire it while the release is inside its guarded update.
//
// RED before the fix: the release held no per-bead lock, so the competing writer
// acquires immediately and lands in the middle of the sequence — the exact
// interleaving the fingerprint compare cannot see, because the compare is
// already behind it.
func TestReleaseHoldsTheStartLockAcrossTheGuardedUpdate(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	split := &cacheSplitStore{Store: mem, hiddenID: holder.ID}
	hooked := &hookedGetStore{Store: split}

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks-1, hooked, cfg, sessionName, runtime.NewFake())

	acquired := make(chan struct{})
	done := make(chan struct{})
	var launch sync.Once
	hooked.mu.Lock()
	hooked.calls = nil
	hooked.onGet = func(id string, n int) {
		// Reads of the holder on the releasing tick, in order: (1) the raw read,
		// (2) the typed read the gates run against, (3) the guarded re-read,
		// which happens INSIDE the per-bead start lock.
		if id != holder.ID || n != 3 {
			return
		}
		launch.Do(func() {
			go func() {
				defer close(done)
				_ = session.WithSessionBeadStartLock("", holder.ID, func() error {
					close(acquired)
					return mem.SetMetadataBatch(holder.ID, map[string]string{
						"state":                string(session.StateActive),
						"pending_create_claim": "",
					})
				})
			}()
			select {
			case <-acquired:
				t.Errorf("a start-lane writer acquired the per-bead start lock while the release was between its re-read and its close — the close is a compare-then-write, not a guarded update")
			case <-time.After(250 * time.Millisecond):
				// Correctly blocked.
			}
		})
	}
	hooked.mu.Unlock()

	out := runSquatSync(t, hooked, cfg, sessionName, runtime.NewFake())

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the competing writer never acquired the start lock after the release finished — it is being held past the guarded update")
	}
	// The release won the race legitimately (it held the lock first), so it must
	// have completed; the loser lands after it. That ordering — never an
	// interleaving — is the whole point.
	if !strings.Contains(out, "released session_name") {
		t.Fatalf("stderr = %q, want the release to complete once it holds the lock", out)
	}
	assertBeadClosed(t, mem, holder.ID, "holder released while a competing writer waited on the lock")
}

// The iteration-2 suite's store doubles embed the beads.Store INTERFACE, which
// hides optional capabilities: beads.ConditionalWriterFor would report "no
// conditional writer" for a MemStore behind them and the release would silently
// take its unfenced fallback in every test. Exposing the handle is the sanctioned
// way for a wrapper to pass the capability through (beads.ConditionalWriterHandleProvider)
// and is declared here so the iteration-2 file stays byte-unchanged.
func (s *cacheSplitStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(s.Store)
}

func (s *hookedGetStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(s.Store)
}

func (s *hookedWriteStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(s.Store)
}

// HOLE 2, the store-level CAS. The close is issued as CloseIfMatch against the
// beads revision the gates were read at, so it is refused by the STORE if the
// bead moved at all — atomically with the close, not before it.
//
// This test mutates a metadata key the pending-create fingerprint does not
// cover, after the revision has been read and before the close. The fingerprint
// compare therefore PASSES and cannot save us; only the revision fence can. That
// separation is the point: the compare knows about the fields this recovery
// reasons about, the fence knows about every write.
//
// RED before the fix: the close was unconditional, so an interleaved write of an
// unrelated key was invisible and the bead was closed anyway.
func TestSyncSessionBeads_RevisionFenceRejectsACloseThatTheFingerprintMisses(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	split := &cacheSplitStore{Store: mem, hiddenID: holder.ID}
	hooked := &hookedGetStore{Store: split}

	// Precondition: the capability must reach the recovery THROUGH the test's
	// store doubles, or this test would pass for the trivial reason that the
	// fenced path never runs.
	if _, ok := beads.ConditionalWriterFor(hooked); !ok {
		t.Fatal("precondition: the fenced-close capability must be visible through the test store wrappers")
	}

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks-1, hooked, cfg, sessionName, runtime.NewFake())

	var interleaved bool
	hooked.mu.Lock()
	hooked.calls = nil
	hooked.onGet = func(id string, n int) {
		// Holder reads on the releasing tick: (1) raw, (2) typed, (3) the
		// guarded RAW read whose revision fences the close, (4) the guarded
		// typed read. Landing the write before (4) puts it strictly after the
		// fenced revision was captured.
		if id != holder.ID || n != 4 || interleaved {
			return
		}
		interleaved = true
		if err := mem.SetMetadata(id, "unrelated_note", "touched by someone else"); err != nil {
			t.Errorf("interleaving an unrelated write: %v", err)
		}
	}
	hooked.mu.Unlock()

	out := runSquatSync(t, hooked, cfg, sessionName, runtime.NewFake())
	if !interleaved {
		t.Fatal("the interleaved write never fired; the test proved nothing")
	}
	if strings.Contains(out, "changed under us (revision fence") == false {
		t.Fatalf("stderr = %q, want the STORE's revision fence to reject the close", out)
	}
	if strings.Contains(out, "released session_name") {
		t.Fatalf("closed a bead that moved after the gates read it: %q", out)
	}
	assertBeadOpen(t, mem, holder.ID, "holder mutated between the fenced read and the close")

	// And the fingerprint compare genuinely could NOT have caught it: the same
	// write leaves the attempt fingerprint identical.
	after, err := mem.Get(holder.ID)
	if err != nil {
		t.Fatalf("Get(holder): %v", err)
	}
	before := holder
	before.Metadata = map[string]string{}
	for k, v := range holder.Metadata {
		before.Metadata[k] = v
	}
	if pendingCreateAttemptFingerprint(sessionInfoForFingerprint(t, before)) !=
		pendingCreateAttemptFingerprint(sessionInfoForFingerprint(t, after)) {
		t.Fatal("the interleaved write changed the pending-create fingerprint; this test no longer isolates the revision fence")
	}
}

// sessionInfoForFingerprint projects a raw bead the way the recovery does, so a
// test can compare attempt fingerprints across two raw reads.
func sessionInfoForFingerprint(t *testing.T, b beads.Bead) session.Info {
	t.Helper()
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{Title: b.Title, Type: b.Type, Labels: b.Labels, Metadata: b.Metadata})
	if err != nil {
		t.Fatalf("Create(projection fixture): %v", err)
	}
	info, err := session.NewStore(beads.SessionStore{Store: store}).Get(created.ID)
	if err != nil {
		t.Fatalf("Get(projection fixture): %v", err)
	}
	// The synthetic fixture has its own ID; the fingerprint's ID component is
	// compared separately by samePendingCreateAttempt.
	info.ID = b.ID
	return info
}

// F1. An ASLEEP named session that still carries a pending-create claim reaches
// this path through pendingCreateRollbackState, which accepts StateAsleep. The
// sweep's named carve-out was dropped precisely because no runtime gate can
// protect a shape that is stopped by definition — so the collision path must not
// admit it either. It is strictly worse here: the close clears sleep_intent, so
// the successor comes back AWAKE, force-restarting a session a user put to sleep.
func TestSyncSessionBeads_KeepsAsleepNamedSessionNameHolderWithPendingCreateClaim(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	// The healed-to-asleep shape the reconciler documents: state=asleep, claim
	// still set, no last_woke_at — a never-started lease, long expired.
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, map[string]string{
		"state":        string(session.StateAsleep),
		"session_key":  "sk-live-conversation",
		"sleep_intent": "user",
	})
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	out := runSquatSyncTicks(t, namedNameReleaseConfirmTicks*2, store, cfg, sessionName, runtime.NewFake())
	if strings.Contains(out, "released session_name") {
		t.Fatalf("closed an ASLEEP named session: %q", out)
	}
	assertBeadOpen(t, mem, holder.ID, "asleep named holder carrying a pending-create claim")

	got, err := mem.Get(holder.ID)
	if err != nil {
		t.Fatalf("Get(holder): %v", err)
	}
	if v := strings.TrimSpace(got.Metadata["sleep_intent"]); v != "user" {
		t.Fatalf("sleep_intent = %q, want %q preserved — a reopen-eligible close still force-restarts the session", v, "user")
	}
}
