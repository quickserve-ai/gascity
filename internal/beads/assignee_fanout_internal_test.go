package beads

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

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
