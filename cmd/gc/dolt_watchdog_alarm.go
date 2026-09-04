package main

// Durable escalation for scope-watchdog data-plane stops (ga-drkbcd).
//
// SUPERVISOR.LOG, WHICH THE BEAD ASKED FOR. ~/.gc/supervisor.log is machine-
// scoped rather than city-scoped and is rotated by the supervisor's own start
// path, so the watchdog must never open it by NAME: an appender holding the old
// fd across a rotation writes into the archived inode. What it can safely have
// is the fd its spawner already holds. Under the installed service the
// supervisor's own stdout/stderr ARE that file (launchd StandardOutPath /
// StandardErrorPath, systemd StandardOutput=append:), the CityRuntime that
// starts managed dolt runs inside the supervisor process, and rotation is a
// rename — so a duplicate of that fd keeps pointing at the same open file the
// supervisor is writing, with no name lookup and no second opener.
//
// The watchdog's own two standard streams cannot carry it: its stderr is
// redirected into dolt.log at spawn, and its stdout is the PID-handshake pipe
// the spawner closes as soon as the handshake is read (writing there later
// risks EPIPE on fd 1, which kills the process holding the town's database).
// So the spawner passes the channel explicitly as an extra inherited fd, and
// only when that fd is a REGULAR FILE — never a pipe or a tty, because the
// watchdog outlives its spawner and a pipe write end it held open would wedge
// whoever is reading the other end.
//
// This is a summary line, not the record: the durable record is the emergency
// spool below. dolt.log keeps the full detail.
//
// WHY A SUMMARY IS NOT ENOUGH ON ITS OWN. The watchdog's dolt.log lines reach
// nobody: a repo-wide search for "gc scope watchdog" finds only the Fprintf
// sites that write them. They are write-only forensics — precisely how the
// 2026-08-15 outage stayed silent, with the evidence on disk and no mechanism
// carrying it anywhere. The supervisor-log summary above is a second copy for
// the plane an operator already watches, never the record itself.
//
// WHAT IS ACTUALLY READ. The city event log, .gc/events.jsonl, and the
// dolt-independent emergency spool that feeds it (internal/emergency). The
// spool is purpose-built for a process that cannot assume dolt or the
// controller is alive: WriteSpool is an atomic 0600 file under
// .gc/emergency/, and RecordSignaledToCityLog opens its own flock-guarded
// FileRecorder and appends an emergency.signaled event, with no config load, no
// supervisor and no database. emergency.Record is already a registered event
// payload, so this adds no new CI surface.
//
// Every step is best-effort. An escalation that fails must never delay or
// prevent the data-plane stop it is reporting; the caller logs the failure to
// dolt.log and carries on.

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/emergency"
)

// managedDoltWatchdogAlarmActor is the emergency-record actor for every alarm
// raised by the scope watchdog, so the records are greppable as a class.
const managedDoltWatchdogAlarmActor = "dolt-scope-watchdog"

const (
	// managedDoltWatchdogSupervisorFDEnv marks the extra inherited fd the
	// spawner handed the watchdog for supervisor-plane escalation summaries.
	// The declaration is what makes adopting the fd safe: a watchdog started
	// any other way (a hand-run re-exec) may have an unrelated fd 3, and must
	// not write an alarm into it.
	managedDoltWatchdogSupervisorFDEnv = "GC_DOLT_WATCHDOG_SUPERVISOR_FD"

	// managedDoltWatchdogSupervisorFD is where exec places the first entry of
	// cmd.ExtraFiles: 0, 1 and 2 are the child's standard streams.
	managedDoltWatchdogSupervisorFD = 3
)

// managedDoltWatchdogSupervisorChannel is the adopted escalation channel inside
// the watchdog process, or nil when it was handed none. Package-level because
// the alarm sites are reached from the supervise loop with no plumbing between
// them, and because a test needs to install one.
var managedDoltWatchdogSupervisorChannel *os.File

// managedDoltWatchdogSupervisorChannelForSpawn returns the file a spawner may
// hand the watchdog as its escalation channel, or nil. Only a regular file
// qualifies — see the file header: a pipe or tty would tie the spawner's
// readers (or an operator's terminal) to the watchdog's whole lifetime.
func managedDoltWatchdogSupervisorChannelForSpawn(stderr *os.File) *os.File {
	if stderr == nil {
		return nil
	}
	info, err := stderr.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	return stderr
}

// adoptManagedDoltWatchdogSupervisorChannel picks up the inherited escalation
// fd inside the re-exec'd watchdog. The fd is re-checked with a raw fstat
// BEFORE any *os.File wraps it, so a declaration that does not match reality
// yields no channel — and no wrapper whose finalizer would close a descriptor
// this process does not own. It is then close-on-exec'd so the dolt sql-server
// the watchdog spawns never inherits a handle on the supervisor's log.
func adoptManagedDoltWatchdogSupervisorChannel(env string) *os.File {
	fd, err := strconv.Atoi(strings.TrimSpace(env))
	if err != nil || fd < managedDoltWatchdogSupervisorFD {
		return nil
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return nil
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "gc-supervisor-escalation")
}

// writeManagedDoltWatchdogSupervisorSummary puts one line about a data-plane
// stop where the supervisor plane captures it. Best-effort and never fatal: an
// escalation summary that cannot be written must not disturb the stop it
// reports, and the durable record is the emergency spool regardless.
func writeManagedDoltWatchdogSupervisorSummary(line string) {
	if managedDoltWatchdogSupervisorChannel == nil || strings.TrimSpace(line) == "" {
		return
	}
	fmt.Fprintf(managedDoltWatchdogSupervisorChannel, "%s %s\n", time.Now().UTC().Format(time.RFC3339), line) //nolint:errcheck // best-effort escalation
}

// managedDoltWatchdogAlarm is one escalation request.
type managedDoltWatchdogAlarm struct {
	CityPath   string
	ConfigFile string
	Severity   string
	Cause      string
	Message    string
	DoltPID    int
	// Diagnostics receives the events.jsonl recorder's own complaints. The
	// FileRecorder reports flock timeouts and append failures to this writer
	// instead of returning them, so discarding it makes a LOST live alarm
	// indistinguishable from a delivered one — the same failure shape as the
	// silence this whole mechanism exists to end. Callers point it at dolt.log.
	// nil discards, for callers that have nowhere to put it.
	Diagnostics io.Writer
}

// escalateManagedDoltWatchdogAlarm writes the alarm to the city emergency spool
// and mirrors it into .gc/events.jsonl. It reports the spool path it wrote, or
// an error explaining why the alarm could not be made durable — which the
// caller logs rather than acts on.
//
// A blank city path disables escalation: the watchdog is then running outside a
// city (the test harness spawns it that way), and there is no spool to write
// to. Reported as an error so the reason still reaches the log.
func escalateManagedDoltWatchdogAlarm(alarm managedDoltWatchdogAlarm) (string, error) {
	if strings.TrimSpace(alarm.CityPath) == "" {
		return "", fmt.Errorf("no city path: emergency escalation unavailable")
	}
	hostname, _ := os.Hostname()
	metadata := map[string]string{
		"component": "managed-dolt",
		"cause":     alarm.Cause,
	}
	if alarm.DoltPID > 0 {
		metadata["dolt_pid"] = strconv.Itoa(alarm.DoltPID)
	}
	if configFile := strings.TrimSpace(alarm.ConfigFile); configFile != "" {
		metadata["dolt_config"] = configFile
	}
	rec, err := emergency.NewRecord(emergency.RecordOptions{
		Severity:   alarm.Severity,
		Actor:      managedDoltWatchdogAlarmActor,
		Message:    alarm.Message,
		SourcePath: alarm.ConfigFile,
		SourcePID:  os.Getpid(),
		Hostname:   hostname,
		Metadata:   metadata,
	})
	if err != nil {
		return "", fmt.Errorf("build emergency record: %w", err)
	}
	spoolPath, err := emergency.WriteSpool(alarm.CityPath, rec)
	if err != nil {
		return "", fmt.Errorf("write emergency spool: %w", err)
	}
	// The spool file is already durable at this point; a failure to mirror into
	// events.jsonl loses the live channel, not the record — but it must SAY so.
	diagnostics := alarm.Diagnostics
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	if err := emergency.RecordSignaledToCityLog(alarm.CityPath, rec, diagnostics); err != nil {
		return spoolPath, fmt.Errorf("mirror emergency to events.jsonl: %w", err)
	}
	return spoolPath, nil
}
