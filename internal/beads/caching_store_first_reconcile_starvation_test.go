package beads

import (
	"testing"
)

// TestFirstReconcileDeadlineSlidesOnEveryLocalWrite is the ga-yc0chj Part-2
// reproduction. It pins CURRENT (defective) behavior so a future fix has a red
// test to turn green — do not read the assertions as desired semantics.
//
// nextReconcileDelay anchors the "when is the next full scan due" deadline on
// stats.LastReconcileAt, falling back to lastFreshAt while that is still zero:
//
//	lastFullScanAt := c.stats.LastReconcileAt
//	if lastFullScanAt.IsZero() { lastFullScanAt = c.lastFreshAt }
//	dueAt := lastFullScanAt.Add(c.adaptiveIntervalLocked())
//
// stats.LastReconcileAt is written in exactly one place (mergeSnapshotLocked),
// so it stays zero until the FIRST reconcile completes. lastFreshAt is written
// by markFreshLocked, which ~25 write paths call on every local mutation.
//
// The consequence: between process start and the first completed reconcile,
// every local write pushes the first reconcile a full adaptive interval into
// the future. On a store whose write traffic is denser than its interval — the
// CITY store of a busy fleet, carrying session beads, mail, and wisps for every
// agent, against a 30 s SMALL cadence — the first reconcile can be starved
// indefinitely. The loop is alive, its goroutine is armed, and it logs nothing,
// because nothing ever becomes due.
//
// It is self-clearing (one write lull longer than the interval lets a single
// reconcile through) and thereafter immune (LastReconcileAt is now non-zero, so
// writes no longer move the anchor) — which is exactly why the field symptom
// reads as an intermittent fault with no trigger that recovers on its own.
func TestFirstReconcileDeadlineSlidesOnEveryLocalWrite(t *testing.T) {
	backing := NewMemStore()
	c := NewCachingStoreForTestWithPrefix(backing, "ga", nil)
	if err := c.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	c.mu.RLock()
	interval := c.adaptiveIntervalLocked()
	anchor := c.lastFreshAt
	neverReconciled := c.stats.LastReconcileAt.IsZero()
	c.mu.RUnlock()

	if !neverReconciled {
		t.Fatal("stats.LastReconcileAt is already set after PrimeActive; the pre-first-reconcile window this test describes does not exist")
	}
	if anchor.IsZero() {
		t.Fatal("lastFreshAt is zero after PrimeActive; nextReconcileDelay would take its reconcile-now escape instead")
	}

	// Before any write the first scan is due exactly one interval after the
	// pre-prime stamp.
	if d := c.nextReconcileDelay(anchor.Add(interval)); d != 0 {
		t.Fatalf("delay at anchor+interval = %s, want 0 (due)", d)
	}

	// One ordinary local write — the kind the city store takes constantly.
	if _, err := c.Create(Bead{Title: "session bead"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	c.mu.RLock()
	moved := c.lastFreshAt
	stillNeverReconciled := c.stats.LastReconcileAt.IsZero()
	c.mu.RUnlock()
	if !moved.After(anchor) {
		t.Fatalf("lastFreshAt = %s, want it advanced past %s by the local write", moved, anchor)
	}
	if !stillNeverReconciled {
		t.Fatal("a local write set stats.LastReconcileAt; it must only be set by a completed reconcile")
	}

	// THE DEFECT: measured at the instant of the write, the first reconcile is
	// a full interval away again. Writes arriving faster than the interval
	// therefore never let it come due.
	if d := c.nextReconcileDelay(moved); d != interval {
		t.Fatalf("delay at the write instant = %s, want %s (a full interval — the write reset the countdown)", d, interval)
	}
	// And the deadline the loop will actually see is anchored on the WRITE,
	// not on the pre-prime stamp it was anchored on a moment ago.
	probe := anchor.Add(interval)
	if d := c.nextReconcileDelay(probe); d != moved.Add(interval).Sub(probe) {
		t.Fatalf("delay at the old deadline = %s, want %s (deadline moved with lastFreshAt)", d, moved.Add(interval).Sub(probe))
	}

	// CONTRAST: let exactly one reconcile complete. From here the anchor is
	// stats.LastReconcileAt, which no write touches, so the cadence is immune
	// to write traffic — matching the field log, where the city cache goes from
	// total silence to a perfectly regular heartbeat after its first scan lands.
	c.runReconciliation()

	c.mu.RLock()
	lastReconcile := c.stats.LastReconcileAt
	c.mu.RUnlock()
	if lastReconcile.IsZero() {
		t.Fatal("runReconciliation did not set stats.LastReconcileAt")
	}
	due := lastReconcile.Add(interval)
	if d := c.nextReconcileDelay(due); d != 0 {
		t.Fatalf("post-reconcile delay at due time = %s, want 0", d)
	}
	if _, err := c.Create(Bead{Title: "another session bead"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if d := c.nextReconcileDelay(due); d != 0 {
		t.Fatalf("a local write postponed the next reconcile by %s even though LastReconcileAt is set; the starvation window should be closed", d)
	}
}
