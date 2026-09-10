package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagedDoltStopIntentPath(t *testing.T) {
	cases := []struct {
		name       string
		configFile string
		want       string
	}{
		{"beside the config", "/city/.gc/runtime/packs/dolt/dolt-config.yaml", "/city/.gc/runtime/packs/dolt/" + managedDoltStopIntentFileName},
		{"empty config has no marker", "", ""},
		{"blank config has no marker", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltStopIntentPath(tc.configFile); got != tc.want {
				t.Errorf("managedDoltStopIntentPath(%q) = %q, want %q", tc.configFile, got, tc.want)
			}
		})
	}
}

func TestManagedDoltStopIntentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "dolt-config.yaml")

	if _, found := readManagedDoltStopIntent(configFile); found {
		t.Fatal("read a stop intent before any was recorded")
	}

	if err := recordManagedDoltStopIntent(configFile, 4242, "gc managed dolt stop"); err != nil {
		t.Fatalf("record stop intent: %v", err)
	}
	intent, found := readManagedDoltStopIntent(configFile)
	if !found {
		t.Fatal("recorded stop intent was not readable")
	}
	if intent.PID != 4242 {
		t.Errorf("intent pid = %d, want 4242", intent.PID)
	}
	if intent.RequesterPID != os.Getpid() {
		t.Errorf("intent requester pid = %d, want %d", intent.RequesterPID, os.Getpid())
	}
	if intent.Reason != "gc managed dolt stop" {
		t.Errorf("intent reason = %q, want %q", intent.Reason, "gc managed dolt stop")
	}
	if !managedDoltStopIntentCovers(intent, 4242, time.Now()) {
		t.Error("a just-recorded intent does not cover its own pid")
	}

	if err := clearManagedDoltStopIntent(configFile); err != nil {
		t.Fatalf("clear stop intent: %v", err)
	}
	if _, found := readManagedDoltStopIntent(configFile); found {
		t.Error("stop intent survived clearing")
	}
	// Clearing an absent marker is the common case (every spawn does it) and
	// must not error.
	if err := clearManagedDoltStopIntent(configFile); err != nil {
		t.Errorf("clearing an absent stop intent: %v", err)
	}
}

func TestRecordManagedDoltStopIntentIgnoresUnusableTargets(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "dolt-config.yaml")
	if err := recordManagedDoltStopIntent(configFile, 0, "no pid"); err != nil {
		t.Fatalf("recording an intent for pid 0: %v", err)
	}
	if _, found := readManagedDoltStopIntent(configFile); found {
		t.Error("recorded a stop intent for pid 0")
	}
	if err := recordManagedDoltStopIntent("", 1234, "no config"); err != nil {
		t.Fatalf("recording an intent with no config: %v", err)
	}
}

// TestManagedDoltStopIntentCovers pins the intentional-vs-unexpected rule: this
// predicate alone decides whether a status-0 exit of the data plane alarms.
func TestManagedDoltStopIntentCovers(t *testing.T) {
	now := time.Date(2026, 8, 15, 17, 18, 5, 0, time.UTC)
	stamp := func(offset time.Duration) string {
		return now.Add(offset).Format(time.RFC3339Nano)
	}

	cases := []struct {
		name   string
		intent managedDoltStopIntent
		pid    int
		want   bool
	}{
		{
			name:   "fresh marker for the exiting pid covers",
			intent: managedDoltStopIntent{PID: 17493, RequestedAt: stamp(-2 * time.Second)},
			pid:    17493,
			want:   true,
		},
		{
			name:   "marker for a different pid does not cover",
			intent: managedDoltStopIntent{PID: 96363, RequestedAt: stamp(-2 * time.Second)},
			pid:    17493,
			want:   false,
		},
		{
			name:   "marker older than the TTL does not cover",
			intent: managedDoltStopIntent{PID: 17493, RequestedAt: stamp(-managedDoltStopIntentTTL - time.Second)},
			pid:    17493,
			want:   false,
		},
		{
			name:   "marker at the TTL boundary still covers",
			intent: managedDoltStopIntent{PID: 17493, RequestedAt: stamp(-managedDoltStopIntentTTL)},
			pid:    17493,
			want:   true,
		},
		{
			name:   "marker inside the future skew covers",
			intent: managedDoltStopIntent{PID: 17493, RequestedAt: stamp(managedDoltStopIntentFutureSkew / 2)},
			pid:    17493,
			want:   true,
		},
		{
			name:   "marker beyond the future skew does not cover",
			intent: managedDoltStopIntent{PID: 17493, RequestedAt: stamp(managedDoltStopIntentFutureSkew + time.Second)},
			pid:    17493,
			want:   false,
		},
		{
			name:   "unparseable timestamp does not cover",
			intent: managedDoltStopIntent{PID: 17493, RequestedAt: "not a timestamp"},
			pid:    17493,
			want:   false,
		},
		{
			name:   "empty marker does not cover",
			intent: managedDoltStopIntent{},
			pid:    17493,
			want:   false,
		},
		{
			name:   "a zero exiting pid never matches",
			intent: managedDoltStopIntent{PID: 0, RequestedAt: stamp(0)},
			pid:    0,
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDoltStopIntentCovers(tc.intent, tc.pid, now); got != tc.want {
				t.Errorf("managedDoltStopIntentCovers(%+v, %d) = %v, want %v", tc.intent, tc.pid, got, tc.want)
			}
		})
	}
}

func TestDescribeManagedDoltStopIntent(t *testing.T) {
	described := describeManagedDoltStopIntent(managedDoltStopIntent{
		PID:          17493,
		RequesterPID: 900,
		Requester:    "gc dolt-state stop --city /city",
		Reason:       "gc managed dolt stop",
	})
	for _, want := range []string{"gc managed dolt stop", "requester pid 900", "gc dolt-state stop"} {
		if !strings.Contains(described, want) {
			t.Errorf("description %q missing %q", described, want)
		}
	}
	if got := describeManagedDoltStopIntent(managedDoltStopIntent{}); got == "" {
		t.Error("an empty intent must still describe itself rather than render blank")
	}
}

func TestTruncateManagedDoltStopIntentField(t *testing.T) {
	long := make([]byte, managedDoltStopIntentRequesterMaxLen+50)
	for i := range long {
		long[i] = 'a'
	}
	got := truncateManagedDoltStopIntentField(string(long))
	if len(got) > managedDoltStopIntentRequesterMaxLen+len("…") {
		t.Errorf("truncated field is %d bytes, want at most %d", len(got), managedDoltStopIntentRequesterMaxLen+len("…"))
	}
	if got := truncateManagedDoltStopIntentField("  short  "); got != "short" {
		t.Errorf("truncate trimmed value = %q, want %q", got, "short")
	}
}

// TestManagedDoltStartupFailureTeardownRecordsStopIntent is the ga-drkbcd D1
// regression. gc's OWN startup-failure teardown
// (terminateManagedDoltStartedProcess) signals the server it just spawned, and
// dolt answers SIGTERM by exiting status 0 — the branch the watchdog now
// alarms on. The readiness timeout that reaches this teardown is a routine
// event on a loaded box, so without an intent marker every one of them would
// raise a CRITICAL alarm into the emergency spool for a stop gc asked for
// itself. The teardown must therefore record its intent like any other stopper.
func TestManagedDoltStartupFailureTeardownRecordsStopIntent(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	var serverPID int
	cityPath := installStartManagedDoltLoopStubs(t, startManagedDoltLoopStubs{
		startFn: func(cityPath, configFile, _ string, _ *os.File) (managedDoltStartedProcess, error) {
			// A server that shuts down gracefully on SIGTERM, exactly as dolt
			// does — so the teardown below produces a status-0 exit.
			serverPID = startFakeOwnedManagedDolt(t, configFile,
				"trap 'exit 0' TERM\n"+
					": > \"$GC_TEST_READY\"\n"+
					"while : ; do sleep 0.05; done\n",
				"GC_TEST_READY="+readyPath)
			waitForFakeManagedDoltReady(t, readyPath)
			return managedDoltStartedProcess{CityPath: cityPath, PID: serverPID}, nil
		},
		// The readiness-timeout shape: the process is alive, it just never
		// answered a query in time.
		waitReadyFn: func(_, _, _, _ string, _ int, _ time.Duration, _ bool) (managedDoltWaitReadyReport, error) {
			return managedDoltWaitReadyReport{Ready: false, PIDAlive: true}, nil
		},
		logSuffixFn:     func(string, int64) (string, error) { return "", nil },
		portAvailableFn: func(string, int) bool { return true },
	})
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolve managed dolt layout: %v", err)
	}

	startedAt := time.Now()
	_, err = startManagedDoltProcessWithOptions(cityPath, "127.0.0.1", "17991", "root", "warning", -1, 50*time.Millisecond, false)
	if err == nil {
		t.Fatal("expected the readiness timeout to fail the start")
	}
	if !strings.Contains(err.Error(), "did not become query-ready") {
		t.Fatalf("start failed for the wrong reason: %v", err)
	}
	if serverPID <= 0 {
		t.Fatal("the stubbed spawn never produced a server pid")
	}

	// What the watchdog sees when the teardown's SIGTERM lands: a status-0 exit
	// of the server it supervises. It must be attributable to gc, not an alarm.
	report := classifyManagedDoltWatchdogChildExit(
		observeManagedDoltWatchdogChildExit(serverPID, layout.ConfigFile, layout.LogFile, startedAt, nil, false))
	if report.Alarm {
		t.Errorf("gc's own startup-failure teardown raised a CRITICAL alarm: %v", report.Lines)
	}
	if report.Cause != managedDoltExitCauseRequested {
		t.Errorf("teardown exit classified %q, want %q (covered by a stop intent)", report.Cause, managedDoltExitCauseRequested)
	}
	if report.AlarmMessage != "" {
		t.Errorf("teardown exit produced an alarm message: %q", report.AlarmMessage)
	}
}
