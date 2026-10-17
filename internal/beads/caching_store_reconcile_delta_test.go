package beads

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
		deps, err := s.MemStore.DepList(b.ID, "down")
		if err != nil {
			return nil, err
		}
		digest[b.ID] = fmt.Sprintf("%s|%s|%s|%v|%v", b.Status, b.Title, b.Assignee, b.Labels, deps)
	}
	return digest, nil
}

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

// TestReconcileDigestSQLCarriesEveryJoinedTerm pins the digest's surface
// against the native List's projection: each table the List joins onto a row
// must have a term, or a write there goes unseen until the next full list.
// The terms' behaviour against a server-mode Dolt is the integration
// follow-up; the embedded store exports no SQL pool and answers unsupported.
func TestReconcileDigestSQLCarriesEveryJoinedTerm(t *testing.T) {
	for _, tier := range reconcileDigestTiers {
		sql := reconcileDigestSQL(tier)
		for _, want := range []string{
			"FROM " + tier.rows + " r",
			"r.is_blocked",
			"r.updated_at",
			"r.content_hash",
			"r.row_lock",
			"FROM " + tier.labels + " GROUP BY issue_id",
			"FROM " + tier.deps + " e",
			"ti.content_hash",
			"FROM dependencies",
			"FROM wisp_dependencies",
			"FROM leases",
			"FROM " + tier.comments + " GROUP BY issue_id",
			"WHERE r.status <> 'closed'",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("%s digest SQL lacks %q", tier.rows, want)
			}
		}
	}
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

	got := cache.recordedDigestLocked(plan, fresh, deferred, nil, false, nil)
	if len(got) != 2 || got["kept"] != "k1" || got["absorbed"] != "a2" {
		t.Fatalf("recorded digest = %v, want only kept and absorbed", got)
	}
	if fp := cache.reconcileFingerprints; len(fp) != 2 || fp["kept"] == 0 || fp["absorbed"] == 0 {
		t.Fatalf("recorded fingerprints = %v, want kept and absorbed", fp)
	}
	if got := cache.recordedDigestLocked(plan, fresh, deferred, nil, false, errors.New("projection failed")); got != nil {
		t.Fatalf("recorded digest after a failed projection = %v, want none", got)
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
