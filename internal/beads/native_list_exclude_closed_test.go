package beads

import (
	"context"
	"slices"
	"sort"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// ga-tyf38i: a request excludes bd's closed at the server exactly when the
// query drops closed rows anyway, and the cache's full scan is one of them.
func TestNativeListPlanExcludesClosedExactlyWhenTheQueryDropsIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query ListQuery
		want  bool
	}{
		{"cache full scan", cacheFullScanQuery(), true},
		{"no status, closed not included", ListQuery{AllowScan: true}, true},
		{"no status, closed included", ListQuery{AllowScan: true, IncludeClosed: true}, false},
		{"folded open", ListQuery{Status: "open", AllowScan: true}, true},
		{"folded open, closed included", ListQuery{Status: "open", IncludeClosed: true, AllowScan: true}, true},
		{"closed", ListQuery{Status: "closed", AllowScan: true}, false},
		// These push a status of their own, which already leaves closed out.
		{"in_progress", ListQuery{Status: "in_progress", AllowScan: true}, false},
		{"live raw open", ListQuery{Status: "open", Live: true, AllowScan: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, req := range NativeListPlan(tc.query) {
				got := slices.Equal(req.ExcludeStatus, []string{"closed"})
				if got != tc.want {
					t.Errorf("request %d: ExcludeStatus=%v, want closed excluded=%v", i, req.ExcludeStatus, tc.want)
				}
				if !tc.want && len(req.ExcludeStatus) > 0 {
					t.Errorf("request %d: unexpected ExcludeStatus %v", i, req.ExcludeStatus)
				}
			}
		})
	}
}

// The exclusion is exact: no bd status but closed folds to gc's closed.
func TestNativeListExcludesClosedOnlyBdClosedMapsToGcClosed(t *testing.T) {
	for _, s := range []string{"open", "in_progress", "blocked", "deferred", "hooked", "pinned", "a-custom-status", ""} {
		if mapBdStatus(s) == "closed" {
			t.Errorf("mapBdStatus(%q) = closed; excluding only bd closed would hide it", s)
		}
	}
	if mapBdStatus("closed") != "closed" {
		t.Fatal("mapBdStatus(closed) is not closed")
	}
}

// A cache primed over a native store holds exactly what it held before the
// exclusion was pushed: every non-closed row in both planes (a parked status,
// a status no vocabulary names, a pinned row, a wisp), and no closed row.
func TestCachingStoreOverNativeFullScanExcludesClosedAndKeepsTheRest(t *testing.T) {
	rows := []*beadslib.Issue{
		{ID: "gc-open", Title: "open", Status: beadslib.StatusOpen, IssueType: "task"},
		{ID: "gc-busy", Title: "busy", Status: beadslib.StatusInProgress, IssueType: "task"},
		{ID: "gc-parked", Title: "parked", Status: "deferred", IssueType: "task"},
		{ID: "gc-odd", Title: "odd", Status: "a-custom-status", IssueType: "task"},
		{ID: "gc-pinned", Title: "pinned", Status: beadslib.StatusOpen, IssueType: "task", Pinned: true},
		{ID: "gc-wisp", Title: "wisp", Status: beadslib.StatusOpen, IssueType: "molecule", Ephemeral: true},
		{ID: "gc-done", Title: "done", Status: beadslib.StatusClosed, IssueType: "task"},
		{ID: "gc-done-wisp", Title: "done wisp", Status: beadslib.StatusClosed, IssueType: "molecule", Ephemeral: true},
	}
	reader := filteringReader{rows: rows}
	var fullScans []issueops.ListRequest
	storage := &nativeDoltReaderSpy{list: func(ctx context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
		fullScans = append(fullScans, req)
		return reader.list(ctx, req)
	}}
	cache := NewCachingStoreForTest(newNativeDoltStoreForTest(storage), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if len(fullScans) == 0 {
		t.Fatal("Prime sent no listing")
	}
	for i, req := range fullScans {
		if !slices.Equal(req.ExcludeStatus, []string{"closed"}) {
			t.Errorf("full-scan request %d: ExcludeStatus=%v, want [closed]", i, req.ExcludeStatus)
		}
	}
	got, err := cache.List(ListQuery{AllowScan: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := beadIDsInOrder(got)
	sort.Strings(ids)
	want := []string{"gc-busy", "gc-odd", "gc-open", "gc-parked", "gc-pinned", "gc-wisp"}
	if !slices.Equal(ids, want) {
		t.Fatalf("cache after Prime holds %v, want %v", ids, want)
	}
}
