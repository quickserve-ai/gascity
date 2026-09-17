package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/liveness"
)

// These tests pin the degraded-read marker against the binding's own state
// machine: a read that served committed metadata must say so however the
// binding's state moves while that read is in flight. They drive the real
// dial and retry through the two package seams, never through sleeps.

// swapLivenessDialSeams replaces the dialer and, when retry is non-nil, the
// background-retry launcher for the duration of the test.
func swapLivenessDialSeams(t *testing.T, dial func(string, string) (liveness.Store, error), retry func(func())) {
	t.Helper()
	origDial, origRetry := openScopeLivenessStoreFn, startLivenessRetry
	t.Cleanup(func() {
		openScopeLivenessStoreFn = origDial
		startLivenessRetry = origRetry
	})
	openScopeLivenessStoreFn = dial
	if retry != nil {
		startLivenessRetry = retry
	}
}

func livenessReadMarked(b beads.Bead) bool {
	return b.Metadata[liveness.ReadDegradedKey] == "true"
}

// seedHeldSession creates a session bead whose sleep_intent lives only in the
// liveness table, written through a healthy overlay over the same backing.
func seedHeldSession(t *testing.T, backing beads.Store, lv liveness.Store) beads.Bead {
	t.Helper()
	seed := wrapStoreWithBeadPolicies(backing, &config.City{}, newLivenessBindingForTest(lv, liveness.ModeTable))
	session := mustCreateSessionBead(t, seed, nil)
	if err := seed.SetMetadataBatch(session.ID, map[string]string{"sleep_intent": "user-hold"}); err != nil {
		t.Fatalf("seed SetMetadataBatch: %v", err)
	}
	return session
}

// TestLivenessOverlayMarksAReadWhoseRetryInstallsThePool pins counsel-astra's
// blocking race (fork PR #59, round 2): the acquisition that launches a
// background retry hands the read no pool, so the read serves committed
// metadata; if the retry installs the pool before the read asks whether the
// binding is degraded, a separate status check answers "healthy" and the
// fallback read comes back unmarked. The retry seam runs the dial inline, so
// the pool is installed before the launching acquisition returns.
func TestLivenessOverlayMarksAReadWhoseRetryInstallsThePool(t *testing.T) {
	lv := liveness.NewMemStore()
	swapLivenessDialSeams(t,
		func(string, string) (liveness.Store, error) { return lv, nil },
		func(dial func()) { dial() },
	)
	backing := beads.NewMemStore()
	session := seedHeldSession(t, backing, lv)

	binding := &livenessBinding{scopeRoot: t.TempDir(), mode: liveness.ModeTable}
	store := wrapStoreWithBeadPolicies(backing, &config.City{}, binding)
	if got := mustGetBead(t, store, session.ID); livenessReadMarked(got) || got.Metadata["sleep_intent"] != "user-hold" {
		t.Fatalf("precondition: the first read did not overlay through the dialed pool: %v", got.Metadata)
	}

	// The pool dies, and its backoff has already run out: the next
	// acquisition launches the retry.
	binding.noteOpError(errors.New("invalid connection"))
	binding.mu.Lock()
	binding.lastAttempt = time.Now().Add(-2 * livenessOpenRetryInterval)
	binding.mu.Unlock()

	got := mustGetBead(t, store, session.ID)
	if got.Metadata["sleep_intent"] != "" {
		t.Fatalf("precondition: the read was overlaid (%v); the interleaving did not happen", got.Metadata)
	}
	if !livenessReadMarked(got) {
		t.Errorf("a read that served committed metadata came back unmarked because the retry installed the pool before the degraded check: %v", got.Metadata)
	}

	// The retry did land: the next read is overlaid and healthy.
	if next := mustGetBead(t, store, session.ID); livenessReadMarked(next) || next.Metadata["sleep_intent"] != "user-hold" {
		t.Errorf("read after the retry = %v, want the overlaid intent and no marker", next.Metadata)
	}
}

// TestLivenessOverlayMarksAReadDuringTheInitialDial is the second shape of the
// same race: a read that arrives while the binding's FIRST dial is still out
// gets no pool and serves committed metadata, although the binding has never
// failed. With an endpoint configured and its dial unfinished, the read is
// degraded.
func TestLivenessOverlayMarksAReadDuringTheInitialDial(t *testing.T) {
	lv := liveness.NewMemStore()
	entered, release := make(chan struct{}), make(chan struct{})
	swapLivenessDialSeams(t, func(string, string) (liveness.Store, error) {
		close(entered)
		<-release
		return lv, nil
	}, nil)
	backing := beads.NewMemStore()
	session := seedHeldSession(t, backing, lv)

	binding := &livenessBinding{scopeRoot: t.TempDir(), mode: liveness.ModeTable}
	store := wrapStoreWithBeadPolicies(backing, &config.City{}, binding)

	type readResult struct {
		bead beads.Bead
		err  error
	}
	dialing := make(chan readResult, 1)
	go func() {
		b, err := store.Get(session.ID)
		dialing <- readResult{bead: b, err: err}
	}()
	<-entered

	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get during the initial dial: %v", err)
	}
	if got.Metadata["sleep_intent"] != "" {
		t.Fatalf("precondition: the concurrent read was overlaid (%v)", got.Metadata)
	}
	if !livenessReadMarked(got) {
		t.Errorf("a read during the initial dial served committed metadata unmarked: %v", got.Metadata)
	}

	close(release)
	first := <-dialing
	if first.err != nil {
		t.Fatalf("the dialing read failed: %v", first.err)
	}
	if livenessReadMarked(first.bead) || first.bead.Metadata["sleep_intent"] != "user-hold" {
		t.Errorf("the read that performed the dial = %v, want it overlaid and unmarked", first.bead.Metadata)
	}
}

// TestLivenessOverlayLeavesANoEndpointScopeUnmarked is the control: a scope
// with no liveness endpoint has only committed metadata, so its reads are exact
// — after its dial, during the long backoff, and through a retry that still
// finds no endpoint.
func TestLivenessOverlayLeavesANoEndpointScopeUnmarked(t *testing.T) {
	swapLivenessDialSeams(t,
		func(string, string) (liveness.Store, error) {
			return nil, fmt.Errorf("%w: test scope", errNoLivenessEndpoint)
		},
		func(dial func()) { dial() },
	)
	binding := &livenessBinding{scopeRoot: t.TempDir(), mode: liveness.ModeTable}
	store := wrapStoreWithBeadPolicies(beads.NewMemStore(), &config.City{}, binding)
	session := mustCreateSessionBead(t, store, map[string]string{"sleep_intent": "user-hold"})

	if got := mustGetBead(t, store, session.ID); livenessReadMarked(got) {
		t.Errorf("first read of a no-endpoint scope marked degraded: %v", got.Metadata)
	}
	if got := mustGetBead(t, store, session.ID); livenessReadMarked(got) {
		t.Errorf("read during the no-endpoint backoff marked degraded: %v", got.Metadata)
	}
	binding.mu.Lock()
	binding.lastAttempt = time.Now().Add(-2 * livenessNoEndpointRetryInterval)
	binding.mu.Unlock()
	if got := mustGetBead(t, store, session.ID); livenessReadMarked(got) || got.Metadata["sleep_intent"] != "user-hold" {
		t.Errorf("read through a retry that still finds no endpoint = %v, want committed metadata unmarked", got.Metadata)
	}
}

// TestLivenessAcquireForReadReportsTheReturnedHandle pins the accessor the
// overlay reads through: the degraded flag comes from the same critical
// section as the handle and describes THAT handle, whatever the binding does
// before the caller uses it.
func TestLivenessAcquireForReadReportsTheReturnedHandle(t *testing.T) {
	var none *livenessBinding
	if store, degraded := none.acquireForRead(); store != nil || degraded {
		t.Errorf("nil binding = (%v, %v), want (nil, false)", store, degraded)
	}

	lv := liveness.NewMemStore()
	entered, release := make(chan struct{}), make(chan struct{})
	outcome := "block"
	swapLivenessDialSeams(t, func(string, string) (liveness.Store, error) {
		switch outcome {
		case "block":
			close(entered)
			<-release
			return lv, nil
		case "up":
			return lv, nil
		case "no-endpoint":
			return nil, fmt.Errorf("%w: test scope", errNoLivenessEndpoint)
		default:
			return nil, errors.New("connection refused")
		}
	}, func(dial func()) { dial() })

	type acquired struct {
		store    liveness.Store
		degraded bool
	}
	b := &livenessBinding{scopeRoot: t.TempDir(), mode: liveness.ModeTable}

	// The first dial is out: a concurrent reader gets no pool and is degraded.
	first := make(chan acquired, 1)
	go func() {
		store, degraded := b.acquireForRead()
		first <- acquired{store, degraded}
	}()
	<-entered
	if store, degraded := b.acquireForRead(); store != nil || !degraded {
		t.Errorf("during the first dial = (%v, %v), want (nil, true)", store, degraded)
	}
	close(release)
	if got := <-first; got.store != lv || got.degraded {
		t.Errorf("the dialing acquisition = (%v, %v), want (the dialed store, false)", got.store, got.degraded)
	}
	if store, degraded := b.acquireForRead(); store != lv || degraded {
		t.Errorf("healthy = (%v, %v), want (the dialed store, false)", store, degraded)
	}

	// The pool is retired: degraded through the backoff.
	b.noteOpError(errors.New("invalid connection"))
	if store, degraded := b.acquireForRead(); store != nil || !degraded {
		t.Errorf("during the backoff = (%v, %v), want (nil, true)", store, degraded)
	}

	// The backoff runs out and the retry installs a pool before this call
	// returns. The call still handed out no pool, so it is still degraded.
	outcome = "up"
	b.mu.Lock()
	b.lastAttempt = time.Now().Add(-2 * livenessOpenRetryInterval)
	b.mu.Unlock()
	if store, degraded := b.acquireForRead(); store != nil || !degraded {
		t.Errorf("the acquisition that launched the retry = (%v, %v), want (nil, true)", store, degraded)
	}
	if store, degraded := b.acquireForRead(); store != lv || degraded {
		t.Errorf("after the retry landed = (%v, %v), want (the dialed store, false)", store, degraded)
	}

	// A scope with no endpoint is exact, on its dial and through its backoff.
	outcome = "no-endpoint"
	exact := &livenessBinding{scopeRoot: t.TempDir(), mode: liveness.ModeTable}
	for i := 0; i < 2; i++ {
		if store, degraded := exact.acquireForRead(); store != nil || degraded {
			t.Errorf("no-endpoint acquisition %d = (%v, %v), want (nil, false)", i, store, degraded)
		}
	}

	// A first dial that cannot reach the endpoint is degraded, and stays so
	// through the backoff.
	outcome = "down"
	unreachable := &livenessBinding{scopeRoot: t.TempDir(), mode: liveness.ModeTable}
	for i := 0; i < 2; i++ {
		if store, degraded := unreachable.acquireForRead(); store != nil || !degraded {
			t.Errorf("unreachable acquisition %d = (%v, %v), want (nil, true)", i, store, degraded)
		}
	}
}
