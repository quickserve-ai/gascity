package beads

import (
	"context"
	"errors"
	"slices"
	"testing"

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
// names no Status and does not include closed rows, and only when the store has
// a set.
func TestNativeListPlanPushesTheNotClosedSetOnlyForTheDefault(t *testing.T) {
	set := builtinNotClosed + ",staged_ready"
	for _, tc := range []struct {
		name      string
		query     ListQuery
		notClosed string
		want      string
	}{
		{"no Status, not IncludeClosed", ListQuery{AllowScan: true, TierMode: TierBoth, Live: true}, set, set},
		{"no Status, IncludeClosed", ListQuery{AllowScan: true, IncludeClosed: true}, set, ""},
		{"a folded open stays Go-side", ListQuery{Status: "open", AllowScan: true}, set, ""},
		{"a Live open stays raw", ListQuery{Status: "open", AllowScan: true, Live: true}, set, "open"},
		{"an exact status stays exact", ListQuery{Status: "in_progress"}, set, "in_progress"},
		{"no set, no push", ListQuery{AllowScan: true}, "", ""},
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
			if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true}); err != nil {
				t.Fatalf("List: %v", err)
			}
			if !slices.Equal(sent, []string{tc.want}) {
				t.Fatalf("List sent Status %q, want %q", sent, tc.want)
			}
		})
	}
}
