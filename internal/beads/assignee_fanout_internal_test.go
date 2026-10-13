package beads

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// IDs is matched only Go-side (the backing filter has no id list), so a native
// read carrying IDs must not let the backing cut its limit first: the backing
// would return the route's top row, the IDs filter would discard it, and the
// wanted row would never be read. A plural read fans out to single-route reads
// that keep IDs, so the guard holds for them too.
func TestNativeDoltStoreListIDsReadsUnlimitedBeforeIDsFilter(t *testing.T) {
	base := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	all := []*beadslib.Issue{
		{ID: "other", Title: "m", Status: beadslib.StatusOpen, IssueType: "message", Assignee: "a", Priority: 0, CreatedAt: base},
		{ID: "wanted", Title: "m", Status: beadslib.StatusOpen, IssueType: "message", Assignee: "a", Priority: 2, CreatedAt: base},
	}
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			var out []*beadslib.Issue
			for _, issue := range all {
				if filter.Assignee == nil || issue.Assignee == *filter.Assignee {
					out = append(out, issue)
				}
			}
			sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
			if filter.Limit > 0 && len(out) > filter.Limit {
				out = out[:filter.Limit]
			}
			return out, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	for _, query := range []ListQuery{
		{Assignees: []string{"a", "b"}, IDs: []string{"wanted"}, Limit: 1},
		{Assignee: "a", IDs: []string{"wanted"}, Limit: 1},
	} {
		got, err := store.List(query)
		if err != nil {
			t.Fatalf("List(%+v): %v", query, err)
		}
		if want := []string{"wanted"}; !slices.Equal(fanoutTestIDs(got), want) {
			t.Fatalf("List(%+v) ids = %v, want %v", query, fanoutTestIDs(got), want)
		}
	}
}

// The native store's backing filter carries one assignee, so a multi-route
// read must reach SearchIssues once per route with that route set — never as
// one unfiltered search hydrating every open message (pl-adj).
func TestNativeDoltStoreListAssigneesSearchesEachRoutePredicated(t *testing.T) {
	base := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	p0, p2 := 0, 2
	all := []*beadslib.Issue{
		{ID: "a-old", Title: "m", Status: beadslib.StatusOpen, IssueType: "message", Assignee: "rig/route-a", Priority: p2, CreatedAt: base},
		{ID: "b-new", Title: "m", Status: beadslib.StatusOpen, IssueType: "message", Assignee: "route-b", Priority: p2, CreatedAt: base.Add(2 * time.Hour)},
		{ID: "b-urgent", Title: "m", Status: beadslib.StatusOpen, IssueType: "message", Assignee: "route-b", Priority: p0, CreatedAt: base},
		{ID: "a-mid", Title: "m", Status: beadslib.StatusOpen, IssueType: "message", Assignee: "rig/route-a", Priority: p2, CreatedAt: base.Add(time.Hour)},
		{ID: "other", Title: "m", Status: beadslib.StatusOpen, IssueType: "message", Assignee: "someone-else", Priority: p2, CreatedAt: base.Add(3 * time.Hour)},
	}
	var (
		mu        sync.Mutex
		searched  []string
		unfilterd int
	)
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			mu.Lock()
			defer mu.Unlock()
			if filter.Assignee == nil {
				unfilterd++
				return all, nil
			}
			searched = append(searched, *filter.Assignee)
			var out []*beadslib.Issue
			for _, issue := range all {
				if issue.Assignee == *filter.Assignee {
					out = append(out, issue)
				}
			}
			// The backing's default order: priority ASC, created_at DESC, id ASC.
			sort.SliceStable(out, func(i, j int) bool {
				if out[i].Priority != out[j].Priority {
					return out[i].Priority < out[j].Priority
				}
				if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
					return out[i].CreatedAt.After(out[j].CreatedAt)
				}
				return out[i].ID < out[j].ID
			})
			return out, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.List(ListQuery{
		Type:      "message",
		Status:    "open",
		TierMode:  TierBoth,
		Assignees: []string{"rig/route-a", "route-b", "route-a-unused"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if unfilterd != 0 {
		t.Fatalf("SearchIssues ran %d unfiltered searches, want 0", unfilterd)
	}
	slices.Sort(searched)
	if want := []string{"rig/route-a", "route-a-unused", "route-b"}; !slices.Equal(searched, want) {
		t.Fatalf("searched assignees = %v, want one search per route %v", searched, want)
	}
	if want := []string{"b-urgent", "b-new", "a-mid", "a-old"}; !slices.Equal(fanoutTestIDs(got), want) {
		t.Fatalf("List ids = %v, want backing default order %v", fanoutTestIDs(got), want)
	}
}

// assigneeFanOutRoutes decides which queries fan out: only a plural,
// route-bearing query does, and its routes come back trimmed and distinct.
func TestAssigneeFanOutRoutes(t *testing.T) {
	cases := []struct {
		name  string
		query ListQuery
		want  []string
	}{
		{"no assignees", ListQuery{AllowScan: true}, nil},
		{"singular assignee", ListQuery{Assignee: "a"}, nil},
		{"one route", ListQuery{Assignees: []string{"a"}}, nil},
		{"two routes", ListQuery{Assignees: []string{"a", "b"}}, []string{"a", "b"}},
		{"duplicates and blanks compact", ListQuery{Assignees: []string{" a ", "a", "", "b"}}, []string{"a", "b"}},
		{"compacts to one", ListQuery{Assignees: []string{"a", " a"}}, []string{"a"}},
		{"compacts to none", ListQuery{Assignees: []string{"", " "}}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := assigneeFanOutRoutes(tc.query)
			if (got == nil) != (tc.want == nil) || !slices.Equal(got, tc.want) {
				t.Fatalf("assigneeFanOutRoutes = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// A plural query whose routes are all blank matches nothing and reads nothing.
func TestListPerAssigneeWithNoRoutesReadsNothing(t *testing.T) {
	calls := 0
	got, err := listPerAssignee("test", ListQuery{Assignees: []string{"", " "}}, []string{}, func(ListQuery) ([]Bead, error) {
		calls++
		return nil, nil
	})
	if err != nil || len(got) != 0 || calls != 0 {
		t.Fatalf("listPerAssignee = %v, %v after %d reads; want empty, nil, 0 reads", got, err, calls)
	}
}

// Each per-route read carries the route as its singular predicate and keeps
// every other filter of the original query.
func TestListPerAssigneeNarrowsEachReadToOneRoute(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []ListQuery
	)
	query := ListQuery{Type: "message", Status: "open", Label: "x", Assignees: []string{"a", "b"}, Limit: 7}
	_, err := listPerAssignee("test", query, assigneeFanOutRoutes(query), func(q ListQuery) ([]Bead, error) {
		mu.Lock()
		seen = append(seen, q)
		mu.Unlock()
		return nil, nil
	})
	if err != nil {
		t.Fatalf("listPerAssignee: %v", err)
	}
	sort.Slice(seen, func(i, j int) bool { return seen[i].Assignee < seen[j].Assignee })
	for i, route := range []string{"a", "b"} {
		q := seen[i]
		if q.Assignee != route || len(q.Assignees) != 0 {
			t.Fatalf("read %d = Assignee %q Assignees %v, want only Assignee %q", i, q.Assignee, q.Assignees, route)
		}
		if q.Type != "message" || q.Status != "open" || q.Label != "x" || q.Limit != 7 {
			t.Fatalf("read %d dropped a filter: %+v", i, q)
		}
		if strings.TrimSpace(q.Assignee) != q.Assignee {
			t.Fatalf("read %d route not trimmed: %q", i, q.Assignee)
		}
	}
}

func fanoutTestIDs(rows []Bead) []string {
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	return ids
}

// Per-route reads run in the caller's goroutine: the bd trace names a read's
// caller by walking the calling goroutine's stack, so a read on a fresh
// goroutine would record scope "unknown" for every inbox read.
func TestListPerAssigneeReadsRunOnCallersStack(t *testing.T) {
	routes := []string{"route-a", "route-b", "route-c"}
	reads := 0
	_, err := listPerAssignee("test", ListQuery{Assignees: routes}, routes, func(ListQuery) ([]Bead, error) {
		reads++
		callers := captureBDTraceCallers()
		if !slices.ContainsFunc(callers, func(fn string) bool {
			return strings.HasSuffix(fn, ".TestListPerAssigneeReadsRunOnCallersStack")
		}) {
			t.Errorf("read's stack %v does not reach the caller; trace scope would be lost", callers)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("listPerAssignee: %v", err)
	}
	if reads != len(routes) {
		t.Fatalf("reads = %d, want %d", reads, len(routes))
	}
}

// A backing cuts its limit in its own order (priority first), so under a
// created-desc sort a route's limited read can drop the route's newest row.
// The per-route reads must then come back whole and the merge must cut:
// route-a holds an urgent old row and a normal new one; the newest row across
// both routes is route-a's new one.
func TestListPerAssigneeNonDefaultSortCutsAfterMerge(t *testing.T) {
	base := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	p0, p2 := 0, 2
	rows := map[string][]Bead{
		"route-a": {
			{ID: "a-old", Assignee: "route-a", Priority: &p0, CreatedAt: base},
			{ID: "a-new", Assignee: "route-a", Priority: &p2, CreatedAt: base.Add(2 * time.Hour)},
		},
		"route-b": {
			{ID: "b-mid", Assignee: "route-b", Priority: &p2, CreatedAt: base.Add(time.Hour)},
		},
	}
	// backingRead mimics bd: default order, then --limit.
	backingRead := func(q ListQuery) []Bead {
		out := slices.Clone(rows[q.Assignee])
		sort.SliceStable(out, func(i, j int) bool { return backingDefaultLess(out[i], out[j]) })
		if q.Limit > 0 && len(out) > q.Limit {
			out = out[:q.Limit]
		}
		return out
	}
	routes := []string{"route-a", "route-b"}

	got, err := listPerAssignee("test", ListQuery{Assignees: routes, Sort: SortCreatedDesc, Limit: 1}, routes, func(q ListQuery) ([]Bead, error) {
		return backingRead(q), nil
	})
	if err != nil {
		t.Fatalf("listPerAssignee created-desc: %v", err)
	}
	if ids := fanoutTestIDs(got); !slices.Equal(ids, []string{"a-new"}) {
		t.Fatalf("created-desc limit 1 = %v, want [a-new]", ids)
	}

	// Under SortDefault the backing's cut is the merged order's cut, so the
	// per-route limit stays on.
	var limits []int
	got, err = listPerAssignee("test", ListQuery{Assignees: routes, Limit: 1}, routes, func(q ListQuery) ([]Bead, error) {
		limits = append(limits, q.Limit)
		return backingRead(q), nil
	})
	if err != nil {
		t.Fatalf("listPerAssignee default: %v", err)
	}
	if !slices.Equal(limits, []int{1, 1}) {
		t.Fatalf("default-sort per-route limits = %v, want [1 1]", limits)
	}
	if ids := fanoutTestIDs(got); !slices.Equal(ids, []string{"a-old"}) {
		t.Fatalf("default limit 1 = %v, want [a-old]", ids)
	}
}

// A route whose read fails is never dropped silently. With rows from the
// other routes the caller gets them under a *PartialResultError carrying its
// own op; with no rows at all it gets the joined failures under that op.
func TestListPerAssigneeFailedRouteIsNeverDropped(t *testing.T) {
	errBoom := errors.New("boom")
	errBust := errors.New("bust")
	routes := []string{"route-a", "route-b", "route-c"}

	got, err := listPerAssignee("bd list", ListQuery{Assignees: routes}, routes, func(q ListQuery) ([]Bead, error) {
		if q.Assignee == "route-b" {
			return nil, errBoom
		}
		return []Bead{{ID: q.Assignee + "-1", Assignee: q.Assignee}}, nil
	})
	var partial *PartialResultError
	if !errors.As(err, &partial) {
		t.Fatalf("one failed route: err = %v, want *PartialResultError", err)
	}
	if partial.Op != "bd list" {
		t.Fatalf("PartialResultError.Op = %q, want %q", partial.Op, "bd list")
	}
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), `assignee "route-b"`) {
		t.Fatalf("partial err = %v, want it to wrap the route-b failure", err)
	}
	if ids := fanoutTestIDs(got); !slices.Equal(ids, []string{"route-a-1", "route-c-1"}) {
		t.Fatalf("partial rows = %v, want [route-a-1 route-c-1]", ids)
	}

	got, err = listPerAssignee("native list", ListQuery{Assignees: routes}, routes, func(q ListQuery) ([]Bead, error) {
		if q.Assignee == "route-a" {
			return nil, errBust
		}
		return nil, errBoom
	})
	if got != nil {
		t.Fatalf("all routes failed: rows = %v, want nil", fanoutTestIDs(got))
	}
	if errors.As(err, &partial) {
		t.Fatalf("all routes failed: err = %v, want a plain error, not *PartialResultError", err)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "native list: ") || !errors.Is(err, errBoom) || !errors.Is(err, errBust) {
		t.Fatalf("all routes failed: err = %v, want both failures under op %q", err, "native list")
	}
}

// A bead two routes both return (the same row read twice, e.g. while its
// assignee moves between spellings) comes back once, in its merged place, and
// counts once against the limit.
func TestListPerAssigneeDedupesIDsAcrossRoutes(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 9, 25, h, 0, 0, 0, time.UTC) }
	p1, p2 := 1, 2
	shared := Bead{ID: "shared", Priority: &p1, CreatedAt: at(9)}
	byRoute := map[string][]Bead{
		"a": {shared, {ID: "a-only", Priority: &p2, CreatedAt: at(8)}},
		"b": {shared, {ID: "b-only", Priority: &p2, CreatedAt: at(10)}},
	}
	query := ListQuery{Assignees: []string{"a", "b"}, Limit: 2}
	got, err := listPerAssignee("test", query, assigneeFanOutRoutes(query), func(q ListQuery) ([]Bead, error) {
		return byRoute[q.Assignee], nil
	})
	if err != nil {
		t.Fatalf("listPerAssignee: %v", err)
	}
	if want := []string{"shared", "b-only"}; !slices.Equal(fanoutTestIDs(got), want) {
		t.Fatalf("listPerAssignee ids = %v, want %v", fanoutTestIDs(got), want)
	}
}

// SQLiteStore sends plural assignees to SQL as assignee IN (...), so a
// multi-route read selects only those routes' rows and can take its LIMIT in
// SQL, instead of decoding every row the other filters select and matching
// the routes in Go.
func TestSQLiteStoreListPluralAssigneesPredicatedInSQL(t *testing.T) {
	sqlText, args := sqliteListSQL(ListQuery{Assignees: []string{"rig/a", "b"}, Type: "message", Limit: 2}, "b.id")
	if !strings.Contains(sqlText, "b.assignee IN (?,?)") || !strings.Contains(sqlText, "LIMIT 2") {
		t.Fatalf("sql = %q, want an assignee IN predicate and the limit in SQL", sqlText)
	}
	if !slices.Contains(args, any("rig/a")) || !slices.Contains(args, any("b")) {
		t.Fatalf("args = %v, want both routes bound", args)
	}

	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	want := map[string]bool{}
	for _, assignee := range []string{"rig/a", "rig/a", "b", "c", ""} {
		b, err := store.Create(Bead{Title: "m", Type: "message", Assignee: assignee})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if assignee == "rig/a" || assignee == "b" {
			want[b.ID] = true
		}
	}
	got, err := store.List(ListQuery{Assignees: []string{"rig/a", "b"}, Type: "message"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("List = %v, want the %d rows of rig/a and b", fanoutTestIDs(got), len(want))
	}
	for _, b := range got {
		if !want[b.ID] {
			t.Fatalf("List returned %s (assignee %q), not a listed route's row", b.ID, b.Assignee)
		}
	}
	limited, err := store.List(ListQuery{Assignees: []string{"rig/a", "b"}, Type: "message", Sort: SortCreatedAsc, Limit: 2})
	if err != nil {
		t.Fatalf("List limited: %v", err)
	}
	if len(limited) != 2 || !want[limited[0].ID] || !want[limited[1].ID] {
		t.Fatalf("List limited = %v, want two rows of the listed routes", fanoutTestIDs(limited))
	}
}

// A plural-assignee count is answered one predicated CountIssues per route and
// summed: a bead has one assignee and the routes are distinct, so the
// per-route counts partition the total. Reporting it unsupported sent callers
// to a hydrating List instead.
func TestNativeDoltStoreCountPluralAssigneesSumsPerRouteCounts(t *testing.T) {
	counts := map[string]int64{"rig/a": 3, "b": 4}
	var seen []string
	storage := &nativeDoltStorageSpy{
		countIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) (int64, error) {
			if filter.Assignee == nil {
				t.Fatal("CountIssues ran with no assignee predicate")
			}
			seen = append(seen, *filter.Assignee)
			return counts[*filter.Assignee], nil
		},
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			t.Fatal("Count hydrated rows through SearchIssues")
			return nil, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Count(context.Background(), ListQuery{Assignees: []string{"rig/a", "b", " rig/a"}, Type: "message"})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 7 {
		t.Fatalf("Count = %d, want 7 (3 + 4)", got)
	}
	if want := []string{"rig/a", "b"}; !slices.Equal(seen, want) {
		t.Fatalf("CountIssues routes = %v, want %v (one count per distinct route)", seen, want)
	}
}
