package beads

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// These tests pin the storage-handle lifecycle behind ga-yuiof4.1: an
// operation keeps using the handle it acquired even after a reconnect or
// CloseStore has swapped that handle out, and the handle is closed only once
// the last such operation has let go of it — never under a running operation.
//
// The focused tests run in a testing/synctest bubble (inStoreBubble).
// synctest.Wait returns only once every other goroutine in the bubble is
// durably blocked, so a check made after it sees every effect already set in
// motion — including a retired handle's Close, which the store runs on a
// detached goroutine — without waiting on wall-clock time.

// heldReadStorage is a spy storage whose read of holdID parks until the test
// releases it, and which counts reads and Close calls.
type heldReadStorage struct {
	*nativeDoltStorageSpy
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
	reads       atomic.Int32
	closes      atomic.Int32
	// closesSeenByHeldRead is the Close count the held read observed when it
	// resumed; it must be zero, since no read may run on a closed handle.
	closesSeenByHeldRead atomic.Int32
}

func newHeldReadStorage(holdID string, otherErr error) *heldReadStorage {
	h := &heldReadStorage{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	h.closesSeenByHeldRead.Store(-1)
	h.nativeDoltStorageSpy = &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			h.reads.Add(1)
			id := ""
			if len(f.IDs) > 0 {
				id = f.IDs[0]
			}
			if id == holdID {
				h.enterOnce.Do(func() { close(h.entered) })
				<-h.release
				h.closesSeenByHeldRead.Store(h.closes.Load())
				return []*beadslib.Issue{spyIssue(id)}, nil
			}
			if otherErr != nil {
				return nil, otherErr
			}
			return []*beadslib.Issue{spyIssue(id)}, nil
		},
		close: func() error {
			h.closes.Add(1)
			return nil
		},
	}
	return h
}

func (h *heldReadStorage) releaseHeld() { h.releaseOnce.Do(func() { close(h.release) }) }

func spyIssue(id string) *beadslib.Issue {
	return &beadslib.Issue{ID: id, Title: "spy", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2}
}

// closeSettle is the observation window the real-time stress test watches
// for a CloseStore that must NOT return while an operation it has not waited
// for is still held. It is the window of a negative assertion, which
// TESTING.md keeps short and explicit rather than on a hang budget.
const closeSettle = 100 * time.Millisecond

// bubbleWatchdog bounds, on the real clock, a test body run by inStoreBubble.
// It is a pure hang detector, so it is the package's hang budget. Every bound
// inside a bubble runs on the fake clock (fakeClockBound), so the bubble holds
// no real-clock deadline for the watchdog to undercut.
const bubbleWatchdog = beadsHangBudget

// inStoreBubble runs body in a testing/synctest bubble with a real-time
// watchdog. Inside the bubble every bound a test sets (time.After) runs on the
// bubble's fake clock, which advances only while every goroutine is durably
// blocked. A goroutine parked on a sync.Mutex or sync.RWMutex is not durably
// blocked, so a regression that deadlocks the store on s.mu would stop the
// fake clock and hang the test binary until the go test -timeout; the
// watchdog, armed outside the bubble on the real clock, fails the run first,
// with every goroutine's stack.
func inStoreBubble(t *testing.T, body func(t *testing.T)) {
	t.Helper()
	name := t.Name()
	watchdog := time.AfterFunc(bubbleWatchdog, func() {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		panic(fmt.Sprintf("%s: still running after %s of real time; a goroutine is likely deadlocked on the store's s.mu, which synctest's fake clock cannot advance past\n%s",
			name, bubbleWatchdog, buf))
	})
	defer watchdog.Stop()
	synctest.Test(t, body)
}

// TestNativeDoltStoreReconnectDefersOldHandleCloseUntilInFlightReadReleases:
// a reconnect that swaps the handle out from under a running read must leave
// the old handle open for that read, close it exactly once when the read lets
// go, and serve every later read from the fresh handle.
func TestNativeDoltStoreReconnectDefersOldHandleCloseUntilInFlightReadReleases(t *testing.T) {
	inStoreBubble(t, func(t *testing.T) {
		old := newHeldReadStorage("gc-held", errors.New("begin read tx: invalid connection"))
		t.Cleanup(old.releaseHeld)
		fresh := newHeldReadStorage("", nil)

		var reopens atomic.Int32
		store := newNativeDoltStoreForTest(old)
		store.reopen = func(context.Context) (beadslib.Storage, error) {
			reopens.Add(1)
			return fresh, nil
		}

		heldDone := startGet(store, "gc-held")
		select {
		case <-old.entered:
		case <-time.After(fakeClockBound):
			t.Fatal("held read never reached the old handle")
		}

		// A second read fails transiently on the old handle and reconnects
		// while the held read is still running on it.
		if err := getWithin(t, store, "gc-trigger"); err != nil {
			t.Fatalf("Get across reconnect: %v", err)
		}
		if got := reopens.Load(); got != 1 {
			t.Fatalf("reopen calls = %d, want 1", got)
		}

		// Every goroutine but the held read is now idle: a close of the old
		// handle, had anything set one off, has already run.
		synctest.Wait()
		if got := old.closes.Load(); got != 0 {
			t.Fatalf("old handle Close calls while a read is still running on it = %d, want 0", got)
		}

		// Later reads are served by the fresh handle, not the retired one.
		oldReads := old.reads.Load()
		freshReads := fresh.reads.Load()
		if err := getWithin(t, store, "gc-after"); err != nil {
			t.Fatalf("Get after reconnect: %v", err)
		}
		if got := fresh.reads.Load(); got != freshReads+1 {
			t.Fatalf("fresh handle reads = %d, want %d", got, freshReads+1)
		}
		if got := old.reads.Load(); got != oldReads {
			t.Fatalf("retired handle reads = %d, want %d (no new read may reach it)", got, oldReads)
		}

		old.releaseHeld()
		if err := waitErr(t, heldDone, fakeClockBound, "held read on the retired handle"); err != nil {
			t.Fatalf("held read on the retired handle: %v", err)
		}
		if got := old.closesSeenByHeldRead.Load(); got != 0 {
			t.Fatalf("Close calls observed by the held read when it resumed = %d, want 0", got)
		}

		// The last release closed the retired handle on a detached goroutine;
		// once the bubble is idle that close has run, and nothing else has.
		synctest.Wait()
		if got := old.closes.Load(); got != 1 {
			t.Fatalf("old handle Close calls after the last read released = %d, want exactly 1", got)
		}
		if got := fresh.closes.Load(); got != 0 {
			t.Fatalf("fresh handle Close calls = %d, want 0 while it is current", got)
		}
	})
}

// TestNativeDoltStoreCloseStoreWaitsForInFlightReadThenClosesOnce: CloseStore
// latches the store closed at once, but closes the handle only after the read
// already running on it lets go, and exactly once.
func TestNativeDoltStoreCloseStoreWaitsForInFlightReadThenClosesOnce(t *testing.T) {
	inStoreBubble(t, func(t *testing.T) {
		storage := newHeldReadStorage("gc-held", nil)
		t.Cleanup(storage.releaseHeld)
		store := newNativeDoltStoreForTest(storage)

		heldDone := startGet(store, "gc-held")
		select {
		case <-storage.entered:
		case <-time.After(fakeClockBound):
			t.Fatal("held read never reached the handle")
		}

		closeDone := startCloseStore(store)
		// Once the bubble is idle, CloseStore has latched the store closed and
		// is waiting for the running read, or it has returned.
		synctest.Wait()
		if !nativeDoltStoreClosedForTest(store) {
			t.Fatal("CloseStore did not latch the store closed while a read was in flight")
		}
		select {
		case err := <-closeDone:
			t.Fatalf("CloseStore returned (%v) while a read was still running on the handle", err)
		default:
		}

		// New operations after the latch fail fast rather than waiting.
		if err := getWithin(t, store, "gc-new"); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("Get after the close latch = %v, want ErrStoreClosed", err)
		}
		if err := updateWithin(t, store, "gc-new"); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("Update after the close latch = %v, want ErrStoreClosed", err)
		}
		if s, release, err := store.acquireStorage(); !errors.Is(err, ErrStoreClosed) {
			if release != nil {
				release()
			}
			t.Fatalf("acquireStorage after the close latch = (%T, %v), want ErrStoreClosed", s, err)
		}
		if got := storage.closes.Load(); got != 0 {
			t.Fatalf("handle Close calls while a read is still running on it = %d, want 0", got)
		}

		storage.releaseHeld()
		if err := waitErr(t, heldDone, fakeClockBound, "held read"); err != nil {
			t.Fatalf("held read: %v", err)
		}
		if err := waitErr(t, closeDone, fakeClockBound, "CloseStore after the in-flight read released"); err != nil {
			t.Fatalf("CloseStore: %v", err)
		}
		if got := storage.closesSeenByHeldRead.Load(); got != 0 {
			t.Fatalf("Close calls observed by the held read when it resumed = %d, want 0", got)
		}
		// CloseStore closes synchronously: the count is final when it returns.
		if got := storage.closes.Load(); got != 1 {
			t.Fatalf("handle Close calls after CloseStore returned = %d, want 1", got)
		}
		if err := waitErr(t, startCloseStore(store), fakeClockBound, "second CloseStore"); err != nil {
			t.Fatalf("second CloseStore: %v", err)
		}
		if got := storage.closes.Load(); got != 1 {
			t.Fatalf("handle Close calls after a second CloseStore = %d, want still 1", got)
		}
	})
}

// TestNativeDoltStoreCloseStoreReturnsStorageCloseError keeps CloseStore's
// contract of reporting the handle's own Close error.
func TestNativeDoltStoreCloseStoreReturnsStorageCloseError(t *testing.T) {
	errClose := errors.New("close failed")
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{close: func() error { return errClose }})
	if err := waitErr(t, startCloseStore(store), beadsHangBudget, "CloseStore"); !errors.Is(err, errClose) {
		t.Fatalf("CloseStore = %v, want the storage Close error", err)
	}
}
