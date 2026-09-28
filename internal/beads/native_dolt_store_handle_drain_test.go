package beads

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// These tests pin the storage-handle lifecycle behind ga-yuiof4.1: an
// operation keeps using the handle it acquired even after a reconnect or
// CloseStore has swapped that handle out, and the handle is closed only once
// the last such operation has let go of it — never under a running operation.

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

// waitForCount polls get until it returns want or the deadline passes, and
// returns the last value seen.
func waitForCount(get func() int32, want int32, within time.Duration) int32 {
	deadline := time.Now().Add(within)
	got := get()
	for got != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		got = get()
	}
	return got
}

// closeSettle is how long a test watches for a Close that must NOT happen.
// The retired handle is closed on a detached goroutine, so a premature close
// would land within this window.
const closeSettle = 100 * time.Millisecond

// TestNativeDoltStoreReconnectDefersOldHandleCloseUntilInFlightReadReleases:
// a reconnect that swaps the handle out from under a running read must leave
// the old handle open for that read, close it exactly once when the read lets
// go, and serve every later read from the fresh handle.
func TestNativeDoltStoreReconnectDefersOldHandleCloseUntilInFlightReadReleases(t *testing.T) {
	old := newHeldReadStorage("gc-held", errors.New("begin read tx: invalid connection"))
	t.Cleanup(old.releaseHeld)
	fresh := newHeldReadStorage("", nil)

	var reopens atomic.Int32
	store := newNativeDoltStoreForTest(old)
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		reopens.Add(1)
		return fresh, nil
	}

	heldDone := make(chan error, 1)
	go func() {
		_, err := store.Get("gc-held")
		heldDone <- err
	}()
	select {
	case <-old.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("held read never reached the old handle")
	}

	// A second read fails transiently on the old handle and reconnects while
	// the held read is still running on it.
	triggerDone := make(chan error, 1)
	go func() {
		_, err := store.Get("gc-trigger")
		triggerDone <- err
	}()
	select {
	case err := <-triggerDone:
		if err != nil {
			t.Fatalf("Get across reconnect: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get across reconnect did not return while another read was running on the old handle")
	}
	if got := reopens.Load(); got != 1 {
		t.Fatalf("reopen calls = %d, want 1", got)
	}

	time.Sleep(closeSettle)
	if got := old.closes.Load(); got != 0 {
		t.Fatalf("old handle Close calls while a read is still running on it = %d, want 0", got)
	}

	// Later reads are served by the fresh handle, not the retired one.
	oldReads := old.reads.Load()
	freshReads := fresh.reads.Load()
	if _, err := store.Get("gc-after"); err != nil {
		t.Fatalf("Get after reconnect: %v", err)
	}
	if got := fresh.reads.Load(); got != freshReads+1 {
		t.Fatalf("fresh handle reads = %d, want %d", got, freshReads+1)
	}
	if got := old.reads.Load(); got != oldReads {
		t.Fatalf("retired handle reads = %d, want %d (no new read may reach it)", got, oldReads)
	}

	old.releaseHeld()
	select {
	case err := <-heldDone:
		if err != nil {
			t.Fatalf("held read on the retired handle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held read did not return after release")
	}
	if got := old.closesSeenByHeldRead.Load(); got != 0 {
		t.Fatalf("Close calls observed by the held read when it resumed = %d, want 0", got)
	}

	if got := waitForCount(old.closes.Load, 1, 5*time.Second); got != 1 {
		t.Fatalf("old handle Close calls after the last read released = %d, want 1", got)
	}
	time.Sleep(closeSettle)
	if got := old.closes.Load(); got != 1 {
		t.Fatalf("old handle Close calls = %d, want exactly 1", got)
	}
	if got := fresh.closes.Load(); got != 0 {
		t.Fatalf("fresh handle Close calls = %d, want 0 while it is current", got)
	}
}

// TestNativeDoltStoreCloseStoreWaitsForInFlightReadThenClosesOnce: CloseStore
// latches the store closed at once, but closes the handle only after the read
// already running on it lets go, and exactly once.
func TestNativeDoltStoreCloseStoreWaitsForInFlightReadThenClosesOnce(t *testing.T) {
	storage := newHeldReadStorage("gc-held", nil)
	t.Cleanup(storage.releaseHeld)
	store := newNativeDoltStoreForTest(storage)

	heldDone := make(chan error, 1)
	go func() {
		_, err := store.Get("gc-held")
		heldDone <- err
	}()
	select {
	case <-storage.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("held read never reached the handle")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- store.CloseStore() }()

	// Observe the latch without blocking on s.mu: a store whose CloseStore
	// queues a writer behind the in-flight read would otherwise park this
	// check too, and the test would hang instead of failing.
	latched := false
	deadline := time.Now().Add(5 * time.Second)
	for !latched && time.Now().Before(deadline) {
		if store.mu.TryRLock() {
			latched = store.closed
			store.mu.RUnlock()
		}
		if !latched {
			time.Sleep(time.Millisecond)
		}
	}
	if !latched {
		t.Fatal("CloseStore did not latch the store closed within 5s while a read was in flight")
	}

	// New operations after the latch fail fast rather than waiting.
	if _, err := store.Get("gc-new"); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("Get after the close latch = %v, want ErrStoreClosed", err)
	}
	if err := store.Update("gc-new", UpdateOpts{}); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("Update after the close latch = %v, want ErrStoreClosed", err)
	}
	if s, release, err := store.acquireStorage(); !errors.Is(err, ErrStoreClosed) {
		if release != nil {
			release()
		}
		t.Fatalf("acquireStorage after the close latch = (%T, %v), want ErrStoreClosed", s, err)
	}

	select {
	case err := <-closeDone:
		t.Fatalf("CloseStore returned (%v) while a read was still running on the handle", err)
	case <-time.After(closeSettle):
	}
	if got := storage.closes.Load(); got != 0 {
		t.Fatalf("handle Close calls while a read is still running on it = %d, want 0", got)
	}

	storage.releaseHeld()
	select {
	case err := <-heldDone:
		if err != nil {
			t.Fatalf("held read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held read did not return after release")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("CloseStore: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CloseStore did not return after the in-flight read released")
	}
	if got := storage.closesSeenByHeldRead.Load(); got != 0 {
		t.Fatalf("Close calls observed by the held read when it resumed = %d, want 0", got)
	}
	// CloseStore closes synchronously: the count is final when it returns.
	if got := storage.closes.Load(); got != 1 {
		t.Fatalf("handle Close calls after CloseStore returned = %d, want 1", got)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatalf("second CloseStore: %v", err)
	}
	if got := storage.closes.Load(); got != 1 {
		t.Fatalf("handle Close calls after a second CloseStore = %d, want still 1", got)
	}
}

// TestNativeDoltStoreCloseStoreReturnsStorageCloseError keeps CloseStore's
// contract of reporting the handle's own Close error.
func TestNativeDoltStoreCloseStoreReturnsStorageCloseError(t *testing.T) {
	errClose := errors.New("close failed")
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{close: func() error { return errClose }})
	if err := store.CloseStore(); !errors.Is(err, errClose) {
		t.Fatalf("CloseStore = %v, want the storage Close error", err)
	}
}
