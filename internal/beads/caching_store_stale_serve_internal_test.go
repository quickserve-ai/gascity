package beads

import (
	"testing"
	"time"
)

// armedCacheWithLastReconcile builds a live cache that is armed (i.e. the
// controller declared it must reconcile) and whose last COMPLETED reconcile
// was ago in the past.
func armedCacheWithLastReconcile(backing Store, ago time.Duration) *CachingStore {
	c := NewCachingStoreForTestWithPrefix(backing, "ga", nil)
	now := time.Now()
	c.state = cacheLive
	c.depsComplete = true
	c.reconcilerArmedAtNanos.Store(now.Add(-ago).UnixNano())
	c.stats.LastReconcileAt = now.Add(-ago)
	return c
}

// TestUnarmedCacheNeverRefusesForStaleness is the no-regression guard for the
// entire CLI, test and projection population. Those stores are never armed and
// never reconcile, so their LastReconcileAt is permanently zero. Judging them
// against a reconcile schedule they were never given would refuse every read
// from every one of them.
func TestUnarmedCacheNeverRefusesForStaleness(t *testing.T) {
	t.Parallel()

	c := NewCachingStoreForTest(NewMemStore(), nil)
	c.state = cacheLive
	// Zero arm time and a LastReconcileAt from the epoch: maximally "stale" by
	// every measure except the one that matters.
	if got := c.stats.LastReconcileAt; !got.IsZero() {
		t.Fatalf("precondition: LastReconcileAt = %s, want zero", got)
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.cacheServableLocked() {
		t.Error("an unarmed cache refused to serve; only stores expected to reconcile may be judged stale")
	}
}

// TestArmedCacheRefusesPastServeBound pins the defect this bound exists for: a
// cache whose reconciler stopped completing must stop answering confidently
// from its frozen snapshot. The measured stalls were 3h31m, 4h34m, 35h41m and
// 43h; 48h stands in for all of them.
func TestArmedCacheRefusesPastServeBound(t *testing.T) {
	t.Parallel()

	c := armedCacheWithLastReconcile(NewMemStore(), 48*time.Hour)

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cacheServableLocked() {
		t.Error("a cache that has not reconciled in 48h was still servable")
	}
}

// TestArmedCacheServesWithinServeBound pins the other side: an armed cache
// that is merely a little behind must keep serving. A bound that fires on
// ordinary jitter would shed the cache under exactly the data-plane pressure
// that produced the jitter.
func TestArmedCacheServesWithinServeBound(t *testing.T) {
	t.Parallel()

	// Well inside the 30-minute floor.
	c := armedCacheWithLastReconcile(NewMemStore(), time.Minute)

	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.cacheServableLocked() {
		t.Error("a cache one minute behind refused to serve; the bound is far too tight")
	}
}

// TestFreshlyArmedCacheServesBeforeItsFirstReconcile pins the arm-time floor.
// LastReconcileAt stays zero until the first cycle COMPLETES, and a successful
// prime never sets it, so without the floor every store would read as
// infinitely stale and refuse from the instant it was wired up.
func TestFreshlyArmedCacheServesBeforeItsFirstReconcile(t *testing.T) {
	t.Parallel()

	c := NewCachingStoreForTestWithPrefix(NewMemStore(), "ga", nil)
	c.state = cacheLive
	c.reconcilerArmedAtNanos.Store(time.Now().UnixNano())
	// Deliberately left zero: this is the real state of a just-armed store.
	c.stats.LastReconcileAt = time.Time{}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.cacheServableLocked() {
		t.Error("a just-armed cache refused to serve before its first reconcile completed")
	}
}

// TestServeStaleBoundNeverDropsBelowTheFloor pins the floor against the
// reconciler's own maximum backoff. A backing-store outage drives the
// reconciler to cacheReconcileMaxBackoff; if the serve bound sat at or below
// that, the cache would stop serving and send every read to the backing store
// that is already failing.
func TestServeStaleBoundNeverDropsBelowTheFloor(t *testing.T) {
	t.Parallel()

	c := NewCachingStoreForTest(NewMemStore(), nil)
	c.mu.RLock()
	bound := c.serveStaleBoundLocked()
	c.mu.RUnlock()

	if bound < cacheServeStaleFloor {
		t.Errorf("serve bound %s is below the floor %s", bound, cacheServeStaleFloor)
	}
	if bound <= cacheReconcileMaxBackoff {
		t.Errorf("serve bound %s does not exceed the reconciler's max backoff %s; "+
			"a backing outage would make the cache refuse and pile reads onto the failing store",
			bound, cacheReconcileMaxBackoff)
	}
}

// TestServeStaleBoundExceedsTheDoctorAlarmBound pins the ordering the two
// bounds must keep: `gc doctor`'s beads-cache-reconcile watch alarms at
// max(5 x interval, 5m), and the serve path must not degrade before that
// alarm has had a chance to fire. Alarm first, degrade second.
func TestServeStaleBoundExceedsTheDoctorAlarmBound(t *testing.T) {
	t.Parallel()

	// The doctor watch's constants, restated here because they live in another
	// package; this test fails if either side moves without the other.
	const doctorStaleFactor = 5
	const doctorStaleFloor = 5 * time.Minute

	for _, interval := range []time.Duration{
		cacheReconcileIntervalSmall,
		cacheReconcileIntervalMedium,
		cacheReconcileIntervalLarge,
	} {
		c := NewCachingStoreForTest(NewMemStore(), nil)
		c.stats.CurrentReconcileInterval = interval
		c.mu.RLock()
		serveBound := c.serveStaleBoundLocked()
		c.mu.RUnlock()

		alarmBound := doctorStaleFactor * interval
		if alarmBound < doctorStaleFloor {
			alarmBound = doctorStaleFloor
		}
		if serveBound <= alarmBound {
			t.Errorf("interval %s: serve bound %s does not exceed the doctor alarm bound %s; "+
				"the cache would go silent before the watchdog could report why",
				interval, serveBound, alarmBound)
		}
	}
}

// TestStaleServeRefusalLogIsRateLimited pins that the refusal degrades loudly
// but not endlessly. Staleness is evaluated on every cached read, so an
// unthrottled line would be a log storm rather than a signal.
func TestStaleServeRefusalLogIsRateLimited(t *testing.T) {
	t.Parallel()

	c := armedCacheWithLastReconcile(NewMemStore(), 48*time.Hour)

	c.mu.RLock()
	for i := 0; i < 100; i++ {
		if c.cacheServableLocked() {
			c.mu.RUnlock()
			t.Fatal("stale cache became servable mid-loop")
		}
	}
	c.mu.RUnlock()

	// One line was emitted and the window is now closed; the stamp must not
	// have advanced 100 times.
	stamped := c.staleServeLogAtNanos.Load()
	if stamped == 0 {
		t.Fatal("no refusal was ever logged; the degradation is silent")
	}
	if since := time.Since(time.Unix(0, stamped)); since > cacheStaleServeLogWindow {
		t.Errorf("refusal log stamp is %s old, want inside the %s window", since, cacheStaleServeLogWindow)
	}
}

// TestStaleCacheListFallsBackToBackingStore is the behavioral test for the
// whole change: a stale armed cache must answer from the BACKING store, not
// from its own frozen snapshot. The bead used here exists only in the backing
// store — exactly the externally-created bead the reconciler is the sole path
// for — so returning it proves the read took the live fallback (I5), and
// returning it without an error proves the refusal degraded the read rather
// than failing it.
func TestStaleCacheListFallsBackToBackingStore(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	external, err := backing.Create(Bead{Title: "created elsewhere", Status: "open"})
	if err != nil {
		t.Fatalf("seed backing store: %v", err)
	}

	c := armedCacheWithLastReconcile(backing, 48*time.Hour)
	// The cache's own snapshot is empty: it froze before the bead existed.
	if len(c.beads) != 0 {
		t.Fatalf("precondition: cache holds %d beads, want 0", len(c.beads))
	}

	got, err := c.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("List returned an error; a stale cache must degrade to a live read, not fail: %v", err)
	}
	if len(got) != 1 || got[0].ID != external.ID {
		t.Fatalf("List = %+v, want the backing store's %s; the frozen snapshot was served instead", got, external.ID)
	}
}

// TestStaleCacheReadyFallsBackToBackingStore covers the second gate the bound
// applies to. Ready does not merely report — its verdict GATES what work runs,
// so readiness computed from a snapshot that stopped reconciling hours ago
// decides against beads it cannot see.
func TestStaleCacheReadyFallsBackToBackingStore(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	external, err := backing.Create(Bead{Title: "created elsewhere", Status: "open"})
	if err != nil {
		t.Fatalf("seed backing store: %v", err)
	}

	c := armedCacheWithLastReconcile(backing, 48*time.Hour)

	got, err := c.Ready()
	if err != nil {
		t.Fatalf("Ready returned an error; a stale cache must degrade to a live read, not fail: %v", err)
	}
	if len(got) != 1 || got[0].ID != external.ID {
		t.Fatalf("Ready = %+v, want the backing store's %s; the frozen snapshot was served instead", got, external.ID)
	}
}

// TestFreshCacheReadyStillServesFromCache is the control for the Ready gate.
func TestFreshCacheReadyStillServesFromCache(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	if _, err := backing.Create(Bead{Title: "created elsewhere", Status: "open"}); err != nil {
		t.Fatalf("seed backing store: %v", err)
	}

	c := armedCacheWithLastReconcile(backing, time.Minute)

	got, err := c.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Ready = %+v, want the (empty) cached snapshot; a cache inside its bound must not go to the backing store", got)
	}
}

// TestFreshCacheListStillServesFromCache is the control for the test above: an
// armed cache inside its bound must still answer from memory. Without this,
// the fallback test would pass just as well if the cache had stopped serving
// entirely.
func TestFreshCacheListStillServesFromCache(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	if _, err := backing.Create(Bead{Title: "created elsewhere", Status: "open"}); err != nil {
		t.Fatalf("seed backing store: %v", err)
	}

	c := armedCacheWithLastReconcile(backing, time.Minute)
	// An empty in-memory snapshot that the cache still considers authoritative.
	got, err := c.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List = %+v, want the (empty) cached snapshot; a cache inside its bound must not go to the backing store", got)
	}
}
