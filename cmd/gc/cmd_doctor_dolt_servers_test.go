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
		layout: func(string) (managedDoltRuntimeLayout, error) {
			if layoutErr != nil {
				return managedDoltRuntimeLayout{}, layoutErr
			}
			return managedDoltRuntimeLayout{
				ConfigFile: filepath.Join(city, ".gc", "runtime", "packs", "dolt", "dolt-config.yaml"),
				DataDir:    filepath.Join(city, ".beads", "dolt"),
			}, nil
		},
		now: func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.Local) },
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
