package beads

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// A ListQuery naming ids pushes them as IDFilter, one request per
// nativeServerEdgeAnchorCap ids. Without it the delta reconcile's narrowed read
// listed the whole ledger and filtered in Go.
func TestNativeListPlanPushesIDsInServerSizedChunks(t *testing.T) {
	ids := make([]string, 250)
	for i := range ids {
		ids[i] = fmt.Sprintf("gc-%d", i)
	}
	plan := NativeListPlan(ListQuery{IDs: ids})
	if len(plan) != 3 {
		t.Fatalf("plan has %d requests, want 3 chunks of at most %d ids", len(plan), nativeServerEdgeAnchorCap)
	}
	var got []string
	for i, req := range plan {
		if !NativeListRequestKeyed(req) {
			t.Errorf("request %d is unkeyed; an id filter narrows the candidate set", i)
		}
		chunk := strings.Split(req.IDFilter, ",")
		if len(chunk) > nativeServerEdgeAnchorCap {
			t.Errorf("request %d carries %d ids, over the cap", i, len(chunk))
		}
		got = append(got, chunk...)
	}
	if !slices.Equal(got, ids) {
		t.Fatalf("chunks carry %d ids, want all %d in order", len(got), len(ids))
	}
}

// A limit is never pushed beside ids: a backend that refuses IDFilter is read
// unfiltered, and a pushed limit would return the ledger's first rows, not the
// asked-for ones.
func TestNativeListPlanPushesNoLimitBesideIDs(t *testing.T) {
	plan := NativeListPlan(ListQuery{IDs: []string{"gc-z"}, IncludeClosed: true, TierMode: TierBoth, Sort: SortCreatedAsc, Limit: 1})
	for i, req := range plan {
		if req.Limit == nil || *req.Limit != 0 {
			t.Fatalf("request %d limit = %v, want explicit 0 (unlimited)", i, req.Limit)
		}
	}
	control := NativeListPlan(ListQuery{IncludeClosed: true, TierMode: TierBoth, Sort: SortCreatedAsc, Limit: 1})
	if control[0].Limit == nil || *control[0].Limit != 1 {
		t.Fatalf("control: an id-less exact query still pushes its limit, got %v", control[0].Limit)
	}
}

func idListRows() []*beadslib.Issue {
	var rows []*beadslib.Issue
	for _, id := range []string{"gc-a", "gc-b", "gc-c"} {
		rows = append(rows, &beadslib.Issue{ID: id, Title: id, Status: beadslib.StatusOpen, IssueType: "task"})
	}
	return rows
}

func pageOf(rows []*beadslib.Issue, keep func(string) bool) issueops.IssuePage {
	var page issueops.IssuePage
	for _, issue := range rows {
		if keep(issue.ID) {
			page.Items = append(page.Items, &issueops.IssueWithCounts{Issue: cloneNativeIssueForTest(issue)})
		}
	}
	return page
}

func TestNativeDoltStoreListPushesTheIDs(t *testing.T) {
	var sent []string
	storage := &nativeDoltReaderSpy{
		list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			sent = append(sent, req.IDFilter)
			want := strings.Split(req.IDFilter, ",")
			return pageOf(idListRows(), func(id string) bool { return req.IDFilter == "" || slices.Contains(want, id) }), nil
		},
	}
	got, err := newNativeDoltStoreForTest(storage).List(ListQuery{IDs: []string{"gc-a", "gc-c"}})
	if err != nil {
		t.Fatal(err)
	}
	if ids := idListResultIDs(got); !slices.Equal(ids, []string{"gc-a", "gc-c"}) {
		t.Fatalf("List = %v, want gc-a and gc-c", ids)
	}
	if !slices.Equal(sent, []string{"gc-a,gc-c"}) {
		t.Fatalf("sent id filters %q, want one request carrying both ids", sent)
	}
}

// An http-native reader refuses IDFilter client-side with *ErrUnsupported:
// List reads the unfiltered set ONCE (not once per chunk) and still answers
// exactly the ids asked for.
func TestNativeDoltStoreListFallsBackOnceWhenIDsAreRefused(t *testing.T) {
	var sent []string
	storage := &nativeDoltReaderSpy{
		list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			sent = append(sent, req.IDFilter)
			if req.IDFilter != "" {
				return issueops.IssuePage{}, &beadslib.ErrUnsupported{Op: "List", Backend: "http"}
			}
			return pageOf(idListRows(), func(string) bool { return true }), nil
		},
	}
	ids := []string{"gc-a", "gc-c"}
	for i := 0; i < 2*nativeServerEdgeAnchorCap; i++ {
		ids = append(ids, fmt.Sprintf("gc-missing-%d", i))
	}
	got, err := newNativeDoltStoreForTest(storage).List(ListQuery{IDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	if got := idListResultIDs(got); !slices.Equal(got, []string{"gc-a", "gc-c"}) {
		t.Fatalf("List = %v, want exactly gc-a and gc-c", got)
	}
	if len(sent) != 2 || sent[0] == "" || sent[1] != "" {
		t.Fatalf("sent %d requests (filters %q), want one refused filter then one unfiltered read", len(sent), sent)
	}
}

func idListResultIDs(beads []Bead) []string {
	out := make([]string, 0, len(beads))
	for _, b := range beads {
		out = append(out, b.ID)
	}
	slices.Sort(out)
	return out
}
