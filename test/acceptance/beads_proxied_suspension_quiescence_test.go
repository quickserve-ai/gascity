//go:build acceptance_a

// Suspension is quiescence: a suspended rig, and every scope of a suspended
// city, gets no bd call from gc at all — no cache scan, no order sweep, no
// health ping, no maintenance order — and its bd-owned proxy and Dolt child are
// stopped on suspend, the same `bd dolt stop` gc stop runs last. Measured
// through a BD_BIN shim that records every fork with the scope it targeted,
// over a window long enough for every periodic backstop to come round.
package acceptance_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// realQuiescenceWindow is long enough for every periodic backstop that
// touches a scope on a running city (the 30s beads-health, the 1m
// order-tracking sweep, the controller's reconcile and demand passes) to come
// round several times.
const realQuiescenceWindow = 3 * time.Minute

// backstopSpeedup is the factor this test runs under: clock.BackstopSpeedupEnv
// parsed the way a test-hooks gc parses it, or 1 when unset. Bazel's PR
// targets set it (and run the testhooks-stamped gc); the nightly
// *_realtime_test targets leave it unset and run the real three minutes.
func backstopSpeedup() int {
	n, _, ok := clock.ParseBackstopSpeedup(os.Getenv(clock.BackstopSpeedupEnv))
	if !ok {
		return 1
	}
	return n
}

// quiescenceWindow is realQuiescenceWindow divided by backstopSpeedup. gc
// divides the backstop cadences that go through clock.Backstop (listed in
// clock.BackstopSpeedupEnv's doc) by the same factor, so each of those still
// comes round as many times inside the window. A cadence outside that list
// keeps its real period and comes round fewer times; the nightly
// *_realtime_test targets, which run the full window, cover it.
func quiescenceWindow() time.Duration { return realQuiescenceWindow / time.Duration(backstopSpeedup()) }

// forwardBackstopSpeedup hands this process's clock.BackstopSpeedupEnv, if
// any, to the gc processes env starts (the supervisor and its controller
// inherit it), so the window and gc's divided cadences shrink together. Those
// backstops come round as many times in the shortened window as in the real
// one only if gc really divided them, so with a factor above 1 it requires
// gc's active-hook notice: a gc without test hooks ignores the variable and
// fails the test here.
func forwardBackstopSpeedup(t *testing.T, env *helpers.Env) *helpers.Env {
	t.Helper()
	v, ok := os.LookupEnv(clock.BackstopSpeedupEnv)
	if !ok {
		return env.Without(clock.BackstopSpeedupEnv)
	}
	env = env.With(clock.BackstopSpeedupEnv, v)
	if backstopSpeedup() > 1 {
		_, stderr, _ := helpers.RunGCStreams(env, "", "version")
		if !strings.Contains(stderr, clock.BackstopActiveNotice) {
			t.Fatalf("%s=%s but gc did not report %q (a gc without test hooks ignores it, so its backstops would come round "+
				"fewer times in the shortened window); gc stderr:\n%s", clock.BackstopSpeedupEnv, v, clock.BackstopActiveNotice, stderr)
		}
	}
	return env
}

// minLiveCityCalls is the fewest bd calls the suspended-rig row expects its
// running, unsuspended city to get over the window. The backstops whose visits
// to the rig the row counts visit the city too; a city they left alone would
// make the rig's zero vacuous. beads-health alone comes round six times in
// realQuiescenceWindow, and as often in the divided window under a speedup.
const minLiveCityCalls = 3

// suspendDrainWait bounds how long a suspend takes to reach quiescence: the
// controller stops the suspended scope's sessions over its next ticks, then
// stops the scope's bd pair.
const suspendDrainWait = 3 * time.Minute

// scopeRecordingBD is a BD_BIN shim that records, per fork, the scope it
// targets (BEADS_DIR and the working directory), its argv, when it ran and
// the processes that called it, then execs the real bd.
type scopeRecordingBD struct {
	path    string
	logPath string
	// offset is the log's size at the last reset: calls before it are kept
	// on disk, so a failure can show what ran just before its window opened.
	offset  int64
	resetAt time.Time
}

func newScopeRecordingBD(t *testing.T, realBD string) *scopeRecordingBD {
	t.Helper()
	dir := filepath.Join(helpers.TempDir(t), "scope-recording-bd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &scopeRecordingBD{path: filepath.Join(dir, "bd"), logPath: filepath.Join(dir, "calls.log")}
	script := fmt.Sprintf(`#!/bin/sh
# The calling process chain (parent, grandparent, great-grandparent) names
# whatever made a call the row did not expect.
callers=""
pid=$PPID
for _ in 1 2 3; do
	case "$pid" in ''|0|1) break ;; esac
	callers="$callers [$(ps -o args= -p "$pid" 2>/dev/null | cut -c1-200)]"
	pid=$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')
done
record="ts=$(date +%%s) beads_dir=${BEADS_DIR:-} pwd=$(pwd) argv=$* callers=$callers"
printf '%%s\n' "$record" >>%s 2>/dev/null || true
exec %s "$@"
`, shellQuoteArg(r.logPath), shellQuoteArg(realBD))
	if err := os.WriteFile(r.path, []byte(script), 0o755); err != nil { //nolint:gosec // the shim must be executable
		t.Fatal(err)
	}
	return r
}

func shellQuoteArg(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// reset starts a new window: callsUnder sees only calls recorded after it.
// The earlier calls stay in the log for callsUnderBefore.
func (r *scopeRecordingBD) reset(t *testing.T) {
	t.Helper()
	r.resetAt = time.Now()
	info, err := os.Stat(r.logPath)
	switch {
	case os.IsNotExist(err):
		r.offset = 0
	case err != nil:
		t.Fatal(err)
	default:
		r.offset = info.Size()
	}
}

// logLines returns the log's lines after the last reset (after=true) or
// before it (after=false).
func (r *scopeRecordingBD) logLines(t *testing.T, after bool) []string {
	t.Helper()
	data, err := os.ReadFile(r.logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	cut := recordBoundary(data, r.offset)
	if after {
		data = data[cut:]
	} else {
		data = data[:cut]
	}
	return strings.Split(string(data), "\n")
}

// recordBoundary moves a reset offset forward to the end of the record it
// falls inside. A record whose write straddled the reset started before the
// window, so it belongs before it, as the old delete-at-reset dropped it whole;
// cutting it in two would count its tail as a call inside the window.
func recordBoundary(data []byte, offset int64) int64 {
	if offset >= int64(len(data)) {
		return int64(len(data))
	}
	if offset <= 0 || data[offset-1] == '\n' {
		return max(offset, 0)
	}
	if i := bytes.IndexByte(data[offset:], '\n'); i >= 0 {
		return offset + int64(i) + 1
	}
	return int64(len(data))
}

// underRoot keeps the lines whose target (not their callers) names root.
func underRoot(lines []string, root string) []string {
	var hits []string
	for _, line := range lines {
		// The callers only explain a call; the scope is what it targeted.
		target, _, _ := strings.Cut(line, " callers=")
		if line != "" && strings.Contains(target, root) {
			hits = append(hits, line)
		}
	}
	return hits
}

// callsUnderBefore returns the calls under root recorded in the span before
// the last reset. A call that started before the window and caused something
// inside it (a proxy it started, a stop that followed) shows up here.
func (r *scopeRecordingBD) callsUnderBefore(t *testing.T, root string, span time.Duration) []string {
	t.Helper()
	from := r.resetAt.Add(-span).Unix()
	var hits []string
	for _, line := range underRoot(r.logLines(t, false), root) {
		var ts int64
		if _, err := fmt.Sscanf(line, "ts=%d", &ts); err == nil && ts >= from {
			hits = append(hits, line)
		}
	}
	return hits
}

// preWindowNote renders callsUnderBefore for a failure message.
func (r *scopeRecordingBD) preWindowNote(t *testing.T, root string) string {
	t.Helper()
	const span = 30 * time.Second
	before := r.callsUnderBefore(t, root, span)
	if len(before) == 0 {
		return fmt.Sprintf("\nno call under %s in the %s before the window opened", root, span)
	}
	return fmt.Sprintf("\n%d call(s) under %s in the %s before the window opened:\n%s", len(before), root, span, strings.Join(before, "\n"))
}

// callsUnder returns the calls recorded since the last reset that targeted a
// scope under root.
func (r *scopeRecordingBD) callsUnder(t *testing.T, root string) []string {
	t.Helper()
	return underRoot(r.logLines(t, true), root)
}

// runEveryCityOrder runs each city-level order once with `gc order run`, all
// at once (one after another they took half a minute), and returns their
// names. A failing order fails the test: an order that stopped short never
// reached the scopes it would touch, so the caller's assertion on which scopes
// the orders touched would pass without measuring it.
func runEveryCityOrder(t *testing.T, city *helpers.City) []string {
	t.Helper()
	out, err := city.GCStdout("order", "list", "--json")
	if err != nil {
		t.Fatalf("gc order list --json: %v\n%s", err, out)
	}
	var list struct {
		Orders []struct {
			Name string `json:"name"`
			Rig  string `json:"rig"`
		} `json:"orders"`
	}
	lastJSONLine(t, out, &list)
	var ran []string
	var wg sync.WaitGroup
	for _, order := range list.Orders {
		if order.Rig != "" {
			continue
		}
		ran = append(ran, order.Name)
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if out, err := city.GC("order", "run", name); err != nil {
				t.Errorf("gc order run %s: %v\n%s", name, err, out)
			}
		}(order.Name)
	}
	wg.Wait()
	if len(ran) == 0 {
		t.Fatal("the city has no city-level orders to run")
	}
	return ran
}

// quiescenceCity is a running city with one rig, both bd-owned proxied
// scopes with their pairs up, whose bd forks go through a scopeRecordingBD.
type quiescenceCity struct {
	city    *helpers.City
	rigDir  string
	rigName string
	shim    *scopeRecordingBD
}

// newQuiescenceCity starts the city each suspension row measures. Each row
// gets its own: the two quiescence windows are minutes of waiting each, and
// as independent top-level tests the sharded Bazel lane runs them at once.
func newQuiescenceCity(t *testing.T) *quiescenceCity {
	t.Helper()
	// Multi-minute quiescence windows over a running city: not Tier A smoke
	// material. Bazel's acceptance lane sets the switch and runs each row as
	// a target of its own.
	helpers.RequireTopologyMatrix(t)
	bdPath, doltPath := requireProxiedTooling(t)
	env, wrappedBD := proxiedEnvWithBD(t, bdPath, doltPath)
	shim := newScopeRecordingBD(t, wrappedBD)
	env = forwardBackstopSpeedup(t, env.With("BD_BIN", shim.path))
	// The orders the rows run, and the controller's, call bare `gc` through
	// PATH: unless that gc is the binary under test, they never start or run
	// another gc, and a row measures nothing or the wrong binary.
	if err := helpers.VerifyPathGC(env); err != nil {
		t.Fatal(err)
	}

	city := helpers.NewCity(t, env)
	cityRoot := city.Dir
	rigDir := createGitRig(t)
	t.Cleanup(func() {
		// The city's own cleanups (helpers.City) have stopped it by now, so
		// only a pair still running calls for a further gc stop.
		stopIfPairsRemain(t, env, cityRoot, rigDir)
		helpers.RunGC(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best effort
		for _, root := range []string{cityRoot, rigDir} {
			if leaked := waitForNoDoltProcesses(t, root, 20*time.Second); len(leaked) > 0 {
				t.Errorf("processes under %s outlived the test:\n%s", root, strings.Join(leaked, "\n"))
			}
		}
	})
	city.Init("claude")
	city.RigAdd(rigDir, "")
	const rigName = "testrig"
	if len(doltProcessesUnder(t, rigDir)) == 0 {
		if out, err := city.GC("bd", "--rig", rigName, "list", "--json"); err != nil {
			t.Fatalf("gc bd --rig %s list: %v\n%s", rigName, err, out)
		}
	}
	return &quiescenceCity{city: city, rigDir: rigDir, rigName: rigName, shim: shim}
}

func TestProxiedSuspensionIsQuiescenceSuspendedRig(t *testing.T) {
	q := newQuiescenceCity(t)
	city, rigDir, rigName, shim := q.city, q.rigDir, q.rigName, q.shim

	if out, err := city.GC("rig", "suspend", rigName); err != nil {
		t.Fatalf("gc rig suspend: %v\n%s", err, out)
	}
	if leaked := waitForNoDoltProcesses(t, rigDir, suspendDrainWait); len(leaked) > 0 {
		t.Fatalf("the suspended rig's pair is still running:\n%s", strings.Join(leaked, "\n"))
	}
	shim.reset(t)
	start := time.Now()
	// Every city-level order once, so no order's cadence can hide a
	// visit to the suspended rig behind the window.
	ran := runEveryCityOrder(t, city)
	t.Logf("ran %d city-level orders in %s", len(ran), time.Since(start).Round(time.Second))
	window := quiescenceWindow()
	t.Logf("quiescence window %s (backstop speedup %d)", window, backstopSpeedup())
	// The window starts once the orders are done, so the periodic backstops
	// get all of it.
	cityCallsBefore := len(shim.callsUnder(t, city.Dir))
	time.Sleep(window)
	// The city itself is not suspended, so its controller keeps touching it
	// through the window: a city that went quiet would make the rig's zero
	// below vacuous.
	if n := len(shim.callsUnder(t, city.Dir)) - cityCallsBefore; n < minLiveCityCalls {
		t.Fatalf("gc touched the running city %d time(s) in the %s window, want at least %d: its periodic backstops did not run", n, window, minLiveCityCalls)
	} else {
		t.Logf("gc touched the running city %d time(s) in the %s window", n, window)
	}
	if calls := shim.callsUnder(t, rigDir); len(calls) > 0 {
		t.Fatalf("gc touched the suspended rig %d time(s) in %s (orders run: %s):\n%s%s", len(calls), time.Since(start).Round(time.Second), strings.Join(ran, ", "), strings.Join(calls, "\n"), shim.preWindowNote(t, rigDir))
	}
	if started := doltProcessesUnder(t, rigDir); len(started) > 0 {
		t.Fatalf("the suspended rig's pair came back:\n%s%s", strings.Join(started, "\n"), shim.preWindowNote(t, rigDir))
	}
	if out, err := city.GC("rig", "resume", rigName); err != nil {
		t.Fatalf("gc rig resume: %v\n%s", err, out)
	}
	if out, err := city.GC("bd", "--rig", rigName, "list", "--json"); err != nil {
		t.Fatalf("gc bd --rig %s list after resume: %v\n%s", rigName, err, out)
	}
}

func TestProxiedSuspensionIsQuiescenceSuspendedCity(t *testing.T) {
	q := newQuiescenceCity(t)
	city, rigDir, shim := q.city, q.rigDir, q.shim
	cityRoot := city.Dir

	if out, err := city.GC("suspend"); err != nil {
		t.Fatalf("gc suspend: %v\n%s", err, out)
	}
	// The controller stops the city's sessions first and only then the
	// pairs, so this waits for the whole drain.
	for _, root := range []string{cityRoot, rigDir} {
		if leaked := waitForNoDoltProcesses(t, root, suspendDrainWait); len(leaked) > 0 {
			sessions, _ := city.GC("session", "list")
			t.Fatalf("a suspended city's pair under %s is still running:\n%s\nsessions:\n%s", root, strings.Join(leaked, "\n"), sessions)
		}
	}
	shim.reset(t)
	window := quiescenceWindow()
	t.Logf("quiescence window %s (backstop speedup %d)", window, backstopSpeedup())
	time.Sleep(window)
	for _, root := range []string{cityRoot, rigDir} {
		if calls := shim.callsUnder(t, root); len(calls) > 0 {
			t.Errorf("gc touched %s %d time(s) in %s while the city was suspended:\n%s%s", root, len(calls), window, strings.Join(calls, "\n"), shim.preWindowNote(t, root))
		}
		if started := doltProcessesUnder(t, root); len(started) > 0 {
			t.Errorf("a pair under %s came back while the city was suspended:\n%s%s", root, strings.Join(started, "\n"), shim.preWindowNote(t, root))
		}
	}
	if out, err := city.GC("resume"); err != nil {
		t.Fatalf("gc resume: %v\n%s", err, out)
	}
	if out, err := city.GC("bd", "list", "--json"); err != nil {
		t.Fatalf("gc bd list after resume: %v\n%s", err, out)
	}
}

// TestScopeRecordingBDResetKeepsAStraddlingRecordBeforeTheWindow pins the reset
// boundary: a record whose write was in flight at reset is not counted inside
// the window, and a record written whole after reset is.
func TestScopeRecordingBDResetKeepsAStraddlingRecordBeforeTheWindow(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	r := &scopeRecordingBD{logPath: logPath}
	before := "ts=1 beads_dir=/scope/.beads argv=show callers= [x]\n"
	head := "ts=2 beads_dir="
	tail := "/scope/.beads argv=list callers= [/scope/y]\n"
	after := "ts=3 beads_dir=/scope/.beads argv=dolt stop callers= [z]\n"
	if err := os.WriteFile(logPath, []byte(before+head), 0o644); err != nil {
		t.Fatal(err)
	}
	r.reset(t)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(tail + after); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got := r.callsUnder(t, "/scope")
	if len(got) != 1 || !strings.HasPrefix(got[0], "ts=3 ") {
		t.Fatalf("callsUnder after reset = %q, want only the ts=3 record", got)
	}
	if prior := underRoot(r.logLines(t, false), "/scope"); len(prior) != 2 {
		t.Fatalf("records before the window = %q, want the ts=1 record and the straddling ts=2 record", prior)
	}

	empty := &scopeRecordingBD{logPath: filepath.Join(t.TempDir(), "none.log")}
	empty.reset(t)
	if err := os.WriteFile(empty.logPath, []byte(after), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := empty.callsUnder(t, "/scope"); len(got) != 1 {
		t.Fatalf("callsUnder after a reset with no log = %q, want the one record", got)
	}
}
