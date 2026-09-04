package main

// Stop-intent markers for the managed dolt sql-server (ga-drkbcd).
//
// The scope watchdog supervises the server but does not own every stop of it.
// `gc dolt stop` — and the recovery path behind it — signals the dolt PID
// DIRECTLY (stopManagedDoltProcessWithOptions), never the watchdog. dolt
// handles SIGTERM and shuts down gracefully, so cmd.Wait() in the watchdog
// returns nil. From the watchdog's side a shutdown we asked for and a healthy
// server that decided to exit 0 mid-service are therefore the SAME observation,
// and both rendered as the reassuring line "exited cleanly". That is why the
// 2026-08-15 data-plane stop was silent: the most disruptive event of the day
// took the branch every alarm treats as the good one.
//
// The marker closes that gap by writing the intent down where the watchdog can
// read it. The stopper records "I am about to stop pid N" beside the server's
// --config file — the one path the stopper (via managedDoltRuntimeLayout) and
// the watchdog (via its argv) already agree on — and the watchdog consults it
// when its child exits with status 0. A status-0 exit covered by a fresh marker
// is a shutdown we requested; a status-0 exit with no marker is an UNEXPECTED
// clean exit and alarms.
//
// Every write and delete here is advisory and best-effort: a marker that fails
// to land never fails a stop. The degradation is a false ALARM on a stop we did
// request, which is the safe direction — the failure mode being replaced is
// silence.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	// managedDoltStopIntentFileName is the marker's basename inside the
	// server's pack state dir (the directory holding its --config file).
	managedDoltStopIntentFileName = "dolt-stop-intent.json"

	// managedDoltStopIntentTTL bounds how long a marker can vouch for a clean
	// exit. It only has to outlast one stop: SIGTERM, the configured
	// SIGTERM→SIGKILL grace (config.DefaultDoltStopTimeout, 30s), and dolt's
	// own journal flush. Ten minutes is far past that while still guaranteeing
	// a marker abandoned by a crashed stopper cannot silence a later unexpected
	// exit indefinitely. The primary staleness defense is not the TTL but the
	// PID match plus the clear-on-spawn in startManagedDoltProcessWithOptions:
	// a marker can only ever name the current server generation.
	managedDoltStopIntentTTL = 10 * time.Minute

	// managedDoltStopIntentFutureSkew tolerates a marker stamped slightly in
	// the future (clock adjustment between the stopper and the watchdog).
	// Beyond it the marker is not trusted — an unexplained future timestamp is
	// exactly the state where we would rather alarm than reassure.
	managedDoltStopIntentFutureSkew = time.Minute

	// managedDoltStopIntentRequesterMaxLen bounds the recorded requester argv
	// so a pathological command line cannot bloat the marker or the log line
	// that quotes it.
	managedDoltStopIntentRequesterMaxLen = 240
)

// managedDoltStopIntent is one recorded "gc is stopping this server" decision.
// It is the attribution the 2026-08-15 postmortem could not produce: who asked,
// from which process, when, and for which server PID.
type managedDoltStopIntent struct {
	PID          int    `json:"pid"`
	RequestedAt  string `json:"requested_at"`
	RequesterPID int    `json:"requester_pid"`
	Requester    string `json:"requester"`
	Reason       string `json:"reason"`
}

// managedDoltStopIntentPath locates the marker for the server started with
// configFile. Both sides derive it from the config path rather than from the
// city layout because the watchdog is a bare re-exec that receives only the
// config path, the log path and the city path — resolving a layout there would
// re-read env that may have moved since the spawn.
func managedDoltStopIntentPath(configFile string) string {
	configFile = strings.TrimSpace(configFile)
	if configFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configFile), managedDoltStopIntentFileName)
}

// managedDoltStopIntentRequester describes the calling process for the marker.
// It reads its own argv rather than forking ps: the stopper IS the requester,
// so no inspection is needed and the stop path stays fork-free.
func managedDoltStopIntentRequester() string {
	return truncateManagedDoltStopIntentField(strings.Join(os.Args, " "))
}

func truncateManagedDoltStopIntentField(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= managedDoltStopIntentRequesterMaxLen {
		return value
	}
	return value[:managedDoltStopIntentRequesterMaxLen] + "…"
}

// recordManagedDoltStopIntent writes the marker before a stop signals pid.
// Best-effort by contract: the returned error is for tests and callers that
// want to log it, never for failing a stop.
func recordManagedDoltStopIntent(configFile string, pid int, reason string) error {
	path := managedDoltStopIntentPath(configFile)
	if path == "" || pid <= 0 {
		return nil
	}
	intent := managedDoltStopIntent{
		PID:          pid,
		RequestedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		RequesterPID: os.Getpid(),
		Requester:    managedDoltStopIntentRequester(),
		Reason:       truncateManagedDoltStopIntentField(reason),
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsys.WriteFileAtomic(fsys.OSFS{}, path, data, 0o644)
}

// clearManagedDoltStopIntent removes the marker. Called when a stop completes
// and again before every fresh spawn, so a marker never outlives the server
// generation it was written for.
func clearManagedDoltStopIntent(configFile string) error {
	path := managedDoltStopIntentPath(configFile)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// readManagedDoltStopIntent loads the marker. A missing or unreadable marker
// reports found=false, which the exit classifier treats as "nobody asked" —
// the alarming direction.
func readManagedDoltStopIntent(configFile string) (managedDoltStopIntent, bool) {
	path := managedDoltStopIntentPath(configFile)
	if path == "" {
		return managedDoltStopIntent{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return managedDoltStopIntent{}, false
	}
	var intent managedDoltStopIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return managedDoltStopIntent{}, false
	}
	return intent, true
}

// managedDoltStopIntentCovers reports whether intent explains a stop of pid
// observed at now. It is the whole intentional-vs-unexpected decision, kept
// pure so the classification is testable without processes or files.
//
// Coverage requires all of: a positive PID that matches exactly, a parseable
// timestamp, and a timestamp inside [now-TTL, now+skew]. Anything else — a
// marker for a different PID, an unparseable stamp, a stale marker, a marker
// from the future — fails to cover, and the exit alarms.
func managedDoltStopIntentCovers(intent managedDoltStopIntent, pid int, now time.Time) bool {
	if pid <= 0 || intent.PID != pid {
		return false
	}
	requestedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(intent.RequestedAt))
	if err != nil {
		return false
	}
	age := now.Sub(requestedAt)
	if age > managedDoltStopIntentTTL {
		return false
	}
	return age >= -managedDoltStopIntentFutureSkew
}

// describeManagedDoltStopIntent renders the marker for a log line. It names the
// requester so the log answers "who stopped the database" directly rather than
// leaving the reader to correlate timestamps across files.
func describeManagedDoltStopIntent(intent managedDoltStopIntent) string {
	parts := make([]string, 0, 3)
	if reason := strings.TrimSpace(intent.Reason); reason != "" {
		parts = append(parts, reason)
	}
	if intent.RequesterPID > 0 {
		parts = append(parts, "requester pid "+strconv.Itoa(intent.RequesterPID))
	}
	if requester := strings.TrimSpace(intent.Requester); requester != "" {
		parts = append(parts, "argv "+requester)
	}
	if len(parts) == 0 {
		return "gc (unattributed marker)"
	}
	return strings.Join(parts, ", ")
}
