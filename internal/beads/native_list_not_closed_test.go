package beads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
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
	// live, when set, is read on every WorkspaceConfig call instead of
	// value: an operator changing status.custom while the store is open.
	live *string
	// liveErr, when set, is read on every WorkspaceConfig call instead of err:
	// a setting that was readable and then is not.
	liveErr *error
}

func (s statusSettingStorage) WorkspaceConfig() (issueops.WorkspaceConfig, error) {
	value := s.value
	if s.live != nil {
		value = *s.live
	}
	err := s.err
	if s.liveErr != nil {
		err = *s.liveErr
	}
	return statusSettingConfig{infraSettingConfig: infraSettingConfig{}, value: value, err: err}, nil
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
			store.listOutsideProbe = noOutsideRows
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

// The mayor's condition 1 and 2(b): a store that refuses the pushed set answers
// the opted-in listing unpushed, with the out-of-vocabulary row, and is never an
// error. A refusal that quotes a name does not latch: the set was read for this
// listing, so the reader's own cache is what lags (Opus round 2, must-fix 1).
func TestARefusedNamedStatusFallsBackWithoutLatching(t *testing.T) {
	var sent []string
	reader := vocabularyReader(outOfVocabularyRows(), "launched", &sent)
	store := newNativeDoltStoreWithStorage(statusSettingStorage{nativeDoltReaderSpy: reader, value: "staged_ready,launched"}, "native-test")
	store.listOutsideProbe = noOutsideRows
	var logs syncBuffer
	store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
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
	if store.statusPushdownOff.Load() {
		t.Fatal("a refusal quoting a name the fresh set holds latched the pushdown off")
	}
	if !slices.Equal(sent, []string{set, ""}) {
		t.Fatalf("sent %q, want the set then one unpushed retry", sent)
	}
	if !strings.Contains(logs.String(), "refused_status=launched") {
		t.Fatalf("logs = %q, want the line naming the refused status", logs.String())
	}
	sent = nil
	if _, err := store.List(opted); err != nil {
		t.Fatalf("second List: %v", err)
	}
	if !slices.Equal(sent, []string{set, ""}) {
		t.Fatalf("the next listing sent %q, want it to try the fresh set again", sent)
	}
	if stats := store.listCounters.snapshot(); stats.GuardFallbacks != 2 || stats.GuardRuns != 2 {
		t.Fatalf("stats = %+v, want 2 runs and 2 fallbacks", stats)
	}
}

// The mayor's condition 2(b), the opt-in path: a status the vocabulary read at
// open does not hold, added to status.custom while the store is open, is in
// the set the next opted-in listing pushes, so its row (a workflow root an
// agent moved into it, say) is still seen by the order dispatcher's fold. Red
// when the opted-in listing pushes the set cached at open.
func TestAnOptedInListingSeesAStatusAddedAfterOpen(t *testing.T) {
	var sent []string
	setting := ""
	rows := []*beadslib.Issue{
		{ID: "gc-open", Title: "o", Status: beadslib.StatusOpen, IssueType: "task"},
		{ID: "gc-review", Title: "r", Status: beadslib.Status("review"), IssueType: "task"},
		{ID: "gc-done", Title: "c", Status: beadslib.StatusClosed, IssueType: "task"},
	}
	store := newNativeDoltStoreWithStorage(statusSettingStorage{nativeDoltReaderSpy: vocabularyReader(rows, "", &sent), live: &setting}, "native-test")
	store.listOutsideProbe = noOutsideRows
	store.loadListNotClosedStatus(context.Background())
	if store.listNotClosedStatus != builtinNotClosed {
		t.Fatalf("set at open = %q, want the built-ins only", store.listNotClosedStatus)
	}
	setting = "review" // the operator adds the status after open
	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true, PushNotClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if ids := listedIDs(got); !slices.Equal(ids, []string{"gc-open", "gc-review"}) {
		t.Fatalf("opted-in listing = %v, want the open row and the row in the status added after open", ids)
	}
	if want := builtinNotClosed + ",review"; !slices.Equal(sent, []string{want}) {
		t.Fatalf("sent %q, want the set read afresh (%q)", sent, want)
	}
}

// An opted-in listing whose vocabulary cannot be read pushes nothing: the full
// read, never a guessed set.
func TestAnOptedInListingWithAnUnreadableVocabularyReadsUnpushed(t *testing.T) {
	var sent []string
	store := newNativeDoltStoreWithStorage(statusSettingStorage{nativeDoltReaderSpy: vocabularyReader(outOfVocabularyRows(), "", &sent), err: errors.New("config read failed")}, "native-test")
	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true, PushNotClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if ids := listedIDs(got); !slices.Equal(ids, []string{"gc-drift", "gc-open"}) {
		t.Fatalf("listing = %v, want every non-closed row", ids)
	}
	if !slices.Equal(sent, []string{""}) {
		t.Fatalf("sent %q, want nothing pushed", sent)
	}
}

// typedVocabularyReader is vocabularyReader that also refuses a pushed type:
// with typeMsg when it is set, else with a bare validation error that names
// neither predicate.
func typedVocabularyReader(rows []*beadslib.Issue, refuseStatus string, refuseType bool, typeMsg string, sent *[]string) *nativeDoltReaderSpy {
	return &nativeDoltReaderSpy{
		list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			*sent = append(*sent, req.Status+"|"+req.IssueType)
			if refuseType && req.IssueType != "" {
				if typeMsg == "" {
					return issueops.IssuePage{}, fmt.Errorf("listIssues: %w", issueops.ErrValidation)
				}
				return issueops.IssuePage{}, fmt.Errorf("listIssues: %w: %s", issueops.ErrValidation, typeMsg)
			}
			if refuseStatus != "" && slices.Contains(strings.Split(req.Status, ","), refuseStatus) {
				return issueops.IssuePage{}, fmt.Errorf("listIssues: %w: invalid status %q", issueops.ErrValidation, refuseStatus)
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

// The two fallbacks compose (both reads' finding on round 1): an opted-in
// listing that also pushes a type the backend refuses is answered, never an
// error until restart, and only the predicate the refusal names latches off.
func TestTheNotClosedAndTypeFallbacksCompose(t *testing.T) {
	set := builtinNotClosed
	for _, tc := range []struct {
		name                       string
		refuseStatus               string
		typeMsg                    string
		wantSent                   []string
		wantIDs                    []string
		wantStatusOff, wantTypeOff bool
		// wantStatusLine is what the status latch's one line must carry
		// ("" when that latch must not fire).
		wantStatusLine string
	}{
		// The status set stays pushed, so the out-of-vocabulary row is the
		// documented residue: only the in-vocabulary row comes back.
		{"the type refused by name", "", `invalid issue type "task"`, []string{set + "|task", set + "|"}, []string{"gc-open"}, false, true, ""},
		{"a refusal that names neither drops both", "", "", []string{set + "|task", "|"}, []string{"gc-drift", "gc-open"}, true, true, "refused_status=\"\""},
		{"both refused, one after the other", "open", `invalid issue type "task"`, []string{set + "|task", set + "|", "|"}, []string{"gc-drift", "gc-open"}, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent []string
			reader := typedVocabularyReader(outOfVocabularyRows(), tc.refuseStatus, true, tc.typeMsg, &sent)
			store := newNativeDoltStoreWithStorage(statusSettingStorage{nativeDoltReaderSpy: reader}, "native-test")
			store.listOutsideProbe = noOutsideRows
			store.listPushableTypes = map[string]bool{"task": true}
			var logs syncBuffer
			store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			got, err := store.List(ListQuery{Type: "task", AllowScan: true, TierMode: TierBoth, Live: true, PushNotClosed: true})
			if err != nil {
				t.Fatalf("List = %v, want an answer, never an error", err)
			}
			if ids := listedIDs(got); !slices.Equal(ids, tc.wantIDs) {
				t.Fatalf("listing = %v, want %v", ids, tc.wantIDs)
			}
			if !slices.Equal(sent, tc.wantSent) {
				t.Fatalf("sent %q, want %q", sent, tc.wantSent)
			}
			if store.statusPushdownOff.Load() != tc.wantStatusOff || store.typePushdownOff.Load() != tc.wantTypeOff {
				t.Fatalf("latches status=%v type=%v, want status=%v type=%v",
					store.statusPushdownOff.Load(), store.typePushdownOff.Load(), tc.wantStatusOff, tc.wantTypeOff)
			}
			var statusLines []string
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				if strings.Contains(line, "not-closed status set") {
					statusLines = append(statusLines, line)
				}
			}
			if tc.wantStatusLine == "" {
				if len(statusLines) != 0 {
					t.Fatalf("status latch logged %q, want no status line", statusLines)
				}
			} else if len(statusLines) != 1 || !strings.Contains(statusLines[0], tc.wantStatusLine) {
				t.Fatalf("status latch lines = %q, want one carrying %q", statusLines, tc.wantStatusLine)
			}
		})
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent log writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// The mayor's condition 1: the latch logs ONE line naming the store and the
// refused name, however many opted-in listings race into the refusal.
func TestARefusedNotClosedSetLogsOneLineNamingTheStatus(t *testing.T) {
	var (
		mu   sync.Mutex
		sent []string
	)
	base := vocabularyReader(outOfVocabularyRows(), "launched", &[]string{})
	reader := &nativeDoltReaderSpy{list: func(ctx context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, req.Status)
		return base.list(ctx, req)
	}}
	var logs syncBuffer
	store := newNativeDoltStoreWithStorageAndPrefix(statusSettingStorage{nativeDoltReaderSpy: reader, value: "staged_ready,launched"}, "native-test", "native-test")
	store.listOutsideProbe = noOutsideRows
	store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true, PushNotClosed: true}); err != nil {
				t.Errorf("List: %v", err)
			}
		}()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("refusal logged %d lines, want exactly one:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"prefix=native-test", "refused_status=launched"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("latch line %q does not carry %q", lines[0], want)
		}
	}
}

// A latched store's opted-in listing reads unpushed, and that has to be
// countable: the guard runs no probe there, so without the count the latch
// reads as a guard with no work.
func TestALatchedListingCountsAsAFallback(t *testing.T) {
	var sent []string
	reader := vocabularyReader(outOfVocabularyRows(), "", &sent)
	store := newNativeDoltStoreWithStorage(statusSettingStorage{nativeDoltReaderSpy: reader}, "native-test")
	store.listOutsideProbe = noOutsideRows
	store.statusPushdownOff.Store(true)
	if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true, PushNotClosed: true}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Equal(sent, []string{""}) {
		t.Fatalf("sent %q, want nothing pushed", sent)
	}
	if stats := store.listCounters.snapshot(); stats.GuardFallbacks != 1 || stats.GuardRuns != 0 {
		t.Fatalf("stats = %+v, want 1 fallback and no probe", stats)
	}
}

// A status registered while the pushed read runs can already hold rows the set
// did not name, and the next probe will not see them because the set will then
// name that status. The listing answers from a full read instead (Opus round 3).
func TestAStatusRegisteredDuringThePushedReadSendsItUnpushed(t *testing.T) {
	setting := ""
	var sent []string
	base := vocabularyReader(outOfVocabularyRows(), "", &sent)
	reader := &nativeDoltReaderSpy{list: func(ctx context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
		page, err := base.list(ctx, req)
		setting = "staged_ready"
		return page, err
	}}
	store := newNativeDoltStoreWithStorage(statusSettingStorage{nativeDoltReaderSpy: reader, live: &setting}, "native-test")
	store.listOutsideProbe = noOutsideRows
	var logs syncBuffer
	store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true, PushNotClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if ids := listedIDs(got); !slices.Equal(ids, []string{"gc-drift", "gc-open"}) {
		t.Fatalf("listing = %v, want the row in the new status too", ids)
	}
	if !slices.Equal(sent, []string{builtinNotClosed, ""}) {
		t.Fatalf("sent %q, want the set then one unpushed read", sent)
	}
	if !strings.Contains(logs.String(), "status.custom changed during a pushed") {
		t.Fatalf("logs = %q, want the line saying why", logs.String())
	}
	if stats := store.listCounters.snapshot(); stats.GuardFallbacks != 1 {
		t.Fatalf("stats = %+v, want 1 fallback", stats)
	}
}

// A re-check that cannot read the setting proves nothing about what the pushed
// read hid, so the listing answers from a full read, and that read's own failure
// is the listing's failure, never the pushed page (Astra at 26c81a9dd59d).
func TestAFailedRecheckSendsTheListingUnpushed(t *testing.T) {
	retryFailed := errors.New("unpushed read failed")
	for _, tc := range []struct {
		name     string
		retryErr error
	}{
		{name: "the unpushed read answers"},
		{name: "the unpushed read fails", retryErr: retryFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent []string
			var settingErr error
			base := vocabularyReader(outOfVocabularyRows(), "", &sent)
			calls := 0
			reader := &nativeDoltReaderSpy{list: func(ctx context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
				calls++
				if calls == 1 {
					page, err := base.list(ctx, req)
					settingErr = errors.New("status.custom unreadable")
					return page, err
				}
				if tc.retryErr != nil {
					return issueops.IssuePage{}, tc.retryErr
				}
				return base.list(ctx, req)
			}}
			store := newNativeDoltStoreWithStorage(statusSettingStorage{nativeDoltReaderSpy: reader, liveErr: &settingErr}, "native-test")
			store.listOutsideProbe = noOutsideRows
			got, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true, PushNotClosed: true})
			if calls != 2 {
				t.Fatalf("reads = %d, want the pushed read then one unpushed read", calls)
			}
			if tc.retryErr != nil {
				if !errors.Is(err, tc.retryErr) {
					t.Fatalf("List err = %v, want the unpushed read's failure", err)
				}
				if len(got) != 0 {
					t.Fatalf("listing = %v, want no rows from the pushed read", listedIDs(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if ids := listedIDs(got); !slices.Equal(ids, []string{"gc-drift", "gc-open"}) {
				t.Fatalf("listing = %v, want the full read's rows", ids)
			}
			if !slices.Equal(sent, []string{builtinNotClosed, ""}) {
				t.Fatalf("sent %q, want the set then one unpushed read", sent)
			}
			if stats := store.listCounters.snapshot(); stats.GuardFallbacks != 1 {
				t.Fatalf("stats = %+v, want 1 fallback", stats)
			}
		})
	}
}

type backingOnlyStore struct {
	Store
	backing Store
}

func (s backingOnlyStore) Backing() Store { return s.backing }

// A wrong true here would let a non-idempotent order rely on a narrowed listing.
// A proxy answers false even over a bd leaf: it can demote between the listing
// and the check, so its leaf now says nothing about that listing (Astra at
// 903dd063089f).
func TestIgnoresPushNotClosed(t *testing.T) {
	bd := NewBdStore(t.TempDir(), func(string, string, ...string) ([]byte, error) { return []byte("[]"), nil })
	mem := NewMemStore()
	for _, tc := range []struct {
		name  string
		store Store
		want  bool
	}{
		{"bd", bd, true},
		{"a cache directly over bd", NewCachingStoreForTest(bd, nil), true},
		{"a demoted proxy, whose read leaf is bd", &ProxiedStore{bd: bd}, false},
		{"a cache over a demoted proxy", NewCachingStoreForTest(&ProxiedStore{bd: bd}, nil), false},
		{"a proxy-shaped wrapper over bd", backingOnlyStore{backing: bd}, false},
		{"a native store can push", &NativeDoltStore{}, false},
		{"a cache over a native store", NewCachingStoreForTest(&NativeDoltStore{}, nil), false},
		{"another store", mem, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IgnoresPushNotClosed(tc.store); got != tc.want {
				t.Fatalf("IgnoresPushNotClosed = %t, want %t", got, tc.want)
			}
		})
	}
}

// The gate's exemption for a store that ignores the pushdown rests on this: for
// the dispatcher's own question a bd store sends the same command and returns
// the same rows with the flag as without it. If bd learns the pushdown, this
// fails until BdStore stops declaring IgnoresPushNotClosed (the mayor's
// condition, 2026-11-10T00:09:14Z, ga-tyf38i).
func TestBdStoreIndexAnswerEqualsItsFullRead(t *testing.T) {
	const rows = `[
		{"id":"gc-open","title":"t","status":"open","issue_type":"task","labels":["order-run:digest"]},
		{"id":"gc-track","title":"t","status":"in_progress","issue_type":"task","labels":["order-tracking","order-run:digest"]},
		{"id":"gc-mol","title":"t","status":"open","issue_type":"molecule","labels":["order-run:sweep"]},
		{"id":"gc-stray","title":"t","status":"staged_ready","issue_type":"task","labels":["order-run:stray"]}
	]`
	for _, tc := range []struct {
		name string
		wrap func(*BdStore) Store
	}{
		{"bare", func(s *BdStore) Store { return s }},
		{"directly cached", func(s *BdStore) Store { return NewCachingStoreForTest(s, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			store := tc.wrap(NewBdStore(t.TempDir(), func(_, name string, args ...string) ([]byte, error) {
				calls = append(calls, name+" "+strings.Join(args, " "))
				return []byte(rows), nil
			}))
			if !IgnoresPushNotClosed(store) {
				t.Fatalf("%T does not declare it ignores the pushdown", store)
			}
			live := HandlesFor(store).Live
			read := func(push bool) ([]Bead, []string) {
				calls = nil
				got, err := live.List(ListQuery{AllowScan: true, TierMode: TierBoth, PushNotClosed: push})
				if err != nil {
					t.Fatalf("List(push=%t): %v", push, err)
				}
				return got, slices.Clone(calls)
			}
			pushed, pushedCalls := read(true)
			full, fullCalls := read(false)
			if len(pushedCalls) == 0 || !slices.Equal(pushedCalls, fullCalls) {
				t.Fatalf("bd commands differ:\n pushed: %v\n full:   %v", pushedCalls, fullCalls)
			}
			if !reflect.DeepEqual(pushed, full) {
				t.Fatalf("rows differ:\n pushed: %+v\n full:   %+v", pushed, full)
			}
			want := map[string]string{"gc-open": "order-run:digest", "gc-track": "order-run:digest", "gc-mol": "order-run:sweep", "gc-stray": "order-run:stray"}
			seen := map[string]bool{}
			for _, b := range pushed {
				if label, ok := want[b.ID]; ok && slices.Contains(b.Labels, label) {
					seen[b.ID] = true
				}
			}
			if len(seen) != len(want) {
				t.Fatalf("rows with their order-run labels = %v, want all of %v; rows: %+v", seen, want, pushed)
			}
		})
	}
}
