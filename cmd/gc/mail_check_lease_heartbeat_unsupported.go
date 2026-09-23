//go:build !((linux && !android) || (darwin && !ios))

package main

// spawnDetachedLeaseHeartbeat needs setsid; on platforms without it the tick
// is a recorded no-op (the lease refresher, like the reaper ordering it
// serves, is a unix-fleet concern — ga-56nq1a).
func spawnDetachedLeaseHeartbeat(logPath string) {
	appendLeaseHeartbeatLog(logPath, "spawn: unsupported platform, heartbeat not started")
}
