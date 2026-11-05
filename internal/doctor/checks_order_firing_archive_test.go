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
	if !strings.Contains(details, "silent: never fired; no controller start in the active event log or the newest event archive (inconclusive)") {
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

// TestOrderFiringCurrent_MissingStartIsInconclusiveAtAnyArchiveCount: the
// check inspects only the active log and the newest archive, so when neither
// holds a controller.started the start is unknown, whatever the archive count,
// and a never-fired order whose store answers zero is inconclusive (advisory)
// (pl-yhyw review r2). With zero or one archive the two sources are every
// retained log, but a start found in neither is still not observed: the
// controller may have started before them, or never. The one exception is the
// row with no archive and no firing: nothing shows the controller ever ran, so
// the order stays OK and controller-down is left to its own doctor finding.
func TestOrderFiringCurrent_MissingStartIsInconclusiveAtAnyArchiveCount(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		archives int
		fired    bool // another order's firing in the active log
		neverRan bool // nothing shows the controller ever ran
	}{
		{"no archive, firing in active log", 0, true, false},
		{"no archive, no firing", 0, false, true},
		{"one archive, firing in active log", 1, true, false},
		{"one archive, no firing", 1, false, false},
		{"several archives, firing in active log", 3, true, false},
		{"several archives, no firing", 3, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := orderFiringTestCity(t)
			writeOrderFiringTestOrder(t, cityPath, "silent", "cooldown", "1h")
			for i := 0; i < tc.archives; i++ {
				first := uint64(100*i + 1)
				stamp := now.Add(time.Duration(i-tc.archives) * time.Hour).Format("20060102T150405Z")
				writeOrderFiringArchive(t, cityPath, stamp, first, first+99,
					orderFiringEventLine(first+10, events.OrderCompleted, "other", now.Add(time.Duration(i-tc.archives)*time.Hour)))
			}
			active := events.Event{Type: events.OrderCompleted, Subject: "other", Ts: now.Add(-3 * time.Minute)}
			if tc.fired {
				active.Type = events.OrderFired
			}
			writeOrderFiringTestEvents(t, cityPath, active)

			check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(func(orders.Order) (time.Time, error) {
				return time.Time{}, nil
			}))
			check.clock = func() time.Time { return now }
			result := check.Run(&CheckContext{CityPath: cityPath})

			if tc.neverRan {
				if result.Status != StatusOK {
					t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
				}
				if !strings.Contains(strings.Join(result.Details, "\n"), "silent: never fired (controller start unknown)") {
					t.Fatalf("details = %v, want controller-start-unknown message", result.Details)
				}
				return
			}
			if result.Status != StatusError || result.Severity != SeverityAdvisory {
				t.Fatalf("status = %v severity = %v, want advisory error; msg = %s; details = %v", result.Status, result.Severity, result.Message, result.Details)
			}
			if !strings.Contains(result.Message, "inconclusive") {
				t.Fatalf("message = %q, want it to say the result is inconclusive", result.Message)
			}
			if !strings.Contains(strings.Join(result.Details, "\n"), "silent: never fired; no controller start in the active event log or the newest event archive (inconclusive)") {
				t.Fatalf("details = %v, want the never-fired order reported inconclusive", result.Details)
			}
		})
	}
}

// TestOrderFiringCurrent_UnreadableNewestArchiveWhenStartNeededIsAnError: a
// never-fired order needs the controller start, the active log has none, and
// the newest archive cannot be read. The check fails as it did before the
// bound rather than guessing the start.
func TestOrderFiringCurrent_UnreadableNewestArchiveWhenStartNeededIsAnError(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "silent", "cooldown", "1h")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "events.jsonl.archive-20261029T235635Z-seq-1-100.gz"), []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Subject: "other", Ts: now.Add(-3 * time.Minute)},
	)

	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(func(orders.Order) (time.Time, error) {
		return time.Time{}, nil
	}))
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusError || result.Severity != SeverityBlocking {
		t.Fatalf("status = %v severity = %v, want blocking error; msg = %s", result.Status, result.Severity, result.Message)
	}
	if !strings.HasPrefix(result.Message, "read controller start events:") {
		t.Fatalf("message = %q, want the controller-start read error", result.Message)
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

// fullTailOrderFiringEvents is an active log whose order.fired tail fills its
// limit with recent noise, with head firings older than that tail before it:
// the shape of a busy city's log a few hours after a rotation (pl-tfwh).
func fullTailOrderFiringEvents(now time.Time, head ...events.Event) []events.Event {
	evts := []events.Event{{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)}}
	evts = append(evts, head...)
	for i := 0; i < orderFiringEventTailLimit+500; i++ {
		evts = append(evts, events.Event{Type: events.OrderFired, Subject: fmt.Sprintf("noise-%d", i%16), Ts: now.Add(-time.Minute)})
	}
	return evts
}

// orderFiringNamedLastRun answers every store lookup with a fresh run and
// records which orders asked.
func orderFiringNamedLastRun(now time.Time, mu *sync.Mutex, asked *[]string) OrderFiringCurrentLastRunFunc {
	return func(order orders.Order) (time.Time, error) {
		mu.Lock()
		*asked = append(*asked, order.ScopedName())
		mu.Unlock()
		return now.Add(-10 * time.Minute), nil
	}
}

// TestOrderFiringCurrent_FullTailReadsTheWholeActiveLogNotTheStore is the
// shape that timed the check out from about 14:55Z on 2026-11-03 until the next
// rotation (pl-tfwh): long-interval orders fired early in the active log, then
// more than a full tail of other firings came after them. Their firings must
// come from one read of the whole active log, with no store round-trip, and
// within a budget a store fan-out cannot meet. The archive is not gzip, so the
// check must not need it either.
func TestOrderFiringCurrent_FullTailReadsTheWholeActiveLogNotTheStore(t *testing.T) {
	now := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "six-hourly", "cron", "0 */6 * * *")
	writeOrderFiringTestOrder(t, cityPath, "slow-cooldown", "cooldown", "4h")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "events.jsonl.archive-20261103T120922Z-seq-1-100.gz"), []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	evts := fullTailOrderFiringEvents(now,
		events.Event{Type: events.OrderFired, Subject: "six-hourly", Ts: now.Add(-5 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "slow-cooldown", Ts: now.Add(-3 * time.Hour)},
	)
	// Bulk of a real log: lines that are not firings, which the read must pass
	// over without decoding (2026-11-03: 50k lines, 7.8k of them firings).
	for i := 0; i < 40000; i++ {
		evts = append(evts, events.Event{Type: "bead.updated", Subject: fmt.Sprintf("ga-%d", i), Ts: now.Add(-time.Minute)})
	}
	// A recent controller start, so the budget times the firing reads rather
	// than the backward walk for a start (every order here has a firing).
	evts = append(evts, events.Event{Type: events.ControllerStarted, Ts: now.Add(-30 * time.Second)})
	writeOrderFiringTestEvents(t, cityPath, evts...)

	var calls atomic.Int32
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(orderFiringRefusingLastRun(&calls)))
	check.clock = func() time.Time { return now }
	start := time.Now()
	result := check.Run(&CheckContext{CityPath: cityPath})
	elapsed := time.Since(start)

	if result.Status != StatusOK || len(result.Details) != 0 {
		t.Fatalf("status = %v details = %v, want ok with no details; msg = %s", result.Status, result.Details, result.Message)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("store lookups = %d, want 0: the active log holds every firing", n)
	}
	routine := strings.Join(check.routineDetails, "\n")
	for _, want := range []string{"six-hourly: last fired 5h ago", "slow-cooldown: last fired 3h ago"} {
		if !strings.Contains(routine, want) {
			t.Fatalf("routine = %v, want %q", check.routineDetails, want)
		}
	}
	if budget := time.Second; elapsed > budget {
		t.Fatalf("check took %s on a full tail, want under %s", elapsed, budget)
	}
}

// TestOrderFiringCurrent_FullTailThenNewestArchiveThenTheStore: once the whole
// active log has been read, an order fired only before the rotation is answered
// by the newest archive, and only an order in neither asks the store.
func TestOrderFiringCurrent_FullTailThenNewestArchiveThenTheStore(t *testing.T) {
	now := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "archived-daily", "cron", "0 4 * * *")
	writeOrderFiringTestOrder(t, cityPath, "weekly", "cron", "0 14 * * 4")
	writeOrderFiringArchive(t, cityPath, "20261103T120922Z", 1, 100,
		orderFiringEventLine(50, events.OrderFired, "archived-daily", now.Add(-19*time.Hour)))
	writeOrderFiringTestEvents(t, cityPath, fullTailOrderFiringEvents(now)...)

	var mu sync.Mutex
	var asked []string
	var archiveReads, activeReads atomic.Int32
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(orderFiringNamedLastRun(now, &mu, &asked)))
	countOrderFiringArchiveReads(check, &archiveReads)
	check.readActive = func(path string) ([]events.Event, error) {
		if archiveReads.Load() != 0 {
			t.Errorf("the newest archive was read before the whole active log")
		}
		activeReads.Add(1)
		return readActiveOrderFirings(path)
	}
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if n := activeReads.Load(); n != 1 {
		t.Fatalf("whole-active-log reads = %d, want 1", n)
	}

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want ok; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if strings.Join(asked, ",") != "weekly" {
		t.Fatalf("store lookups = %v, want only [weekly]: archived-daily is in the newest archive", asked)
	}
	if n := archiveReads.Load(); n != 1 {
		t.Fatalf("newest-archive reads = %d, want 1", n)
	}
	if !strings.Contains(strings.Join(check.routineDetails, "\n"), "archived-daily: last fired 19h ago") {
		t.Fatalf("routine = %v, want archived-daily dated by the archive", check.routineDetails)
	}
}

// TestOrderFiringCurrent_FullTailUnreadableActiveLogFallsBackToTheStore: the
// whole-log read only spares store round-trips. If it fails, the pending
// orders go to the store as before, the archive behind the log is not read
// (what the tail missed lies in the active log), and the detail says why.
func TestOrderFiringCurrent_FullTailUnreadableActiveLogFallsBackToTheStore(t *testing.T) {
	now := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "events.jsonl.archive-20261029T235635Z-seq-1-100.gz"), []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOrderFiringTestEvents(t, cityPath, fullTailOrderFiringEvents(now)...)

	var mu sync.Mutex
	var asked []string
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(orderFiringNamedLastRun(now, &mu, &asked)))
	check.readActive = func(string) ([]events.Event, error) { return nil, fmt.Errorf("scanning events: boom") }
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want ok; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if strings.Join(asked, ",") != "cleanup-cooldown" {
		t.Fatalf("store lookups = %v, want [cleanup-cooldown]", asked)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "active event log unreadable past its tail") {
		t.Fatalf("details = %v, want the unreadable active log named", result.Details)
	}
	if strings.Contains(details, "newest event archive unreadable") {
		t.Fatalf("details = %v: the archive was read although the active log was not read whole", result.Details)
	}
}

// TestOrderFiringCurrent_FullTailKeepsTheTailAcrossARotation: a rotation
// between the tail read and the whole-log read leaves the reread a young log
// without the firings the tail saw. Those firings must still count: the order
// the tail dated asks no store, and only the truly pending one does.
func TestOrderFiringCurrent_FullTailKeepsTheTailAcrossARotation(t *testing.T) {
	now := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "noise-0", "cooldown", "1h")
	writeOrderFiringTestOrder(t, cityPath, "weekly", "cron", "0 14 * * 4")
	writeOrderFiringTestEvents(t, cityPath, fullTailOrderFiringEvents(now)...)

	var mu sync.Mutex
	var asked []string
	check := NewOrderFiringCurrentCheck(cfg, cityPath, WithOrderFiringCurrentLastRunFunc(orderFiringNamedLastRun(now, &mu, &asked)))
	// The reread lands on the fresh log a rotation left: nothing in it yet.
	check.readActive = func(string) ([]events.Event, error) { return nil, nil }
	check.clock = func() time.Time { return now }
	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want ok; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if strings.Join(asked, ",") != "weekly" {
		t.Fatalf("store lookups = %v, want only [weekly]: noise-0 fired in the tail", asked)
	}
}

// TestReadActiveOrderFiringsWithoutAnActiveLogReadsNoArchive: with no active
// log at all, the read is empty; it does not hand back the newest archive's
// firings as the active log's.
func TestReadActiveOrderFiringsWithoutAnActiveLogReadsNoArchive(t *testing.T) {
	now := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)
	cityPath, _ := orderFiringTestCity(t)
	writeOrderFiringArchive(t, cityPath, "20261103T120922Z", 1, 100,
		orderFiringEventLine(50, events.OrderFired, "archived", now.Add(-time.Hour)))

	got, err := readActiveOrderFirings(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("readActiveOrderFirings: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want no events: there is no active log", got)
	}
}

// TestReadActiveOrderFiringsReadsNoArchive: the whole-log read folds the active
// log's firings to the newest per order and never opens an archive (the one
// here is not gzip, so opening it would fail the read).
func TestReadActiveOrderFiringsReadsNoArchive(t *testing.T) {
	now := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)
	cityPath, _ := orderFiringTestCity(t)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "events.jsonl.archive-20261029T235635Z-seq-1-100.gz"), []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFired, Subject: "a", Ts: now.Add(-3 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "b", Ts: now.Add(-2 * time.Hour)},
		events.Event{Type: events.OrderCompleted, Subject: "a", Ts: now.Add(-90 * time.Minute)},
		events.Event{Type: events.OrderFired, Subject: "a", Ts: now.Add(-time.Hour)},
	)

	got, err := readActiveOrderFirings(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("readActiveOrderFirings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (newest firing per order): %+v", len(got), got)
	}
	for _, e := range got {
		if e.Type != events.OrderFired {
			t.Fatalf("got a %q event, want only order.fired", e.Type)
		}
		want := map[string]time.Time{"a": now.Add(-time.Hour), "b": now.Add(-2 * time.Hour)}[e.Subject]
		if !e.Ts.Equal(want) {
			t.Fatalf("%s: ts = %s, want %s", e.Subject, e.Ts, want)
		}
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
	// Details print only in verbose mode; the summary line must carry it too.
	if !strings.Contains(result.Message, "newest event archive unreadable") {
		t.Fatalf("message = %q, want the unreadable archive named outside verbose details", result.Message)
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
