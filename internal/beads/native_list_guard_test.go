package beads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

func noOutsideRows(context.Context, beadslib.Storage, string) ([]nativeListOutsideRow, error) {
	return nil, nil
}

func guardStore(t *testing.T, sent *[]string, logs *syncBuffer) *NativeDoltStore {
	t.Helper()
	return guardStoreOver(t, statusSettingStorage{nativeDoltReaderSpy: vocabularyReader(outOfVocabularyRows(), "", sent)}, logs)
}

func guardStoreOver(t *testing.T, storage beadslib.Storage, logs *syncBuffer) *NativeDoltStore {
	t.Helper()
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "native-test", "native-test")
	store.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return store
}

// probeOverRows answers the guard from the same rows the reader serves: the
// ones whose status is neither in the pushed set nor closed. A row it reports
// is a row the listing really holds, never a canned one.
func probeOverRows(rows []*beadslib.Issue, found *[]nativeListOutsideRow) nativeListOutsideProbe {
	return func(_ context.Context, _ beadslib.Storage, notClosed string) ([]nativeListOutsideRow, error) {
		allowed := strings.Split(notClosed, ",")
		var out []nativeListOutsideRow
		for _, row := range rows {
			if status := string(row.Status); status != "closed" && !slices.Contains(allowed, status) {
				out = append(out, nativeListOutsideRow{ID: row.ID, Status: status})
			}
		}
		if found != nil {
			*found = out
		}
		return out, nil
	}
}

var guardListing = ListQuery{AllowScan: true, TierMode: TierBoth, Live: true, PushNotClosed: true}

// The case the guard exists for: a row in a status the vocabulary no longer
// names. The pushed set is an allow-list, so a pushed read would hide it.
func TestARowOutsideTheVocabularySendsTheListingUnpushed(t *testing.T) {
	var sent []string
	var logs syncBuffer
	store := guardStore(t, &sent, &logs)
	store.listOutsideProbe = func(context.Context, beadslib.Storage, string) ([]nativeListOutsideRow, error) {
		return []nativeListOutsideRow{{ID: "gc-drift", Status: "drifted"}}, nil
	}
	got, err := store.List(guardListing)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if ids := listedIDs(got); !slices.Equal(ids, []string{"gc-drift", "gc-open"}) {
		t.Fatalf("listing = %v, want the out-of-vocabulary row too", ids)
	}
	if !slices.Equal(sent, []string{""}) {
		t.Fatalf("sent %q, want nothing pushed", sent)
	}
	for _, want := range []string{"prefix=native-test", "count=1", "statuses=drifted", "runs=1 rows=1 fallbacks=1"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log %q lacks %q", logs.String(), want)
		}
	}
	stats := store.listRequestStats()
	if stats.GuardRuns != 1 || stats.GuardRows != 1 || stats.GuardFallbacks != 1 {
		t.Fatalf("stats = %+v, want one run, one row, one fallback", stats)
	}
}

// A guard that errors must never hide a row: this listing reads unpushed.
func TestAFailedGuardSendsTheListingUnpushed(t *testing.T) {
	var sent []string
	var logs syncBuffer
	store := guardStore(t, &sent, &logs)
	store.listOutsideProbe = func(context.Context, beadslib.Storage, string) ([]nativeListOutsideRow, error) {
		return nil, errors.New("probe timed out")
	}
	got, err := store.List(guardListing)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if ids := listedIDs(got); !slices.Equal(ids, []string{"gc-drift", "gc-open"}) {
		t.Fatalf("listing = %v, want every non-closed row", ids)
	}
	if !slices.Equal(sent, []string{""}) {
		t.Fatalf("sent %q, want nothing pushed", sent)
	}
	if !strings.Contains(logs.String(), "probe timed out") {
		t.Fatalf("log %q does not name the failure", logs.String())
	}
	if stats := store.listRequestStats(); stats.GuardFallbacks != 1 || stats.GuardRows != 0 {
		t.Fatalf("stats = %+v, want one fallback and no rows", stats)
	}
}

// The default probe on a storage with no SQL handle fails toward the full read.
func TestAStorageWithNoSQLHandleReadsUnpushed(t *testing.T) {
	var sent []string
	var logs syncBuffer
	store := guardStore(t, &sent, &logs)
	if _, err := store.List(guardListing); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Equal(sent, []string{""}) {
		t.Fatalf("sent %q, want nothing pushed", sent)
	}
}

// A clean guard leaves the pushdown on, and each listing runs the guard: a
// status renamed back clears itself with no latch.
func TestACleanGuardPushesAndNothingLatches(t *testing.T) {
	var sent []string
	var logs syncBuffer
	store := guardStore(t, &sent, &logs)
	outside := []nativeListOutsideRow{{ID: "gc-drift", Status: "drifted"}}
	store.listOutsideProbe = func(context.Context, beadslib.Storage, string) ([]nativeListOutsideRow, error) {
		return outside, nil
	}
	if _, err := store.List(guardListing); err != nil {
		t.Fatalf("List: %v", err)
	}
	outside = nil
	if _, err := store.List(guardListing); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sent) != 2 || sent[0] != "" || sent[1] != builtinNotClosed {
		t.Fatalf("sent %q, want unpushed then the pushed set", sent)
	}
	if stats := store.listRequestStats(); stats.GuardRuns != 2 || stats.GuardFallbacks != 1 {
		t.Fatalf("stats = %+v, want two runs and one fallback", stats)
	}
}

func TestAnOrdinaryListingRunsNoGuard(t *testing.T) {
	var sent []string
	var logs syncBuffer
	store := guardStore(t, &sent, &logs)
	store.listOutsideProbe = func(context.Context, beadslib.Storage, string) ([]nativeListOutsideRow, error) {
		t.Fatal("the guard ran on a listing that did not opt in")
		return nil, nil
	}
	if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if stats := store.listRequestStats(); stats.GuardRuns != 0 {
		t.Fatalf("stats = %+v, want no guard runs", stats)
	}
}

// The mayor's term. A molecule root gc has no name for: the pushed read hides
// it (the first half, with a probe that lies and says the ledger is clean), and
// the guard, finding that same row, brings it back and says so.
func TestAMoleculeRootOutsideTheVocabularyAppearsBecauseOfTheGuard(t *testing.T) {
	const rootID, stray = "gc-mol-root", "retired"
	rows := []*beadslib.Issue{
		{ID: "gc-open", Title: "o", Status: beadslib.StatusOpen, IssueType: "task"},
		{ID: rootID, Title: "m", Status: beadslib.Status(stray), IssueType: "molecule"},
		{ID: "gc-done", Title: "c", Status: beadslib.StatusClosed, IssueType: "task"},
	}
	if !IsMoleculeType("molecule") {
		t.Fatal("the fixture's root is not a molecule type")
	}

	t.Run("a pushed read hides it", func(t *testing.T) {
		var sent []string
		var logs syncBuffer
		store := guardStoreOver(t, statusSettingStorage{nativeDoltReaderSpy: vocabularyReader(rows, "", &sent)}, &logs)
		store.listOutsideProbe = noOutsideRows
		got, err := store.List(guardListing)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if ids := listedIDs(got); !slices.Equal(ids, []string{"gc-open"}) {
			t.Fatalf("listing = %v, want the root hidden: the fixture proves nothing otherwise", ids)
		}
		if !slices.Equal(sent, []string{builtinNotClosed}) {
			t.Fatalf("sent %q, want the pushed set", sent)
		}
	})

	t.Run("the guard makes it appear", func(t *testing.T) {
		var sent []string
		var logs syncBuffer
		var found []nativeListOutsideRow
		store := guardStoreOver(t, statusSettingStorage{nativeDoltReaderSpy: vocabularyReader(rows, "", &sent)}, &logs)
		store.listOutsideProbe = probeOverRows(rows, &found)
		got, err := store.List(guardListing)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if !slices.Equal(found, []nativeListOutsideRow{{ID: rootID, Status: stray}}) {
			t.Fatalf("probe found %+v, want exactly the root", found)
		}
		if ids := listedIDs(got); !slices.Equal(ids, []string{rootID, "gc-open"}) {
			t.Fatalf("listing = %v, want the root visible", ids)
		}
		for _, b := range got {
			if b.ID == rootID && (!IsMoleculeType(b.Type) || b.Status != "open") {
				t.Fatalf("root = %+v, want a molecule folded to open", b)
			}
		}
		if !slices.Equal(sent, []string{""}) {
			t.Fatalf("sent %q, want nothing pushed", sent)
		}
		for _, want := range []string{"level=WARN", "rows sit outside", "prefix=native-test", "count=1", "statuses=" + stray, "rows=\"" + rootID + "=" + stray} {
			if !strings.Contains(logs.String(), want) {
				t.Fatalf("log %q lacks %q", logs.String(), want)
			}
		}
	})

	t.Run("a status the scope names is pushed and still visible", func(t *testing.T) {
		var sent []string
		var logs syncBuffer
		store := guardStoreOver(t, statusSettingStorage{nativeDoltReaderSpy: vocabularyReader(rows, "", &sent), value: stray}, &logs)
		store.listOutsideProbe = probeOverRows(rows, nil)
		got, err := store.List(guardListing)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if ids := listedIDs(got); !slices.Equal(ids, []string{rootID, "gc-open"}) {
			t.Fatalf("listing = %v, want the root visible", ids)
		}
		if want := builtinNotClosed + "," + stray; !slices.Equal(sent, []string{want}) {
			t.Fatalf("sent %q, want %q", sent, want)
		}
		if logs.String() != "" {
			t.Fatalf("a clean listing warned: %s", logs.String())
		}
	})
}

// The guard runs only where the planner could push: on a listing it cannot
// change it neither probes, counts nor warns, even when status.custom is
// unreadable.
func TestTheGuardRunsOnlyOnAListingThePlannerCouldPush(t *testing.T) {
	in := func(q ListQuery) ListQuery { q.PushNotClosed = true; return q }
	for _, tc := range []struct {
		name  string
		query ListQuery
	}{
		{"a folded open", in(ListQuery{Status: "open", AllowScan: true})},
		{"a Live open", in(ListQuery{Status: "open", AllowScan: true, Live: true})},
		{"an exact status", in(ListQuery{Status: "in_progress", AllowScan: true})},
		{"closed rows included", in(ListQuery{IncludeClosed: true, AllowScan: true})},
		{"the whole active universe", in(ListQuery{CompleteActive: true, AllowScan: true})},
	} {
		for _, settingErr := range []error{nil, errors.New("status.custom unreadable")} {
			t.Run(fmt.Sprintf("%s, setting error %v", tc.name, settingErr), func(t *testing.T) {
				var sent []string
				var logs syncBuffer
				store := guardStoreOver(t, statusSettingStorage{
					nativeDoltReaderSpy: vocabularyReader(outOfVocabularyRows(), "", &sent),
					err:                 settingErr,
				}, &logs)
				store.listOutsideProbe = func(context.Context, beadslib.Storage, string) ([]nativeListOutsideRow, error) {
					t.Fatal("the guard ran on a listing it cannot change")
					return nil, nil
				}
				if _, err := store.List(tc.query); err != nil {
					t.Fatalf("List: %v", err)
				}
				if stats := store.listRequestStats(); stats.GuardRuns != 0 || stats.GuardFallbacks != 0 {
					t.Fatalf("stats = %+v, want nothing counted", stats)
				}
				if logs.String() != "" {
					t.Fatalf("warned on a listing the guard cannot change: %s", logs.String())
				}
			})
		}
	}
}

// A status.custom that cannot be used reads this listing unpushed, as a counted
// fallback and a warning, whether the read failed or the value does not parse.
func TestAnUnreadableVocabularyIsACountedWarnedFallback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		err     error
		wantLog string
	}{
		{"the read fails", "", errors.New("settings read timed out"), "settings read timed out"},
		{"the value does not parse", "staged ready", nil, "does not parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent []string
			var logs syncBuffer
			store := guardStoreOver(t, statusSettingStorage{
				nativeDoltReaderSpy: vocabularyReader(outOfVocabularyRows(), "", &sent),
				value:               tc.value,
				err:                 tc.err,
			}, &logs)
			store.listOutsideProbe = func(context.Context, beadslib.Storage, string) ([]nativeListOutsideRow, error) {
				t.Fatal("the guard probed with no set to push")
				return nil, nil
			}
			if _, err := store.List(guardListing); err != nil {
				t.Fatalf("List: %v", err)
			}
			if !slices.Equal(sent, []string{""}) {
				t.Fatalf("sent %q, want nothing pushed", sent)
			}
			for _, want := range []string{"level=WARN", "status.custom", tc.wantLog, "prefix=native-test"} {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("log %q lacks %q", logs.String(), want)
				}
			}
			if stats := store.listRequestStats(); stats.GuardFallbacks != 1 || stats.GuardRuns != 0 {
				t.Fatalf("stats = %+v, want one fallback and no guard run", stats)
			}
		})
	}
}

type closableStatusStorage struct {
	statusSettingStorage
}

func (closableStatusStorage) Close() error { return nil }

// A withReadRetry retry runs the guard again; only the attempt that ended the
// List is counted.
func TestAGuardRetriedByWithReadRetryCountsOnce(t *testing.T) {
	statusStorage := func(list func(context.Context, issueops.ListRequest) (issueops.IssuePage, error)) closableStatusStorage {
		return closableStatusStorage{statusSettingStorage{nativeDoltReaderSpy: &nativeDoltReaderSpy{list: list}}}
	}
	dead := statusStorage(func(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
		return issueops.IssuePage{}, errors.New("[mysql] i/o timeout")
	})
	healthy := statusStorage(func(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
		return issueops.IssuePage{Items: []*issueops.IssueWithCounts{nativeIssueRowForTest("gc-1")}}, nil
	})
	var reopens int32
	store := storeWithReopen(dead, healthy, &reopens)
	store.logger = slog.New(slog.DiscardHandler)
	probes := 0
	store.listOutsideProbe = func(context.Context, beadslib.Storage, string) ([]nativeListOutsideRow, error) {
		probes++
		return []nativeListOutsideRow{{ID: "gc-drift", Status: "drifted"}}, nil
	}
	if _, err := store.List(guardListing); err != nil {
		t.Fatalf("List: %v", err)
	}
	if reopens == 0 || probes != 2 {
		t.Fatalf("reopens = %d, probes = %d; the retry did not run the guard twice, so the test exercises nothing", reopens, probes)
	}
	if stats := store.listRequestStats(); stats.GuardRuns != 1 || stats.GuardRows != 1 || stats.GuardFallbacks != 1 {
		t.Fatalf("stats = %+v, want the last attempt only: one run, one row, one fallback", stats)
	}
}

// Every 120th guard run logs one Info line, so a healthy guard is visible.
func TestEveryHundredAndTwentiethGuardRunLogsOneLine(t *testing.T) {
	var sent []string
	var logs syncBuffer
	store := guardStore(t, &sent, &logs)
	store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	outside := []nativeListOutsideRow{{ID: "gc-drift", Status: "drifted"}}
	store.listOutsideProbe = func(context.Context, beadslib.Storage, string) ([]nativeListOutsideRow, error) {
		found := outside
		outside = nil
		return found, nil
	}
	list := func(times int) {
		t.Helper()
		for range times {
			if _, err := store.List(guardListing); err != nil {
				t.Fatalf("List: %v", err)
			}
		}
	}
	lines := func() int { return strings.Count(logs.String(), "level=INFO") }
	list(119)
	if n := lines(); n != 0 {
		t.Fatalf("%d Info lines before the 120th run:\n%s", n, logs.String())
	}
	list(1)
	if n := lines(); n != 1 {
		t.Fatalf("%d Info lines at the 120th run, want 1:\n%s", n, logs.String())
	}
	for _, want := range []string{"prefix=native-test", "guard_runs=120", "guard_rows=1", "guard_fallbacks=1"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log %q lacks %q", logs.String(), want)
		}
	}
	list(120)
	if n := lines(); n != 2 || !strings.Contains(logs.String(), "guard_runs=240") {
		t.Fatalf("%d Info lines at the 240th run, want 2 with guard_runs=240:\n%s", n, logs.String())
	}
}

// A listing that runs no guard must not repeat the line a multiple left behind.
func TestAListingWithoutAGuardRunLogsNoGuardLine(t *testing.T) {
	var sent []string
	var logs syncBuffer
	store := guardStore(t, &sent, &logs)
	store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	store.listOutsideProbe = noOutsideRows
	for range 120 {
		if _, err := store.List(guardListing); err != nil {
			t.Fatalf("List: %v", err)
		}
	}
	if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if n := strings.Count(logs.String(), "level=INFO"); n != 1 {
		t.Fatalf("%d Info lines, want 1:\n%s", n, logs.String())
	}
}

// A storage with no SQL handle stays that way: one warning for the store, an
// unpushed read and a counted fallback every time.
func TestNoSQLHandleWarnsOncePerStoreAndAlwaysReadsUnpushed(t *testing.T) {
	var sent []string
	var logs syncBuffer
	store := guardStore(t, &sent, &logs)
	for range 3 {
		if _, err := store.List(guardListing); err != nil {
			t.Fatalf("List: %v", err)
		}
	}
	if !slices.Equal(sent, []string{"", "", ""}) {
		t.Fatalf("sent %q, want every listing unpushed", sent)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("warned %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "no SQL handle") || !strings.Contains(logs.String(), "prefix=native-test") {
		t.Fatalf("log %q does not name the store and the cause", logs.String())
	}
	if stats := store.listRequestStats(); stats.GuardRuns != 3 || stats.GuardFallbacks != 3 {
		t.Fatalf("stats = %+v, want three runs and three fallbacks", stats)
	}

	var otherLogs syncBuffer
	other := guardStore(t, &sent, &otherLogs)
	if _, err := other.List(guardListing); err != nil {
		t.Fatalf("List: %v", err)
	}
	if n := strings.Count(otherLogs.String(), "level=WARN"); n != 1 {
		t.Fatalf("a second store warned %d times, want its own one warning", n)
	}
}

// Any other probe failure can clear, so each one still warns.
func TestAnotherProbeFailureWarnsEveryListing(t *testing.T) {
	var sent []string
	var logs syncBuffer
	store := guardStore(t, &sent, &logs)
	store.listOutsideProbe = func(context.Context, beadslib.Storage, string) ([]nativeListOutsideRow, error) {
		return nil, errors.New("probe timed out")
	}
	for range 2 {
		if _, err := store.List(guardListing); err != nil {
			t.Fatalf("List: %v", err)
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 2 {
		t.Fatalf("warned %d times, want twice:\n%s", n, logs.String())
	}
}

func TestTheGuardProbeIsBounded(t *testing.T) {
	query, _ := nativeListGuardQuery("open")
	if !strings.HasSuffix(query, ") LIMIT 100") {
		t.Fatalf("query = %q, want LIMIT 100 on the whole union", query)
	}
	if nativeListGuardTimeout != 10*time.Second {
		t.Fatalf("nativeListGuardTimeout = %v, want 10s", nativeListGuardTimeout)
	}
}

// The bound is on the context the probe runs under, not only a constant.
func TestTheGuardProbeRunsUnderTheTimeout(t *testing.T) {
	var sent []string
	var logs syncBuffer
	store := guardStore(t, &sent, &logs)
	called := false
	store.listOutsideProbe = func(ctx context.Context, _ beadslib.Storage, _ string) ([]nativeListOutsideRow, error) {
		called = true
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("the probe ran with no deadline")
		}
		if left := time.Until(deadline); left > nativeListGuardTimeout {
			t.Fatalf("deadline in %v, want at most %v", left, nativeListGuardTimeout)
		}
		return nil, nil
	}
	if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth, Live: true, PushNotClosed: true}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !called {
		t.Fatal("the probe did not run")
	}
}

// A count at the limit is "at least": the log says it is capped.
func TestAGuardCountAtTheLimitIsLoggedAsCapped(t *testing.T) {
	for _, tc := range []struct {
		rows int
		want string
	}{
		{99, "count=99 count_capped=false"},
		{100, "count=100 count_capped=true"},
	} {
		var sent []string
		var logs syncBuffer
		store := guardStore(t, &sent, &logs)
		store.listOutsideProbe = func(context.Context, beadslib.Storage, string) ([]nativeListOutsideRow, error) {
			return make([]nativeListOutsideRow, tc.rows), nil
		}
		if _, err := store.List(guardListing); err != nil {
			t.Fatalf("List: %v", err)
		}
		if !strings.Contains(logs.String(), tc.want) {
			t.Fatalf("log %q lacks %q", logs.String(), tc.want)
		}
	}
}

func TestNativeListGuardQueryCoversBothTablesAndClosed(t *testing.T) {
	query, args := nativeListGuardQuery("open, in_progress")
	if strings.Count(query, "NOT IN (?,?,?)") != 2 || !strings.Contains(query, "FROM issues") || !strings.Contains(query, "FROM wisps") {
		t.Fatalf("query = %q", query)
	}
	want := []any{"open", "in_progress", "closed", "open", "in_progress", "closed"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

func TestNativeListOutsideStatusesNamesThreeDistinct(t *testing.T) {
	rows := []nativeListOutsideRow{{Status: "a"}, {Status: "a"}, {Status: "b"}, {Status: "c"}, {Status: "d"}}
	if got := nativeListOutsideStatuses(rows, 3); got != "a,b,c" {
		t.Fatalf("got %q", got)
	}
}
