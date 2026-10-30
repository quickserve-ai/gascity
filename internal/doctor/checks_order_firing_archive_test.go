package doctor

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// writeOrderFiringArchive writes a rotated, gzipped event archive beside the
// city's active event log.
func writeOrderFiringArchive(t *testing.T, cityPath, stamp string, firstSeq, lastSeq uint64, body string) {
	t.Helper()
	dir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating event dir: %v", err)
	}
	base := fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz", stamp, firstSeq, lastSeq)
	f, err := os.Create(filepath.Join(dir, base))
	if err != nil {
		t.Fatalf("creating archive: %v", err)
	}
	defer f.Close() //nolint:errcheck // test fixture
	gw := gzip.NewWriter(f)
	if _, err := gw.Write([]byte(body)); err != nil {
		t.Fatalf("writing archive body: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}
}

// TestLatestControllerStartedAtFallsBackToArchivesWithoutWalkingThemAll covers
// the condition that made this expensive in production: the controller started
// before the last rotation, so the active log holds no controller.started and
// every invocation took the archived path.
//
// The older archives are intentionally not valid gzip. Reading any of them
// fails loudly, so this asserts both halves at once: the archived controller
// start is still found, and the archives older than it are never opened.
func TestLatestControllerStartedAtFallsBackToArchivesWithoutWalkingThemAll(t *testing.T) {
	cityPath := t.TempDir()

	// Active log: real events, but no controller start.
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Ts: time.Date(2026, 8, 5, 1, 0, 0, 0, time.UTC)},
	)

	dir := filepath.Join(cityPath, ".gc")
	for _, a := range []struct {
		stamp             string
		firstSeq, lastSeq uint64
	}{
		{"20260730T040012Z", 1, 100},
		{"20260731T172025Z", 101, 200},
	} {
		base := fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz", a.stamp, a.firstSeq, a.lastSeq)
		if err := os.WriteFile(filepath.Join(dir, base), []byte("not gzip"), 0o600); err != nil {
			t.Fatalf("writing unreadable archive: %v", err)
		}
	}

	want := time.Date(2026, 8, 4, 21, 46, 0, 0, time.UTC)
	writeOrderFiringArchive(t, cityPath, "20260804T214650Z", 201, 300,
		fmt.Sprintf(`{"seq":210,"type":%q,"ts":%q,"actor":"test"}`+"\n",
			events.ControllerStarted, want.Format(time.RFC3339Nano)))

	check := NewOrderFiringCurrentCheck(nil, cityPath)
	got, err := check.latestControllerStartedAt(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("latestControllerStartedAt read past the newest matching archive: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("latestControllerStartedAt = %s, want %s", got.UTC(), want)
	}
}

// TestLatestControllerStartedAtPrefersTheActiveLog keeps the cheap path cheap:
// when the active log holds a controller start, the archives are irrelevant. An
// unreadable archive proves none of them is touched.
func TestLatestControllerStartedAtPrefersTheActiveLog(t *testing.T) {
	cityPath := t.TempDir()
	want := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: want},
	)

	dir := filepath.Join(cityPath, ".gc")
	if err := os.WriteFile(
		filepath.Join(dir, "events.jsonl.archive-20260730T040012Z-seq-1-100.gz"),
		[]byte("not gzip"), 0o600); err != nil {
		t.Fatalf("writing unreadable archive: %v", err)
	}

	check := NewOrderFiringCurrentCheck(nil, cityPath)
	got, err := check.latestControllerStartedAt(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("latestControllerStartedAt touched the archives: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("latestControllerStartedAt = %s, want %s", got.UTC(), want)
	}
}

// TestLatestControllerStartedAtReportsUnknownWhenNothingRecorded pins the
// zero-time contract classifyOrderFiring depends on: absence must stay absence
// rather than becoming a bogus timestamp.
func TestLatestControllerStartedAtReportsUnknownWhenNothingRecorded(t *testing.T) {
	cityPath := t.TempDir()
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Ts: time.Date(2026, 8, 5, 1, 0, 0, 0, time.UTC)},
	)

	check := NewOrderFiringCurrentCheck(nil, cityPath)
	got, err := check.latestControllerStartedAt(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("latestControllerStartedAt: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("latestControllerStartedAt = %s, want zero time when no controller start was recorded", got.UTC())
	}
}

// writeTruncatedOrderFiringArchive writes a gzip archive whose trailer is cut
// off: its lines decode, but reading it to the end fails. A check that decodes
// it fails loudly, which is how the tests below prove it was never decoded
// while it still holds a real controller start.
func writeTruncatedOrderFiringArchive(t *testing.T, cityPath, stamp string, firstSeq, lastSeq uint64, body string) {
	t.Helper()
	writeOrderFiringArchive(t, cityPath, stamp, firstSeq, lastSeq, body)
	path := filepath.Join(cityPath, ".gc", fmt.Sprintf("events.jsonl.archive-%s-seq-%d-%d.gz", stamp, firstSeq, lastSeq))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading archive: %v", err)
	}
	if err := os.WriteFile(path, raw[:len(raw)-8], 0o644); err != nil {
		t.Fatalf("truncating archive: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening truncated archive: %v", err)
	}
	defer f.Close() //nolint:errcheck // test fixture
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("truncated archive header: %v", err)
	}
	if _, err := io.ReadAll(gr); err == nil {
		t.Fatal("fixture: the truncated archive reads cleanly; it would not catch a decode")
	}
}

func orderFiringEventLine(seq uint64, typ, subject string, ts time.Time) string {
	return fmt.Sprintf(`{"seq":%d,"type":%q,"subject":%q,"ts":%q,"actor":"test"}`+"\n", seq, typ, subject, ts.Format(time.RFC3339Nano))
}

// orderFiringRefusingLastRun fails any store lookup and counts the attempts.
func orderFiringRefusingLastRun(calls *atomic.Int32) OrderFiringCurrentLastRunFunc {
	return func(order orders.Order) (time.Time, error) {
		calls.Add(1)
		return time.Time{}, fmt.Errorf("store lookup for %s: the newest archive answers it", order.ScopedName())
	}
}

// TestOrderFiringCurrent_FreshRotationReadsNewestArchiveNotTheStore is the
// post-rotation shape that timed the check out (pl-yhyw): the active log holds
// minutes of events, every firing sits in the newest archive, and the last
// controller start is two archives back. The firings must come from the newest
// archive rather than one store round-trip per order, and the controller start
// must not be looked up at all, since every order has a firing to judge by.
func TestOrderFiringCurrent_FreshRotationReadsNewestArchiveNotTheStore(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestOrder(t, cityPath, "four-hourly", "cron", "0 */4 * * *")

	writeTruncatedOrderFiringArchive(t, cityPath, "20261027T110659Z", 1, 100,
		orderFiringEventLine(5, events.ControllerStarted, "", now.Add(-64*time.Hour)))
	writeOrderFiringArchive(t, cityPath, "20261029T235635Z", 101, 200,
		orderFiringEventLine(150, events.OrderFired, "four-hourly", now.Add(-2*time.Hour))+
			orderFiringEventLine(190, events.OrderFired, "cleanup-cooldown", now.Add(-20*time.Minute)))
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderCompleted, Subject: "cleanup-cooldown", Ts: now.Add(-3 * time.Minute)},
	)

	var calls atomic.Int32
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(orderFiringRefusingLastRun(&calls)))
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want ok; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("store lookups = %d, want 0: the newest archive holds every firing", n)
	}
	routine := strings.Join(check.routineDetails, "\n")
	for _, want := range []string{"cleanup-cooldown: last fired 20m ago", "four-hourly: last fired 2h ago"} {
		if !strings.Contains(routine, want) {
			t.Fatalf("routine = %v, want %q", check.routineDetails, want)
		}
	}
}

// TestOrderFiringCurrent_NeverFiredFindsControllerStartInNewestArchive keeps
// the never-fired verdict whole when the controller start sits in the newest
// archive: it dates the never-fired order. The older archive is truncated, so
// decoding it would fail the run.
func TestOrderFiringCurrent_NeverFiredFindsControllerStartInNewestArchive(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestOrder(t, cityPath, "silent", "cooldown", "1h")

	writeTruncatedOrderFiringArchive(t, cityPath, "20261027T110659Z", 1, 100,
		orderFiringEventLine(5, events.ControllerStarted, "", now.Add(-72*time.Hour)))
	writeOrderFiringArchive(t, cityPath, "20261029T235635Z", 101, 200,
		orderFiringEventLine(110, events.ControllerStarted, "", now.Add(-48*time.Hour))+
			orderFiringEventLine(190, events.OrderFired, "cleanup-cooldown", now.Add(-20*time.Minute)))
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderCompleted, Subject: "cleanup-cooldown", Ts: now.Add(-3 * time.Minute)},
	)

	var lookedUp []string
	var mu sync.Mutex
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(func(order orders.Order) (time.Time, error) {
		mu.Lock()
		defer mu.Unlock()
		lookedUp = append(lookedUp, order.ScopedName())
		return time.Time{}, nil
	}))
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusError || result.Severity != SeverityBlocking {
		t.Fatalf("status = %v severity = %v, want blocking error; msg = %s; details = %v", result.Status, result.Severity, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "silent: never fired since controller start 48h ago") {
		t.Fatalf("details = %v, want the newest archive's controller start to date the never-fired order", result.Details)
	}
	if fmt.Sprint(lookedUp) != "[silent]" {
		t.Fatalf("store lookups = %v, want [silent] only", lookedUp)
	}
}

// TestOrderFiringCurrent_StartOlderThanNewestArchiveIsInconclusive is the
// never-fired path that still walked archives (pl-yhyw review r1, P1): every
// order but one fired in the newest archive, the store has no run for that
// one, and the last controller start is two archives back. The check reads
// only the active log and the newest archive, once; with no start in either
// and older archives unread, the start is unknown and the never-fired order is
// inconclusive (advisory), not a reason to decode older archives. The older
// archives are truncated, so decoding either fails the run.
func TestOrderFiringCurrent_StartOlderThanNewestArchiveIsInconclusive(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestOrder(t, cityPath, "four-hourly", "cron", "0 */4 * * *")
	writeOrderFiringTestOrder(t, cityPath, "silent", "cooldown", "1h")

	writeTruncatedOrderFiringArchive(t, cityPath, "20261027T110659Z", 1, 100,
		orderFiringEventLine(5, events.ControllerStarted, "", now.Add(-64*time.Hour)))
	writeTruncatedOrderFiringArchive(t, cityPath, "20261028T110659Z", 101, 200,
		orderFiringEventLine(150, events.OrderFired, "four-hourly", now.Add(-30*time.Hour)))
	writeOrderFiringArchive(t, cityPath, "20261029T235635Z", 201, 300,
		orderFiringEventLine(250, events.OrderFired, "four-hourly", now.Add(-2*time.Hour))+
			orderFiringEventLine(290, events.OrderFired, "cleanup-cooldown", now.Add(-20*time.Minute)))
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderCompleted, Subject: "cleanup-cooldown", Ts: now.Add(-3 * time.Minute)},
	)

	var lookedUp []string
	var mu sync.Mutex
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(func(order orders.Order) (time.Time, error) {
		mu.Lock()
		defer mu.Unlock()
		lookedUp = append(lookedUp, order.ScopedName())
		return time.Time{}, nil
	}))
	var archiveReads atomic.Int32
	countOrderFiringArchiveReads(check, &archiveReads)
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusError || result.Severity != SeverityAdvisory {
		t.Fatalf("status = %v severity = %v, want advisory error; msg = %s; details = %v", result.Status, result.Severity, result.Message, result.Details)
	}
	if !strings.Contains(result.Message, "inconclusive") {
		t.Fatalf("message = %q, want it to say the result is inconclusive, not stale", result.Message)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "silent: never fired; controller start is older than the newest event archive (inconclusive)") {
		t.Fatalf("details = %v, want the never-fired order reported inconclusive", result.Details)
	}
	routine := strings.Join(check.routineDetails, "\n")
	for _, want := range []string{"cleanup-cooldown: last fired 20m ago", "four-hourly: last fired 2h ago"} {
		if !strings.Contains(routine, want) {
			t.Fatalf("routine = %v, want %q: the current orders' classification is unchanged", check.routineDetails, want)
		}
	}
	if fmt.Sprint(lookedUp) != "[silent]" {
		t.Fatalf("store lookups = %v, want [silent] only", lookedUp)
	}
	if n := archiveReads.Load(); n != 1 {
		t.Fatalf("newest-archive reads = %d, want exactly 1", n)
	}
}

// TestOrderFiringCurrent_ControllerStartIsLastInSequence: the start that dates
// a never-fired order is the last controller.started in sequence order, as
// events.LatestArchivedMatch picks it, not the greatest timestamp (pl-yhyw
// review r1, P2). A clock stepped back between two starts must not hand the
// never-fired order a first-cycle grace it no longer has.
func TestOrderFiringCurrent_ControllerStartIsLastInSequence(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "silent", "cooldown", "1h")

	writeOrderFiringArchive(t, cityPath, "20261030T113500Z", 101, 200,
		orderFiringEventLine(110, events.ControllerStarted, "", time.Date(2026, 10, 30, 11, 30, 0, 0, time.UTC))+
			orderFiringEventLine(120, events.ControllerStarted, "", time.Date(2026, 10, 30, 7, 0, 0, 0, time.UTC)))
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderCompleted, Subject: "other", Ts: now.Add(-3 * time.Minute)},
	)

	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(func(orders.Order) (time.Time, error) {
		return time.Time{}, nil
	}))
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusError || result.Severity != SeverityBlocking {
		t.Fatalf("status = %v severity = %v, want blocking error; msg = %s; details = %v", result.Status, result.Severity, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "silent: never fired since controller start 5h ago") {
		t.Fatalf("details = %v, want the start at seq 120 (07:00) to date the order", result.Details)
	}
}

// TestOrderFiringCurrent_StaleArchivedFiringStillAsksTheStore: a firing in the
// newest archive that is itself overdue is not enough evidence on its own, so
// the authoritative store lookup still decides, exactly as for a stale firing
// in the active log.
func TestOrderFiringCurrent_StaleArchivedFiringStillAsksTheStore(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringArchive(t, cityPath, "20261029T235635Z", 101, 200,
		orderFiringEventLine(190, events.OrderFired, "cleanup-cooldown", now.Add(-5*time.Hour)))
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
	)

	var calls atomic.Int32
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(func(orders.Order) (time.Time, error) {
		calls.Add(1)
		return now.Add(-10 * time.Minute), nil
	}))
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want ok; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("store lookups = %d, want 1", n)
	}
	if !strings.Contains(strings.Join(check.routineDetails, "\n"), "last fired 10m ago") {
		t.Fatalf("routine = %v, want the store's newer run to decide", check.routineDetails)
	}
}

// TestOrderFiringCurrent_NoArchiveReadWhenTheActiveLogAnswers: when the active
// tail answers every order, no archive is opened (the newest is not gzip).
func TestOrderFiringCurrent_NoArchiveReadWhenTheActiveLogAnswers(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "events.jsonl.archive-20261029T235635Z-seq-1-100.gz"), []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-10 * time.Minute)},
	)

	var calls atomic.Int32
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(orderFiringRefusingLastRun(&calls)))
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK || len(result.Details) != 0 {
		t.Fatalf("status = %v details = %v, want ok with no details: no archive needed reading", result.Status, result.Details)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("store lookups = %d, want 0", n)
	}
}

// TestOrderFiringCurrent_FullTailSkipsTheArchive: a tail that filled its limit
// came from an active log long enough that the archive behind it is older than
// what the tail missed, so the store answers instead of an archive read.
func TestOrderFiringCurrent_FullTailSkipsTheArchive(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "events.jsonl.archive-20261029T235635Z-seq-1-100.gz"), []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	evts := []events.Event{{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)}}
	for i := 0; i < orderFiringEventTailLimit; i++ {
		evts = append(evts, events.Event{Type: events.OrderFired, Subject: fmt.Sprintf("noise-%d", i%16), Ts: now.Add(-time.Minute)})
	}
	writeOrderFiringTestEvents(t, cityPath, evts...)

	var calls atomic.Int32
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(func(orders.Order) (time.Time, error) {
		calls.Add(1)
		return now.Add(-10 * time.Minute), nil
	}))
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK || len(result.Details) != 0 {
		t.Fatalf("status = %v details = %v, want ok with no details", result.Status, result.Details)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("store lookups = %d, want 1", n)
	}
}

// TestOrderFiringCurrent_UnreadableNewestArchiveFallsBackToTheStore: the
// archive read only spares store round-trips, so an archive that cannot be
// read sends the pending orders to the store as before, and says so.
func TestOrderFiringCurrent_UnreadableNewestArchiveFallsBackToTheStore(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "events.jsonl.archive-20261029T235635Z-seq-1-100.gz"), []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
	)

	var calls atomic.Int32
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(func(orders.Order) (time.Time, error) {
		calls.Add(1)
		return now.Add(-10 * time.Minute), nil
	}))
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want ok; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("store lookups = %d, want 1", n)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "newest event archive unreadable") {
		t.Fatalf("details = %v, want the unreadable archive named", result.Details)
	}
}

// countOrderFiringArchiveReads wraps the check's newest-archive reader so a
// test can assert how many times an archive was decompressed.
func countOrderFiringArchiveReads(check *OrderFiringCurrentCheck, reads *atomic.Int32) {
	read := check.readArchive
	check.readArchive = func(path string, types []string) ([]events.Event, error) {
		reads.Add(1)
		return read(path, types)
	}
}
