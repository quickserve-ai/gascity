package main

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParsePSProcessRows(t *testing.T) {
	output := "" +
		"  17490     1 /usr/local/bin/gc __gc-managed-dolt-scope-watchdog /city/dolt-config.yaml\n" +
		"  17493 17490 dolt sql-server --config /city/dolt-config.yaml\n" +
		"      1     0 /sbin/launchd\n" +
		"garbage row\n" +
		"  abc   def   nonsense\n" +
		"\n"
	rows := parsePSProcessRows(output)
	if len(rows) != 3 {
		t.Fatalf("parsed %d rows, want 3: %+v", len(rows), rows)
	}
	if rows[17493].PPID != 17490 {
		t.Errorf("dolt ppid = %d, want 17490", rows[17493].PPID)
	}
	if !strings.Contains(rows[17493].Args, "dolt sql-server") {
		t.Errorf("dolt args = %q", rows[17493].Args)
	}
	if rows[1].PPID != 0 {
		t.Errorf("launchd ppid = %d, want 0", rows[1].PPID)
	}
}

func TestManagedDoltProcessAncestry(t *testing.T) {
	rows := map[int]psProcessRow{
		17493: {PID: 17493, PPID: 17490, Args: "dolt sql-server --config /city/dolt-config.yaml"},
		17490: {PID: 17490, PPID: 900, Args: "gc __gc-managed-dolt-scope-watchdog"},
		900:   {PID: 900, PPID: 1, Args: "gc supervisor run"},
		1:     {PID: 1, PPID: 0, Args: "/sbin/launchd"},
	}

	chain := managedDoltProcessAncestry(17493, rows, managedDoltSignalAncestryMaxDepth)
	if len(chain) != 3 {
		t.Fatalf("ancestry = %v, want 3 entries", chain)
	}
	if !strings.Contains(chain[0], "17490") || !strings.Contains(chain[2], "launchd") {
		t.Errorf("ancestry did not walk to launchd: %v", chain)
	}

	// An orphaned watchdog — the production shape, since the spawning gc exits
	// while the watchdog stays — reports a one-hop chain to PID 1.
	orphan := managedDoltProcessAncestry(17490, map[int]psProcessRow{
		17490: {PID: 17490, PPID: 1, Args: "gc __gc-managed-dolt-scope-watchdog"},
		1:     {PID: 1, PPID: 0, Args: "/sbin/launchd"},
	}, managedDoltSignalAncestryMaxDepth)
	if len(orphan) != 1 || !strings.Contains(orphan[0], "launchd") {
		t.Errorf("orphan ancestry = %v, want a single launchd entry", orphan)
	}

	// A ps table read mid-teardown can name a parent that is already gone.
	gone := managedDoltProcessAncestry(17493, map[int]psProcessRow{
		17493: {PID: 17493, PPID: 17490, Args: "dolt sql-server"},
	}, managedDoltSignalAncestryMaxDepth)
	if len(gone) != 1 || !strings.Contains(gone[0], "gone or unreadable") {
		t.Errorf("missing-parent ancestry = %v", gone)
	}

	// A PPID cycle must terminate rather than spin.
	cycle := managedDoltProcessAncestry(10, map[int]psProcessRow{
		10: {PID: 10, PPID: 11, Args: "a"},
		11: {PID: 11, PPID: 10, Args: "b"},
	}, managedDoltSignalAncestryMaxDepth)
	if len(cycle) != 1 {
		t.Errorf("cyclic ancestry = %v, want a single entry before the cycle is cut", cycle)
	}

	if got := managedDoltProcessAncestry(10, rows, 0); got != nil {
		t.Errorf("zero depth ancestry = %v, want nil", got)
	}
}

func TestManagedDoltLifecycleActorArgs(t *testing.T) {
	cases := []struct {
		args string
		want bool
	}{
		{"dolt sql-server --config /city/dolt-config.yaml", true},
		{"/usr/local/bin/gc supervisor run", true},
		{"/sbin/launchd", true},
		{"pkill -f dolt", true},
		{"killall dolt", true},
		{"/bin/kill -TERM 17490", true},
		{"bd list --status open", true},
		{"/usr/bin/systemd --user", true},
		{"node /app/server.js", false},
		{"", false},
		{"   ", false},
		// The executable-field rule: a process that merely MENTIONS gc in an
		// argument is not a lifecycle actor.
		{"grep -r gc /tmp", false},
		{"vim dolt-config.yaml", false},
	}
	for _, tc := range cases {
		t.Run(tc.args, func(t *testing.T) {
			if got := managedDoltLifecycleActorArgs(tc.args); got != tc.want {
				t.Errorf("managedDoltLifecycleActorArgs(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestFilterManagedDoltLifecycleActorLines(t *testing.T) {
	rows := map[int]psProcessRow{
		17490: {PID: 17490, PPID: 1, Args: "gc __gc-managed-dolt-scope-watchdog"},
		17493: {PID: 17493, PPID: 17490, Args: "dolt sql-server --config /city/dolt-config.yaml"},
		20000: {PID: 20000, PPID: 999, Args: "pkill -f dolt"},
		30000: {PID: 30000, PPID: 999, Args: "node /app/server.js"},
	}

	lines := filterManagedDoltLifecycleActorLines(rows, 17490, managedDoltSignalActorMaxLines)
	joined := strings.Join(lines, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "pid 17490 ") {
			t.Errorf("the watchdog listed itself as an actor:\n%s", joined)
		}
	}
	if strings.Contains(joined, "node /app/server.js") {
		t.Errorf("an unrelated process was listed as a lifecycle actor:\n%s", joined)
	}
	if !strings.Contains(joined, "pkill -f dolt") {
		t.Errorf("the signal-sending command was not captured:\n%s", joined)
	}
	if len(lines) != 2 {
		t.Errorf("listed %d actors, want 2:\n%s", len(lines), joined)
	}
	// Rows come out in PID order so two reads of the same table render alike.
	if !strings.HasPrefix(lines[0], "pid 17493") {
		t.Errorf("actors are not PID-ordered: %v", lines)
	}

	if got := filterManagedDoltLifecycleActorLines(rows, 0, 0); got != nil {
		t.Errorf("zero limit = %v, want nil", got)
	}

	capped := filterManagedDoltLifecycleActorLines(rows, 0, 1)
	if len(capped) != 2 || !strings.Contains(capped[1], "more lifecycle processes not listed") {
		t.Errorf("cap did not report the omission: %v", capped)
	}
}

func TestCollectManagedDoltSignalAttributionRecordsSelfIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	old := managedDoltSignalProcessTable
	managedDoltSignalProcessTable = func() (map[int]psProcessRow, error) {
		return map[int]psProcessRow{
			os.Getpid(): {PID: os.Getpid(), PPID: os.Getppid(), Args: "gc __gc-managed-dolt-scope-watchdog"},
			20000:       {PID: 20000, PPID: 1, Args: "pkill -f dolt"},
		}, nil
	}
	t.Cleanup(func() { managedDoltSignalProcessTable = old })

	receivedAt := time.Date(2026, 8, 15, 17, 42, 42, 0, time.UTC)
	attribution := collectManagedDoltSignalAttribution(syscall.SIGTERM, receivedAt)
	if attribution.PID != os.Getpid() {
		t.Errorf("attribution pid = %d, want %d", attribution.PID, os.Getpid())
	}
	if attribution.PPID != os.Getppid() {
		t.Errorf("attribution ppid = %d, want %d", attribution.PPID, os.Getppid())
	}
	if attribution.Signal != "terminated" {
		t.Errorf("attribution signal = %q, want %q", attribution.Signal, "terminated")
	}

	lines := strings.Join(formatManagedDoltSignalAttribution(attribution), "\n")
	for _, want := range []string{
		"stop signal attribution",
		"signal terminated received at 2026-08-15T17:42:42Z",
		"the sending pid is NOT recoverable",
		"pkill -f dolt",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("attribution lines missing %q:\n%s", want, lines)
		}
	}
}

func TestFormatManagedDoltSignalAttributionReportsSnapshotFailure(t *testing.T) {
	old := managedDoltSignalProcessTable
	managedDoltSignalProcessTable = func() (map[int]psProcessRow, error) {
		return nil, errors.New("ps timed out")
	}
	t.Cleanup(func() { managedDoltSignalProcessTable = old })

	attribution := collectManagedDoltSignalAttribution(syscall.SIGTERM, time.Now())
	lines := strings.Join(formatManagedDoltSignalAttribution(attribution), "\n")
	if !strings.Contains(lines, "process snapshot unavailable: ps timed out") {
		t.Errorf("a failed snapshot was not reported:\n%s", lines)
	}
	// A snapshot we could not take must not be silently rendered as "nothing
	// was running" — that would read as evidence.
	if strings.Contains(lines, "no lifecycle processes on the table") {
		t.Errorf("a failed snapshot rendered as an empty process table:\n%s", lines)
	}
}

func TestFormatManagedDoltSignalAttributionReportsEmptyTable(t *testing.T) {
	lines := strings.Join(formatManagedDoltSignalAttribution(managedDoltSignalAttribution{
		Signal: "terminated", ReceivedAt: time.Now(), PID: 17490, PPID: 1,
	}), "\n")
	for _, want := range []string{"ancestry: none readable", "no lifecycle processes on the table at receipt", "parent exited (reparented)"} {
		if !strings.Contains(lines, want) {
			t.Errorf("attribution lines missing %q:\n%s", want, lines)
		}
	}
}

func TestManagedDoltSignalProcessTableReadsTheLivePS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process table required")
	}
	rows, err := managedDoltSignalProcessTable()
	if err != nil {
		t.Skipf("ps unavailable in this environment: %v", err)
	}
	if _, ok := rows[os.Getpid()]; !ok {
		t.Errorf("the live ps snapshot does not contain this test process (pid %d); %d rows parsed", os.Getpid(), len(rows))
	}
}

func TestSortInts(t *testing.T) {
	values := []int{5, 1, 4, 1, 3}
	sortInts(values)
	for i := 1; i < len(values); i++ {
		if values[i-1] > values[i] {
			t.Fatalf("sortInts left %v unsorted", values)
		}
	}
	sortInts(nil)
}

func TestTruncateManagedDoltSignalArgs(t *testing.T) {
	long := strings.Repeat("a", managedDoltSignalActorArgsMaxLen+40)
	got := truncateManagedDoltSignalArgs(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("long args were not truncated: %d bytes", len(got))
	}
	if got := truncateManagedDoltSignalArgs("  dolt sql-server  "); got != "dolt sql-server" {
		t.Errorf("truncate trimmed args = %q", got)
	}
}
