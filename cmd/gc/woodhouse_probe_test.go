package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ---------------------------------------------------------------------------
// WOODHOUSE ADVERSARIAL PROBES (ga-2otk73 iteration 3 review). Not part of the
// change; written to test claims the commit message makes.
// ---------------------------------------------------------------------------

// bdShapedStore is the PRODUCTION read shape: a store that implements
// beads.ConditionalWriter (CachingStore does) but whose Get returns
// Revision == 0, because bd 1.1.0 has no revision column and no --if-revision
// (internal/beads/bdstore.go:655-663 — "Pre-#4682 bd omits it, so it decodes to
// 0"). The controller runs CachingStore -> policy -> BdStore(bd 1.1.0).
type bdShapedStore struct {
	beads.Store
	mu           sync.Mutex
	closeIfMatch int
	getHook      func(id string, n int)
	getCalls     map[string]int
	txHook       func()
	txFired      bool
}

// sessionStateActiveForProbe avoids importing internal/session just for one
// constant; the literal is what CommitStartedPatch writes.
const sessionStateActiveForProbe = "active"

func (s *bdShapedStore) Tx(msg string, fn func(beads.Tx) error) error {
	s.mu.Lock()
	hook := s.txHook
	if hook != nil {
		s.txFired = true
		s.txHook = nil
	}
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.Store.Tx(msg, fn)
}

func (s *bdShapedStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	if s.getCalls == nil {
		s.getCalls = map[string]int{}
	}
	s.getCalls[id]++
	n := s.getCalls[id]
	hook := s.getHook
	s.mu.Unlock()
	if hook != nil {
		hook(id, n)
	}
	b, err := s.Store.Get(id)
	// bd strips it: the JSON key is absent, so it decodes to the zero value.
	b.Revision = 0
	return b, err
}

// ConditionalWriterHandle mirrors what CachingStore exposes: the capability IS
// present. Only the revision token is missing.
func (s *bdShapedStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	w, ok := beads.ConditionalWriterFor(s.Store)
	if !ok {
		return nil, false
	}
	return &countingConditionalWriter{ConditionalWriter: w, owner: s}, true
}

type countingConditionalWriter struct {
	beads.ConditionalWriter
	owner *bdShapedStore
}

func (w *countingConditionalWriter) CloseIfMatch(id string, expectedRevision int64) error {
	w.owner.mu.Lock()
	w.owner.closeIfMatch++
	w.owner.mu.Unlock()
	return w.ConditionalWriter.CloseIfMatch(id, expectedRevision)
}

// PROBE H2-a. On the store the controller actually runs, the "genuine
// store-level CAS" never executes: expectedRevision is 0, so
// closeDeadNamedSessionNameHolder's `expectedRevision > 0` guard skips the
// fenced path silently and takes the unconditional Tx close.
//
// The interleave is the SAME one the change's own
// TestSyncSessionBeads_RevisionFenceRejectsACloseThatTheFingerprintMisses uses:
// an unrelated metadata key written after the fenced revision was read. There
// the fence rejects the close. Here it does not, because there is no fence.
func TestWoodhouse_RevisionFenceIsInertOnARevisionlessStore(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	split := &cacheSplitStore{Store: mem, hiddenID: holder.ID}
	bdish := &bdShapedStore{Store: split}

	// Same precondition the change's test asserts: the capability is visible.
	if _, ok := beads.ConditionalWriterFor(bdish); !ok {
		t.Fatal("precondition: the conditional-write capability must be visible")
	}

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks-1, bdish, cfg, sessionName, runtime.NewFake())

	var interleaved bool
	bdish.mu.Lock()
	bdish.getCalls = nil
	bdish.getHook = func(id string, n int) {
		if id != holder.ID || n != 4 || interleaved {
			return
		}
		interleaved = true
		if err := mem.SetMetadata(id, "unrelated_note", "touched by someone else"); err != nil {
			t.Errorf("interleaving an unrelated write: %v", err)
		}
	}
	bdish.mu.Unlock()

	out := runSquatSync(t, bdish, cfg, sessionName, runtime.NewFake())
	if !interleaved {
		t.Fatal("the interleaved write never fired; the probe proved nothing")
	}
	bdish.mu.Lock()
	casCalls := bdish.closeIfMatch
	bdish.mu.Unlock()

	t.Logf("CloseIfMatch calls = %d; stderr = %q", casCalls, out)
	if casCalls != 0 {
		t.Fatalf("CloseIfMatch was called %d times on a revisionless store — the probe's premise is wrong", casCalls)
	}
	if strings.Contains(out, "revision fence") {
		t.Fatalf("a revision fence fired on a store with no revisions: %q", out)
	}
	if !strings.Contains(out, "released session_name") {
		t.Fatalf("expected the UNFENCED close to go through on a revisionless store; stderr = %q", out)
	}
	assertBeadClosed(t, mem, holder.ID, "holder closed with the CAS silently skipped")
}

// PROBE H2-b. Same store shape, but the interleaved write is a REAL start
// commit landing between the guarded re-read and the close, driven by a writer
// that does NOT take the per-bead start lock (any writer outside the three
// start-lane sites: the API server, gc session CLI, a bd CLI metadata write, a
// heartbeat). With no CAS and no shared lock, nothing stops the close.
func TestWoodhouse_UnlockedWriterCanLandInsideTheGuardedUpdateOnARevisionlessStore(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	split := &cacheSplitStore{Store: mem, hiddenID: holder.ID}
	bdish := &bdShapedStore{Store: split}

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks-1, bdish, cfg, sessionName, runtime.NewFake())

	// Land the commit at the instant the close's own transaction opens: strictly
	// AFTER the guarded re-read and the fingerprint compare, i.e. inside the
	// residual window. This is the exact interleaving the reviewer named.
	bdish.mu.Lock()
	bdish.txHook = func() {
		// NOTE: no session.WithSessionBeadStartLock. This models every writer
		// that is not one of the three start-lane sites the change taught to
		// take it — the API server's own writers, a bd CLI metadata write, any
		// out-of-process start commit whose cfg.Env carries no GC_CITY_PATH.
		if err := mem.SetMetadataBatch(holder.ID, map[string]string{
			"state":                string(sessionStateActiveForProbe),
			"pending_create_claim": "",
			"creation_complete_at": time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			t.Errorf("landing the out-of-band start commit: %v", err)
		}
	}
	bdish.mu.Unlock()

	out := runSquatSync(t, bdish, cfg, sessionName, runtime.NewFake())
	bdish.mu.Lock()
	fired := bdish.txFired
	bdish.mu.Unlock()
	if !fired {
		t.Fatal("the out-of-band start commit never fired; the probe proved nothing")
	}
	t.Logf("stderr = %q", out)
	got, err := mem.Get(holder.ID)
	if err != nil {
		t.Fatalf("Get(holder): %v", err)
	}
	t.Logf("RESULT: holder status = %q after a start commit landed inside the close window", got.Status)
	if got.Status == "closed" {
		t.Log("RESULT: a bead whose create had ALREADY committed was closed — the residual TOCTOU window is open on a revisionless store")
	}
}
