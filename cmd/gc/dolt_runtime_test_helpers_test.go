package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func writeReachableManagedDoltState(t *testing.T, cityPath string) int {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dolt): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatalf("MkdirAll(city .beads): %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	port := ln.Addr().(*net.TCPAddr).Port
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltState: %v", err)
	}
	return port
}

func writeReachableProviderManagedDoltState(t *testing.T, cityPath string) int {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dolt): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll(city .beads/dolt): %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	port := ln.Addr().(*net.TCPAddr).Port
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath), doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("write provider Dolt state: %v", err)
	}
	return port
}

//nolint:unused // exercised by native_dolt_rebind_integration_test.go
func occupyManagedDoltPort(t *testing.T, port int) {
	t.Helper()

	cmd := exec.Command("python3", "-c", `
import signal
import socket
import sys
import time

port = int(sys.argv[1])
deadline = time.time() + 10.0
sock = None
while time.time() < deadline:
    candidate = socket.socket()
    candidate.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        candidate.bind(("127.0.0.1", port))
        candidate.listen(5)
        sock = candidate
        break
    except OSError:
        candidate.close()
        time.sleep(0.05)

if sock is None:
    raise SystemExit(3)

def _stop(*_args):
    raise SystemExit(0)

signal.signal(signal.SIGTERM, _stop)
signal.signal(signal.SIGINT, _stop)
while True:
    conn, _ = sock.accept()
    conn.close()
`, strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start managed port blocker: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("managed port blocker for %d exited early", port)
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("managed port blocker on %d did not become ready", port)
}

// startFakeOwnedManagedDolt starts a stand-in for the managed dolt sql-server
// that every check in the stop path accepts as ours. Ownership is decided by
// the process's argv carrying "--config <configFile>"
// (inspectManagedDoltOwnership → containsProcessConfig), so a shell spawned
// with those trailing arguments is indistinguishable from the real server to
// the inspection, and it can be scripted to answer SIGTERM the way dolt does —
// or to refuse it. The process is reaped in the background so it never lingers
// as a zombie that pidAlive would still report as alive.
func startFakeOwnedManagedDolt(t *testing.T, configFile, body string, env ...string) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process semantics required")
	}
	cmd := exec.Command("/bin/sh", "-c", body, "dolt", "sql-server", "--config", configFile)
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake owned managed dolt: %v", err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
		}
	})
	return pid
}

// waitForFakeManagedDoltReady blocks until the stand-in server has installed
// its signal disposition and touched its ready file. Without it a SIGTERM can
// land on a shell that has not yet run `trap`, which is a different scenario
// from the one under test.
func waitForFakeManagedDoltReady(t *testing.T, readyPath string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake managed dolt never became ready (%s)", readyPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
