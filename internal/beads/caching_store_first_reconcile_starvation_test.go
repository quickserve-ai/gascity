package beads

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Local writes must not postpone discovery of externally-created work during
// the first reconcile interval after an active-only prime.
func TestFirstReconcileDiscoversExternalWorkDuringLocalWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backing := NewMemStore()
		c := NewCachingStoreForTestWithPrefix(backing, "ga", nil)
		if err := c.PrimeActive(); err != nil {
			t.Fatal(err)
		}
		c.StartReconciler(context.Background(), WithStaggerOff(), "first-scan-test")
		defer c.StopReconciler()
		external, err := backing.Create(Bead{Title: "externally routed work"})
		if err != nil {
			t.Fatal(err)
		}

		// Advance virtual time, keeping local traffic denser than the 30s
		// cadence. Wait synchronizes the real reconciler after each tick.
		for range 4 {
			<-time.NewTimer(10 * time.Second).C
			synctest.Wait()
			if _, err := c.Create(Bead{Title: "local session update"}); err != nil {
				t.Fatal(err)
			}
		}
		items, err := c.List(ListQuery{Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.ID == external.ID {
				return
			}
		}
		t.Fatalf("externally-created bead %s remains invisible after the first reconcile deadline despite a healthy backing store", external.ID)
	})
}
