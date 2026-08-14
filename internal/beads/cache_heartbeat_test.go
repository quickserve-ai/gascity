package beads

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReconcileHeartbeatRoundTrip pins the durable record's write/read contract:
// what the reconciler publishes is exactly what `gc doctor` reads back, and a
// scope that has published nothing reads as absent rather than as an error.
func TestReconcileHeartbeatRoundTrip(t *testing.T) {
	city := t.TempDir()
	armed := time.Date(2026, 8, 13, 14, 14, 15, 0, time.UTC)
	want := ReconcileHeartbeat{
		Scope:           "city",
		Prefix:          "ga",
		PID:             os.Getpid(),
		ArmedAt:         armed,
		LastReconcileAt: armed.Add(30 * time.Second),
		IntervalMs:      30_000,
		State:           "live",
		UpdatedAt:       armed.Add(30 * time.Second),
	}
	if err := WriteReconcileHeartbeat(city, want); err != nil {
		t.Fatalf("WriteReconcileHeartbeat: %v", err)
	}

	got, ok, err := ReadReconcileHeartbeat(city, "city")
	if err != nil || !ok {
		t.Fatalf("ReadReconcileHeartbeat = (_, %v, %v), want (record, true, nil)", ok, err)
	}
	if !got.ArmedAt.Equal(want.ArmedAt) || !got.LastReconcileAt.Equal(want.LastReconcileAt) {
		t.Errorf("timestamps round-tripped as armed=%s last=%s, want armed=%s last=%s",
			got.ArmedAt, got.LastReconcileAt, want.ArmedAt, want.LastReconcileAt)
	}
	if got.Prefix != want.Prefix || got.PID != want.PID || got.IntervalMs != want.IntervalMs || got.State != want.State {
		t.Errorf("record = %+v, want %+v", got, want)
	}

	// A scope that never published reads absent, not an error — "no record"
	// is the normal state for every store that is not supposed to reconcile.
	if _, ok, err := ReadReconcileHeartbeat(city, "never-armed"); ok || err != nil {
		t.Errorf("ReadReconcileHeartbeat(missing) = (_, %v, %v), want (_, false, nil)", ok, err)
	}

	// The temp file used for the atomic rename must not survive.
	entries, err := os.ReadDir(ReconcileHeartbeatDir(city))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file %q in the heartbeat dir", e.Name())
		}
	}
}

// TestReconcilePublishesHeartbeatOnArmAndOnCompletion pins the two publish
// points that make absence detectable: the arm stamp (so a reconciler that
// never completes a cycle is still visible as "supposed to be reconciling"),
// and every completed reconcile (so the record's own staleness measures the
// gap the rate-limited log line cannot).
func TestReconcilePublishesHeartbeatOnSinkInstallAndOnCompletion(t *testing.T) {
	backing := NewMemStore()
	if _, err := backing.Create(Bead{Title: "seed"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	c := NewCachingStoreForTestWithPrefix(backing, "ga", nil)

	var published []ReconcileHeartbeat
	before := time.Now()
	c.SetReconcileHeartbeatSink(func(hb ReconcileHeartbeat) {
		published = append(published, hb)
	})

	// The arm record is published SYNCHRONOUSLY by the install itself — not by
	// StartReconciler, which typically runs on an unwaited goroutine whose
	// late write races teardown of the city root (the controllerState
	// TempDir-cleanup failures). Install-time publish also makes a store that
	// never starts its reconciler visible to the watchdog instead of silent.
	if len(published) != 1 {
		t.Fatalf("publish count after sink install = %d, want 1 (install must publish the arm record synchronously)", len(published))
	}
	arm := published[0]
	if !arm.LastReconcileAt.IsZero() {
		t.Errorf("arm record LastReconcileAt = %s, want zero (no cycle completed yet)", arm.LastReconcileAt)
	}
	if arm.ArmedAt.IsZero() || arm.ArmedAt.Before(before) {
		t.Errorf("arm record ArmedAt = %s, want a fresh stamp at install time (>= %s); a zero ArmedAt reads as unknown/quiet to the doctor check", arm.ArmedAt, before)
	}
	if arm.IntervalMs <= 0 {
		t.Errorf("arm record IntervalMs = %d, want a positive cadence so readers can judge staleness", arm.IntervalMs)
	}
	if arm.Prefix != "ga" || arm.PID != os.Getpid() {
		t.Errorf("arm record = %+v, want prefix=ga pid=%d", arm, os.Getpid())
	}

	c.runReconciliation()

	if len(published) != 2 {
		t.Fatalf("publish count after one reconcile = %d, want 2", len(published))
	}
	done := published[1]
	if done.LastReconcileAt.IsZero() {
		t.Fatal("post-reconcile record LastReconcileAt is zero; a completed reconcile must advance the heartbeat")
	}
	if !done.ArmedAt.Equal(arm.ArmedAt) {
		t.Errorf("post-reconcile ArmedAt = %s, want the install-time arm stamp %s", done.ArmedAt, arm.ArmedAt)
	}
	if done.IntervalMs <= 0 {
		t.Errorf("post-reconcile IntervalMs = %d, want a positive cadence", done.IntervalMs)
	}
}

// TestSetReconcileHeartbeatSinkNilDoesNotPublishOrStamp pins that clearing the
// sink neither publishes nor starts the watchdog clock: only a real installer
// opts a store into liveness reporting.
func TestSetReconcileHeartbeatSinkNilDoesNotPublishOrStamp(t *testing.T) {
	backing := NewMemStore()
	c := NewCachingStoreForTestWithPrefix(backing, "ga", nil)
	c.SetReconcileHeartbeatSink(nil)
	c.lifecycleMu.Lock()
	armed := c.reconcilerArmedAt
	c.lifecycleMu.Unlock()
	if !armed.IsZero() {
		t.Errorf("reconcilerArmedAt = %s after nil install, want zero", armed)
	}
}

// TestReconcileHeartbeatSinkDefaultsOff pins that a store nobody wired
// publishes nothing: every CLI-path and test store keeps its previous behavior.
func TestReconcileHeartbeatSinkDefaultsOff(t *testing.T) {
	backing := NewMemStore()
	c := NewCachingStoreForTestWithPrefix(backing, "ga", nil)
	// No panic, no write, no observable effect.
	c.publishReconcileHeartbeat()
	c.runReconciliation()
}
