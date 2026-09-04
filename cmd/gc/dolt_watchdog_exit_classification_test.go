package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClassifyManagedDoltWatchdogChildExit is the ga-drkbcd regression. The
// 2026-08-15 outage was silent because a status-0 exit nobody asked for and a
// shutdown gc requested produced the same reassuring line. These cases pin the
// separation in both directions: an unrequested clean exit must alarm, and
// every requested stop must not.
func TestClassifyManagedDoltWatchdogChildExit(t *testing.T) {
	now := time.Date(2026, 8, 15, 17, 18, 5, 0, time.UTC)
	freshIntent := managedDoltStopIntent{
		PID:          17493,
		RequestedAt:  now.Add(-3 * time.Second).Format(time.RFC3339Nano),
		RequesterPID: 900,
		Requester:    "gc stop --city /city",
		Reason:       "gc managed dolt stop",
	}

	cases := []struct {
		name        string
		exit        managedDoltWatchdogChildExit
		wantCause   managedDoltWatchdogExitCause
		wantAlarm   bool
		wantExit    int
		wantContain []string
		wantAbsent  []string
	}{
		{
			name: "unrequested status-0 exit alarms",
			exit: managedDoltWatchdogChildExit{
				PID: 17493, WatchdogPID: 17490, ConfigFile: "/city/dolt-config.yaml",
				Uptime: 96 * time.Hour, Now: now,
				LastServerLogLine: "INFO client connections: 3370000",
			},
			wantCause: managedDoltExitCauseUnexpectedClean,
			wantAlarm: true,
			wantExit:  1,
			wantContain: []string{
				"ALARM UNEXPECTED CLEAN EXIT",
				"pid 17493",
				"status 0",
				"no stop request from gc",
				"watchdog pid 17490",
				"/city/dolt-config.yaml",
				"96h0m0s",
				"INFO client connections: 3370000",
			},
		},
		{
			name: "a stop request covering the pid is not an alarm",
			exit: managedDoltWatchdogChildExit{
				PID: 17493, WatchdogPID: 17490, Uptime: time.Minute, Now: now,
				Intent: freshIntent, IntentFound: true,
			},
			wantCause:   managedDoltExitCauseRequested,
			wantAlarm:   false,
			wantExit:    0,
			wantContain: []string{"exited cleanly", "stop requested by", "gc managed dolt stop", "requester pid 900"},
			wantAbsent:  []string{"ALARM"},
		},
		{
			name: "a stop request for a DIFFERENT pid still alarms",
			exit: managedDoltWatchdogChildExit{
				PID: 96363, WatchdogPID: 17490, Uptime: time.Minute, Now: now,
				Intent: freshIntent, IntentFound: true,
			},
			wantCause:   managedDoltExitCauseUnexpectedClean,
			wantAlarm:   true,
			wantExit:    1,
			wantContain: []string{"ALARM UNEXPECTED CLEAN EXIT", "pid 96363"},
		},
		{
			name: "a stale stop request still alarms",
			exit: managedDoltWatchdogChildExit{
				PID: 17493, WatchdogPID: 17490, Uptime: time.Minute, Now: now,
				Intent: managedDoltStopIntent{
					PID:         17493,
					RequestedAt: now.Add(-managedDoltStopIntentTTL - time.Minute).Format(time.RFC3339Nano),
				},
				IntentFound: true,
			},
			wantCause:   managedDoltExitCauseUnexpectedClean,
			wantAlarm:   true,
			wantExit:    1,
			wantContain: []string{"ALARM UNEXPECTED CLEAN EXIT"},
		},
		{
			name: "a signal racing the exit is a requested stop, not an alarm",
			exit: managedDoltWatchdogChildExit{
				PID: 17493, WatchdogPID: 17490, Uptime: time.Minute, Now: now,
				SignalPending: true,
			},
			wantCause:   managedDoltExitCauseRequested,
			wantAlarm:   false,
			wantExit:    0,
			wantContain: []string{"exited cleanly", "stop signal delivered to this watchdog"},
			wantAbsent:  []string{"ALARM"},
		},
		{
			name: "a non-zero exit keeps the existing error wording",
			exit: managedDoltWatchdogChildExit{
				PID: 37030, WatchdogPID: 17490, Uptime: 5 * time.Second, Now: now,
				WaitErr: errors.New("exit status 2"),
			},
			wantCause:   managedDoltExitCauseError,
			wantAlarm:   false,
			wantExit:    1,
			wantContain: []string{"exited with error", "pid 37030", "exit status 2"},
			wantAbsent:  []string{"ALARM", "exited cleanly"},
		},
		{
			name: "an unrequested clean exit with a silent server says so",
			exit: managedDoltWatchdogChildExit{
				PID: 17493, WatchdogPID: 17490, Uptime: time.Minute, Now: now,
			},
			wantCause:   managedDoltExitCauseUnexpectedClean,
			wantAlarm:   true,
			wantExit:    1,
			wantContain: []string{"the server wrote nothing before exiting"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := classifyManagedDoltWatchdogChildExit(tc.exit)
			if report.Cause != tc.wantCause {
				t.Errorf("cause = %q, want %q", report.Cause, tc.wantCause)
			}
			if report.Alarm != tc.wantAlarm {
				t.Errorf("alarm = %v, want %v", report.Alarm, tc.wantAlarm)
			}
			if report.ExitCode != tc.wantExit {
				t.Errorf("exit code = %d, want %d", report.ExitCode, tc.wantExit)
			}
			joined := strings.Join(report.Lines, "\n")
			for _, want := range tc.wantContain {
				if !strings.Contains(joined, want) {
					t.Errorf("log lines missing %q; got:\n%s", want, joined)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(joined, absent) {
					t.Errorf("log lines unexpectedly contain %q; got:\n%s", absent, joined)
				}
			}
			if tc.wantAlarm && strings.TrimSpace(report.AlarmMessage) == "" {
				t.Error("an alarming exit produced no escalation message")
			}
			if !tc.wantAlarm && report.AlarmMessage != "" {
				t.Errorf("a non-alarming exit produced an escalation message %q", report.AlarmMessage)
			}
		})
	}
}

// TestClassifyManagedDoltWatchdogCleanExitNeverReusesTheHealthyWording is the
// narrow guard the bead asks for: the reassuring phrase must not appear on an
// exit nobody requested, and the alarm must not be mistakable for health.
func TestClassifyManagedDoltWatchdogCleanExitNeverReusesTheHealthyWording(t *testing.T) {
	report := classifyManagedDoltWatchdogChildExit(managedDoltWatchdogChildExit{
		PID: 17493, WatchdogPID: 17490, Uptime: time.Hour, Now: time.Now(),
	})
	joined := strings.Join(report.Lines, "\n")
	if strings.Contains(joined, "exited cleanly") {
		t.Errorf("an unrequested status-0 exit still renders as %q:\n%s", "exited cleanly", joined)
	}
}

func TestLastManagedDoltServerLogLine(t *testing.T) {
	cases := []struct {
		name string
		tail string
		want string
	}{
		{
			name: "skips the watchdog's own lines",
			tail: "INFO dolt: connections served 3370000\n" +
				managedDoltWatchdogLogPrefix + " supervising dolt sql-server pid 17493\n",
			want: "INFO dolt: connections served 3370000",
		},
		{
			name: "returns the last server line",
			tail: "first\nsecond\nthird\n",
			want: "third",
		},
		{"blank tail yields nothing", "\n  \n", ""},
		{"watchdog-only tail yields nothing", managedDoltWatchdogLogPrefix + " supervising\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastManagedDoltServerLogLine(tc.tail); got != tc.want {
				t.Errorf("lastManagedDoltServerLogLine = %q, want %q", got, tc.want)
			}
		})
	}

	long := strings.Repeat("x", managedDoltServerLogLineMaxLen+100)
	got := lastManagedDoltServerLogLine(long + "\n")
	if len([]rune(got)) > managedDoltServerLogLineMaxLen+1 {
		t.Errorf("long line was not truncated: %d runes", len([]rune(got)))
	}
}

func TestReadManagedDoltServerLogTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dolt.log")

	if got := readManagedDoltServerLogTail(path); got != "" {
		t.Errorf("tail of a missing log = %q, want empty", got)
	}
	if got := readManagedDoltServerLogTail(""); got != "" {
		t.Errorf("tail of an empty path = %q, want empty", got)
	}

	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readManagedDoltServerLogTail(path); got != "alpha\nbeta\n" {
		t.Errorf("tail of a small log = %q", got)
	}

	// A log larger than the window is truncated to the window, and the
	// partial first line is dropped so the caller never quotes a fragment.
	big := strings.Repeat("filler line that is long enough to matter\n", 1000)
	if err := os.WriteFile(path, []byte(big+"LAST SERVER LINE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tail := readManagedDoltServerLogTail(path)
	if len(tail) > managedDoltServerLogTailBytes {
		t.Errorf("tail is %d bytes, want at most %d", len(tail), managedDoltServerLogTailBytes)
	}
	if !strings.HasPrefix(tail, "filler line") {
		t.Errorf("tail did not drop its partial first line; starts with %q", tail[:min(40, len(tail))])
	}
	if got := lastManagedDoltServerLogLine(tail); got != "LAST SERVER LINE" {
		t.Errorf("last server line = %q, want %q", got, "LAST SERVER LINE")
	}
}
