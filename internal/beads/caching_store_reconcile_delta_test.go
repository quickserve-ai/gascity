package beads

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// digestingStore is a MemStore whose digest covers every field the cache
// holds, and which records each List the cache makes: a full scan (no IDs)
// or the ids a delta pass asked for.
type digestingStore struct {
	*MemStore
	mu        sync.Mutex
	fullLists int
	idLists   [][]string
	gets      int
	digestErr error
}

func newDigestingStore() *digestingStore { return &digestingStore{MemStore: NewMemStore()} }

func (s *digestingStore) List(q ListQuery) ([]Bead, error) {
	s.mu.Lock()
	if len(q.IDs) == 0 {
		s.fullLists++
	} else {
		s.idLists = append(s.idLists, slices.Clone(q.IDs))
	}
	s.mu.Unlock()
	return s.MemStore.List(q)
}

func (s *digestingStore) Get(id string) (Bead, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.MemStore.Get(id)
}

func (s *digestingStore) reconcileDigestForCache() (map[string]string, error) {
	if s.digestErr != nil {
		return nil, s.digestErr
	}
	rows, err := s.MemStore.List(cacheFullScanQuery())
	if err != nil {
		return nil, err
	}
	digest := make(map[string]string, len(rows))
	for _, b := range rows {
		deps, err := s.DepList(b.ID, "down")
		if err != nil {
			return nil, err
		}
		digest[b.ID] = fmt.Sprintf("%s|%s|%s|%v|%v", b.Status, b.Title, b.Assignee, b.Labels, deps)
	}
	return digest, nil
}

// planReconcile decides, row by row, what a delta pass lists and what it
// leaves alone, and declines the delta whenever its preconditions fail.
func TestPlanReconcileListsWhatMovedAndKeepsTheRest(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	backing := newDigestingStore()
	var ids []string
	for _, title := range []string{"same", "moved", "dirty", "rewritten", "evicted", "new"} {
		b, err := backing.Create(Bead{Title: title})
		if err != nil {
			t.Fatalf("Create %s: %v", title, err)
		}
		ids = append(ids, b.ID)
	}
	same, moved, dirty, rewritten, evicted, fresh := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5]
	digest, err := backing.reconcileDigestForCache()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil, WithClock(func() time.Time { return now }))

	arm := func() {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		cache.state = cacheLive
		cache.lastFullListAt = now.Add(-time.Minute)
		cache.reconcileDigest = map[string]string{}
		cache.reconcileFingerprints = map[string]uint64{}
		cache.beads = map[string]Bead{}
		cache.dirty = map[string]struct{}{}
		for _, id := range []string{same, moved, dirty, rewritten} {
			b, _ := backing.MemStore.Get(id)
			cache.beads[id] = b
			cache.reconcileDigest[id] = digest[id]
			cache.reconcileFingerprints[id] = reconcileFingerprint(b, cache.deps[id])
		}
		cache.reconcileDigest[evicted] = digest[evicted] // certified, then evicted
		cache.reconcileDigest[moved] = "an older digest"
		cache.dirty[dirty] = struct{}{}
		r := cache.beads[rewritten]
		r.Title = "rewritten by an event"
		cache.beads[rewritten] = r
	}

	arm()
	plan := cache.planReconcile(now)
	if !plan.delta {
		t.Fatalf("plan declined the delta with a live cache and a fresh baseline")
	}
	if want := []string{moved, dirty, rewritten, evicted, fresh}; !slices.Equal(plan.fetch, slices.Sorted(slices.Values(want))) {
		t.Fatalf("fetch = %v, want %v", plan.fetch, want)
	}
	if _, kept := plan.keep[same]; !kept || len(plan.keep) != 1 {
		t.Fatalf("keep = %v, want only %s", plan.keep, same)
	}

	for name, spoil := range map[string]func(){
		"no baseline":           func() { cache.reconcileDigest = nil },
		"the cache is not live": func() { cache.state = cachePartial },
		"the full list is old":  func() { cache.lastFullListAt = now.Add(-cacheReconcileFullListEvery) },
	} {
		arm()
		cache.mu.Lock()
		spoil()
		cache.mu.Unlock()
		if plan := cache.planReconcile(now); plan.delta || plan.digest == nil {
			t.Fatalf("%s: plan = delta %v with digest %v; want a full pass that still read the digest", name, plan.delta, plan.digest != nil)
		}
	}
}

// The digest a pass records certifies only content the pass verified.
func TestReconcileRecordsOnlyTheDigestThePassVerified(t *testing.T) {
	cache := NewCachingStoreForTest(NewMemStore(), nil)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	row := func(id, title string, labels ...string) Bead {
		return Bead{ID: id, Title: title, Status: "open", Labels: labels}
	}
	cache.beads = map[string]Bead{
		"kept":      row("kept", "k"),
		"rewritten": row("rewritten", "event rewrote me"),
		"absorbed":  row("absorbed", "new"),
		"fenced":    row("fenced", "same title", "local-label"),
		"deferred":  row("deferred", "d"),
	}
	cache.deps = map[string][]Dep{}
	cache.reconcileFingerprints = map[string]uint64{
		"kept":      reconcileFingerprint(row("kept", "k"), nil),
		"rewritten": reconcileFingerprint(row("rewritten", "as certified"), nil),
	}
	plan := reconcilePlan{
		digest: map[string]string{"kept": "k1", "rewritten": "r1", "absorbed": "a2", "fenced": "f2", "deferred": "d2", "lost": "l1"},
		keep:   map[string]struct{}{"kept": {}, "rewritten": {}},
	}
	fresh := map[string]Bead{
		"absorbed": row("absorbed", "new"),
		"fenced":   row("fenced", "same title", "remote-label"),
		"deferred": cache.beads["deferred"],
		"lost":     row("lost", "not installed"),
	}
	deferred := map[string]struct{}{"deferred": {}}

	got := cache.recordedDigestLocked(plan, fresh, deferred, map[string][]Dep{}, true, nil)
	if len(got) != 2 || got["kept"] != "k1" || got["absorbed"] != "a2" {
		t.Fatalf("recorded digest = %v, want only kept and absorbed", got)
	}
	if fp := cache.reconcileFingerprints; len(fp) != 2 || fp["kept"] == 0 || fp["absorbed"] == 0 {
		t.Fatalf("recorded fingerprints = %v, want kept and absorbed", fp)
	}
	if got := cache.recordedDigestLocked(plan, fresh, deferred, map[string][]Dep{}, true, errors.New("projection failed")); got != nil {
		t.Fatalf("recorded digest after a failed projection = %v, want none", got)
	}
}

// TestReconcileDoesNotCertifyEdgesThePassNeverRead: when the dependency read
// failed and the listed row carries no edges, the cache keeps its own edges.
// Recording that row would certify them against a digest that may already
// describe different ones.
func TestReconcileDoesNotCertifyEdgesThePassNeverRead(t *testing.T) {
	cache := NewCachingStoreForTest(NewMemStore(), nil)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	row := Bead{ID: "absorbed", Title: "new", Status: "open"}
	cache.beads = map[string]Bead{"absorbed": row}
	cache.deps = map[string][]Dep{"absorbed": {{IssueID: "absorbed", DependsOnID: "stale-blocker", Type: "blocks"}}}
	plan := reconcilePlan{digest: map[string]string{"absorbed": "a2"}}
	fresh := map[string]Bead{"absorbed": row}

	if got := cache.recordedDigestLocked(plan, fresh, nil, nil, false, nil); len(got) != 0 {
		t.Fatalf("recorded digest = %v, want none: the pass read no edges", got)
	}
	if fp := cache.reconcileFingerprints; len(fp) != 0 {
		t.Fatalf("recorded fingerprints = %v, want none", fp)
	}
	read := map[string][]Dep{"absorbed": cloneDeps(cache.deps["absorbed"])}
	if got := cache.recordedDigestLocked(plan, fresh, nil, read, true, nil); got["absorbed"] != "a2" {
		t.Fatalf("control: recorded digest = %v, want absorbed once its edges were read", got)
	}
}
