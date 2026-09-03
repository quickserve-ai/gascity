package beads

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestReconcileOverdueMeasuresFromTheReconcileClockNotTheWriteClock is the
// central assertion of ga-yc0chj. During the measured 3h31m stall the cache was
// still taking writes, so lastFreshAt stayed current the whole time; anything
// keyed on it would have reported a healthy cache. Only a COMPLETED reconcile
// may reset the staleness clock.
func TestReconcileOverdueMeasuresFromTheReconcileClockNotTheWriteClock(t *testing.T) {
	c := NewCachingStore(NewMemStore(), nil)
	now := time.Date(2026, 9, 3, 17, 0, 0, 0, time.UTC)
	stale := now.Add(-4 * time.Hour)

	c.mu.Lock()
	c.stats.ReconcilerArmedAt = stale
	c.stats.LastReconcileAt = stale
	// A write one second ago: exactly what event-bus traffic did through the
	// stall. This must NOT make the cache look fresh.
	c.markFreshLocked(now.Add(-time.Second))
	overdue := c.reconcileOverdueLocked(now)
	c.mu.Unlock()

	if !overdue {
		t.Fatal("a cache whose last COMPLETED reconcile was 4h ago reads as fresh because a write bumped lastFreshAt — this is the ga-yc0chj blindness")
	}
}

// An armed reconciler that has never completed a pass is the exact ga-yc0chj
// shape (prime hung, loop never ran), so zero LastReconcileAt must not read as
// fresh — it is measured from the arming clock instead.
func TestReconcileOverdueCoversArmedButNeverReconciled(t *testing.T) {
	c := NewCachingStore(NewMemStore(), nil)
	now := time.Date(2026, 9, 3, 17, 0, 0, 0, time.UTC)

	c.mu.Lock()
	c.stats.ReconcilerArmedAt = now.Add(-4 * time.Hour)
	c.stats.LastReconcileAt = time.Time{}
	overdue := c.reconcileOverdueLocked(now)
	c.mu.Unlock()

	if !overdue {
		t.Fatal("armed 4h ago with no completed reconcile reads as fresh; that is the stall this watchdog exists for")
	}
}

// A store with no reconciler armed at all was never promised one — the
// suspended-rig opt-out builds exactly that — so it must not alarm.
func TestReconcileNotOverdueWhenNoReconcilerWasEverArmed(t *testing.T) {
	c := NewCachingStore(NewMemStore(), nil)
	now := time.Date(2026, 9, 3, 17, 0, 0, 0, time.UTC)

	c.mu.Lock()
	overdue := c.reconcileOverdueLocked(now)
	c.mu.Unlock()

	if overdue {
		t.Fatal("a store with no reconciler armed reported overdue; every deliberately static store would alarm")
	}
}

// A cache reconciling on cadence must stay silent, including right after a pass.
func TestReconcileNotOverdueWhileTicking(t *testing.T) {
	c := NewCachingStore(NewMemStore(), nil)
	now := time.Date(2026, 9, 3, 17, 0, 0, 0, time.UTC)

	c.mu.Lock()
	c.stats.ReconcilerArmedAt = now.Add(-time.Hour)
	c.stats.LastReconcileAt = now.Add(-30 * time.Second)
	overdue := c.reconcileOverdueLocked(now)
	bound := c.reconcileStaleBoundLocked()
	c.mu.Unlock()

	if overdue {
		t.Fatalf("a cache that reconciled 30s ago reported overdue (bound %v)", bound)
	}
	// The floor is what keeps a SMALL cadence from alarming after a couple of
	// minutes on a loaded box: five 30s intervals is 2m30s, well inside the
	// noise, so the floor has to win there.
	if bound < cacheReconcileStaleFloor {
		t.Fatalf("effective bound %v is below the floor %v", bound, cacheReconcileStaleFloor)
	}
}

// The watchdog must actually RECORD the stall, not merely be able to compute
// it: the whole defect is that nothing watched.
func TestCheckReconcileOverdueRecordsTheStall(t *testing.T) {
	var logged []string
	c := NewCachingStore(NewMemStore(), nil)
	c.problemf = func(msg string) { logged = append(logged, msg) }
	now := time.Date(2026, 9, 3, 17, 0, 0, 0, time.UTC)

	c.mu.Lock()
	c.stats.ReconcilerArmedAt = now.Add(-4 * time.Hour)
	c.stats.LastReconcileAt = now.Add(-4 * time.Hour)
	c.mu.Unlock()

	c.checkReconcileOverdue(now)

	st := c.Stats()
	if st.ReconcileOverdueCount != 1 {
		t.Fatalf("ReconcileOverdueCount = %d, want 1 — the stall was computed and then dropped on the floor", st.ReconcileOverdueCount)
	}
	if !st.LastReconcileOverdueAt.Equal(now) {
		t.Fatalf("LastReconcileOverdueAt = %v, want %v", st.LastReconcileOverdueAt, now)
	}
	if len(logged) == 0 {
		t.Fatal("nothing reached problemf; a stall nobody is told about is the defect, not the fix")
	}
	if !strings.Contains(logged[0], "reconcile") {
		t.Fatalf("logged %q, want it to name the reconcile stall", logged[0])
	}
}

// THE REGRESSION GUARD FOR THE WORST THING THIS WATCHDOG COULD DO.
//
// The obvious implementation reports through recordProblemLocked. That helper
// stamps stats.LastProblemAt — and nextReconcileDelay computes the retry
// deadline as LastProblemAt + backoff. A watchdog ticking every minute would
// push that deadline out by a minute every minute, so a store already in
// backoff (exactly the store this watchdog fires on) would never retry again:
// the alarm would convert a recoverable stall into a permanent one. The
// watchdog must leave every reconciler clock alone.
func TestOverdueWatchdogNeverTouchesTheRetryBackoffClock(t *testing.T) {
	c := NewCachingStore(NewMemStore(), nil)
	c.problemf = func(string) {}
	now := time.Date(2026, 9, 3, 17, 0, 0, 0, time.UTC)
	failedAt := now.Add(-90 * time.Second)

	c.mu.Lock()
	c.stats.ReconcilerArmedAt = now.Add(-4 * time.Hour)
	c.stats.LastReconcileAt = now.Add(-4 * time.Hour)
	// A store mid-backoff: five failures, last problem 90s ago.
	c.syncFailures = 5
	c.stats.LastProblemAt = failedAt
	c.stats.ProblemCount = 5
	c.mu.Unlock()

	dueBefore := c.nextReconcileDelay(now)
	for i := 0; i < 10; i++ {
		c.checkReconcileOverdue(now.Add(time.Duration(i) * time.Minute))
	}
	dueAfter := c.nextReconcileDelay(now)

	st := c.Stats()
	if !st.LastProblemAt.Equal(failedAt) {
		t.Fatalf("LastProblemAt moved from %v to %v — the watchdog is writing the retry-backoff anchor, so the reconciler's next attempt is being pushed out every tick", failedAt, st.LastProblemAt)
	}
	if st.ProblemCount != 5 {
		t.Fatalf("ProblemCount = %d, want 5 — the watchdog must keep its own counter, not inflate the reconciler's", st.ProblemCount)
	}
	if dueAfter != dueBefore {
		t.Fatalf("next reconcile delay moved from %v to %v across ten watchdog ticks — retry starvation", dueBefore, dueAfter)
	}
	if st.ReconcileOverdueCount != 10 {
		t.Fatalf("ReconcileOverdueCount = %d, want 10", st.ReconcileOverdueCount)
	}
}

// A restarted reconciler must not read as instantly overdue. ReconcilerArmedAt
// is refreshed on restart while LastReconcileAt still holds the pre-pause value,
// so the staleness clock has to be the LATER of the two.
func TestRestartedReconcilerIsNotInstantlyOverdue(t *testing.T) {
	c := NewCachingStore(NewMemStore(), nil)
	now := time.Date(2026, 9, 3, 17, 0, 0, 0, time.UTC)

	c.mu.Lock()
	c.stats.LastReconcileAt = now.Add(-6 * time.Hour) // before a long intentional pause
	c.stats.ReconcilerArmedAt = now.Add(-10 * time.Second)
	overdue := c.reconcileOverdueLocked(now)
	c.mu.Unlock()

	if overdue {
		t.Fatal("a reconciler armed 10s ago reads as overdue because of a stale pre-restart LastReconcileAt — that false-alarms on the event most likely to follow an operator touching the store")
	}
}

// The log line must be rate-limited on a window LONGER than the tick, or a
// stall produces a line every tick for its whole duration — and the cure for
// that noise is always to stop believing the alarm. The counter must keep
// advancing regardless: suppression quiets the log, never the evidence.
func TestOverdueLogIsRateLimitedButTheCounterIsNot(t *testing.T) {
	var logged []string
	c := NewCachingStore(NewMemStore(), nil)
	c.problemf = func(msg string) { logged = append(logged, msg) }
	base := time.Date(2026, 9, 3, 17, 0, 0, 0, time.UTC)

	c.mu.Lock()
	c.stats.ReconcilerArmedAt = base.Add(-4 * time.Hour)
	c.stats.LastReconcileAt = base.Add(-4 * time.Hour)
	c.mu.Unlock()

	if cacheReconcileOverdueLogWindow <= cacheReconcileWatchdogTick {
		t.Fatalf("log window %v is not longer than the watchdog tick %v; the alarm will log every tick", cacheReconcileOverdueLogWindow, cacheReconcileWatchdogTick)
	}

	// Ten ticks one minute apart, all inside one 15-minute log window.
	for i := 0; i < 10; i++ {
		c.checkReconcileOverdue(base.Add(time.Duration(i) * time.Minute))
	}
	if len(logged) != 1 {
		t.Fatalf("problemf fired %d times across ten one-minute ticks inside one log window, want 1: %v", len(logged), logged)
	}
	// Past the window, it speaks again — a stall that outlives the window is
	// still worth one more line.
	c.checkReconcileOverdue(base.Add(cacheReconcileOverdueLogWindow + time.Minute))
	if len(logged) != 2 {
		t.Fatalf("problemf fired %d times, want 2 — the alarm went permanently quiet on an ongoing stall", len(logged))
	}
	if got := c.Stats().ReconcileOverdueCount; got != 11 {
		t.Fatalf("ReconcileOverdueCount = %d, want 11 — suppression must quiet the LOG, never the counter", got)
	}
}

// End to end through the real goroutine, and it must actually WEDGE: the point
// of a separate watchdog goroutine is that it survives the reconcile loop being
// stuck inside a context-free backing.List, so the test has to WAIT until the
// loop is genuinely inside that call before judging anything.
//
// Cleanup order matters and is the reason for the explicit release below rather
// than a deferred one: StopReconciler waits on the reconcile goroutine, which
// cannot return until the scan is released, so releasing after the stop would
// deadlock the test.
func TestWatchdogFiresWhileTheReconcileLoopIsWedged(t *testing.T) {
	backing := &fullScanBlockingStore{
		Store:   NewMemStore(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	c := NewCachingStore(backing, nil)

	fired := make(chan string, 8)
	c.problemf = func(msg string) {
		select {
		case fired <- msg:
		default:
		}
	}

	prevTick := cacheReconcileWatchdogTick
	cacheReconcileWatchdogTick = 5 * time.Millisecond
	defer func() { cacheReconcileWatchdogTick = prevTick }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartReconciler(ctx, WithStaggerOff(), "watchdog-test")

	// Wait for the loop to be genuinely inside backing.List. Without this the
	// watchdog would fire during the loop's first poll delay and the test would
	// pass without ever exercising a wedge.
	select {
	case <-backing.entered:
	case <-time.After(30 * time.Second):
		close(backing.release)
		c.StopReconciler()
		t.Fatal("the reconcile loop never entered backing.List; the wedge was never established")
	}

	// Now it is stuck. Backdate the arming clock so the next watchdog tick is
	// already past the staleness bound, rather than sleeping through a real one.
	c.mu.Lock()
	c.stats.ReconcilerArmedAt = time.Now().Add(-24 * time.Hour)
	c.stats.LastReconcileAt = time.Time{}
	c.mu.Unlock()

	var got string
	select {
	case got = <-fired:
	case <-time.After(30 * time.Second):
		close(backing.release)
		c.StopReconciler()
		t.Fatal("watchdog never fired while the reconcile loop was wedged in backing.List — it is not independent of the wedged goroutine")
	}

	// Release BEFORE stopping: StopReconciler waits for the reconcile goroutine,
	// which is inside the blocked scan.
	close(backing.release)
	c.StopReconciler()

	if !strings.Contains(got, "reconcile") {
		t.Fatalf("problem = %q, want it to name the reconcile stall", got)
	}
}

// fullScanBlockingStore blocks every full-scan List until released, modelling a
// reconcile loop wedged in a context-free backing call. It signals `entered`
// once, so a test can wait for the wedge to be real before judging it.
type fullScanBlockingStore struct {
	Store
	enterOnce sync.Once
	entered   chan struct{}
	release   chan struct{}
}

func (s *fullScanBlockingStore) List(query ListQuery) ([]Bead, error) {
	if query.AllowScan {
		s.enterOnce.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.Store.List(query)
}
