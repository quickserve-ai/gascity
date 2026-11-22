package beads

import (
	"sync/atomic"
	"testing"
	"time"
)

type listCountingStore struct {
	*MemStore
	lists atomic.Int64
}

func (s *listCountingStore) List(q ListQuery) ([]Bead, error) {
	s.lists.Add(1)
	return s.MemStore.List(q)
}

// A closed reconcile gate skips the periodic full scan without stopping the
// loop; reopening it lets the next due cycle run.
func TestReconcileGateSkipsTheFullScanWhileClosed(t *testing.T) {
	backing := &listCountingStore{MemStore: NewMemStore()}
	var allowed atomic.Bool
	cs := NewCachingStoreForTest(backing, nil)
	WithReconcileGate(allowed.Load)(cs)

	// Never primed, so a reconcile is due.
	cs.reconcileIfDue(time.Now())
	if got := backing.lists.Load(); got != 0 {
		t.Fatalf("closed gate: backing List called %d time(s), want 0", got)
	}

	allowed.Store(true)
	cs.reconcileIfDue(time.Now())
	if got := backing.lists.Load(); got == 0 {
		t.Fatal("open gate: the due reconcile did not scan the backing store")
	}
}

// The reconcile tracker spans the gate check and the scan: a step is in flight
// before the gate is asked and until the scan returns, and it ends whether the
// gate allowed it or not (ga-vblzkw).
func TestReconcileTrackerSpansTheGateAndTheScan(t *testing.T) {
	var inFlight, begun, ended atomic.Int64
	var allowed atomic.Bool
	var gateSaw, scanSaw atomic.Int64
	backing := &trackerProbeStore{MemStore: NewMemStore(), onList: func() { scanSaw.Store(inFlight.Load()) }}
	cs := NewCachingStoreForTest(backing, nil)
	WithReconcileGate(func() bool {
		gateSaw.Store(inFlight.Load())
		return allowed.Load()
	})(cs)
	WithReconcileTracker(func() func() {
		begun.Add(1)
		inFlight.Add(1)
		return func() {
			ended.Add(1)
			inFlight.Add(-1)
		}
	})(cs)

	cs.reconcileIfDue(time.Now())
	if gateSaw.Load() != 1 {
		t.Fatalf("closed gate asked with %d step(s) in flight, want 1: the step must begin before the gate", gateSaw.Load())
	}
	if backing.lists.Load() != 0 || inFlight.Load() != 0 || begun.Load() != 1 || ended.Load() != 1 {
		t.Fatalf("closed gate: lists=%d inFlight=%d begun=%d ended=%d, want 0/0/1/1", backing.lists.Load(), inFlight.Load(), begun.Load(), ended.Load())
	}

	allowed.Store(true)
	cs.reconcileIfDue(time.Now())
	if backing.lists.Load() == 0 {
		t.Fatal("open gate: the due reconcile did not scan")
	}
	if scanSaw.Load() != 1 {
		t.Fatalf("scan ran with %d step(s) in flight, want 1", scanSaw.Load())
	}
	if inFlight.Load() != 0 || begun.Load() != ended.Load() {
		t.Fatalf("after the scan: inFlight=%d begun=%d ended=%d, want 0 and begun == ended", inFlight.Load(), begun.Load(), ended.Load())
	}
}

type trackerProbeStore struct {
	*MemStore
	lists  atomic.Int64
	onList func()
}

func (s *trackerProbeStore) List(q ListQuery) ([]Bead, error) {
	s.lists.Add(1)
	if s.onList != nil {
		s.onList()
	}
	return s.MemStore.List(q)
}
