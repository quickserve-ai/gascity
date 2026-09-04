package main

// Durable escalation for scope-watchdog data-plane stops (ga-drkbcd).
//
// WHY NOT supervisor.log, WHICH THE BEAD ASKED FOR. ~/.gc/supervisor.log is the
// supervisor process's own teed stdout/stderr (cmd_supervisor.go), machine-
// scoped rather than city-scoped, with no append helper and no reader but
// `gc supervisor logs | tail`. The watchdog is a different process with a
// different lifetime; appending to another component's rotating fd would buy
// reach into a file nothing parses.
//
// The watchdog's own dolt.log lines are worse: a repo-wide search for
// "gc scope watchdog" finds only the Fprintf sites that write them. They are
// write-only forensics. That is precisely how the 2026-08-15 outage stayed
// silent — the evidence existed and no mechanism carried it anywhere.
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

	"github.com/gastownhall/gascity/internal/emergency"
)

// managedDoltWatchdogAlarmActor is the emergency-record actor for every alarm
// raised by the scope watchdog, so the records are greppable as a class.
const managedDoltWatchdogAlarmActor = "dolt-scope-watchdog"

// managedDoltWatchdogAlarm is one escalation request.
type managedDoltWatchdogAlarm struct {
	CityPath   string
	ConfigFile string
	Severity   string
	Cause      string
	Message    string
	DoltPID    int
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
	// events.jsonl loses the live channel, not the record.
	if err := emergency.RecordSignaledToCityLog(alarm.CityPath, rec, io.Discard); err != nil {
		return spoolPath, fmt.Errorf("mirror emergency to events.jsonl: %w", err)
	}
	return spoolPath, nil
}
