package beads

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// These tests extend native_dolt_store_handle_drain_test.go to the cases a
// single outstanding use cannot tell apart: several uses on a retired handle,
// a release called twice, writes that span a reconnect, CloseStore racing
// operations that are running on a handle a reconnect already retired, and a
// read that panics (ga-yuiof4.1 review rounds 2 and 3).
//
// Where a test asserts that a CloseStore has NOT returned, it first waits for
// the store's test-only checkpoint (beforeCloseDrain / beforeCloseWaitForFirst)
// so the closer is known to have reached the wait under test, and it checks
// the ordering that matters with a reading taken on the closer's own goroutine
// the moment it returns, not with a timing window alone.

// lifecycleSpy is a spy storage that records what the handle lifecycle tests
// assert on. A read of failID fails with a transient connection error, which
// sends the read through reconnect; every other read succeeds. A transaction
// for which shouldHold reports true parks until releaseTx; txResult, when set,
// decides the n-th transaction's result. With blockClose set, Close parks until
// releaseClose. It counts Close calls, and counts any operation that started
// or finished after Close had been called on it.
type lifecycleSpy struct {
	*nativeDoltStorageSpy
	closes        atomic.Int32
	closeReturned atomic.Int32 // Close calls that have returned
	reads         atomic.Int32
	txs           atomic.Int32
	active        atomic.Int32 // operations currently running on this spy
	ranClosed     atomic.Int32 // operations that overlapped a Close of this spy

	// The fields below are set before the spy is handed to a store.
	shouldHold func(commitMsg string) bool
	txResult   func(n int32) error
	delay      func() // runs inside every operation, to widen its window
	onClose    func() // runs at the start of Close, to take a reading
	blockClose bool

	txEntered    chan struct{}
	txRelease    chan struct{}
	closeEntered chan struct{}
	closeRelease chan struct{}
	enterOnce    sync.Once
	releaseOnce  sync.Once
	closeOnce    sync.Once
	unblockOnce  sync.Once
}

func newLifecycleSpy(failID string, holdTx bool) *lifecycleSpy {
	l := &lifecycleSpy{
		shouldHold:   func(string) bool { return holdTx },
		txEntered:    make(chan struct{}),
		txRelease:    make(chan struct{}),
		closeEntered: make(chan struct{}),
		closeRelease: make(chan struct{}),
	}
	l.nativeDoltStorageSpy = &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			defer l.enter()()
			l.reads.Add(1)
			id := ""
			if len(f.IDs) > 0 {
				id = f.IDs[0]
			}
			if failID != "" && id == failID {
				return nil, errors.New("begin read tx: invalid connection")
			}
			return []*beadslib.Issue{spyIssue(id)}, nil
		},
		runInTransaction: func(_ context.Context, commitMsg string, _ func(beadslib.Transaction) error) error {
			defer l.enter()()
			n := l.txs.Add(1)
			if l.shouldHold(commitMsg) {
				l.enterOnce.Do(func() { close(l.txEntered) })
				<-l.txRelease
			}
			if l.txResult != nil {
				return l.txResult(n)
			}
			return nil
		},
		close: func() error {
			l.closes.Add(1)
			if l.onClose != nil {
				l.onClose()
			}
			if l.blockClose {
				l.closeOnce.Do(func() { close(l.closeEntered) })
				<-l.closeRelease
			}
			l.closeReturned.Add(1)
			return nil
		},
	}
	return l
}

// enter records an operation starting on the spy and returns its end. Both
// ends check for a Close that has already happened: no operation may run on a
// closed handle.
func (l *lifecycleSpy) enter() func() {
	if l.closes.Load() > 0 {
		l.ranClosed.Add(1)
	}
	l.active.Add(1)
	if l.delay != nil {
		l.delay()
	}
	return func() {
		if l.closes.Load() > 0 {
			l.ranClosed.Add(1)
		}
		l.active.Add(-1)
	}
}

func (l *lifecycleSpy) releaseTx()    { l.releaseOnce.Do(func() { close(l.txRelease) }) }
func (l *lifecycleSpy) releaseClose() { l.unblockOnce.Do(func() { close(l.closeRelease) }) }

// storeReopeningTo builds a test store on old whose reopen hook hands back
// fresh.
func storeReopeningTo(old, fresh beadslib.Storage) *NativeDoltStore {
	store := newNativeDoltStoreForTest(old)
	store.reopen = func(context.Context) (beadslib.Storage, error) { return fresh, nil }
	return store
}

// newCheckpoint returns a hook for one of the store's test-only checkpoints
// and a channel closed the first time the hook runs.
func newCheckpoint() (func(), <-chan struct{}) {
	reached := make(chan struct{})
	var once sync.Once
	return func() { once.Do(func() { close(reached) }) }, reached
}

// waitErr waits for one result from done and fails the test if none arrives
// within the bound, so a regression that parks the operation fails the test
// instead of hanging it until the go test timeout. Tests that park an
// operation register its release with t.Cleanup, so a failure here also lets
// the parked goroutine finish.
func waitErr(t *testing.T, done <-chan error, within time.Duration, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatalf("%s did not return within %s", what, within)
		return nil
	}
}

func startGet(store *NativeDoltStore, id string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := store.Get(id)
		done <- err
	}()
	return done
}

func startUpdate(store *NativeDoltStore, id string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- store.Update(id, UpdateOpts{}) }()
	return done
}

// getWithin runs store.Get(id) under a bound; see waitErr.
func getWithin(t *testing.T, store *NativeDoltStore, id string, within time.Duration) error {
	t.Helper()
	return waitErr(t, startGet(store, id), within, fmt.Sprintf("Get(%q)", id))
}

// updateWithin runs store.Update(id) under a bound; see waitErr.
func updateWithin(t *testing.T, store *NativeDoltStore, id string, within time.Duration) error {
	t.Helper()
	return waitErr(t, startUpdate(store, id), within, fmt.Sprintf("Update(%q)", id))
}

// waitForCloseLatch waits until CloseStore has latched the store closed. It
// never blocks on s.mu, so a store whose CloseStore queues a writer behind a
// running operation fails the test instead of hanging it.
func waitForCloseLatch(t *testing.T, store *NativeDoltStore, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if store.mu.TryRLock() {
			latched := store.closed
			store.mu.RUnlock()
			if latched {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("CloseStore did not latch the store closed within %s", within)
}

// startCloseStore runs CloseStore on its own goroutine and returns its result.
func startCloseStore(store *NativeDoltStore) <-chan error {
	done := make(chan error, 1)
	go func() { done <- store.CloseStore() }()
	return done
}

// closeStoreResult is what one CloseStore call returned, plus a reading taken on
// the closer's own goroutine the moment it returned.
type closeStoreResult struct {
	err      error
	observed int32
}

// startCloseStoreObserving runs CloseStore on its own goroutine and, as soon
// as it returns, records observe(). Taking the reading there, rather than on
// the test goroutine later, pins what was true when CloseStore returned.
func startCloseStoreObserving(store *NativeDoltStore, observe func() int32) <-chan closeStoreResult {
	done := make(chan closeStoreResult, 1)
	go func() {
		err := store.CloseStore()
		done <- closeStoreResult{err: err, observed: observe()}
	}()
	return done
}

func waitCloseResult(t *testing.T, done <-chan closeStoreResult, within time.Duration, what string) closeStoreResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(within):
		t.Fatalf("%s did not return within %s", what, within)
		return closeStoreResult{}
	}
}

// TestNativeDoltStoreRetiredHandleStaysOpenUntilEveryUseReleases: with two
// uses outstanding on a handle a reconnect retires, releasing the first must
// not close it; releasing the second closes it exactly once.
func TestNativeDoltStoreRetiredHandleStaysOpenUntilEveryUseReleases(t *testing.T) {
	old := newLifecycleSpy("gc-fail", false)
	fresh := newLifecycleSpy("", false)
	store := storeReopeningTo(old, fresh)

	_, _, releaseA, err := store.acquireStorageGen()
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	t.Cleanup(releaseA)
	_, _, releaseB, err := store.acquireStorageGen()
	if err != nil {
		t.Fatalf("acquire B: %v", err)
	}
	t.Cleanup(releaseB)
	if err := getWithin(t, store, "gc-fail", 5*time.Second); err != nil {
		t.Fatalf("Get across reconnect: %v", err)
	}

	releaseA()
	time.Sleep(closeSettle)
	if got := old.closes.Load(); got != 0 {
		t.Fatalf("retired handle Close calls after the first of two uses released = %d, want 0", got)
	}

	releaseB()
	if got := waitForCount(old.closes.Load, 1, 5*time.Second); got != 1 {
		t.Fatalf("retired handle Close calls after the last use released = %d, want 1", got)
	}
	time.Sleep(closeSettle)
	if got := old.closes.Load(); got != 1 {
		t.Fatalf("retired handle Close calls = %d, want exactly 1", got)
	}
	if got := fresh.closes.Load(); got != 0 {
		t.Fatalf("current handle Close calls = %d, want 0", got)
	}
}

// TestNativeDoltStoreDoubleReleaseDoesNotDrainAnotherUse: calling one release
// twice while another use of the same retired handle is still running must
// not close the handle under that other use.
func TestNativeDoltStoreDoubleReleaseDoesNotDrainAnotherUse(t *testing.T) {
	old := newLifecycleSpy("gc-fail", false)
	fresh := newLifecycleSpy("", false)
	store := storeReopeningTo(old, fresh)

	_, _, releaseA, err := store.acquireStorageGen()
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	t.Cleanup(releaseA)
	_, _, releaseB, err := store.acquireStorageGen()
	if err != nil {
		t.Fatalf("acquire B: %v", err)
	}
	t.Cleanup(releaseB)
	if err := getWithin(t, store, "gc-fail", 5*time.Second); err != nil {
		t.Fatalf("Get across reconnect: %v", err)
	}

	releaseA()
	releaseA()
	time.Sleep(closeSettle)
	if got := old.closes.Load(); got != 0 {
		t.Fatalf("retired handle Close calls after one release ran twice with another use outstanding = %d, want 0", got)
	}

	releaseB()
	if got := waitForCount(old.closes.Load, 1, 5*time.Second); got != 1 {
		t.Fatalf("retired handle Close calls after the last use released = %d, want 1", got)
	}
	time.Sleep(closeSettle)
	if got := old.closes.Load(); got != 1 {
		t.Fatalf("retired handle Close calls = %d, want exactly 1", got)
	}
}

// TestNativeDoltStoreWriteSpanningReconnectCompletesOnItsOriginalHandle: a
// write whose first attempt loses a serialization race after a reconnect has
// swapped in a fresh handle retries on the handle it started on (the
// transaction it repeats belongs to that handle), that handle closes only
// after the write, and later writes go to the fresh handle.
func TestNativeDoltStoreWriteSpanningReconnectCompletesOnItsOriginalHandle(t *testing.T) {
	old := newLifecycleSpy("gc-fail", true)
	old.txResult = func(n int32) error {
		if n == 1 {
			return errors.New("Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction")
		}
		return nil
	}
	t.Cleanup(old.releaseTx)
	fresh := newLifecycleSpy("", false)
	store := storeReopeningTo(old, fresh)

	writeDone := startUpdate(store, "gc-w")
	select {
	case <-old.txEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("write never reached the original handle")
	}

	if err := getWithin(t, store, "gc-fail", 5*time.Second); err != nil {
		t.Fatalf("Get across reconnect: %v", err)
	}
	time.Sleep(closeSettle)
	if got := old.closes.Load(); got != 0 {
		t.Fatalf("original handle Close calls while a write is running on it = %d, want 0", got)
	}

	// The first attempt now fails with a serialization conflict; the retry
	// must run on the original handle, not the fresh one now current.
	old.releaseTx()
	if err := waitErr(t, writeDone, 5*time.Second, "write spanning the reconnect"); err != nil {
		t.Fatalf("write spanning the reconnect: %v", err)
	}
	if got := old.txs.Load(); got != 2 {
		t.Fatalf("transactions on the original handle = %d, want 2 (the conflicting attempt and its retry)", got)
	}
	if got := fresh.txs.Load(); got != 0 {
		t.Fatalf("transactions on the fresh handle = %d, want 0 (the retry must stay on the original handle)", got)
	}
	if got := waitForCount(old.closes.Load, 1, 5*time.Second); got != 1 {
		t.Fatalf("original handle Close calls after the write released it = %d, want 1", got)
	}
	if got := old.ranClosed.Load(); got != 0 {
		t.Fatalf("operations that overlapped a Close of the original handle = %d, want 0", got)
	}

	if err := updateWithin(t, store, "gc-w2", 5*time.Second); err != nil {
		t.Fatalf("write after reconnect: %v", err)
	}
	if got := fresh.txs.Load(); got != 1 {
		t.Fatalf("transactions on the fresh handle after reconnect = %d, want 1", got)
	}
	if got := old.txs.Load(); got != 2 {
		t.Fatalf("transactions on the retired handle = %d, want still 2", got)
	}
}

// TestNativeDoltStoreCloseStoreWaitsForWriteOnRetiredHandle: CloseStore must
// not return while any operation that started before it is still running,
// including one running on a handle a reconnect has already retired.
func TestNativeDoltStoreCloseStoreWaitsForWriteOnRetiredHandle(t *testing.T) {
	old := newLifecycleSpy("gc-fail", true)
	t.Cleanup(old.releaseTx)
	fresh := newLifecycleSpy("", false)
	// Taken when the current handle is closed: the write on the retired handle
	// must have finished by then.
	var oldActiveAtFreshClose atomic.Int32
	oldActiveAtFreshClose.Store(-1)
	fresh.onClose = func() { oldActiveAtFreshClose.Store(old.active.Load()) }
	store := storeReopeningTo(old, fresh)
	atDrain, reachedDrain := newCheckpoint()
	store.beforeCloseDrain = atDrain

	writeDone := startUpdate(store, "gc-w")
	select {
	case <-old.txEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("write never reached the original handle")
	}
	if err := getWithin(t, store, "gc-fail", 5*time.Second); err != nil {
		t.Fatalf("Get across reconnect: %v", err)
	}

	closeDone := startCloseStoreObserving(store, old.active.Load)
	select {
	case <-reachedDrain:
	case res := <-closeDone:
		t.Fatalf("CloseStore returned (err=%v, operations still running on the retired handle=%d) without reaching its wait for operations in flight", res.err, res.observed)
	case <-time.After(5 * time.Second):
		t.Fatal("CloseStore never reached its wait for operations in flight")
	}
	// Past the checkpoint the closer is in its wait, which the running write
	// must hold open.
	select {
	case res := <-closeDone:
		t.Fatalf("CloseStore returned (%v) while a write that started before it was still running on the handle a reconnect retired", res.err)
	case <-time.After(closeSettle):
	}
	if err := getWithin(t, store, "gc-new", 5*time.Second); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("Get after the close latch = %v, want ErrStoreClosed", err)
	}

	old.releaseTx()
	if err := waitErr(t, writeDone, 5*time.Second, "write"); err != nil {
		t.Fatalf("write: %v", err)
	}
	res := waitCloseResult(t, closeDone, 5*time.Second, "CloseStore after the write released")
	if res.err != nil {
		t.Fatalf("CloseStore: %v", res.err)
	}
	if res.observed != 0 {
		t.Fatalf("operations running on the retired handle when CloseStore returned = %d, want 0", res.observed)
	}
	if got := oldActiveAtFreshClose.Load(); got != 0 {
		t.Fatalf("operations running on the retired handle when CloseStore closed the current one = %d, want 0", got)
	}
	if got := fresh.closes.Load(); got != 1 {
		t.Fatalf("current handle Close calls when CloseStore returned = %d, want 1", got)
	}
	if got := waitForCount(old.closes.Load, 1, 5*time.Second); got != 1 {
		t.Fatalf("retired handle Close calls = %d, want 1", got)
	}
	if got := old.ranClosed.Load(); got != 0 {
		t.Fatalf("operations that overlapped a Close of the retired handle = %d, want 0", got)
	}
}

// TestNativeDoltStoreSecondCloseStoreWaitsForTheFirstToFinish: a CloseStore
// that finds the store already closing must not return until the first one
// has finished: after every operation in flight has released, and after the
// first closer's Close of the handle has itself returned.
func TestNativeDoltStoreSecondCloseStoreWaitsForTheFirstToFinish(t *testing.T) {
	spy := newLifecycleSpy("", true)
	spy.blockClose = true
	t.Cleanup(spy.releaseTx)
	t.Cleanup(spy.releaseClose)
	store := newNativeDoltStoreForTest(spy)
	atDrain, reachedDrain := newCheckpoint()
	store.beforeCloseDrain = atDrain
	atWaitForFirst, reachedWaitForFirst := newCheckpoint()
	store.beforeCloseWaitForFirst = atWaitForFirst

	writeDone := startUpdate(store, "gc-w")
	select {
	case <-spy.txEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("write never reached the handle")
	}

	first := startCloseStoreObserving(store, spy.closeReturned.Load)
	select {
	case <-reachedDrain:
	case res := <-first:
		t.Fatalf("first CloseStore returned (%v) without reaching its wait for operations in flight", res.err)
	case <-time.After(5 * time.Second):
		t.Fatal("first CloseStore never reached its wait for operations in flight")
	}
	second := startCloseStoreObserving(store, spy.closeReturned.Load)
	select {
	case <-reachedWaitForFirst:
	case res := <-second:
		t.Fatalf("second CloseStore returned (%v) without waiting for the first, which was still waiting for a write in flight", res.err)
	case <-time.After(5 * time.Second):
		t.Fatal("second CloseStore never reached its wait for the first")
	}
	select {
	case res := <-second:
		t.Fatalf("second CloseStore returned (%v) while the first was still waiting for a write in flight", res.err)
	case res := <-first:
		t.Fatalf("first CloseStore returned (%v) while a write was in flight", res.err)
	case <-time.After(closeSettle):
	}

	// Let the write finish: the first closer goes on to Close the handle,
	// which parks until the test releases it.
	spy.releaseTx()
	if err := waitErr(t, writeDone, 5*time.Second, "write"); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-spy.closeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first CloseStore never called Close on the handle after the write released")
	}
	select {
	case res := <-second:
		t.Fatalf("second CloseStore returned (%v) while the first was still inside the handle's Close", res.err)
	case res := <-first:
		t.Fatalf("first CloseStore returned (%v) while the handle's Close had not returned", res.err)
	case <-time.After(closeSettle):
	}

	spy.releaseClose()
	for name, done := range map[string]<-chan closeStoreResult{"first": first, "second": second} {
		res := waitCloseResult(t, done, 5*time.Second, name+" CloseStore")
		if res.err != nil {
			t.Fatalf("%s CloseStore: %v", name, res.err)
		}
		if res.observed != 1 {
			t.Fatalf("%s CloseStore returned when the handle's Close had returned %d times, want 1", name, res.observed)
		}
	}
	if got := spy.closes.Load(); got != 1 {
		t.Fatalf("handle Close calls = %d, want exactly 1", got)
	}
}

// TestNativeDoltStoreReadPanicReleasesItsStorageUse: a read that panics
// (recovered further up, as an HTTP handler does) must still release its
// storage use, or CloseStore would wait for it forever.
func TestNativeDoltStoreReadPanicReleasesItsStorageUse(t *testing.T) {
	spy := newLifecycleSpy("", false)
	store := newNativeDoltStoreForTest(spy)

	recovered := func() (r any) {
		defer func() { r = recover() }()
		_ = store.withReadRetry(func(context.Context, beadslib.Storage) error {
			panic("read blew up")
		})
		return nil
	}()
	if recovered == nil {
		t.Fatal("withReadRetry did not propagate the read's panic")
	}

	if err := waitErr(t, startCloseStore(store), 5*time.Second, "CloseStore after a read panicked"); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	if got := spy.closes.Load(); got != 1 {
		t.Fatalf("handle Close calls = %d, want 1", got)
	}
}

// TestNativeDoltStoreHandleLifecycleStress runs reads, writes and
// reconnect-forcing reads on many goroutines, with one write parked on the
// initial handle while reconnects retire it, then shuts down with two
// concurrent CloseStore calls while everything is still running. Neither
// CloseStore may return while any operation is still running, on any handle;
// by the end every handle the store opened is closed exactly once and no
// operation ran on a closed handle.
func TestNativeDoltStoreHandleLifecycleStress(t *testing.T) {
	const parkedID = "gc-parked"
	var spiesMu sync.Mutex
	var spies []*lifecycleSpy
	snapshot := func() []*lifecycleSpy {
		spiesMu.Lock()
		defer spiesMu.Unlock()
		return append([]*lifecycleSpy(nil), spies...)
	}
	totalActive := func() int32 {
		var n int32
		for _, l := range snapshot() {
			n += l.active.Load()
		}
		return n
	}
	newSpy := func() *lifecycleSpy {
		l := newLifecycleSpy("gc-fail", false)
		l.shouldHold = func(commitMsg string) bool { return strings.Contains(commitMsg, parkedID) }
		l.delay = func() { time.Sleep(time.Duration(rand.IntN(300)) * time.Microsecond) }
		spiesMu.Lock()
		spies = append(spies, l)
		spiesMu.Unlock()
		return l
	}
	initial := newSpy()
	t.Cleanup(initial.releaseTx)
	store := newNativeDoltStoreForTest(initial)
	store.readRetryBudgetOverride = 50 * time.Millisecond
	store.reopen = func(context.Context) (beadslib.Storage, error) { return newSpy(), nil }
	atDrain, reachedDrain := newCheckpoint()
	store.beforeCloseDrain = atDrain
	atWaitForFirst, reachedWaitForFirst := newCheckpoint()
	store.beforeCloseWaitForFirst = atWaitForFirst

	// One write parks on the initial handle; the reconnects below retire that
	// handle while the write is still running on it.
	parkedDone := startUpdate(store, parkedID)
	select {
	case <-initial.txEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("parked write never reached the initial handle")
	}

	var stop atomic.Bool
	t.Cleanup(func() { stop.Store(true) })
	const workers = 16
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				var err error
				switch (w + i) % 4 {
				case 0:
					_, err = store.Get("gc-fail")
				case 1:
					err = store.Update(fmt.Sprintf("gc-w-%d", w), UpdateOpts{})
				default:
					_, err = store.Get(fmt.Sprintf("gc-%d-%d", w, i))
				}
				if errors.Is(err, ErrStoreClosed) {
					return
				}
			}
		}(w)
	}

	// Shut down only once activity is observed: a reconnect has installed a
	// fresh handle (retiring the initial one under the parked write), and
	// reads and writes have run.
	//
	// generation never blocks on s.mu: on a store that held s.mu across the
	// parked write, reconnect would queue a writer and a blocking RLock here
	// would hang the test past its deadline check. While s.mu is unavailable
	// it reports 0, and the loop retries on its next pass.
	generation := func() uint64 {
		if !store.mu.TryRLock() {
			return 0
		}
		defer store.mu.RUnlock()
		return store.generation
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var reads, txs int32
		for _, l := range snapshot() {
			reads += l.reads.Load()
			txs += l.txs.Load()
		}
		gen := generation()
		if gen >= 1 && reads >= 50 && txs >= 10 {
			t.Logf("activity before shutdown: generation=%d reads=%d txs=%d", gen, reads, txs)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no activity within 5s: generation=%d (0 also when s.mu was unavailable) reads=%d txs=%d", gen, reads, txs)
		}
		time.Sleep(time.Millisecond)
	}
	if got := initial.active.Load(); got < 1 {
		t.Fatalf("operations running on the retired initial handle at shutdown = %d, want at least the parked write", got)
	}

	// Two concurrent closers. Whichever latches waits for the operations in
	// flight; the other waits for it. Each records how many operations were
	// still running, on any handle, the moment it returned.
	type namedResult struct {
		name string
		closeStoreResult
	}
	results := make(chan namedResult, 2)
	for _, name := range []string{"closer A", "closer B"} {
		go func(name string) {
			err := store.CloseStore()
			results <- namedResult{name: name, closeStoreResult: closeStoreResult{err: err, observed: totalActive()}}
		}(name)
	}
	var got []namedResult
	drainReached, waitReached := reachedDrain, reachedWaitForFirst
	waitDeadline := time.After(5 * time.Second)
	for drainReached != nil || (waitReached != nil && len(got) == 0) {
		select {
		case <-drainReached:
			drainReached = nil
		case <-waitReached:
			waitReached = nil
		case r := <-results:
			got = append(got, r)
		case <-waitDeadline:
			t.Fatalf("closers did not reach their waits within 5s (drain reached=%v, wait-for-first reached=%v, returned=%d)",
				drainReached == nil, waitReached == nil, len(got))
		}
	}
	// Both closers are now in their waits (a closer that already returned is
	// reported below). Watch a bounded window, as the focused tests do:
	// neither may return while the parked write is still held.
	window := time.After(closeSettle)
	for watching := true; watching; {
		select {
		case r := <-results:
			got = append(got, r)
		case <-window:
			watching = false
		}
	}
	for _, r := range got {
		t.Errorf("%s returned (err=%v) while the parked write was still running on the retired handle (%d operations running)", r.name, r.err, r.observed)
	}

	initial.releaseTx()
	if err := waitErr(t, parkedDone, 5*time.Second, "parked write"); err != nil {
		t.Fatalf("parked write: %v", err)
	}
	for len(got) < 2 {
		select {
		case r := <-results:
			got = append(got, r)
			if r.err != nil {
				t.Errorf("%s: %v", r.name, r.err)
			}
			if r.observed != 0 {
				t.Errorf("%s returned with %d operations still running", r.name, r.observed)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("CloseStore did not return within 5s after the parked write released (%d of 2 returned)", len(got))
		}
	}

	workersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(workersDone)
	}()
	select {
	case <-workersDone:
	case <-time.After(5 * time.Second):
		t.Fatal("workers did not stop after CloseStore")
	}

	all := snapshot()
	closeDeadline := time.Now().Add(2 * time.Second)
	for _, l := range all {
		for l.closes.Load() != 1 && time.Now().Before(closeDeadline) {
			time.Sleep(time.Millisecond)
		}
	}
	for i, l := range all {
		if got := l.closes.Load(); got != 1 {
			t.Errorf("handle %d Close calls = %d, want exactly 1", i, got)
		}
		if got := l.ranClosed.Load(); got != 0 {
			t.Errorf("handle %d: %d operations overlapped its Close", i, got)
		}
	}
	t.Logf("handles opened: %d", len(all))
}
