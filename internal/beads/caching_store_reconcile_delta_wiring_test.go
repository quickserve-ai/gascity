package beads

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func (s *digestingStore) lists() (int, [][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fullLists, slices.Clone(s.idLists)
}

func (s *digestingStore) resetLists() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fullLists, s.idLists, s.gets = 0, nil, 0
}

// deltaCache primes a cache over three rows and runs the baseline full pass
// the delta needs before it can compare.
func deltaCache(t *testing.T, now *time.Time) (*CachingStore, *digestingStore, []Bead) {
	t.Helper()
	backing := newDigestingStore()
	var rows []Bead
	for _, title := range []string{"a", "b", "c"} {
		b, err := backing.Create(Bead{Title: title})
		if err != nil {
			t.Fatalf("Create %s: %v", title, err)
		}
		rows = append(rows, b)
	}
	cache := NewCachingStoreForTest(backing, nil, WithClock(func() time.Time { return *now }))
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.runReconciliation()
	if full, _ := backing.lists(); full == 0 {
		t.Fatalf("the baseline pass made no full list")
	}
	backing.resetLists()
	return cache, backing, rows
}

func TestDeltaReconcileListsOnlyTheRowsWhoseDigestMoved(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	cache, backing, rows := deltaCache(t, &now)

	newTitle := "a, renamed"
	if err := backing.Update(rows[0].ID, UpdateOpts{Title: &newTitle}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	now = now.Add(2 * time.Minute)
	cache.runReconciliation()

	full, ids := backing.lists()
	if full != 0 || len(ids) != 1 || !slices.Equal(ids[0], []string{rows[0].ID}) {
		t.Fatalf("lists = %d full, %v by id; want 0 full and one list of [%s]", full, ids, rows[0].ID)
	}
	backing.mu.Lock()
	gets := backing.gets
	backing.mu.Unlock()
	if gets != 0 {
		t.Fatalf("the pass made %d per-row Gets; a kept row must not be re-verified", gets)
	}
	got, err := cache.Get(rows[0].ID)
	if err != nil || got.Title != newTitle {
		t.Fatalf("Get(%s) = %q, %v; want the renamed row", rows[0].ID, got.Title, err)
	}
	for _, b := range rows[1:] {
		if got, err := cache.Get(b.ID); err != nil || got.Title != b.Title {
			t.Fatalf("Get(%s) = %q, %v; a kept row must still be served", b.ID, got.Title, err)
		}
	}

	// The row the pass installed is certified: the next pass lists nothing.
	backing.resetLists()
	now = now.Add(2 * time.Minute)
	cache.runReconciliation()
	if full, ids := backing.lists(); full != 0 || len(ids) != 0 {
		t.Fatalf("the pass after a delta listed %d full, %v by id; want nothing", full, ids)
	}
}

func TestDeltaReconcileSeesLabelAndDependencyOnlyChanges(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	cache, backing, rows := deltaCache(t, &now)

	if err := backing.Update(rows[1].ID, UpdateOpts{Labels: []string{"new"}}); err != nil {
		t.Fatalf("label Update: %v", err)
	}
	if err := backing.DepAdd(rows[2].ID, rows[0].ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	now = now.Add(2 * time.Minute)
	cache.runReconciliation()

	_, ids := backing.lists()
	if len(ids) != 1 || !slices.Equal(ids[0], []string{rows[1].ID, rows[2].ID}) {
		t.Fatalf("by-id lists = %v, want the label row and the edge row", ids)
	}
	if got, _ := cache.Get(rows[1].ID); !slices.Equal(got.Labels, []string{"new"}) {
		t.Fatalf("labels after delta = %v, want [new]", got.Labels)
	}
	deps, err := cache.DepList(rows[2].ID, "down")
	if err != nil || len(deps) != 1 || deps[0].DependsOnID != rows[0].ID {
		t.Fatalf("DepList after delta = %+v, %v; want the new edge", deps, err)
	}
}

func TestDeltaReconcileEvictsARowThatLeftTheActiveSet(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	cache, backing, rows := deltaCache(t, &now)

	if err := backing.Close(rows[2].ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	now = now.Add(2 * time.Minute)
	cache.runReconciliation()

	active, err := cache.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, b := range active {
		if b.ID == rows[2].ID {
			t.Fatalf("closed row %s is still listed active", b.ID)
		}
	}
	if len(active) != 2 {
		t.Fatalf("active rows = %d, want the 2 kept rows", len(active))
	}
}

func TestDeltaReconcileFallsBackToTheFullList(t *testing.T) {
	cases := map[string]func(*digestingStore, *time.Time){
		"the digest read fails": func(s *digestingStore, now *time.Time) {
			s.digestErr = errors.New("hub timed out")
			*now = now.Add(2 * time.Minute)
		},
		"the last full list is too old": func(_ *digestingStore, now *time.Time) {
			*now = now.Add(cacheReconcileFullListEvery)
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
			cache, backing, _ := deltaCache(t, &now)
			arrange(backing, &now)
			cache.runReconciliation()
			if full, ids := backing.lists(); full != 1 || len(ids) != 0 {
				t.Fatalf("lists = %d full, %v by id; want exactly one full list", full, ids)
			}
		})
	}
}

func TestDeltaReconcileNeedsABackingDigest(t *testing.T) {
	backing := &countingListStore{MemStore: NewMemStore()}
	if _, err := backing.Create(Bead{Title: "a"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.full = 0
	cache.runReconciliation()
	cache.runReconciliation()
	if backing.full != 2 {
		t.Fatalf("full lists = %d, want one per pass without a digest", backing.full)
	}
}

type countingListStore struct {
	*MemStore
	full int
}

func (s *countingListStore) List(q ListQuery) ([]Bead, error) {
	if len(q.IDs) == 0 && strings.TrimSpace(q.Status) == "" {
		s.full++
	}
	return s.MemStore.List(q)
}

// A row the cache changed on its own (here a stale event payload) has the same
// backing digest as last pass; the next delta pass must still re-list it.
func TestDeltaReconcileRelistsARowTheCacheChangedSinceTheLastPass(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	cache, backing, rows := deltaCache(t, &now)

	cache.mu.Lock()
	stale := cache.beads[rows[1].ID]
	stale.Title = "stale event payload"
	cache.beads[rows[1].ID] = stale
	cache.noteMutationLocked(rows[1].ID)
	cache.mu.Unlock()

	now = now.Add(2 * time.Minute)
	cache.runReconciliation()

	_, ids := backing.lists()
	if len(ids) != 1 || !slices.Equal(ids[0], []string{rows[1].ID}) {
		t.Fatalf("by-id lists = %v, want [%s] re-listed", ids, rows[1].ID)
	}
	if got, _ := cache.Get(rows[1].ID); got.Title != rows[1].Title {
		t.Fatalf("title after delta = %q, want the backing's %q", got.Title, rows[1].Title)
	}
}

// A row an event evicted while its backing digest stayed put must come back
// on the next delta pass, not on the next full list.
func TestDeltaReconcileRefetchesARowEvictedWithAnUnchangedDigest(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	cache, backing, rows := deltaCache(t, &now)

	cache.mu.Lock()
	delete(cache.beads, rows[0].ID)
	cache.mu.Unlock()

	now = now.Add(2 * time.Minute)
	cache.runReconciliation()

	if _, ids := backing.lists(); len(ids) != 1 || !slices.Equal(ids[0], []string{rows[0].ID}) {
		t.Fatalf("by-id lists = %v, want the evicted row re-listed", ids)
	}
	cache.mu.RLock()
	_, back := cache.beads[rows[0].ID]
	cache.mu.RUnlock()
	if !back {
		t.Fatalf("row %s is still missing from the cache after the delta pass", rows[0].ID)
	}
}

// A delta pass that listed no rows cannot restore an incomplete edge set.
func TestDeltaReconcileNeverRestoresDependencyCompleteness(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	cache, _, _ := deltaCache(t, &now)

	cache.mu.Lock()
	cache.depsComplete = false
	cache.mu.Unlock()

	now = now.Add(2 * time.Minute)
	cache.runReconciliation()

	cache.mu.RLock()
	complete := cache.depsComplete
	cache.mu.RUnlock()
	if complete {
		t.Fatalf("a delta pass set depsComplete; only a full list may")
	}
}

// A cache install that stamps nothing (here a late DepList overwriting the
// row's edges with an older, empty set) still changes the row's cached
// content, so the next delta pass re-lists it.
func TestDeltaReconcileRelistsAnUnstampedCacheInstall(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	backing := newDigestingStore()
	blocker, _ := backing.Create(Bead{Title: "blocker"})
	work, _ := backing.Create(Bead{Title: "work"})
	if err := backing.DepAdd(work.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil, WithClock(func() time.Time { return now }))
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.runReconciliation()
	backing.resetLists()

	cache.mu.Lock()
	cache.deps[work.ID] = nil
	cache.mu.Unlock()

	now = now.Add(2 * time.Minute)
	cache.runReconciliation()

	if _, ids := backing.lists(); len(ids) != 1 || !slices.Equal(ids[0], []string{work.ID}) {
		t.Fatalf("by-id lists = %v, want [%s] re-listed", ids, work.ID)
	}
	deps, err := cache.DepList(work.ID, "down")
	if err != nil || len(deps) != 1 {
		t.Fatalf("DepList after delta = %+v, %v; want the edge back", deps, err)
	}
}
