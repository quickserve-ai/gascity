package beads

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreReadBudgetBoundsStorageLockBehindPendingReconnect pins
// ga-yuiof4 step 1: the RWMutex convoy lead for the 2026-09-27 48-minute
// city-wide order stall.
//
// Before the fix (ga-yuiof4.1), withReadRetry derived one wall-clock ctx (the
// read budget) for the whole read chain, but acquireStorageGen took
// s.mu.RLock() with no ctx and held it across the read, and the reconnect path
// took s.mu.Lock() with no ctx: first in acquireReconnectGate, to create the
// gate lazily, before it ever called reopen, and again to install the fresh
// handle. A sync.RWMutex refuses NEW readers once a writer is waiting, so on
// that store:
//
//	R1  held RLock across fn() on a hung connection that ignored its ctx;
//	R3  failed transiently, entered reconnect, and waited in s.mu.Lock()
//	    inside acquireReconnectGate, behind R1, before any reopen;
//	R2  (a fresh, unrelated read) then waited in s.mu.RLock() behind R3,
//	    outside any deadline, until R1 returned.
//
// The fixed store holds s.mu only to pick a storage handle, never across I/O,
// and creates the gate without it, so R1 holds nothing while hung and R3's
// reconnect reaches reopen at once; its only s.mu.Lock() is the brief swap
// after reopen.
//
// The contract under test: R2 returns within its read budget (plus a margin),
// and without error, whatever R1 and R3 are doing. The test releases R1
// itself, so it cannot hang past its hard timeout.
//
// R2 is issued once R3's failure has visibly reached the reconnect path, by
// whichever of three signals comes first: the reopen hook having been called
// (the fixed store's reconnect gets that far without waiting on R1), R3
// having returned, or a writer queued on s.mu before any reopen (on the
// pre-fix store, R3's acquireReconnectGate queued behind R1 there: the
// convoy). The fixed store queues no writer before reopen, so there the
// writer signal cannot fire; the pre-fix store never reaches reopen while R1
// is hung, so there only the writer signal fires. The test logs which one
// fired.
func TestNativeDoltStoreReadBudgetBoundsStorageLockBehindPendingReconnect(t *testing.T) {
	const (
		budget      = 200 * time.Millisecond
		margin      = 2 * time.Second
		hardTimeout = 10 * time.Second
	)
	testStart := time.Now()
	hard := time.After(hardTimeout)

	r1Entered := make(chan struct{})
	var r1EnteredOnce sync.Once
	releaseR1 := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseR1) }) }
	t.Cleanup(release)

	searchIssues := func(ctx context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
		id := ""
		if len(f.IDs) > 0 {
			id = f.IDs[0]
		}
		switch id {
		case "gc-r1":
			// A read on a hung server connection: it ignores ctx and only
			// returns when the test releases it.
			r1EnteredOnce.Do(func() { close(r1Entered) })
			<-releaseR1
			return nil, errors.New("invalid connection")
		case "gc-r3":
			// A read that fails transiently, arming the reconnect path.
			return nil, errors.New("invalid connection")
		default:
			// A healthy read that honors ctx the way the beads lib's
			// begin-read-tx does.
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("begin read tx: %w", err)
			}
			return []*beadslib.Issue{{
				ID: id, Title: "healthy", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
			}}, nil
		}
	}
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{searchIssues: searchIssues})
	store.readRetryBudgetOverride = budget
	reopenCalled := make(chan struct{})
	var reopenCalledOnce sync.Once
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		reopenCalledOnce.Do(func() { close(reopenCalled) })
		// A distinct handle per reopen, as the real hook returns, so closing a
		// retired handle can never reach the storage the current handle wraps.
		return &nativeDoltStorageSpy{searchIssues: searchIssues}, nil
	}

	type result struct {
		err  error
		took time.Duration
		at   time.Duration // since testStart
	}
	run := func(id string) <-chan result {
		ch := make(chan result, 1)
		go func() {
			started := time.Now()
			_, err := store.Get(id)
			ch <- result{err: err, took: time.Since(started), at: time.Since(testStart)}
		}()
		return ch
	}

	// R1: holds its storage acquisition across a read that ignores its ctx.
	r1 := run("gc-r1")
	select {
	case <-r1Entered:
	case <-hard:
		t.Fatal("R1 never entered its read")
	}

	// R3: fails transiently and heads into reconnect.
	r3 := run("gc-r3")

	// Wait until R3's failure has reached the reconnect path. The reopen and
	// R3 signals are checked first. The writer check uses TryRLock, which
	// fails only while a writer holds or waits for the lock (readerCount < 0),
	// so it observes a queued Lock() directly rather than guessing from
	// elapsed time. A queued writer has no signal to wait on, which is why
	// this one observation goes through pollBoundary.
	var r3Res result
	r3Returned := false
	const reopenCalledLabel = "reopen hook called (R3's reconnect did not queue behind R1)"
	syncCtx, cancelSync := context.WithTimeout(context.Background(), margin)
	defer cancelSync()
	syncCondition, synced := pollBoundary(syncCtx, func() (string, bool) {
		select {
		case <-reopenCalled:
			return reopenCalledLabel, true
		case r3Res = <-r3:
			r3Returned = true
			return fmt.Sprintf("R3 returned (took=%s err=%v)", r3Res.took, r3Res.err), true
		default:
		}
		if !store.mu.TryRLock() {
			// On the fixed store the only writer on s.mu here is reconnect's
			// brief handle swap, which comes after the reopen hook; re-check so
			// that swap is never reported as the convoy. On the pre-fix store
			// this writer is acquireReconnectGate's Lock, before any reopen.
			select {
			case <-reopenCalled:
				return reopenCalledLabel, true
			default:
				return "writer queued on s.mu before any reopen: R3's reconnect is waiting behind R1 (the pre-fix convoy)", true
			}
		}
		store.mu.RUnlock()
		return "no writer queued on s.mu, reopen hook not called, R3 still running", false
	})
	syncAt := time.Since(testStart)
	if !synced {
		release()
		t.Fatalf("within %s of R3 starting, last observation: %s: R3's transient failure never reached the reconnect path", margin, syncCondition)
	}
	t.Logf("sync point at %s: %s", syncAt, syncCondition)

	// R2: an unrelated read issued while R1 is still hung.
	r2Start := time.Since(testStart)
	r2 := run("gc-r2")

	var r2Res result
	r2Bounded := false
	select {
	case r2Res = <-r2:
		r2Bounded = true
	case <-time.After(budget + margin):
		t.Logf("R2 still blocked %s after issue (budget %s); goroutine stacks in the store:\n%s",
			budget+margin, budget, storeLockStacks())
	case <-hard:
		t.Fatal("hard timeout waiting on R2")
	}

	// Release R1 in every case, then collect everyone under the hard timeout.
	releasedAt := time.Since(testStart)
	release()
	if !r2Bounded {
		select {
		case r2Res = <-r2:
		case <-hard:
			t.Fatal("R2 did not return even after R1 was released (hard timeout)")
		}
	}
	var r1Res result
	select {
	case r1Res = <-r1:
	case <-hard:
		t.Fatal("R1 did not return after release (hard timeout)")
	}
	if !r3Returned {
		select {
		case r3Res = <-r3:
		case <-hard:
			t.Fatal("R3 did not return after R1 release (hard timeout)")
		}
	}

	t.Logf("timings (since test start): syncPoint=%s r2Issued=%s r1Released=%s", syncAt, r2Start, releasedAt)
	t.Logf("R1: took=%s returnedAt=%s err=%v", r1Res.took, r1Res.at, r1Res.err)
	t.Logf("R3: took=%s returnedAt=%s err=%v", r3Res.took, r3Res.at, r3Res.err)
	t.Logf("R2: took=%s returnedAt=%s err=%v", r2Res.took, r2Res.at, r2Res.err)

	if !r2Bounded {
		t.Fatalf("CONVOY: R2 (budget %s) did not return within %s while R1's read was hung (sync point: %s); it returned only after R1 was released (R2 took %s, returned %s after R1 release) with err=%v",
			budget, budget+margin, syncCondition, r2Res.took, r2Res.at-releasedAt, r2Res.err)
	}
	if r2Res.err != nil {
		t.Fatalf("R2 returned within its budget while R1's read was hung, but failed: %v", r2Res.err)
	}
}

// pollBoundaryTick is how often pollBoundary observes its boundary.
const pollBoundaryTick = time.Millisecond

// pollBoundary observes a black-box boundary that exposes no completion
// signal: it calls observe at once and then on every tick of a ticker, until
// observe reports done or ctx ends, and returns the last observed state with
// whether observe reported done. A caller that gives up can therefore say what
// it last saw. TESTING.md allows polling only at such a boundary, through a
// helper like this one rather than a sleep loop.
//
// Boundary owner: TestNativeDoltStoreReadBudgetBoundsStorageLockBehindPendingReconnect.
// The convoy it pins shows only as a writer queued on s.mu, and a
// sync.RWMutex exposes no signal for a queued writer; giving it one would
// change the production lock for a test.
func pollBoundary(ctx context.Context, observe func() (state string, done bool)) (string, bool) {
	ticker := time.NewTicker(pollBoundaryTick)
	defer ticker.Stop()
	for {
		state, done := observe()
		if done {
			return state, true
		}
		select {
		case <-ctx.Done():
			return state, false
		case <-ticker.C:
		}
	}
}

// storeLockStacks returns the goroutine stacks that are inside the native Dolt
// store, trimmed to the sync and native_dolt_store.go frames, so a red run
// names the exact lock call each reader is parked on.
func storeLockStacks() string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var out strings.Builder
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, "native_dolt_store.go") || !strings.Contains(g, "sync.") {
			continue
		}
		lines := strings.Split(g, "\n")
		out.WriteString(lines[0] + "\n")
		for i := 1; i+1 < len(lines); i += 2 {
			fn, loc := strings.TrimSpace(lines[i]), strings.TrimSpace(lines[i+1])
			if strings.Contains(loc, "native_dolt_store") || strings.HasPrefix(fn, "sync.") {
				if j := strings.LastIndex(loc, " +0x"); j > 0 {
					loc = loc[:j]
				}
				out.WriteString("    " + fn + "\n        " + loc + "\n")
			}
		}
	}
	return out.String()
}
