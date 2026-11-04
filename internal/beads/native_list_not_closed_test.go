package beads

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// statusSettingStorage answers status.custom with value (or err), and every
// other key as unset.
type statusSettingStorage struct {
	*nativeDoltReaderSpy
	value string
	err   error
}

func (s statusSettingStorage) WorkspaceConfig() (issueops.WorkspaceConfig, error) {
	return statusSettingConfig{infraSettingConfig: infraSettingConfig{}, value: s.value, err: s.err}, nil
}

type statusSettingConfig struct {
	infraSettingConfig
	value string
	err   error
}

func (c statusSettingConfig) GetSetting(_ context.Context, req issueops.GetSettingRequest) (issueops.SettingResult, error) {
	if c.err != nil {
		return issueops.SettingResult{}, c.err
	}
	if req.Key != issueops.SettingKeyStatusCustom {
		return issueops.SettingResult{Key: req.Key}, nil
	}
	return issueops.SettingResult{Key: req.Key, Value: c.value}, nil
}

const builtinNotClosed = "open,in_progress,blocked,deferred,pinned,hooked"

func TestNativeListNotClosedStatusFor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting string
		want    string
		ok      bool
	}{
		{"unset", "", builtinNotClosed, true},
		{"flat custom statuses", "staged_ready,launched", builtinNotClosed + ",staged_ready,launched", true},
		{"a category never closes a status", "shipped:done, parked:frozen", builtinNotClosed + ",shipped,parked", true},
		{"duplicates and built-ins once", "launched,open,launched", builtinNotClosed + ",launched", true},
		{"empty entries skipped", " ,launched, ", builtinNotClosed + ",launched", true},
		{"closed is never in the set", "closed", "", false},
		{"an unreadable name pushes nothing", "staged ready", "", false},
		{"a separator smuggled into a name pushes nothing", "a;b", "", false},
		{"an empty name pushes nothing", ":done", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nativeListNotClosedStatusFor(tc.setting)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("nativeListNotClosedStatusFor(%q) = %q, %v; want %q, %v", tc.setting, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// One request rule per branch: the not-closed set travels only for a query that
// opted in (PushNotClosed), names no Status, does not include closed rows and is
// not CompleteActive, and only when the store has a set.
func TestNativeListPlanPushesTheNotClosedSetOnlyWhenOptedIn(t *testing.T) {
	set := builtinNotClosed + ",staged_ready"
	in := func(q ListQuery) ListQuery { q.PushNotClosed = true; return q }
	for _, tc := range []struct {
		name      string
		query     ListQuery
		notClosed string
		want      string
	}{
		// the mayor's opt-in ruling (2026-11-04): a caller that does not ask reads unpushed
		{"no Status, not opted in", ListQuery{AllowScan: true, TierMode: TierBoth, Live: true}, set, ""},
		{"no Status, opted in", in(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true}), set, set},
		{"opted in, IncludeClosed", in(ListQuery{AllowScan: true, IncludeClosed: true}), set, ""},
		{"a folded open stays Go-side", in(ListQuery{Status: "open", AllowScan: true}), set, ""},
		{"a Live open stays raw", in(ListQuery{Status: "open", AllowScan: true, Live: true}), set, "open"},
		{"an exact status stays exact", in(ListQuery{Status: "in_progress"}), set, "in_progress"},
		{"no set, no push", in(ListQuery{AllowScan: true}), "", ""},
		// The cache evicts what its full scan does not return as closed, so a
		// status the set missed must never be narrowed out of that scan.
		{"the cache's full scan is never narrowed", cacheFullScanQuery(), set, ""},
		{"a CompleteActive read is never narrowed, even opted in", in(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true, CompleteActive: true}), set, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, req := range nativeListPlan(tc.query, nativeListDefaultPushableTypes, tc.notClosed) {
				if req.Status != tc.want {
					t.Errorf("request %d Status = %q, want %q", i, req.Status, tc.want)
				}
			}
		})
	}
}

// The exported plan carries no store, so it pushes no not-closed set.
func TestNativeListPlanExportedPushesNoNotClosedSet(t *testing.T) {
	for _, req := range NativeListPlan(ListQuery{AllowScan: true}) {
		if req.Status != "" {
			t.Fatalf("NativeListPlan Status = %q, want none", req.Status)
		}
	}
}

// The store reads its set once at open; an unreadable or unparseable setting
// leaves it pushing nothing.
func TestNativeDoltStoreLoadsItsNotClosedSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting string
		err     error
		want    string
	}{
		{"the qcore shape", "staged_ready,launched", nil, builtinNotClosed + ",staged_ready,launched"},
		{"unset", "", nil, builtinNotClosed},
		{"unreadable", "", errors.New("config table unavailable"), ""},
		{"unparseable", "staged ready", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent []string
			spy := &nativeDoltReaderSpy{
				list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
					sent = append(sent, req.Status)
					return issueops.IssuePage{}, nil
				},
			}
			store := newNativeDoltStoreWithStorage(statusSettingStorage{nativeDoltReaderSpy: spy, value: tc.setting, err: tc.err}, "native-test")
			store.loadListNotClosedStatus(context.Background())
			if store.listNotClosedStatus != tc.want {
				t.Fatalf("listNotClosedStatus = %q, want %q", store.listNotClosedStatus, tc.want)
			}
			if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true, PushNotClosed: true}); err != nil {
				t.Fatalf("List: %v", err)
			}
			if !slices.Equal(sent, []string{tc.want}) {
				t.Fatalf("List sent Status %q, want %q", sent, tc.want)
			}
		})
	}
}

// vocabularyReader answers a listing the way the backend does: a pushed Status
// set is an allow-list over the stored rows, and a name in it that the scope
// does not accept is refused when refuse is set.
func vocabularyReader(rows []*beadslib.Issue, refuse string, sent *[]string) *nativeDoltReaderSpy {
	return &nativeDoltReaderSpy{
		list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			*sent = append(*sent, req.Status)
			if refuse != "" && slices.Contains(strings.Split(req.Status, ","), refuse) {
				return issueops.IssuePage{}, fmt.Errorf("listIssues: %w: invalid status %q", issueops.ErrValidation, refuse)
			}
			var page issueops.IssuePage
			for _, r := range rows {
				if req.Status == "" || slices.Contains(strings.Split(req.Status, ","), string(r.Status)) {
					page.Items = append(page.Items, &issueops.IssueWithCounts{Issue: r})
				}
			}
			return page, nil
		},
	}
}

func outOfVocabularyRows() []*beadslib.Issue {
	return []*beadslib.Issue{
		{ID: "gc-open", Title: "o", Status: beadslib.StatusOpen, IssueType: "task"},
		// a status the vocabulary read at open does not hold (added later, or kept only in config.yaml)
		{ID: "gc-drift", Title: "d", Status: beadslib.Status("staged_ready"), IssueType: "task"},
		{ID: "gc-done", Title: "c", Status: beadslib.StatusClosed, IssueType: "task"},
	}
}

func listedIDs(got []Bead) []string {
	ids := make([]string, 0, len(got))
	for _, b := range got {
		ids = append(ids, b.ID)
	}
	slices.Sort(ids)
	return ids
}

// The mayor's condition 2(a): a row whose status is outside the vocabulary
// still appears in an ordinary no-Status listing, because only an opted-in
// caller is narrowed. Red under the earlier default-on pushdown.
func TestAnOrdinaryListingStillReturnsAnOutOfVocabularyRow(t *testing.T) {
	var sent []string
	store := newNativeDoltStoreWithStorage(statusSettingStorage{nativeDoltReaderSpy: vocabularyReader(outOfVocabularyRows(), "", &sent)}, "native-test")
	store.loadListNotClosedStatus(context.Background())
	if store.listNotClosedStatus != builtinNotClosed {
		t.Fatalf("set = %q, want the built-ins only (staged_ready is not in the vocabulary)", store.listNotClosedStatus)
	}
	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if ids := listedIDs(got); !slices.Equal(ids, []string{"gc-drift", "gc-open"}) {
		t.Fatalf("ordinary listing = %v, want the open and the out-of-vocabulary row", ids)
	}
	if !slices.Equal(sent, []string{""}) {
		t.Fatalf("ordinary listing sent Status %q, want nothing pushed", sent)
	}
}

// The mayor's condition 1 and 2(b): a store that refuses the pushed set (a name
// its vocabulary no longer accepts) answers the opted-in listing unpushed, with
// the out-of-vocabulary row, latches the pushdown off, and is never an error.
func TestARefusedNotClosedSetFallsBackAndLatchesOff(t *testing.T) {
	var sent []string
	reader := vocabularyReader(outOfVocabularyRows(), "launched", &sent)
	store := newNativeDoltStoreWithStorage(statusSettingStorage{nativeDoltReaderSpy: reader, value: "staged_ready,launched"}, "native-test")
	store.loadListNotClosedStatus(context.Background())
	set := builtinNotClosed + ",staged_ready,launched"
	if store.listNotClosedStatus != set {
		t.Fatalf("set = %q, want %q", store.listNotClosedStatus, set)
	}
	opted := ListQuery{AllowScan: true, TierMode: TierBoth, Live: true, PushNotClosed: true}
	got, err := store.List(opted)
	if err != nil {
		t.Fatalf("List = %v, want the unpushed answer, never an error", err)
	}
	if ids := listedIDs(got); !slices.Equal(ids, []string{"gc-drift", "gc-open"}) {
		t.Fatalf("fallback listing = %v, want every non-closed row", ids)
	}
	if !store.statusPushdownOff.Load() {
		t.Fatal("a refused set did not latch the pushdown off")
	}
	if !slices.Equal(sent, []string{set, ""}) {
		t.Fatalf("sent %q, want the set then one unpushed retry", sent)
	}
	sent = nil
	if _, err := store.List(opted); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if !slices.Equal(sent, []string{""}) {
		t.Fatalf("after the latch the opted-in listing sent %q, want nothing pushed", sent)
	}
}
