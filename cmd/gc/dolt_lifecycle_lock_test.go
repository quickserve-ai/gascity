package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// startManagedLockTestCity returns a temp city whose lifecycle lock path is
// derived from the city alone, and an open file description on that lock
// holding LOCK_EX the way gc-beads-bd.sh op_start's `flock -n 9` does.
func startManagedLockTestCity(t *testing.T) (string, string, *os.File) {
	t.Helper()
	// The layout honors these overrides; a live seat's environment must never
	// steer this test onto a real city's lock file.
	for _, key := range []string{"GC_PACK_STATE_DIR", "GC_CITY_RUNTIME_DIR", "GC_DOLT_DATA_DIR", "GC_DOLT_LOG_FILE", "GC_DOLT_STATE_FILE", "GC_DOLT_PID_FILE", "GC_DOLT_LOCK_FILE", "GC_DOLT_CONFIG_FILE"} {
		t.Setenv(key, "")
	}
	cityPath := t.TempDir()
	holder, layout, err := openManagedDoltLifecycleLock(cityPath)
	if err != nil {
		t.Fatalf("openManagedDoltLifecycleLock: %v", err)
	}
	t.Cleanup(func() { releaseManagedDoltLifecycleLock(holder) })
	if !strings.HasPrefix(layout.LockFile, normalizePathForCompare(cityPath)) {
		t.Fatalf("lock file %s is outside the temp city %s", layout.LockFile, cityPath)
	}
	if locked, err := tryManagedDoltLifecycleLock(holder); err != nil || !locked {
		t.Fatalf("holder lock: locked=%v err=%v", locked, err)
	}
	return cityPath, layout.LockFile, holder
}

func lifecycleLockFreeForTest(t *testing.T, cityPath string) bool {
	t.Helper()
	probe, _, err := openManagedDoltLifecycleLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := tryManagedDoltLifecycleLock(probe)
	if err != nil {
		t.Fatal(err)
	}
	releaseManagedDoltLifecycleLock(probe)
	return locked
}

// ga-7yjvin: without a held fd, start-managed opens the lock afresh and the
// script's own lock refuses it. This is the collision that sent op_start to
// the bare, unwatched `dolt sql-server` start.
func TestStartManagedLifecycleLockFreshOpenIsRefusedByCallersLock(t *testing.T) {
	cityPath, _, _ := startManagedLockTestCity(t)
	if _, err := acquireStartManagedLifecycleLock(cityPath, -1); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("fresh-open admission err = %v, want busy", err)
	}
}

func TestStartManagedLifecycleLockAdmitsThroughCallersHeldFD(t *testing.T) {
	cityPath, _, holder := startManagedLockTestCity(t)
	// A dup shares the holder's open file description, as fd 9 does when
	// gc-beads-bd.sh hands it to the gc child.
	inherited, err := syscall.Dup(int(holder.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	release, err := acquireStartManagedLifecycleLock(cityPath, inherited)
	if err != nil {
		t.Fatalf("held-fd admission: %v", err)
	}
	flags, err := unix.FcntlInt(uintptr(inherited), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("held fd not close-on-exec (flags=%d err=%v): the spawned dolt server would inherit the lock", flags, err)
	}
	release()
	if lifecycleLockFreeForTest(t, cityPath) {
		t.Fatal("releasing a held-fd admission dropped the caller's lifecycle lock")
	}
}

// The held-fd path must not weaken the lock: an fd that names the lock file
// through a different open file description is still refused while another
// holder has it, and an fd on any other file is refused outright.
func TestStartManagedLifecycleLockHeldFDDoesNotBypassAnotherHolder(t *testing.T) {
	cityPath, lockPath, _ := startManagedLockTestCity(t)
	other, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	otherFD, err := syscall.Dup(int(other.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireStartManagedLifecycleLock(cityPath, otherFD); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("unheld fd admission err = %v, want busy", err)
	}
	wrong, err := os.Create(filepath.Join(t.TempDir(), "not-the-lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wrong.Close() }()
	wrongFD, err := syscall.Dup(int(wrong.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireStartManagedLifecycleLock(cityPath, wrongFD); err == nil || !strings.Contains(err.Error(), "is not the managed dolt lifecycle lock") {
		t.Fatalf("wrong-file fd admission err = %v, want refusal", err)
	}
}

// A standard descriptor is refused before it is adopted, and is left open:
// closing fd 1 would swallow the failure report the script parses, and a lock
// on fd 2 would ride into the watchdog, which is handed stderr.
func TestStartManagedLifecycleLockRejectsStandardDescriptors(t *testing.T) {
	cityPath, _, _ := startManagedLockTestCity(t)
	for fd := 0; fd <= 2; fd++ {
		if _, err := acquireStartManagedLifecycleLock(cityPath, fd); err == nil || !strings.Contains(err.Error(), "standard descriptor") {
			t.Fatalf("fd %d admission err = %v, want a standard-descriptor refusal", fd, err)
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			t.Fatalf("fd %d was closed by the refusal: %v", fd, err)
		}
	}
}

// gc-beads-bd.sh probes `start-managed --help` for --lifecycle-lock-fd before
// passing it, so an older gc falls back instead of failing the start. Pin
// that the hidden command's help lists the flag, exits 0, and runs nothing.
func TestStartManagedHelpListsLifecycleLockFD(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newDoltStateCmd(&stdout, &stderr)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"start-managed", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("start-managed --help: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "--lifecycle-lock-fd") {
		t.Fatalf("start-managed --help does not list --lifecycle-lock-fd:\n%s", stdout.String())
	}
	if strings.Contains(stderr.String(), "gc dolt-state start-managed:") {
		t.Fatalf("start-managed --help ran the command body:\n%s", stderr.String())
	}
}

func runStartManagedForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newDoltStateCmd(&stdout, &stderr)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"start-managed"}, args...))
	err := cmd.Execute()
	return stdout.String(), err
}

// ga-7yjvin: gc-beads-bd.sh reads start-managed's stdout only. A failure where
// a bare start could add a second server or override ownership must say
// refused<TAB>true; any other failure says error<TAB>reason without refused.
func TestStartManagedBusyLockReportsRefusedOnStdout(t *testing.T) {
	cityPath, _, _ := startManagedLockTestCity(t)
	out, err := runStartManagedForTest(t, "--city", cityPath, "--port", "3307")
	if err == nil {
		t.Fatalf("start-managed succeeded under a held lifecycle lock:\n%s", out)
	}
	if !strings.Contains(out, "refused\ttrue\n") || !strings.Contains(out, "error\tmanaged dolt lifecycle is busy\n") {
		t.Fatalf("busy lock stdout lacks refused/error lines:\n%s", out)
	}
}

func TestStartManagedOwnershipRefusalReportsRefusedOnStdout(t *testing.T) {
	city := handoffGuardTestCity(t)
	writeCommittedHandoffJournal(t, city)
	out, err := runStartManagedForTest(t, "--city", city, "--port", "3307")
	if err == nil {
		t.Fatalf("start-managed accepted a handed-off city:\n%s", out)
	}
	if !strings.Contains(out, "refused\ttrue\n") || !strings.Contains(out, "error\t") {
		t.Fatalf("ownership refusal stdout lacks refused/error lines:\n%s", out)
	}
}

func TestStartManagedPlainFailureReportsErrorWithoutRefused(t *testing.T) {
	cityPath, _, holder := startManagedLockTestCity(t)
	releaseManagedDoltLifecycleLock(holder)
	out, err := runStartManagedForTest(t, "--city", cityPath, "--port", "not-a-port")
	if err == nil {
		t.Fatalf("start-managed accepted an invalid port:\n%s", out)
	}
	if strings.Contains(out, "refused\t") {
		t.Fatalf("plain failure reported refused:\n%s", out)
	}
	if !strings.Contains(out, "error\tinvalid port \"not-a-port\"\n") {
		t.Fatalf("plain failure stdout lacks the error line:\n%s", out)
	}
}

func TestWriteStartManagedFailureKeepsReasonOnOneLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	writeStartManagedFailure(&stdout, &stderr, errors.New("disk\tlow\nsee log"), false)
	if got := stdout.String(); got != "error\tdisk low see log\n" {
		t.Fatalf("stdout = %q, want one error line with tabs/newlines flattened", got)
	}
}

// ga-7yjvin: once the scope watchdog has started, a failed handshake may leave
// a dolt sql-server behind, so the error is a refusal; a failure before the
// watchdog starts is not.
func TestScopeWatchdogSpawnFailureIsRefusedOnlyAfterStart(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dolt-config.yaml")
	if err := os.WriteFile(configPath, []byte("listener:\n  port: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(dir, "dolt.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logFile.Close() }()
	oldExecutable := managedDoltTestExecutable
	t.Cleanup(func() { managedDoltTestExecutable = oldExecutable })

	// Starts, then exits without the handshake line.
	silent := filepath.Join(dir, "silent-watchdog")
	if err := os.WriteFile(silent, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	managedDoltTestExecutable = func() (string, error) { return silent, nil }
	_, err = startManagedDoltSQLServerWithScopeWatchdogEnv("", configPath, logFile.Name(), logFile, os.Environ())
	if err == nil || !isManagedDoltStartRefusal(err) || !strings.Contains(err.Error(), "may have been spawned") {
		t.Fatalf("post-start handshake failure err = %v, want a refusal naming a possible server", err)
	}

	// Never starts.
	managedDoltTestExecutable = func() (string, error) { return filepath.Join(dir, "missing-gc"), nil }
	_, err = startManagedDoltSQLServerWithScopeWatchdogEnv("", configPath, logFile.Name(), logFile, os.Environ())
	if err == nil || isManagedDoltStartRefusal(err) {
		t.Fatalf("pre-start spawn failure err = %v, want a plain error", err)
	}
}
