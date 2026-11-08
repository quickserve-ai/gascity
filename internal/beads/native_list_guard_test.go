package beads

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

func noOutsideRows(context.Context, beadslib.Storage, string) ([]nativeListOutsideRow, error) {
	return nil, nil
}

func guardStore(t *testing.T, sent *[]string, logs *syncBuffer) *NativeDoltStore {
	t.Helper()
	store := newNativeDoltStoreWithStorageAndPrefix(statusSettingStorage{nativeDoltReaderSpy: vocabularyReader(outOfVocabularyRows(), "", sent)}, "native-test", "native-test")
	store.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return store
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
