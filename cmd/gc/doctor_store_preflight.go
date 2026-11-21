package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Bounds the single store probe that gates store-dependent doctor checks (#5064).
const doctorBeadStorePreflightTimeout = 5 * time.Second

// City + per-rig store checks skipped on outage-shaped preflight; keep in sync with buildDoctorChecks.
const (
	// Includes the carry's cloud-wake-bindings check, which reads the city store.
	doctorCityStoreCheckCount   = 20
	doctorPerRigStoreCheckCount = 3
)

// City-scoped store probe before store-dependent checks (also used at gc start warmup). Tests override.
var doctorBeadStorePreflight = defaultDoctorBeadStorePreflight

func defaultDoctorBeadStorePreflight(cityPath string, _ func(string) (beads.Store, error)) error {
	ctx, cancel := context.WithTimeout(context.Background(), doctorBeadStorePreflightTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	// NoRecovery + context-bound runner (O(1) list; process-group kill on timeout).
	env, err := bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, false)
	if err != nil {
		return err
	}
	// Exact env, built the way gc bd builds it: mergeRuntimeEnv strips the
	// ambient projected Dolt keys before applying the city's projection. A
	// layered runner only overwrites keys present in env, so a host the
	// projection deleted for a local target (a rig seat carries its rig's
	// BEADS_DOLT_SERVER_HOST) reached bd paired with the city port and the
	// preflight reported a false outage (ga-7v6z4k).
	exact := runtimeEnvEntriesToMap(mergeRuntimeEnv(processEnvSnapshotExcludingNativeDoltOpen(), env))
	_, err = beads.ExecCommandRunnerWithExactEnvContext(ctx, exact)(cityPath, "bd", "list", "--json", "--limit", "1")
	return err
}

// True for live store outages (breaker/conn/timeout), not missing/uninitialized stores.
//
// Superset of the transport shapes in bdTransportRetryableError
// (cmd/gc/bd_env.go) plus store-pool exhaustion. Deliberately separate:
// that list drives retry, this one drives check omission. Keep them from
// drifting when new bd/Dolt error shapes appear.
func isBeadStoreUnreachable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range []string{
		"dolt circuit breaker is open",
		"server appears down",
		"dolt server unreachable",
		"dolt server not reachable",
		"max waiting connections",
		"client rejected",
		"too many connections",
		"connection refused",
		"dial tcp",
		"bad connection",
		"invalid connection",
		"connection reset",
		"broken pipe",
		"i/o timeout",
		"timed out after",
		"context deadline exceeded",
		"unexpected eof",
		"use of closed network connection",
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

func beadStorePreflightSkipCount(activeRigCount int) int {
	return doctorCityStoreCheckCount + doctorPerRigStoreCheckCount*activeRigCount
}

func beadStorePreflightSkipMessage(skipCount, rigCount int, probeErr error) string {
	// City-scoped probe: skip is a city-outage gate (per-rig endpoints may differ).
	base := fmt.Sprintf(
		"bead store unreachable — skipped %d store checks (%d city, %d rigs); city store was probed (per-rig endpoints, including doltlite, may differ)",
		skipCount, doctorCityStoreCheckCount, rigCount,
	)
	if probeErr == nil {
		return base
	}
	return fmt.Sprintf("%s: %v", base, probeErr)
}
