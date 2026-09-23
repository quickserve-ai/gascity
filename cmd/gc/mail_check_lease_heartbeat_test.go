package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// withLeaseHeartbeatSpawnCounter replaces the platform spawn with a counter.
func withLeaseHeartbeatSpawnCounter(t *testing.T) *int {
	t.Helper()
	restore := leaseHeartbeatSpawn
	t.Cleanup(func() { leaseHeartbeatSpawn = restore })
	count := 0
	leaseHeartbeatSpawn = func(string) { count++ }
	return &count
}

func leaseHeartbeatConfig(armed bool) *config.City {
	cfg := &config.City{}
	cfg.Beads.LeaseHeartbeat = armed
	return cfg
}

// TestMaybeSpawnLeaseHeartbeatIsOffByDefault pins the arming contract: an
// unarmed city (the default) never spawns, whatever the session env says —
// each city arms the refresher deliberately, because reap ordering depends on
// knowing where refresh is live (ga-56nq1a).
// TestLeaseHeartbeatLogRotatesPastTheCap pins the codex round-5 P2: the log
// is appended by every tick for the city's whole life, so once it passes the
// cap the next append moves it to <log>.1 (replacing the previous .1) and
// starts a fresh file; below the cap nothing moves.
func TestLeaseHeartbeatLogRotatesPastTheCap(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "heartbeat.log")
	appendLeaseHeartbeatLog(logPath, "first line")
	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Fatalf("a small log must not rotate; .1 stat err = %v", err)
	}
	if err := os.WriteFile(logPath, make([]byte, leaseHeartbeatLogMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	appendLeaseHeartbeatLog(logPath, "after cap")
	rotated, err := os.Stat(logPath + ".1")
	if err != nil {
		t.Fatalf("expected the oversized log at .1: %v", err)
	}
	if rotated.Size() <= leaseHeartbeatLogMaxBytes {
		t.Fatalf(".1 size = %d, want the oversized generation", rotated.Size())
	}
	fresh, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fresh log missing after rotation: %v", err)
	}
	if !strings.Contains(string(fresh), "after cap") || len(fresh) > 200 {
		t.Fatalf("fresh log = %q (%d bytes), want only the post-rotation line", string(fresh), len(fresh))
	}
	// A second rotation replaces the earlier .1 rather than accumulating.
	if err := os.WriteFile(logPath, make([]byte, leaseHeartbeatLogMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	appendLeaseHeartbeatLog(logPath, "second cap")
	if _, err := os.Stat(logPath + ".1.1"); !os.IsNotExist(err) {
		t.Fatalf("rotation must keep ONE generation; .1.1 stat err = %v", err)
	}
}

func TestMaybeSpawnLeaseHeartbeatIsOffByDefault(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "mc-sess1")
	count := withLeaseHeartbeatSpawnCounter(t)
	maybeSpawnLeaseHeartbeat(t.TempDir(), leaseHeartbeatConfig(false))
	maybeSpawnLeaseHeartbeat(t.TempDir(), nil)
	if *count != 0 {
		t.Fatalf("spawns = %d, want 0 while the city has not armed lease_heartbeat", *count)
	}
}

// TestMaybeSpawnLeaseHeartbeatNeedsASessionIdentity: no GC_SESSION_ID means no
// rows could be resolved anyway, so nothing spawns.
func TestMaybeSpawnLeaseHeartbeatNeedsASessionIdentity(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "")
	count := withLeaseHeartbeatSpawnCounter(t)
	maybeSpawnLeaseHeartbeat(t.TempDir(), leaseHeartbeatConfig(true))
	if *count != 0 {
		t.Fatalf("spawns = %d, want 0 without a session identity", *count)
	}
}

// TestMaybeSpawnLeaseHeartbeatThrottlesPerSession is the cost contract: the
// first armed call spawns and stamps; a second call inside the throttle window
// costs one stat and spawns nothing; an expired stamp spawns again.
func TestMaybeSpawnLeaseHeartbeatThrottlesPerSession(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "mc-sess1")
	count := withLeaseHeartbeatSpawnCounter(t)
	cityPath := t.TempDir()
	cfg := leaseHeartbeatConfig(true)

	maybeSpawnLeaseHeartbeat(cityPath, cfg)
	maybeSpawnLeaseHeartbeat(cityPath, cfg)
	if *count != 1 {
		t.Fatalf("spawns = %d, want 1 (second call inside the throttle window)", *count)
	}

	stamp := filepath.Join(cityPath, ".gc", "runtime", "lease-heartbeat", leaseHeartbeatStampName("mc-sess1"))
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("stamp file missing after spawn: %v", err)
	}
	expired := time.Now().Add(-leaseHeartbeatThrottle - time.Minute)
	if err := os.Chtimes(stamp, expired, expired); err != nil {
		t.Fatal(err)
	}
	maybeSpawnLeaseHeartbeat(cityPath, cfg)
	if *count != 2 {
		t.Fatalf("spawns = %d, want 2 after the stamp expired", *count)
	}
}

// TestMaybeSpawnLeaseHeartbeatThrottleIsPerSession: two sessions do not share
// a stamp — each session's leases are its own to keep alive.
func TestMaybeSpawnLeaseHeartbeatThrottleIsPerSession(t *testing.T) {
	count := withLeaseHeartbeatSpawnCounter(t)
	cityPath := t.TempDir()
	cfg := leaseHeartbeatConfig(true)

	t.Setenv("GC_SESSION_ID", "mc-sess1")
	maybeSpawnLeaseHeartbeat(cityPath, cfg)
	t.Setenv("GC_SESSION_ID", "mc-sess2")
	maybeSpawnLeaseHeartbeat(cityPath, cfg)
	if *count != 2 {
		t.Fatalf("spawns = %d, want one per session", *count)
	}
}
