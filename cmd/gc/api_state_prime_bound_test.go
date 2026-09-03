package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestPrimeThenStartReconcilerWaitsForAHealthyPrime pins the other half of the
// arm-bound contract (the hung-prime half lives in
// TestPrimeThenStartReconcilerArmsReconcilerWhenPrimeHangs): the bound is a
// backstop, not a schedule. A prime that returns normally is still awaited, so
// the reconciler arms against an already-primed cache exactly as before — the
// arm bound must never turn a healthy startup into a primeArmBound wait or a
// race between the prime and the first scan.
func TestPrimeThenStartReconcilerWaitsForAHealthyPrime(t *testing.T) {
	backing := beads.NewMemStore()
	cs := beads.NewCachingStore(backing, nil)

	prev := primeArmBound
	primeArmBound = time.Hour
	defer func() { primeArmBound = prev }()

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
