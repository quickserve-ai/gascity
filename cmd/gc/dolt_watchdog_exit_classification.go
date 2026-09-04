package main

// Classification of a managed dolt sql-server exit observed by the scope
// watchdog (ga-drkbcd ask B).
//
// THE DEFECT. Every alarm in the fleet keys on error or crash, so the branch
// that mattered most on 2026-08-15 — cmd.Wait() returning nil, i.e. the server
// exited with status 0 mid-service — was the reassuring one. "exited cleanly"
// rendered a total data-plane outage as a calm line in a log nothing reads. The
// restart was the only evidence anything happened.
//
// THE FIX. A status-0 exit is split into two outcomes that no longer share a
// word:
//
//   - REQUESTED — someone asked. Either the watchdog itself was signalled
//     (SIGINT/SIGTERM forwarded to the child), or a stop-intent marker written
//     by gc's own stop path (dolt_stop_intent.go) covers this exact PID. Still
//     "exited cleanly", now with the requester named.
//   - UNEXPECTED CLEAN EXIT — nobody asked. This is an alarm: it escalates
//     through the emergency spool and .gc/events.jsonl, and the watchdog exits
//     non-zero rather than 0.
//
// The watchdog's two self-initiated termination paths (scope-gone and signal
// forward) never reach here at all — each drains `done` inline and returns —
// so the only way to arrive at a classification is an exit the watchdog did not
// itself cause. The SignalPending field closes the one remaining race: a
// simultaneous signal and child exit, where select may pick the exit first.
//
// Bias: a stop we cannot attribute alarms. The failure being replaced is
// silence, so a false alarm is the cheap direction.

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// managedDoltServerLogTailBytes bounds the tail read that recovers dolt's
	// own last words for the alarm. The last thing the server said before it
	// chose to exit — a connection burst, a shutdown notice, a memory
	// complaint — is the single most useful line a postmortem can have, and on
	// 2026-08-15 it was five lines above the "exited cleanly" that inverted the
	// conclusion. One bounded ReadAt is cheap enough to take unconditionally.
	managedDoltServerLogTailBytes = 8 * 1024

	// managedDoltServerLogLineMaxLen bounds the quoted line.
	managedDoltServerLogLineMaxLen = 400

	// managedDoltWatchdogLogPrefix marks lines the watchdog itself wrote into
	// the shared dolt.log, so the tail read can skip its own voice and quote
	// the server's.
	managedDoltWatchdogLogPrefix = "gc scope watchdog:"
)

// managedDoltWatchdogExitCause names why the watchdog's child exited.
type managedDoltWatchdogExitCause string

const (
	// managedDoltExitCauseError is a non-zero status or a signal death — the
	// case every existing alarm already covers.
	managedDoltExitCauseError managedDoltWatchdogExitCause = "error"
	// managedDoltExitCauseRequested is a status-0 exit somebody asked for.
	managedDoltExitCauseRequested managedDoltWatchdogExitCause = "requested"
	// managedDoltExitCauseUnexpectedClean is a status-0 exit nobody asked for:
	// the defect this file exists for.
	managedDoltExitCauseUnexpectedClean managedDoltWatchdogExitCause = "unexpected-clean-exit"
)

// managedDoltWatchdogChildExit is the whole evidence set available when the
// watchdog's dolt child exits. Passed as a struct so the classification stays a
// pure function of observations rather than of live process state.
type managedDoltWatchdogChildExit struct {
	PID               int
	WatchdogPID       int
	ConfigFile        string
	Uptime            time.Duration
	WaitErr           error
	SignalPending     bool
	Intent            managedDoltStopIntent
	IntentFound       bool
	Now               time.Time
	LastServerLogLine string
}

// managedDoltWatchdogExitReport is the classification plus everything the
// watchdog should say and do about it.
type managedDoltWatchdogExitReport struct {
	Cause        managedDoltWatchdogExitCause
	Alarm        bool
	ExitCode     int
	Lines        []string
	AlarmMessage string
}

// classifyManagedDoltWatchdogChildExit decides how a child exit is reported.
// Pure: no clocks, no filesystem, no processes — every input is already in
// exit, so the intentional-vs-unexpected rule is testable directly.
func classifyManagedDoltWatchdogChildExit(exit managedDoltWatchdogChildExit) managedDoltWatchdogExitReport {
	uptime := exit.Uptime.Round(time.Second)
	if exit.WaitErr != nil {
		// Non-zero status or signal death. Left on the existing wording so the
		// monitors and eyeballs that already key on "exited with error" keep
		// working; only the uptime is new.
		return managedDoltWatchdogExitReport{
			Cause:    managedDoltExitCauseError,
			ExitCode: 1,
			Lines: []string{fmt.Sprintf("%s dolt sql-server pid %d exited with error after %s: %v",
				managedDoltWatchdogLogPrefix, exit.PID, uptime, exit.WaitErr)},
		}
	}

	if requester, requested := managedDoltCleanExitRequester(exit); requested {
		// "exited cleanly" now means exactly one thing — a shutdown we asked
		// for — instead of covering both outcomes.
		return managedDoltWatchdogExitReport{
			Cause:    managedDoltExitCauseRequested,
			ExitCode: 0,
			Lines: []string{fmt.Sprintf("%s dolt sql-server pid %d exited cleanly after %s (stop requested by %s)",
				managedDoltWatchdogLogPrefix, exit.PID, uptime, requester)},
		}
	}

	alarmMessage := fmt.Sprintf(
		"managed dolt sql-server pid %d exited with status 0 after %s with no stop request from gc: the data plane is DOWN until the watchdog restarts it (watchdog pid %d, config %s)",
		exit.PID, uptime, exit.WatchdogPID, exit.ConfigFile)
	lines := []string{
		managedDoltWatchdogLogPrefix + " ALARM UNEXPECTED CLEAN EXIT: " + alarmMessage,
		// Deliberately avoids the healthy phrase itself: a monitor grepping
		// dolt.log for the reassuring wording must never match an alarm.
		fmt.Sprintf("%s ALARM UNEXPECTED CLEAN EXIT: an unrequested status-0 exit of the database is never routine; this is NOT the healthy requested-shutdown path", managedDoltWatchdogLogPrefix),
	}
	if lastLine := strings.TrimSpace(exit.LastServerLogLine); lastLine != "" {
		lines = append(lines, fmt.Sprintf("%s ALARM UNEXPECTED CLEAN EXIT: last line the server wrote before exiting: %s",
			managedDoltWatchdogLogPrefix, lastLine))
	} else {
		lines = append(lines, managedDoltWatchdogLogPrefix+" ALARM UNEXPECTED CLEAN EXIT: the server wrote nothing before exiting")
	}
	return managedDoltWatchdogExitReport{
		Cause:        managedDoltExitCauseUnexpectedClean,
		Alarm:        true,
		ExitCode:     1,
		Lines:        lines,
		AlarmMessage: alarmMessage,
	}
}

// managedDoltCleanExitRequester names who asked for a status-0 exit, or reports
// that nobody did. Two sources, in order of certainty: a signal the watchdog
// itself received (we are the requester's proxy), then a stop-intent marker
// covering this exact PID.
func managedDoltCleanExitRequester(exit managedDoltWatchdogChildExit) (string, bool) {
	if exit.SignalPending {
		return "a stop signal delivered to this watchdog", true
	}
	if exit.IntentFound && managedDoltStopIntentCovers(exit.Intent, exit.PID, exit.Now) {
		return describeManagedDoltStopIntent(exit.Intent), true
	}
	return "", false
}

// lastManagedDoltServerLogLine returns the last line in tail that the server
// itself wrote, skipping the watchdog's own lines. The watchdog and the server
// share one log file, so without the skip the "last line before exit" would
// usually be the watchdog's own startup banner.
func lastManagedDoltServerLogLine(tail string) string {
	lines := strings.Split(tail, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, managedDoltWatchdogLogPrefix) {
			continue
		}
		if len(line) > managedDoltServerLogLineMaxLen {
			return line[:managedDoltServerLogLineMaxLen] + "…"
		}
		return line
	}
	return ""
}

// readManagedDoltServerLogTail reads the last managedDoltServerLogTailBytes of
// path. Best-effort: an unreadable log costs the alarm one detail, never the
// alarm itself. The first line of the tail is dropped because a byte-offset
// read almost always lands mid-line.
func readManagedDoltServerLogTail(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close() //nolint:errcheck // read-only tail
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	offset := int64(0)
	length := size
	if size > managedDoltServerLogTailBytes {
		offset = size - managedDoltServerLogTailBytes
		length = managedDoltServerLogTailBytes
	}
	buf := make([]byte, length)
	n, err := file.ReadAt(buf, offset)
	if n == 0 && err != nil {
		return ""
	}
	tail := string(buf[:n])
	if offset > 0 {
		if idx := strings.IndexByte(tail, '\n'); idx >= 0 {
			tail = tail[idx+1:]
		}
	}
	return tail
}
