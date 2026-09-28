package beads

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// These tests extend native_dolt_store_handle_drain_test.go to the cases a
// single outstanding use cannot tell apart: several uses on a retired handle,
// a release called twice, writes that span a reconnect, and CloseStore racing
// operations that are running on a handle a reconnect already retired
// (ga-yuiof4.1 review round 2).

// lifecycleSpy is a spy storage that records what the handle lifecycle tests
// assert on. A read of failID fails with a transient connection error, which
// sends the read through reconnect; every other read succeeds. With holdTx set,
// every transaction parks until releaseTx. It counts Close calls, and counts
// any operation that started or finished after Close had been called on it.
type lifecycleSpy struct {
	*nativeDoltStorageSpy
	closes    atomic.Int32
	reads     atomic.Int32
	txs       atomic.Int32
	active    atomic.Int32 // operations currently running on this spy
	ranClosed atomic.Int32 // operations that overlapped a Close of this spy
	// delay, when set, runs inside every operation, to widen the window in
	// which a use is outstanding.
	delay func()

	txEntered   chan struct{}
	txRelease   chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newLifecycleSpy(failID string, holdTx bool) *lifecycleSpy {
	l := &lifecycleSpy{txEntered: make(chan struct{}), txRelease: make(chan struct{})}
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
		runInTransaction: func(context.Context, string, func(beadslib.Transaction) error) error {
			defer l.enter()()
			l.txs.Add(1)
			if holdTx {
				l.enterOnce.Do(func() { close(l.txEntered) })
				<-l.txRelease
			}
			return nil
		},
		close: func() error {
			l.closes.Add(1)
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

func (l *lifecycleSpy) releaseTx() { l.releaseOnce.Do(func() { close(l.txRelease) }) }

// storeReopeningTo builds a test store on old whose reopen hook hands back
// fresh.
func storeReopeningTo(old, fresh beadslib.Storage) *NativeDoltStore {
	store := newNativeDoltStoreForTest(old)
	store.reopen = func(context.Context) (beadslib.Storage, error) { return fresh, nil }
	return store
}

// getWithin runs store.Get(id) and fails the test if it does not return
// within the bound, so a regression that parks it cannot hang the suite.
func getWithin(t *testing.T, store *NativeDoltStore, id string, within time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := store.Get(id)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatalf("Get(%q) did not return within %s", id, within)
		return nil
	}
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
	_, _, releaseB, err := store.acquireStorageGen()
	if err != nil {
		t.Fatalf("acquire B: %v", err)
	}
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
	_, _, releaseB, err := store.acquireStorageGen()
	if err != nil {
		t.Fatalf("acquire B: %v", err)
	}
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
// write that is running when a reconnect swaps the handle finishes on the
// handle it started on, that handle closes only after the write, and later
// writes go to the fresh handle.
func TestNativeDoltStoreWriteSpanningReconnectCompletesOnItsOriginalHandle(t *testing.T) {
	old := newLifecycleSpy("gc-fail", true)
	t.Cleanup(old.releaseTx)
	fresh := newLifecycleSpy("", false)
	store := storeReopeningTo(old, fresh)

	writeDone := make(chan error, 1)
	go func() { writeDone <- store.Update("gc-w", UpdateOpts{}) }()
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

	old.releaseTx()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write spanning the reconnect: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write did not return after release")
	}
	if got := old.txs.Load(); got != 1 {
		t.Fatalf("transactions on the original handle = %d, want 1", got)
	}
	if got := fresh.txs.Load(); got != 0 {
		t.Fatalf("transactions on the fresh handle = %d, want 0 (the write must not move handles)", got)
	}
	if got := waitForCount(old.closes.Load, 1, 5*time.Second); got != 1 {
		t.Fatalf("original handle Close calls after the write released it = %d, want 1", got)
	}
	if got := old.ranClosed.Load(); got != 0 {
		t.Fatalf("operations that overlapped a Close of the original handle = %d, want 0", got)
	}

	if err := store.Update("gc-w2", UpdateOpts{}); err != nil {
		t.Fatalf("write after reconnect: %v", err)
	}
	if got := fresh.txs.Load(); got != 1 {
		t.Fatalf("transactions on the fresh handle after reconnect = %d, want 1", got)
	}
	if got := old.txs.Load(); got != 1 {
		t.Fatalf("transactions on the retired handle = %d, want still 1", got)
	}
}

// TestNativeDoltStoreCloseStoreWaitsForWriteOnRetiredHandle: CloseStore must
// not return while any operation that started before it is still running,
// including one running on a handle a reconnect has already retired. Before
// the store-wide wait, CloseStore waited only for uses of the current handle.
func TestNativeDoltStoreCloseStoreWaitsForWriteOnRetiredHandle(t *testing.T) {
	old := newLifecycleSpy("gc-fail", true)
	t.Cleanup(old.releaseTx)
	fresh := newLifecycleSpy("", false)
	store := storeReopeningTo(old, fresh)

	writeDone := make(chan error, 1)
	go func() { writeDone <- store.Update("gc-w", UpdateOpts{}) }()
	select {
	case <-old.txEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("write never reached the original handle")
	}
	if err := getWithin(t, store, "gc-fail", 5*time.Second); err != nil {
		t.Fatalf("Get across reconnect: %v", err)
	}

	closeDone := startCloseStore(store)
	waitForCloseLatch(t, store, 5*time.Second)
	if _, err := store.Get("gc-new"); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("Get after the close latch = %v, want ErrStoreClosed", err)
	}
	select {
	case err := <-closeDone:
		t.Fatalf("CloseStore returned (%v) while a write that started before it was still running on the handle a reconnect retired", err)
	case <-time.After(closeSettle):
	}

	old.releaseTx()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write did not return after release")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("CloseStore: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CloseStore did not return after the write released")
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
// has finished, which is after every operation that started before the latch
// has released.
func TestNativeDoltStoreSecondCloseStoreWaitsForTheFirstToFinish(t *testing.T) {
	spy := newLifecycleSpy("", true)
	t.Cleanup(spy.releaseTx)
	store := newNativeDoltStoreForTest(spy)

	writeDone := make(chan error, 1)
	go func() { writeDone <- store.Update("gc-w", UpdateOpts{}) }()
	select {
	case <-spy.txEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("write never reached the handle")
	}

	first := startCloseStore(store)
	waitForCloseLatch(t, store, 5*time.Second)
	second := startCloseStore(store)
	select {
	case err := <-second:
		t.Fatalf("second CloseStore returned (%v) while the first was still waiting for a write in flight", err)
	case err := <-first:
		t.Fatalf("first CloseStore returned (%v) while a write was in flight", err)
	case <-time.After(closeSettle):
	}

	spy.releaseTx()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write did not return after release")
	}
	for name, done := range map[string]<-chan error{"first": first, "second": second} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s CloseStore: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s CloseStore did not return after the write released", name)
		}
	}
	if got := spy.closes.Load(); got != 1 {
		t.Fatalf("handle Close calls = %d, want exactly 1", got)
	}
}

// TestNativeDoltStoreHandleLifecycleStress runs reads, writes and
// reconnect-triggering reads on many goroutines, then two concurrent
// CloseStore calls while they are still running. By the end every storage
// handle the store ever opened is closed exactly once, and no operation ran
// on a closed handle. Bounded to well under two seconds.
func TestNativeDoltStoreHandleLifecycleStress(t *testing.T) {
	var spiesMu sync.Mutex
	var spies []*lifecycleSpy
	newSpy := func() *lifecycleSpy {
		l := newLifecycleSpy("gc-fail", false)
		l.delay = func() { time.Sleep(time.Duration(rand.IntN(300)) * time.Microsecond) }
		spiesMu.Lock()
		spies = append(spies, l)
		spiesMu.Unlock()
		return l
	}
	store := newNativeDoltStoreForTest(newSpy())
	store.readRetryBudgetOverride = 50 * time.Millisecond
	var reopens atomic.Int32
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		reopens.Add(1)
		return newSpy(), nil
	}

	const workers = 16
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
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

	time.Sleep(300 * time.Millisecond)
	first := startCloseStore(store)
	second := startCloseStore(store)
	for name, done := range map[string]<-chan error{"first": first, "second": second} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s CloseStore: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s CloseStore did not return", name)
		}
	}

	// Every operation that acquired storage before the latch has released it,
	// and none can start after it, so nothing is running on any handle now.
	spiesMu.Lock()
	snapshot := append([]*lifecycleSpy(nil), spies...)
	spiesMu.Unlock()
	for i, l := range snapshot {
		if got := l.active.Load(); got != 0 {
			t.Errorf("handle %d: %d operations still running after CloseStore returned", i, got)
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

	spiesMu.Lock()
	snapshot = append([]*lifecycleSpy(nil), spies...)
	spiesMu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for _, l := range snapshot {
		for l.closes.Load() != 1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	for i, l := range snapshot {
		if got := l.closes.Load(); got != 1 {
			t.Errorf("handle %d Close calls = %d, want exactly 1", i, got)
		}
		if got := l.ranClosed.Load(); got != 0 {
			t.Errorf("handle %d: %d operations overlapped its Close", i, got)
		}
	}
	t.Logf("handles opened: %d (reopens: %d)", len(snapshot), reopens.Load())
	if len(snapshot) < 2 {
		t.Errorf("handles opened = %d, want at least one reconnect during the run", len(snapshot))
	}
}
