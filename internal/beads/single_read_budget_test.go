package beads

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"

	"github.com/gastownhall/gascity/internal/fsys"
)

// These tests pin ListQuery.SingleReadBudget (pl-nzyv): the cache's full scan
// runs ONCE under its own budget on the native store, and every other read
// keeps the default budget and its reconnect-and-retry chain.

func countingSearchStorage(calls *int32, err error, issues ...*beadslib.Issue) *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			atomic.AddInt32(calls, 1)
			if err != nil {
				return nil, err
			}
			return issues, nil
		},
	}
}

func TestNativeDoltStoreListSingleReadBudgetDoesNotReissue(t *testing.T) {
	var deadCalls, freshCalls, reopens int32
	dead := countingSearchStorage(&deadCalls, errors.New("[mysql] i/o timeout"))
	fresh := countingSearchStorage(&freshCalls, nil, &beadslib.Issue{
		ID: "gc-2", Title: "live", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	store := storeWithReopen(dead, fresh, &reopens)

	_, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, SingleReadBudget: 5 * time.Second})
	if err == nil || !errContains(err, "i/o timeout") {
		t.Fatalf("single-budget List error = %v, want the transient error returned, not retried", err)
	}
	if got := atomic.LoadInt32(&deadCalls); got != 1 {
		t.Fatalf("reads on the failed handle = %d, want exactly one attempt", got)
	}
	if got := atomic.LoadInt32(&freshCalls); got != 0 {
		t.Fatalf("reads on the fresh handle = %d, want 0: the scan must not be reissued", got)
	}
	if got := atomic.LoadInt32(&reopens); got != 1 {
		t.Fatalf("reopens = %d, want 1: the handle is re-pointed for the next read", got)
	}

	// The next read finds the live pool without reconnecting again.
	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("next List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-2" {
		t.Fatalf("next List = %#v, want [gc-2]", got)
	}
	if got := atomic.LoadInt32(&reopens); got != 1 {
		t.Fatalf("reopens after next read = %d, want still 1", got)
	}
}

func TestNativeDoltStoreListWithoutSingleReadBudgetStillRetries(t *testing.T) {
	var deadCalls, freshCalls, reopens int32
	dead := countingSearchStorage(&deadCalls, errors.New("[mysql] i/o timeout"))
	fresh := countingSearchStorage(&freshCalls, nil, &beadslib.Issue{
		ID: "gc-2", Title: "live", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	store := storeWithReopen(dead, fresh, &reopens)

	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("default List after transient error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-2" {
		t.Fatalf("default List = %#v, want [gc-2]", got)
	}
	if atomic.LoadInt32(&deadCalls) != 1 || atomic.LoadInt32(&freshCalls) != 1 {
		t.Fatalf("reads dead=%d fresh=%d, want 1 and 1: the default chain reissues",
			deadCalls, freshCalls)
	}
}

// blockingSearchStorage blocks each read until its context ends and records
// the read's deadline.
func blockingSearchStorage(calls *int32, deadlines chan<- time.Time) *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		searchIssues: func(ctx context.Context, _ string, _ beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			atomic.AddInt32(calls, 1)
			deadline, _ := ctx.Deadline()
			select {
			case deadlines <- deadline:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
}

func TestNativeDoltStoreListSingleReadBudgetReplacesStoreBudget(t *testing.T) {
	const storeBudget = 20 * time.Millisecond
	const scanBudget = 300 * time.Millisecond

	var calls int32
	deadlines := make(chan time.Time, 1)
	store := newNativeDoltStoreForTest(blockingSearchStorage(&calls, deadlines))
	store.readRetryBudgetOverride = storeBudget
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		t.Fatal("a read cut by its own budget must not reopen")
		return nil, nil
	}

	started := time.Now()
	_, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, SingleReadBudget: scanBudget})
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("List error = %v, want the budget's deadline", err)
	}
	deadline := <-deadlines
	if got := deadline.Sub(started); got < scanBudget-50*time.Millisecond || got > scanBudget+200*time.Millisecond {
		t.Fatalf("read deadline = start+%s, want start+~%s (the query's budget, not the store's %s)",
			got, scanBudget, storeBudget)
	}
	if elapsed < scanBudget-50*time.Millisecond {
		t.Fatalf("List returned after %s, before the query's %s budget", elapsed, scanBudget)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("reads = %d, want 1", got)
	}

	// Without the field the store's own budget still governs.
	started = time.Now()
	_, err = store.List(ListQuery{AllowScan: true, TierMode: TierBoth})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("default List error = %v, want the store budget's deadline", err)
	}
	if got := (<-deadlines).Sub(started); got > storeBudget+100*time.Millisecond {
		t.Fatalf("default read deadline = start+%s, want start+~%s", got, storeBudget)
	}
}

// budgetRecordingStore records the SingleReadBudget of every List the cache
// makes, split into full scans (no IDs) and id reads.
type budgetRecordingStore struct {
	*MemStore
	mu       sync.Mutex
	fullScan []time.Duration
	idReads  []time.Duration
}

func (s *budgetRecordingStore) List(q ListQuery) ([]Bead, error) {
	s.mu.Lock()
	if len(q.IDs) == 0 {
		s.fullScan = append(s.fullScan, q.SingleReadBudget)
	} else {
		s.idReads = append(s.idReads, q.SingleReadBudget)
	}
	s.mu.Unlock()
	return s.MemStore.List(q)
}

func TestCacheFullScanCarriesItsBudgetAndDeltaReadsDoNot(t *testing.T) {
	if got := cacheFullScanQuery().SingleReadBudget; got != 300*time.Second {
		t.Fatalf("cacheFullScanQuery().SingleReadBudget = %s, want 300s", got)
	}

	backing := &budgetRecordingStore{MemStore: NewMemStore()}
	b, err := backing.Create(Bead{Title: "one"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if _, err := cache.listForReconcile([]string{b.ID}); err != nil {
		t.Fatalf("listForReconcile: %v", err)
	}

	backing.mu.Lock()
	defer backing.mu.Unlock()
	if len(backing.fullScan) == 0 {
		t.Fatal("Prime made no full scan")
	}
	for i, got := range backing.fullScan {
		if got != cacheFullScanReadBudget {
			t.Fatalf("full scan %d budget = %s, want %s", i, got, cacheFullScanReadBudget)
		}
	}
	if len(backing.idReads) == 0 {
		t.Fatal("listForReconcile made no id read")
	}
	for i, got := range backing.idReads {
		if got != 0 {
			t.Fatalf("delta id read %d budget = %s, want 0 (default budget and retries)", i, got)
		}
	}
}

// blockingFullScanStore holds every full scan until release is closed and
// counts them; entered receives once per scan that started.
type blockingFullScanStore struct {
	*MemStore
	fullScans atomic.Int32
	entered   chan struct{}
	release   chan struct{}
}

func (s *blockingFullScanStore) List(q ListQuery) ([]Bead, error) {
	if len(q.IDs) == 0 {
		s.fullScans.Add(1)
		s.entered <- struct{}{}
		<-s.release
	}
	return s.MemStore.List(q)
}

func TestReconcileSkipsWhileAFullPrimeIsInFlight(t *testing.T) {
	backing := &blockingFullScanStore{
		MemStore: NewMemStore(),
		entered:  make(chan struct{}, 4),
		release:  make(chan struct{}),
	}
	if _, err := backing.MemStore.Create(Bead{Title: "one"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)

	primeDone := make(chan error, 1)
	go func() { primeDone <- cache.Prime(context.Background()) }()
	select {
	case <-backing.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Prime never reached the backing List")
	}

	// The reconciler armed while the prime still runs: a reconcile is "due"
	// (nothing has landed yet), but it must not start a second full scan.
	if d := cache.nextReconcileDelay(cache.clockNow()); d != 0 {
		t.Fatalf("nextReconcileDelay during the initial prime = %s, want 0 (the case under test)", d)
	}
	// Run it on its own goroutine: a regression would block in a second
	// scan, and the test must fail on that, not hang.
	reconcileDone := make(chan struct{})
	go func() {
		cache.reconcileIfDue(cache.clockNow())
		close(reconcileDone)
	}()
	select {
	case <-backing.entered:
		t.Errorf("reconcile started a second full scan while the prime was in flight")
	case <-reconcileDone:
	case <-time.After(5 * time.Second):
		t.Error("reconcileIfDue neither returned nor scanned")
	}

	close(backing.release)
	select {
	case <-reconcileDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reconcileIfDue did not return after release")
	}
	select {
	case err := <-primeDone:
		if err != nil {
			t.Fatalf("Prime: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prime did not finish after release")
	}
	if got := backing.fullScans.Load(); got != 1 {
		t.Fatalf("full scans after the prime = %d, want 1", got)
	}
}

func TestReconcileRecoversFromAPrimeRunningPastTheSkipBound(t *testing.T) {
	backing := &blockingFullScanStore{
		MemStore: NewMemStore(),
		entered:  make(chan struct{}, 4),
		release:  make(chan struct{}),
	}
	if _, err := backing.MemStore.Create(Bead{Title: "one"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var mu sync.Mutex
	now := time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	cache := NewCachingStoreForTest(backing, nil, WithClock(clock))

	primeDone := make(chan error, 1)
	go func() { primeDone <- cache.Prime(context.Background()) }()
	select {
	case <-backing.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Prime never reached the backing List")
	}

	// The prime is hung past every bounded attempt it could make: reconcile
	// must run as the recovery the controller armed it for.
	mu.Lock()
	now = now.Add(cacheFullPrimeSkipBound + time.Second)
	mu.Unlock()
	reconcileDone := make(chan struct{})
	go func() {
		cache.reconcileIfDue(cache.clockNow())
		close(reconcileDone)
	}()
	select {
	case <-backing.entered:
	case <-reconcileDone:
		t.Error("reconcile skipped a prime running past cacheFullPrimeSkipBound; a hung prime would never be recovered")
	case <-time.After(5 * time.Second):
		t.Error("reconcileIfDue neither scanned nor returned")
	}

	close(backing.release)
	for name, ch := range map[string]<-chan struct{}{"reconcile": reconcileDone} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not finish after release", name)
		}
	}
	select {
	case <-primeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Prime did not finish after release")
	}
	if got := backing.fullScans.Load(); got != 2 {
		t.Fatalf("full scans = %d, want 2 (the hung prime's and the recovery reconcile's)", got)
	}
}

func TestStoresWithoutRetryChainIgnoreSingleReadBudget(t *testing.T) {
	file, err := OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	for name, store := range map[string]Store{"mem": NewMemStore(), "file": file} {
		t.Run(name, func(t *testing.T) {
			for _, title := range []string{"a", "b"} {
				if _, err := store.Create(Bead{Title: title}); err != nil {
					t.Fatalf("Create: %v", err)
				}
			}
			// An explicit sort on both sides: the default order of these
			// stores is map iteration, so two correct listings may differ.
			withoutBudget := cacheFullScanQueryWithout()
			withoutBudget.Sort = SortCreatedAsc
			plain, err := store.List(withoutBudget)
			if err != nil {
				t.Fatalf("List without budget: %v", err)
			}
			withBudget := cacheFullScanQuery()
			withBudget.Sort = SortCreatedAsc
			budgeted, err := store.List(withBudget)
			if err != nil {
				t.Fatalf("List with budget: %v", err)
			}
			if len(plain) != 2 || !reflect.DeepEqual(plain, budgeted) {
				t.Fatalf("List with budget = %#v, want the same as without = %#v", budgeted, plain)
			}
			if _, err := store.List(ListQuery{SingleReadBudget: time.Second}); !errors.Is(err, ErrQueryRequiresScan) {
				t.Fatalf("budget-only List error = %v, want ErrQueryRequiresScan: the budget is not a filter", err)
			}
		})
	}
}

func cacheFullScanQueryWithout() ListQuery {
	q := cacheFullScanQuery()
	q.SingleReadBudget = 0
	return q
}
