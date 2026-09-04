package main

// Attribution for a stop signal delivered to the dolt scope watchdog
// (ga-drkbcd ask A).
//
// WHAT IS ACHIEVABLE. On 2026-08-15 an external process sent SIGTERM to the
// scope watchdog and nothing on either side recorded who. It still cannot be
// recorded exactly: Go's os/signal delivers a bare os.Signal with no siginfo,
// so the sender's PID never reaches the program. Recovering it would need a
// SA_SIGINFO handler installed under the Go runtime's own signal machinery
// (cgo, and a fight with the runtime), or the Darwin audit/EndpointSecurity
// pipelines (root plus entitlements). Neither is worth risking the process that
// owns the town's database.
//
// WHAT WE RECORD INSTEAD. Everything about the receiving side that narrows the
// sender, captured at the instant of receipt:
//
//   - The watchdog's own identity — pid, ppid, pgid — and whether the parent is
//     still alive. This alone splits the two production shapes. A live gc
//     parent means the signal plausibly came from our own spawner; ppid 1 means
//     the spawner exited long ago (the normal production shape, since gc exits
//     and the watchdog stays), so the sender was a third party.
//   - The surviving ancestry chain, which names the process tree the watchdog
//     belongs to — a shell, a launchd job, a test harness.
//   - A bounded snapshot of the lifecycle-plausible processes running in that
//     second: anything holding gc, dolt, bd, launchd/systemd, or an explicit
//     kill/pkill/killall command line. A `pkill -f dolt` caught mid-flight in
//     this snapshot IS the attribution; catching it is worth one bounded fork.
//
// COST. The snapshot forks one `ps` with a hard timeout
// (managedDoltSignalActorSnapshotTimeout) and is taken BEFORE the child is
// terminated, because its whole value is the state at receipt. It therefore
// adds at most that timeout to a shutdown whose SIGTERM grace is already
// measured in tens of seconds. Every failure is silent and non-fatal: an
// attribution we cannot collect must never delay or prevent the stop.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// managedDoltSignalActorSnapshotTimeout hard-bounds the one ps fork taken
	// at signal receipt. Short enough to be invisible against the SIGTERM
	// grace, long enough for ps on a loaded box.
	managedDoltSignalActorSnapshotTimeout = 2 * time.Second

	// managedDoltSignalAncestryMaxDepth bounds the parent walk. Real trees are
	// three or four deep; the bound exists so a corrupted ps table with a PPID
	// cycle cannot spin the walk.
	managedDoltSignalAncestryMaxDepth = 8

	// managedDoltSignalActorMaxLines bounds how many lifecycle-plausible
	// processes are logged. The snapshot is a lead, not an inventory.
	managedDoltSignalActorMaxLines = 24

	// managedDoltSignalActorArgsMaxLen bounds each logged command line.
	managedDoltSignalActorArgsMaxLen = 200
)

// psProcessRow is one parsed row of `ps -A -o pid=,ppid=,args=`.
type psProcessRow struct {
	PID  int
	PPID int
	Args string
}

// managedDoltSignalAttribution is everything the watchdog could establish about
// a stop signal at the moment it arrived.
type managedDoltSignalAttribution struct {
	Signal      string
	ReceivedAt  time.Time
	PID         int
	PPID        int
	PGID        int
	ParentAlive bool
	Ancestry    []string
	Actors      []string
	SnapshotErr string
}

// parsePSProcessRows parses `ps -A -o pid=,ppid=,args=` output into a table
// keyed by PID. Malformed rows are skipped rather than failing the parse: a
// partial table still attributes, an empty one does not.
func parsePSProcessRows(output string) map[int]psProcessRow {
	rows := make(map[int]psProcessRow, 256)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil || ppid < 0 {
			continue
		}
		rows[pid] = psProcessRow{PID: pid, PPID: ppid, Args: strings.Join(fields[2:], " ")}
	}
	return rows
}

// managedDoltProcessAncestry renders the parent chain above pid, nearest first.
// The walk stops at PID 1, at an unknown PID, at maxDepth, or on a cycle — a ps
// table read while processes are exiting can contain any of them.
func managedDoltProcessAncestry(pid int, rows map[int]psProcessRow, maxDepth int) []string {
	if maxDepth <= 0 {
		return nil
	}
	chain := make([]string, 0, maxDepth)
	seen := map[int]bool{pid: true}
	current := pid
	for depth := 0; depth < maxDepth; depth++ {
		row, ok := rows[current]
		if !ok {
			break
		}
		parent := row.PPID
		if parent <= 0 || seen[parent] {
			break
		}
		seen[parent] = true
		parentRow, ok := rows[parent]
		if !ok {
			chain = append(chain, fmt.Sprintf("pid %d (gone or unreadable)", parent))
			break
		}
		chain = append(chain, fmt.Sprintf("pid %d %s", parent, truncateManagedDoltSignalArgs(parentRow.Args)))
		if parent == 1 {
			break
		}
		current = parent
	}
	return chain
}

// managedDoltLifecycleActorArgs reports whether a command line belongs to a
// process that plausibly stops a data plane. The list is deliberately narrow:
// a snapshot that logs everything attributes nothing.
func managedDoltLifecycleActorArgs(args string) bool {
	lower := strings.ToLower(strings.TrimSpace(args))
	if lower == "" {
		return false
	}
	// Explicit signal-sending commands: the highest-value catch. A `pkill -f
	// dolt` still on the process table when the signal lands names the sender.
	for _, token := range []string{"pkill", "killall", "/bin/kill", "kill -"} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	// Lifecycle owners: our own binaries and the init systems that supervise
	// them. Matched on the executable field only, so an unrelated process that
	// merely mentions "gc" in an argument is not swept in.
	executable := lower
	if idx := strings.IndexByte(executable, ' '); idx >= 0 {
		executable = executable[:idx]
	}
	if idx := strings.LastIndexByte(executable, '/'); idx >= 0 {
		executable = executable[idx+1:]
	}
	switch executable {
	case "gc", "dolt", "bd", "launchd", "systemd", "launchctl", "systemctl", "supervisord":
		return true
	}
	return false
}

// filterManagedDoltLifecycleActorLines renders the lifecycle-plausible rows of
// a ps table, excluding the watchdog itself and the ps we just forked, capped
// at limit. Kept pure so the selection rule is testable without a process
// table.
func filterManagedDoltLifecycleActorLines(rows map[int]psProcessRow, selfPID, limit int) []string {
	if limit <= 0 {
		return nil
	}
	pids := make([]int, 0, len(rows))
	for pid, row := range rows {
		if pid == selfPID || !managedDoltLifecycleActorArgs(row.Args) {
			continue
		}
		pids = append(pids, pid)
	}
	sortInts(pids)
	lines := make([]string, 0, min(limit, len(pids)))
	for _, pid := range pids {
		if len(lines) == limit {
			lines = append(lines, fmt.Sprintf("… %d more lifecycle processes not listed", len(pids)-limit))
			break
		}
		row := rows[pid]
		lines = append(lines, fmt.Sprintf("pid %d ppid %d %s", row.PID, row.PPID, truncateManagedDoltSignalArgs(row.Args)))
	}
	return lines
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func truncateManagedDoltSignalArgs(args string) string {
	args = strings.TrimSpace(args)
	if len(args) <= managedDoltSignalActorArgsMaxLen {
		return args
	}
	return args[:managedDoltSignalActorArgsMaxLen] + "…"
}

// managedDoltSignalProcessTable is the live `ps` read behind the snapshot. It
// is a package var so tests can supply a table without forking.
var managedDoltSignalProcessTable = func() (map[int]psProcessRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), managedDoltSignalActorSnapshotTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,args=")
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parsePSProcessRows(string(out)), nil
}

// collectManagedDoltSignalAttribution captures everything knowable about a stop
// signal at receipt. It never returns an error: an attribution we could not
// collect is recorded as such (SnapshotErr) and the stop proceeds.
func collectManagedDoltSignalAttribution(sig os.Signal, receivedAt time.Time) managedDoltSignalAttribution {
	pid := os.Getpid()
	ppid := os.Getppid()
	attribution := managedDoltSignalAttribution{
		Signal:      fmt.Sprint(sig),
		ReceivedAt:  receivedAt,
		PID:         pid,
		PPID:        ppid,
		ParentAlive: ppid > 1 && pidAlive(ppid),
	}
	if pgid, err := syscall.Getpgid(pid); err == nil {
		attribution.PGID = pgid
	}
	rows, err := managedDoltSignalProcessTable()
	if err != nil {
		attribution.SnapshotErr = err.Error()
		return attribution
	}
	attribution.Ancestry = managedDoltProcessAncestry(pid, rows, managedDoltSignalAncestryMaxDepth)
	attribution.Actors = filterManagedDoltLifecycleActorLines(rows, pid, managedDoltSignalActorMaxLines)
	return attribution
}

// formatManagedDoltSignalAttribution renders the attribution as watchdog log
// lines. Every line carries the same "gc scope watchdog: stop signal" prefix so
// the whole record can be recovered from a log with one grep — the thing the
// 2026-08-15 postmortem could not do.
func formatManagedDoltSignalAttribution(attribution managedDoltSignalAttribution) []string {
	parentState := "parent exited (reparented)"
	if attribution.ParentAlive {
		parentState = "parent alive"
	}
	lines := []string{
		fmt.Sprintf("gc scope watchdog: stop signal attribution: signal %s received at %s; watchdog pid %d ppid %d pgid %d (%s)",
			attribution.Signal,
			attribution.ReceivedAt.UTC().Format(time.RFC3339Nano),
			attribution.PID, attribution.PPID, attribution.PGID, parentState),
		"gc scope watchdog: stop signal attribution: the sending pid is NOT recoverable — os/signal delivers no siginfo; the records below narrow it",
	}
	if attribution.SnapshotErr != "" {
		return append(lines, "gc scope watchdog: stop signal attribution: process snapshot unavailable: "+attribution.SnapshotErr)
	}
	if len(attribution.Ancestry) == 0 {
		lines = append(lines, "gc scope watchdog: stop signal attribution: ancestry: none readable")
	}
	for i, entry := range attribution.Ancestry {
		lines = append(lines, fmt.Sprintf("gc scope watchdog: stop signal attribution: ancestry[%d]: %s", i, entry))
	}
	if len(attribution.Actors) == 0 {
		lines = append(lines, "gc scope watchdog: stop signal attribution: no lifecycle processes on the table at receipt")
	}
	for _, actor := range attribution.Actors {
		lines = append(lines, "gc scope watchdog: stop signal attribution: lifecycle actor: "+actor)
	}
	return lines
}
