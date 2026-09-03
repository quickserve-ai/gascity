package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/liveness"
)

// This file binds the non-versioned session-liveness store (internal/liveness)
// to a bead-store scope. See internal/liveness/keys.go for why the fields moved.
//
// Resolution rule: the liveness table lives on the SAME managed Dolt database
// that holds the scope's `issues` table, resolved through exactly the helper the
// native-store preflight already uses (canonicalScopeDoltTarget +
// managedDoltOpenDatabase). It therefore works identically whether the scope's
// beads.Store resolved to NativeDoltStore or fell back to the exec/bd BdStore —
// the fallback has no in-process SQL handle to borrow, which is why this opens
// its own.
//
// Every failure mode degrades to "no liveness store", and the splitter then
// passes writes through to versioned metadata exactly as before. A scope with no
// Dolt at all (file / doltlite provider, and every unit test working in a temp
// dir) resolves non-authoritative and gets a nil binding.

// livenessOpenRetryInterval bounds how often a REACHABILITY failure is retried.
// Without it a scope whose Dolt server is briefly down would re-dial on every
// single store operation.
const livenessOpenRetryInterval = 30 * time.Second

// livenessNoEndpointRetryInterval bounds how often a scope with NO Dolt endpoint
// re-resolves. Such a scope — a file/doltlite provider, an unconfigured scope, a
// test temp dir — cannot acquire one without a config change, so retrying it on
// the reachability cadence is pure waste: it re-reads the scope's config files
// every 30 seconds, forever, for every scope the process ever touched. A long
// interval still lets a genuine config change be picked up eventually without
// making a no-Dolt city pay for the check.
const livenessNoEndpointRetryInterval = 10 * time.Minute

// livenessOpenTimeout bounds the dial + schema-seed round trip.
const livenessOpenTimeout = 10 * time.Second

// livenessOpTimeout bounds one liveness read or write. Liveness is telemetry:
// it must never be the thing that wedges a lifecycle transition.
const livenessOpTimeout = 5 * time.Second

// livenessBinding is a lazily-opened, memoized liveness store for one bead-store
// scope. A nil *livenessBinding is a valid value meaning "liveness disabled";
// every method tolerates it.
type livenessBinding struct {
	cityPath  string
	scopeRoot string
	mode      liveness.Mode

	mu          sync.Mutex
	store       liveness.Store
	lastAttempt time.Time
	retryAfter  time.Duration
	dialing     bool
	warned      bool
	// clockOffset is the last known (server - local) skew, retained across a
	// retired pool. Fallback stamps are minted exactly when the store is
	// UNAVAILABLE, so falling back to the raw local clock at that moment would
	// mint the fence in the wrong timebase and it would fence nothing (or
	// everything). The last measured offset is a far better estimate than zero.
	clockOffset     time.Duration
	haveClockOffset bool
}

// livenessBindings memoizes one binding per scope root so a process opens at
// most one liveness connection pool per scope no matter how many bead stores it
// builds over that scope.
var (
	livenessBindingsMu sync.Mutex
	livenessBindings   = map[string]*livenessBinding{}
)

// sessionLivenessFor returns the memoized liveness binding for a scope. It never
// dials here: the first store operation triggers the open.
func sessionLivenessFor(cityPath, scopeRoot string) *livenessBinding {
	scopeRoot = strings.TrimSpace(scopeRoot)
	if scopeRoot == "" {
		return nil
	}
	key := filepath.Clean(scopeRoot)
	livenessBindingsMu.Lock()
	defer livenessBindingsMu.Unlock()
	if b, ok := livenessBindings[key]; ok {
		return b
	}
	b := &livenessBinding{
		cityPath:  strings.TrimSpace(cityPath),
		scopeRoot: key,
		mode:      liveness.ModeFromEnv(),
	}
	livenessBindings[key] = b
	return b
}

// resetSessionLivenessBindingsForTest drops the memo table. Tests that install a
// fake binding use it so state does not leak between cases.
func resetSessionLivenessBindingsForTest() {
	livenessBindingsMu.Lock()
	defer livenessBindingsMu.Unlock()
	livenessBindings = map[string]*livenessBinding{}
}

// Mode reports the write discipline this binding applies. A nil binding reports
// ModeMetadata: with no liveness store there is nowhere to split TO, so the only
// correct behavior is the legacy pass-through.
func (b *livenessBinding) Mode() liveness.Mode {
	if b == nil {
		return liveness.ModeMetadata
	}
	return b.mode
}

// Store returns the live liveness store, opening it on first use and backing off
// after a failure (livenessOpenRetryInterval when the endpoint exists but could
// not be reached, livenessNoEndpointRetryInterval when there is no endpoint at
// all). It returns nil whenever liveness is unavailable — callers treat nil as
// "pass everything through to versioned metadata".
//
// Only the FIRST dial is synchronous, and the binding's lock is never held
// across it. Every retry after a failure runs in the background and Store()
// returns nil immediately: a dead Dolt server must degrade liveness to the
// legacy write path, never add a multi-second stall to each of the controller's
// bead operations. Because a store operation cannot wait for a retry, the write
// it was routing goes to versioned metadata — a commit, which is exactly the
// right trade while the endpoint is down.
func (b *livenessBinding) Store() liveness.Store {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if b.store != nil {
		store := b.store
		b.mu.Unlock()
		return store
	}
	if b.dialing || (!b.lastAttempt.IsZero() && time.Since(b.lastAttempt) < b.retryAfter) {
		b.mu.Unlock()
		return nil
	}
	first := b.lastAttempt.IsZero()
	b.dialing = true
	b.lastAttempt = time.Now()
	b.mu.Unlock()

	if !first {
		go b.dial()
		return nil
	}
	return b.dial()
}

// dial performs one open attempt and installs the result. It runs with the lock
// released; b.dialing keeps a second attempt from starting alongside it.
func (b *livenessBinding) dial() liveness.Store {
	store, err := openScopeLivenessStore(b.cityPath, b.scopeRoot)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dialing = false
	if err != nil {
		// A scope with no endpoint cannot acquire one without a config change, so
		// it backs off far harder than a server that is merely unreachable.
		b.retryAfter = livenessOpenRetryInterval
		if errors.Is(err, errNoLivenessEndpoint) {
			b.retryAfter = livenessNoEndpointRetryInterval
		}
		// A scope with no Dolt endpoint at all — a file/doltlite provider, or any
		// test working in a temp dir — is the expected steady state, not a
		// degradation: log nothing. Only a scope that HAS an endpoint and could
		// not be reached or seeded is worth an operator's attention, and then
		// only once per binding.
		if !b.warned && !errors.Is(err, errNoLivenessEndpoint) {
			b.warned = true
			log.Printf("session liveness: %s unavailable (session telemetry keeps committing to bead metadata): %v", b.scopeRoot, err)
		}
		return nil
	}
	if b.store != nil {
		// Another dial won the race; drop this handle rather than leaking it.
		_ = store.Close()
		return b.store
	}
	b.store = store
	b.warned = false
	return b.store
}

// Now reports the liveness endpoint's clock — the timebase fallback stamps must
// be minted in, because written_at is minted server-side. With no store open
// there is nothing to calibrate against and the local clock is the best
// available estimate.
func (b *livenessBinding) Now() time.Time {
	if b == nil {
		return time.Now().UTC()
	}
	b.mu.Lock()
	store, offset, have := b.store, b.clockOffset, b.haveClockOffset
	b.mu.Unlock()
	if store != nil {
		return store.Now()
	}
	if have {
		return time.Now().UTC().Add(offset)
	}
	return time.Now().UTC()
}

// rememberClockOffset records the skew a live store reports so Now() keeps
// returning server time after that store is retired.
func (b *livenessBinding) rememberClockOffset(store liveness.Store) {
	if b == nil || store == nil {
		return
	}
	b.clockOffset = store.Now().Sub(time.Now().UTC())
	b.haveClockOffset = true
}

// noteOpError retires the pooled handle when err says the transport is gone.
//
// database/sql reconnects on its own, but only to the SAME address — and the
// failure this exists for is a managed-Dolt hard-kill/rebind, which moves the
// PORT. Without this the pool dials the dead port forever and liveness silently
// reverts to committing for the life of the process (which is also what arms the
// stale-shadow the fence guards against). The bead store solves the identical
// problem with WithNativeReopen; this is the same idea, one layer up: drop the
// handle, let the backoff gate the next attempt, and re-RESOLVE the endpoint
// rather than re-dialing the cached one.
//
// A statement-level error (bad SQL, constraint) is left alone: retiring the pool
// for those would turn one bad query into an endpoint flap.
func (b *livenessBinding) noteOpError(err error) {
	if b == nil || !liveness.IsConnectionError(err) {
		return
	}
	b.mu.Lock()
	store := b.store
	b.store = nil
	// Arm the backoff from NOW so a storm of in-flight operations all failing at
	// once produces one re-dial, not one per operation.
	b.lastAttempt = time.Now()
	b.retryAfter = livenessOpenRetryInterval
	b.warned = false
	b.mu.Unlock()
	if store != nil {
		_ = store.Close()
	}
}

// setStoreForTest installs a store directly, bypassing the dialer.
func (b *livenessBinding) setStoreForTest(store liveness.Store, mode liveness.Mode) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.store = store
	b.rememberClockOffset(store)
	b.mode = mode
}

// newLivenessBindingForTest builds an unregistered binding over a supplied store.
func newLivenessBindingForTest(store liveness.Store, mode liveness.Mode) *livenessBinding {
	b := &livenessBinding{mode: mode}
	b.store = store
	b.rememberClockOffset(store)
	return b
}

// errNoLivenessEndpoint marks the benign "this scope has no Dolt at all" case:
// a file or doltlite provider, an unconfigured scope, or any test working in a
// temp dir. It is the expected steady state for those scopes, so it degrades
// silently rather than warning.
var errNoLivenessEndpoint = errors.New("scope has no Dolt endpoint to bind")

// openScopeLivenessStore dials the scope's managed Dolt database and seeds the
// liveness schema.
func openScopeLivenessStore(cityPath, scopeRoot string) (liveness.Store, error) {
	if strings.TrimSpace(cityPath) == "" {
		return nil, fmt.Errorf("%w: no city path for scope %s", errNoLivenessEndpoint, scopeRoot)
	}
	target, ok, err := canonicalScopeDoltTarget(cityPath, scopeRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving Dolt target: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: scope config is not authoritative", errNoLivenessEndpoint)
	}
	if strings.TrimSpace(target.Port) == "" || strings.TrimSpace(target.Database) == "" {
		return nil, fmt.Errorf("%w: incomplete target (port=%q database=%q)", errNoLivenessEndpoint, target.Port, target.Database)
	}
	db, err := managedDoltOpenDatabase(target.Host, target.Port, target.User, target.Database)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", target.Database, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), livenessOpenTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging %s: %w", target.Database, err)
	}
	if err := liveness.EnsureSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := liveness.NewSQLStore(db)
	// Calibrate against the server clock so fallback stamps and the server-minted
	// written_at they fence share a timebase. Best-effort: an uncalibrated store
	// falls back to the local clock, which is exact for a same-host managed Dolt.
	if err := store.Calibrate(ctx); err != nil {
		log.Printf("session liveness: %s: server clock uncalibrated, fallback stamps use the local clock: %v", scopeRoot, err)
	}
	return store, nil
}

// livenessOpContext bounds one liveness read or write.
func livenessOpContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), livenessOpTimeout)
}
