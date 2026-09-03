package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// hangingListStore is a backing store whose List blocks until release is
// closed. It reproduces the exact shape of the ga-yc0chj 43-hour outage:
// beads.Store.List takes no context, so a wedged bd subprocess or a stalled
// Dolt read is not a slow call or a failed one — it is a call that never
// returns, which no timeout, retry, or cancellation inside Prime can reach.
type hangingListStore struct {
	*beads.MemStore
	release <-chan struct{}
}

func (s *hangingListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	<-s.release
	return s.MemStore.List(q)
}

// waitForCond polls until cond holds or the deadline passes.
func waitForCond(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
}

// TestPrimeThenStartReconcilerArmsDespiteHungPrime is the regression test for
// the defect that cost the city cache its reconciler for 43 hours: the initial
// full prime was awaited synchronously, so a prime that never returned meant
// StartReconciler was never reached and the scope simply stopped reconciling,
// permanently and silently.
//
// The backing List here can never return on its own, so the reconciler being
// armed at all is proof the arm no longer depends on the prime completing.
func TestPrimeThenStartReconcilerArmsDespiteHungPrime(t *testing.T) {
	release := make(chan struct{})
	backing := &hangingListStore{MemStore: beads.NewMemStore(), release: release}
	cs := beads.NewCachingStore(backing, nil)

	prev := cachePrimeArmBound
	cachePrimeArmBound = 20 * time.Millisecond
	t.Cleanup(func() { cachePrimeArmBound = prev })

	ctx, cancel := context.WithCancel(context.Background())

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		primeThenStartReconciler(ctx, cs, "test-agent")
	}()

	waitForCond(t, 5*time.Second, "the reconciler to be armed while the prime is still hung", func() bool {
		return !cs.ReconcilerArmedAt().IsZero()
	})

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("primeThenStartReconciler never returned while its prime was hung")
	}

	// Teardown order matters: the abandoned prime holds a cache-worker count
	// that StopReconciler waits on, and only releasing the backing lets it
	// finish. Cancel first so the freed prime and the reconcile loop both exit
	// immediately instead of doing a full pass.
	cancel()
	close(release)
	cs.StopReconciler()
}

// TestPrimeThenStartReconcilerWaitsForAHealthyPrime pins the other half of the
// contract: the bound is a backstop, not a schedule. A prime that returns
// normally is still awaited, so the reconciler arms against an already-primed
// cache exactly as before — the arm bound must never turn a healthy startup
// into a two-minute wait or a race between the prime and the first scan.
func TestPrimeThenStartReconcilerWaitsForAHealthyPrime(t *testing.T) {
	backing := beads.NewMemStore()
	cs := beads.NewCachingStore(backing, nil)

	prev := cachePrimeArmBound
	cachePrimeArmBound = time.Hour
	t.Cleanup(func() { cachePrimeArmBound = prev })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		primeThenStartReconciler(ctx, cs, "test-agent")
	}()

	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		t.Fatal("primeThenStartReconciler did not return; a healthy prime must not wait on the arm bound")
	}

	if !cs.IsLive() {
		t.Error("cache is not live after a healthy prime: the prime was abandoned rather than awaited")
	}
	if cs.ReconcilerArmedAt().IsZero() {
		t.Error("reconciler was not armed after a healthy prime")
	}

	cancel()
	cs.StopReconciler()
}
