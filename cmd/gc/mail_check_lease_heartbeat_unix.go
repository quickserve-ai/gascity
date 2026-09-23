//go:build (linux && !android) || (darwin && !ios)

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// spawnDetachedLeaseHeartbeat forks `gc hook heartbeat` in its own session
// (setsid) with the log file as its only output — never the caller's pipes,
// which the hook runner cuts after its grace delay (a child holding them
// stalls the turn). Start errors go to the same log; the turn never sees
// them.
func spawnDetachedLeaseHeartbeat(logPath string) {
	exe, err := os.Executable()
	if err != nil {
		appendLeaseHeartbeatLog(logPath, "spawn: resolving executable: "+err.Error())
		return
	}
	rotateLeaseHeartbeatLog(logPath)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer logf.Close() //nolint:errcheck
	cmd := exec.Command(exe, "hook", "heartbeat")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	appendLeaseHeartbeatLog(logPath, "spawn: gc hook heartbeat")
	if err := cmd.Start(); err != nil {
		appendLeaseHeartbeatLog(logPath, "spawn: "+err.Error())
		return
	}
	// Reap if the parent outlives the child; when the parent exits first the
	// detached child is re-parented and reaped by init.
	go func() { _ = cmd.Wait() }()
}
