package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// Lease-heartbeat placement (ga-56nq1a stage 1, hook-seam consult 2026-09-23).
//
// The tick rides the start-of-turn mail check because that is the only
// per-turn path EVERY managed provider already traverses: claude and codex
// run `gc mail check --inject` from UserPromptSubmit, and OMP calls it
// directly from before_agent_start (bypassing `gc hook run`, which is why the
// tick lives in the mail-check code path and not in the hook-run wrapper or a
// new hook entry — a new entry would rewrite provider hook files fleet-wide).
// gc installs no Stop hook on any provider, so end-of-turn is not a seam.
//
// The budget is ZERO synchronous cost: a bd invocation costs seconds, so the
// tick forks a detached `gc hook heartbeat` with its OWN log-file
// descriptors — never the inherited pipes, which a hook runner cuts after a
// grace delay — and throttles itself with a per-session stamp file at
// roughly TTL/3, so non-beating turns cost one stat. It writes NOTHING to
// stdout: injected hook stdout becomes provider context.
//
// Known gaps, recorded on ga-56nq1a and deliberately not closed here:
// mid-turn (a long autonomous turn fires no start-of-turn event; stage 2 adds
// a throttled PostToolUse leg), parked in_progress work between turns, and a
// seat whose portfolio spans the other store.

// leaseHeartbeatThrottle is the minimum interval between spawns per session.
// It is derived from bd's DEFAULT lease TTL (5 minutes), not from the ~30m
// TTL the ga-56nq1a plan raises cities to at arming time: a city that arms
// beads.lease_heartbeat without also raising lease.ttl must still refresh
// faster than its leases expire, or the feature strands the very claims it
// exists to keep alive (codex round-2 P1 on PR #128). 90s gives three beats
// per default TTL window; under a raised 30m TTL it is simply generous
// margin. A follow-up may read the effective lease.ttl and relax the cadence;
// the floor here must always assume the DEFAULT.
const leaseHeartbeatThrottle = 90 * time.Second

// leaseHeartbeatSpawn forks the detached heartbeat; platform-specific
// (spawn requires setsid), overridable in tests.
var leaseHeartbeatSpawn = spawnDetachedLeaseHeartbeat

// maybeSpawnLeaseHeartbeat is the whole tick: gated on the city-level
// beads.lease_heartbeat flag and a session identity, throttled by stamp-file
// mtime, then fire-and-forget. Every failure is silent toward the turn (the
// log file carries what the stage-2 observer needs); it never writes to the
// caller's stdout or stderr.
func maybeSpawnLeaseHeartbeat(cityPath string, cfg *config.City) {
	if cfg == nil || !cfg.Beads.LeaseHeartbeat {
		return
	}
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	if sessionID == "" {
		return
	}
	dir := filepath.Join(cityPath, ".gc", "runtime", "lease-heartbeat")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	stamp := filepath.Join(dir, leaseHeartbeatStampName(sessionID))
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < leaseHeartbeatThrottle {
		return
	}
	// Stamp BEFORE spawning: a racing pair of turns costs one duplicate
	// heartbeat, while stamping only after a successful spawn would let a
	// persistently failing spawn retry every turn at full frequency.
	if err := os.WriteFile(stamp, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return
	}
	leaseHeartbeatSpawn(filepath.Join(dir, "heartbeat.log"))
}

// leaseHeartbeatStampName maps a session id to a safe stamp filename.
func leaseHeartbeatStampName(sessionID string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '.':
			return '-'
		}
		return r
	}, sessionID)
	return sanitized + ".stamp"
}

// appendLeaseHeartbeatLog best-effort appends one stamped line to the tick's
// log file — the countable trace for spawn-side failures, which have no other
// observer (the turn must stay clean and the child does not exist yet).
func appendLeaseHeartbeatLog(logPath, message string) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), message) //nolint:errcheck
}
