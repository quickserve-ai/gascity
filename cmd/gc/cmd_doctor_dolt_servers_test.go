package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// doltServersFixture builds a check over a fake city at /city with one rig
// nested under the city root, the layout real cities use.
func doltServersFixture(t *testing.T, procs []DoltProcInfo, discoverErr, layoutErr error) *doltServersCheck {
	t.Helper()
	city := "/city"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "app", Path: filepath.Join(city, "rigs", "app")}},
	}
	return &doltServersCheck{
		cityPath: city,
		cfg:      cfg,
		discover: func() ([]DoltProcInfo, error) { return procs, discoverErr },
		layout: func(scope string) (managedDoltRuntimeLayout, error) {
			if layoutErr != nil {
				return managedDoltRuntimeLayout{}, layoutErr
			}
			return managedDoltRuntimeLayout{
				ConfigFile: filepath.Join(scope, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
				DataDir:    filepath.Join(scope, ".beads", "dolt"),
			}, nil
		},
		otherScopes:     func(string) ([]string, error) { return []string{"/other-city"}, nil },
		cwd:             func(int) (string, bool) { return "", false },
		args:            func(int) (string, error) { return "", errors.New("no full command line in this fixture") },
		recordedPID:     func(managedDoltRuntimeLayout) int { return 0 },
		activeTestRoots: func() []string { return nil },
		startIdentity:   func(int) string { return "" },
		homeDir:         "/home/me",
		tempDir:         "/var/folders/T",
		now:             func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.Local) },
	}
}

func gcDoltProc(pid int, scope string, ports ...int) DoltProcInfo {
	return DoltProcInfo{
		PID:   pid,
		Argv:  []string{"dolt", "sql-server", "--config", filepath.Join(scope, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml")},
		Ports: ports,
	}
}

func TestDoltServersCheck_EnumerationFailureIsSkippedNeverOK(t *testing.T) {
	c := doltServersFixture(t, nil, errors.New("ps: operation not permitted"), nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusSkipped {
		t.Fatalf("status = %v, want StatusSkipped; a check that could not look must not read OK (msg %q)", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "operation not permitted") {
		t.Fatalf("message must name the reason, got %q", r.Message)
	}
}

func TestDoltServersCheck_OnlyManagedServerIsOK(t *testing.T) {
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361)}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK: %q %v", r.Status, r.Message, r.Details)
	}
}

// The case the bead was filed on: a second server on the city's own store.
func TestDoltServersCheck_TwoServersOnManagedStoreIsError(t *testing.T) {
	c := doltServersFixture(t, []DoltProcInfo{
		gcDoltProc(100, "/city", 51361),
		gcDoltProc(200, "/city"), // wedged orphan: no ports visible
	}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error for split-brain: %q", r.Status, r.Message)
	}
	for _, pid := range []string{"pid 100", "pid 200"} {
		if !strings.Contains(r.Message, pid) {
			t.Fatalf("message must name both servers, missing %q: %q", pid, r.Message)
		}
	}
}

// The field case (ga-rd381q): scratch cities left running with no marker file.
func TestDoltServersCheck_ForeignGCLaunchedServerIsWarning(t *testing.T) {
	foreign := gcDoltProc(300, "/tmp/gc-scratch/cmd/gc", 10977)
	foreign.StartIdentity = "Tue Sep  8 12:00:00 2026"
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), foreign}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %q", r.Status, r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	for _, want := range []string{"pid 300", "up 14d", "port 10977", "/tmp/gc-scratch/cmd/gc/.gc/runtime/packs/dolt/dolt-config.yaml"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("details missing %q:\n%s", want, joined)
		}
	}
}

func TestDoltServersCheck_NonGCDoltIsCountedNotWarned(t *testing.T) {
	userDolt := DoltProcInfo{PID: 400, Argv: []string{"dolt", "sql-server", "--data-dir", "/home/me/mydb"}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), userDolt}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK (not gc's server): %q", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "1 not gc-launched") {
		t.Fatalf("the non-gc server must still be counted: %v", r.Details)
	}
}

// Rigs live under the city root. First-match ownership would call a
// rig-local server an HQ stray; the deepest root must win.
func TestDoltServersCheck_RigNestedUnderCityIsRigLocalNotStray(t *testing.T) {
	c := doltServersFixture(t, []DoltProcInfo{
		gcDoltProc(100, "/city", 51361),
		gcDoltProc(500, "/city/rigs/app", 40000),
	}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK (rig-local is dolt-drift's call): %q %v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "1 rig-local") {
		t.Fatalf("want 1 rig-local in details: %v", r.Details)
	}
}

func TestDoltServersCheck_UnderCityRootButNotManagedIsStray(t *testing.T) {
	stray := DoltProcInfo{PID: 600, Argv: []string{"dolt", "sql-server", "--data-dir", "/city/.beads/old-dolt"}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), stray}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %q", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "under the city root") {
		t.Fatalf("details must call it a city stray: %v", r.Details)
	}
}

// Without the layout, the split-brain comparison never ran. Seeing nothing
// else must not render as OK.
func TestDoltServersCheck_LayoutUnresolvableIsNotOK(t *testing.T) {
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361)}, nil, errors.New("no runtime dir"))
	r := c.Run(nil)
	if r.Status == doctor.StatusOK {
		t.Fatalf("status = OK with an unresolvable layout; duplicate analysis did not run: %q", r.Message)
	}
}

// Ports come from a best-effort lsof on hosts without /proc. A server with no
// visible ports must be classified exactly as one with ports.
func TestDoltServersCheck_MissingPortsDoNotChangeClassification(t *testing.T) {
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city"), gcDoltProc(200, "/city")}, nil, nil)
	if r := c.Run(nil); r.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error with no ports visible: %q", r.Status, r.Message)
	}
}

func TestDoltServersCheck_UnparseableStartIdentityOmitsAge(t *testing.T) {
	c := doltServersFixture(t, nil, nil, nil)
	p := gcDoltProc(1, "/x")
	p.StartIdentity = "not a date"
	if got := c.describe(p); strings.Contains(got, "up ") {
		t.Fatalf("describe guessed an age from garbage: %q", got)
	}
}

// A multi-city host runs one managed server per registered city. Those are
// not orphans.
func TestDoltServersCheck_OtherRegisteredCityIsCountedNotWarned(t *testing.T) {
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), gcDoltProc(700, "/other-city", 51362)}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK: %q %v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "1 other registered city") {
		t.Fatalf("want it counted as another city: %v", r.Details)
	}
}

// An unreadable registry must fail toward visible: the server is still
// warned about, and the output says why the verdict may over-report.
func TestDoltServersCheck_UnreadableRegistryStillWarns(t *testing.T) {
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), gcDoltProc(700, "/other-city", 51362)}, nil, nil)
	c.otherScopes = func(string) ([]string, error) { return nil, errors.New("lock held") }
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %q", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "registry unreadable") {
		t.Fatalf("details must say the registry was unreadable: %v", r.Details)
	}
}

// `bd dolt start` launches `dolt sql-server -H .. -P ..` from the data dir:
// no --config, no --data-dir. Identified by cwd, on the managed data dir, it
// is a second server on the city's store.
func TestDoltServersCheck_BdDoltStartOnManagedStoreIsError(t *testing.T) {
	bdStarted := DoltProcInfo{PID: 800, Argv: []string{"dolt", "sql-server"}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), bdStarted}, nil, nil)
	c.cwd = func(pid int) (string, bool) {
		if pid == 800 {
			return "/city/.beads/dolt", true
		}
		return "", false
	}
	r := c.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error: %q %v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(r.Message, "cwd /city/.beads/dolt") {
		t.Fatalf("message must show how pid 800 was identified: %q", r.Message)
	}
}

// With no flags and no readable cwd the check cannot rule out the city's
// store, so it must not count the server as someone else's.
func TestDoltServersCheck_UnidentifiableServerIsWarningNotOK(t *testing.T) {
	bare := DoltProcInfo{PID: 801, Argv: []string{"dolt", "sql-server"}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), bare}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %q %v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "cannot rule out") {
		t.Fatalf("details must say why: %v", r.Details)
	}
}

// dolt-drift judges rig-local servers from .dolt/sql-server.info, a marker
// file. Two servers on one rig store must be caught here.
func TestDoltServersCheck_TwoServersOnOneRigStoreIsError(t *testing.T) {
	c := doltServersFixture(t, []DoltProcInfo{
		gcDoltProc(100, "/city", 51361),
		gcDoltProc(500, "/city/rigs/app", 40000),
		gcDoltProc(501, "/city/rigs/app"),
	}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "one rig store") {
		t.Fatalf("message must name the rig split-brain: %q", r.Message)
	}
}

func TestDoltServersCheck_TestServersActiveCountedOrphanWarned(t *testing.T) {
	active := gcDoltProc(900, "/tmp/TestLive123/city")
	orphan := gcDoltProc(901, "/tmp/TestDead456/city")
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), active, orphan}, nil, nil)
	c.activeTestRoots = func() []string { return []string{"/tmp/TestLive123"} }
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %q", r.Status, r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	if strings.Contains(joined, "pid 900") || !strings.Contains(joined, "pid 901") {
		t.Fatalf("want only the orphan (901) warned:\n%s", joined)
	}
	if !strings.Contains(r.FixHint, "gc dolt cleanup") {
		t.Fatalf("orphan test servers are cleanup's job; hint = %q", r.FixHint)
	}
}

// If the pid the runtime recorded is alive but did not match the layout, a
// duplicate of it is undetectable. Say so instead of reading OK.
func TestDoltServersCheck_RecordedPIDOutsideManagedIsWarning(t *testing.T) {
	odd := DoltProcInfo{PID: 150, Argv: []string{"dolt", "sql-server", "--config", "/elsewhere/dolt.yaml"}}
	c := doltServersFixture(t, []DoltProcInfo{odd}, nil, nil)
	c.recordedPID = func(managedDoltRuntimeLayout) int { return 150 }
	r := c.Run(nil)
	if r.Status == doctor.StatusOK {
		t.Fatalf("status = OK although the recorded managed pid matched no layout: %v", r.Details)
	}
}

func TestTrimFlattenedDoltArgs_NeverCarriesTrailingFlags(t *testing.T) {
	got := trimFlattenedDoltArgs("/city/x.yaml -u root -p hunter2")
	if got != "/city/x.yaml" {
		t.Fatalf("displayDoltPath = %q", got)
	}
}

func TestDoltServersCheck_AgeFallsBackToPSWhenDiscoveryLeftItEmpty(t *testing.T) {
	c := doltServersFixture(t, nil, nil, nil)
	c.startIdentity = func(int) string { return "Tue Sep 22 09:00:00 2026" }
	if got := c.describe(gcDoltProc(1, "/x")); !strings.Contains(got, "up 3h") {
		t.Fatalf("describe = %q, want up 3h", got)
	}
}

// A gc-launched rig server (config) and a `bd dolt start` server (cwd on the
// rig's data dir) serve ONE store through different identities.
func TestDoltServersCheck_RigSplitBrainAcrossIdentitiesIsError(t *testing.T) {
	bdStarted := DoltProcInfo{PID: 502, Argv: []string{"dolt", "sql-server"}}
	c := doltServersFixture(t, []DoltProcInfo{
		gcDoltProc(100, "/city", 51361),
		gcDoltProc(500, "/city/rigs/app", 40000),
		bdStarted,
	}, nil, nil)
	c.cwd = func(pid int) (string, bool) {
		if pid == 502 {
			return "/city/rigs/app/.beads/dolt", true
		}
		return "", false
	}
	if r := c.Run(nil); r.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error: %q %v", r.Status, r.Message, r.Details)
	}
}

// macOS ps discovery flattens the command line: the recovered --config can
// carry the flags after it. That must not defeat the managed-layout match.
func TestDoltServersCheck_FlattenedConfigStillMatchesManagedLayout(t *testing.T) {
	flat := gcDoltProc(200, "/city")
	flat.Argv[len(flat.Argv)-1] += " -u root -p hunter2"
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), flat}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error (a duplicate managed server): %q %v", r.Status, r.Message, r.Details)
	}
	if strings.Contains(r.Message+strings.Join(r.Details, "\n"), "hunter2") {
		t.Fatalf("credential reached doctor output: %q", r.Message)
	}
}

// Codex round 2 on #120 (a): a relative --config/--data-dir is relative to the
// SERVER's working directory, not doctor's. A second server started as
// `--config dolt-config.yaml` from the managed pack dir is on this city's store.
func TestDoltServersCheck_RelativeConfigResolvesAgainstServerCWD(t *testing.T) {
	second := DoltProcInfo{PID: 200, Argv: []string{"dolt", "sql-server", "--config", "dolt-config.yaml"}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), second}, nil, nil)
	c.cwd = func(pid int) (string, bool) {
		if pid == 200 {
			return filepath.Join("/city", ".gc", "runtime", "packs", "dolt"), true
		}
		return "", false
	}
	r := c.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error (split-brain via a relative --config): %q %v", r.Status, r.Message, r.Details)
	}
}

func TestDoltServersCheck_RelativeDataDirResolvesAgainstServerCWD(t *testing.T) {
	second := DoltProcInfo{PID: 200, Argv: []string{"dolt", "sql-server", "--data-dir", filepath.Join(".beads", "dolt")}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), second}, nil, nil)
	c.cwd = func(pid int) (string, bool) {
		if pid == 200 {
			return "/city", true
		}
		return "", false
	}
	r := c.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error (split-brain via a relative --data-dir): %q %v", r.Status, r.Message, r.Details)
	}
}

// A relative path whose anchor cannot be read is UNIDENTIFIABLE, never
// "not gc": the check cannot rule out this city's store.
func TestDoltServersCheck_RelativePathWithUnreadableCWDIsWarningNotOK(t *testing.T) {
	second := DoltProcInfo{PID: 200, Argv: []string{"dolt", "sql-server", "--config", "dolt-config.yaml"}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), second}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %q %v", r.Status, r.Message, r.Details)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "relative") {
		t.Fatalf("the warning must say the path was relative, got %v", r.Details)
	}
}

// Codex round 2 on #120 (b): the macOS ps fallback keeps only --config, so a
// server started with --data-dir from an unrelated cwd must be re-read from
// its full command line, not identified by that cwd.
func TestDoltServersCheck_PSFallbackRecoversDataDirFromFullArgs(t *testing.T) {
	second := DoltProcInfo{PID: 200, Argv: []string{"dolt", "sql-server"}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), second}, nil, nil)
	c.cwd = func(int) (string, bool) { return "/elsewhere", true }
	c.args = func(pid int) (string, error) {
		if pid == 200 {
			return "/usr/local/bin/dolt sql-server --data-dir /city/.beads/dolt -u root -p secret", nil
		}
		return "", errors.New("unexpected pid")
	}
	r := c.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error (managed store found via the full command line): %q %v", r.Status, r.Message, r.Details)
	}
	if strings.Contains(r.Message+strings.Join(r.Details, " "), "secret") {
		t.Fatalf("doctor output must never carry the command line's trailing flags: %q %v", r.Message, r.Details)
	}
}

// Codex round 3 on #120 (a): a rig that inherits the city's dolt endpoint
// should run NO rig-local server. A single markerless one is the orphan this
// check exists for, and dolt-drift sees it only via .dolt/sql-server.info.
func TestDoltServersCheck_SingletonOnInheritedRigStoreIsError(t *testing.T) {
	bdStarted := DoltProcInfo{PID: 502, Argv: []string{"dolt", "sql-server"}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), bdStarted}, nil, nil)
	c.cwd = func(pid int) (string, bool) {
		if pid == 502 {
			return "/city/rigs/app/.beads/dolt", true
		}
		return "", false
	}
	c.inheritedRigs = func() (map[string]string, error) {
		return map[string]string{"/city/rigs/app": "app"}, nil
	}
	r := c.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error: %q %v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(r.Message, `rig "app" inherits`) {
		t.Fatalf("message must name the inherited rig: %q", r.Message)
	}
}

func TestDoltServersCheck_SingletonElsewhereUnderInheritedRigIsWarning(t *testing.T) {
	c := doltServersFixture(t, []DoltProcInfo{
		gcDoltProc(100, "/city", 51361),
		{PID: 503, Argv: []string{"dolt", "sql-server", "--data-dir", "/city/rigs/app/scratch/db"}},
	}, nil, nil)
	c.inheritedRigs = func() (map[string]string, error) {
		return map[string]string{"/city/rigs/app": "app"}, nil
	}
	if r := c.Run(nil); r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %q %v", r.Status, r.Message, r.Details)
	}
}

// The control: the same singleton on an EXPLICIT rig is that rig's own server.
func TestDoltServersCheck_SingletonOnExplicitRigIsOK(t *testing.T) {
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), gcDoltProc(500, "/city/rigs/app", 40000)}, nil, nil)
	c.inheritedRigs = func() (map[string]string, error) { return map[string]string{}, nil }
	if r := c.Run(nil); r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK: %q %v", r.Status, r.Message, r.Details)
	}
}

func TestDoltServersCheck_UnresolvableEndpointOriginIsWarningNotOK(t *testing.T) {
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), gcDoltProc(500, "/city/rigs/app", 40000)}, nil, nil)
	c.inheritedRigs = func() (map[string]string, error) { return nil, errors.New("bad config.yaml") }
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning: %q %v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "not checked") {
		t.Fatalf("the warning must say the singleton analysis did not run: %v", r.Details)
	}
}

// Codex round 3 on #120 (b): a config outside the rig must not hide a
// data-dir on the rig's store. The data-dir is what the server serves.
func TestDoltServersCheck_ConfigElsewhereDataDirOnRigStoreIsSplitBrain(t *testing.T) {
	dup := DoltProcInfo{PID: 504, Argv: []string{"dolt", "sql-server", "--config", "/elsewhere/dolt.yaml", "--data-dir", "/city/rigs/app/.beads/dolt"}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), gcDoltProc(500, "/city/rigs/app", 40000), dup}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusError || !strings.Contains(r.Message, "one rig store") {
		t.Fatalf("status = %v, want the rig split-brain Error: %q %v", r.Status, r.Message, r.Details)
	}
}

// The same, on the ps fallback: argv kept only --config, the data-dir comes
// from the full command line.
func TestDoltServersCheck_ConfigElsewhereDataDirRecoveredFromPS(t *testing.T) {
	dup := DoltProcInfo{PID: 504, Argv: []string{"dolt", "sql-server", "--config", "/elsewhere/dolt.yaml"}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), dup}, nil, nil)
	c.args = func(pid int) (string, error) {
		if pid == 504 {
			return "dolt sql-server --config /elsewhere/dolt.yaml --data-dir /city/.beads/dolt -u root", nil
		}
		return "", errors.New("unexpected pid")
	}
	if r := c.Run(nil); r.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error (duplicate managed server): %q %v", r.Status, r.Message, r.Details)
	}
}

// Codex round 3 on #120 (c): a spaced data-dir recovered from flat ps text
// keeps its whole path.
func TestDoltServersCheck_PSFallbackKeepsSpacedDataDir(t *testing.T) {
	second := DoltProcInfo{PID: 200, Argv: []string{"dolt", "sql-server"}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), second}, nil, nil)
	base := c.layout
	c.layout = func(scope string) (managedDoltRuntimeLayout, error) {
		l, err := base(scope)
		l.DataDir = filepath.Join(scope, "My Store", "dolt")
		return l, err
	}
	c.cwd = func(int) (string, bool) { return "/elsewhere", true }
	c.args = func(pid int) (string, error) {
		return "/usr/local/bin/dolt sql-server --data-dir /city/My Store/dolt -u root -p secret", nil
	}
	r := c.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error (managed store at a spaced path): %q %v", r.Status, r.Message, r.Details)
	}
	if strings.Contains(r.Message+strings.Join(r.Details, " "), "secret") {
		t.Fatalf("doctor output must never carry trailing flags: %q %v", r.Message, r.Details)
	}
}

func TestFlattenedFlagValue(t *testing.T) {
	for _, tc := range []struct{ args, want string }{
		{"dolt sql-server --data-dir /a b/c -u root", "/a b/c"},
		{"dolt sql-server --data-dir=/a b/c", "/a b/c"},
		{"dolt sql-server --data-dir  /x", "/x"},
		{"dolt sql-server --data-dir -u root", ""},
		{"dolt sql-server --data-directory /x", ""},
		{"dolt sql-server x--data-dir /x", ""},
		{"dolt sql-server --config /c.yaml", ""},
	} {
		if got := flattenedFlagValue(tc.args, "--data-dir"); got != tc.want {
			t.Errorf("flattenedFlagValue(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// Codex round 4 on #120 (a): a relative --data-dir is the SERVER's, not
// doctor's. Run from the city root, `--data-dir .beads/dolt` from a server
// started elsewhere must not read as a second managed server.
func TestDoltServersCheck_RelativeDataDirNeverMatchesAgainstDoctorCWD(t *testing.T) {
	city := t.TempDir()
	t.Chdir(city)
	elsewhere := DoltProcInfo{PID: 200, Argv: []string{"dolt", "sql-server", "--data-dir", filepath.Join(".beads", "dolt")}}
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, city, 51361), elsewhere}, nil, nil)
	c.cityPath = city
	c.cfg = &config.City{Workspace: config.Workspace{Name: "demo"}}
	c.cwd = func(pid int) (string, bool) {
		if pid == 200 {
			return "/elsewhere", true
		}
		return "", false
	}
	r := c.Run(nil)
	if r.Status == doctor.StatusError {
		t.Fatalf("a server on /elsewhere/.beads/dolt was counted as this city's managed server: %q %v", r.Message, r.Details)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "1 managed") {
		t.Fatalf("want exactly 1 managed: %v", r.Details)
	}
}

// Codex round 4 on #120 (b): an explicit --data-dir names the store served,
// even beside the managed --config. That server is the rig's, not a second
// managed server.
func TestDoltServersCheck_DataDirTakesPrecedenceOverManagedConfig(t *testing.T) {
	onRig := gcDoltProc(200, "/city")
	onRig.Argv = append(onRig.Argv, "--data-dir", "/city/rigs/app/.beads/dolt")
	c := doltServersFixture(t, []DoltProcInfo{gcDoltProc(100, "/city", 51361), onRig}, nil, nil)
	r := c.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK (1 managed + 1 rig-local): %q %v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(r.Message, "1 managed, 1 rig-local") {
		t.Fatalf("the data dir must decide the store: %q", r.Message)
	}
}
