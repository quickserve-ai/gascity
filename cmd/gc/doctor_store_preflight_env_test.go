package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installPreflightBdStub puts a stub bd first on PATH (and pins BD_BIN to it)
// that records the Dolt host it was given and answers an empty list.
func installPreflightBdStub(t *testing.T) string {
	t.Helper()
	stubDir := t.TempDir()
	record := filepath.Join(stubDir, "seen")
	script := "#!/bin/sh\n" +
		"printf 'host=%s:%s\\n' \"${BEADS_DOLT_SERVER_HOST+set}\" \"${BEADS_DOLT_SERVER_HOST}\" > " + record + "\n" +
		"echo '[]'\n"
	stub := filepath.Join(stubDir, "bd")
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_BIN", stub)
	return record
}

func readPreflightBdRecord(t *testing.T, record string) string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("stub bd never ran: %v", err)
	}
	return strings.TrimSpace(string(data))
}

// A rig seat carries its rig's BEADS_DOLT_SERVER_HOST. The city projection
// deletes the host for a local target; the preflight's bd must not inherit the
// seat's host and pair it with the city port (ga-7v6z4k).
func TestDoctorBeadStorePreflight_LocalCityDropsAmbientSeatHost(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "")
	_ = os.Unsetenv("GC_DOLT_HOST")
	t.Setenv("GC_DOLT_PORT", "")
	_ = os.Unsetenv("GC_DOLT_PORT")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-seat.example")
	record := installPreflightBdStub(t)

	cityPath := t.TempDir()
	if err := defaultDoctorBeadStorePreflight(cityPath, nil); err != nil {
		t.Fatalf("preflight error = %v", err)
	}
	if got := readPreflightBdRecord(t, record); got != "host=:" {
		t.Fatalf("bd saw %q, want host unset (the seat's ambient host leaked)", got)
	}
}

// Control: an external city target still delivers its projected host to bd,
// over a stale ambient BEADS_DOLT_SERVER_HOST.
func TestDoctorBeadStorePreflight_ExternalCityDeliversProjectedHost(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	host := reachableNonLoopbackHost(t)
	t.Setenv("GC_DOLT_HOST", host)
	t.Setenv("GC_DOLT_PORT", "9999")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-seat.example")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	record := installPreflightBdStub(t)

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }() //nolint:errcheck // test cleanup
	if err := writeDoltState(cityDir, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: "2026-04-02T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}

	if err := defaultDoctorBeadStorePreflight(cityDir, nil); err != nil {
		t.Fatalf("preflight error = %v", err)
	}
	if got, want := readPreflightBdRecord(t, record), "host=set:"+host; got != want {
		t.Fatalf("bd saw %q, want %q", got, want)
	}
}
