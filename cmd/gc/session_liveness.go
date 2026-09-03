package main

import (
	"context"
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

// livenessOpenRetryInterval bounds how often a failed liveness open is retried.
// Without it a scope whose Dolt server is briefly down would re-dial on every
// single store operation.
const livenessOpenRetryInterval = 30 * time.Second

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
	dialing     bool
	warned      bool
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
// correct behaviour is the legacy pass-through.
func (b *livenessBinding) Mode() liveness.Mode {
	if b == nil {
		return liveness.ModeMetadata
	}
	return b.mode
}

// Store returns the live liveness store, opening it on first use and retrying a
// failed open no more than once per livenessOpenRetryInterval. It returns nil
// whenever liveness is unavailable — callers treat nil as "pass everything
// through to versioned metadata".
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
	if b.dialing || (!b.lastAttempt.IsZero() && time.Since(b.lastAttempt) < livenessOpenRetryInterval) {
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
		if !b.warned {
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

// setStoreForTest installs a store directly, bypassing the dialer.
func (b *livenessBinding) setStoreForTest(store liveness.Store, mode liveness.Mode) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.store = store
	b.mode = mode
}

// newLivenessBindingForTest builds an unregistered binding over a supplied store.
func newLivenessBindingForTest(store liveness.Store, mode liveness.Mode) *livenessBinding {
	b := &livenessBinding{mode: mode}
	b.store = store
	return b
}

// openScopeLivenessStore dials the scope's managed Dolt database and seeds the
// liveness schema.
func openScopeLivenessStore(cityPath, scopeRoot string) (liveness.Store, error) {
	if strings.TrimSpace(cityPath) == "" {
		return nil, fmt.Errorf("no city path for scope %s", scopeRoot)
	}
	target, ok, err := canonicalScopeDoltTarget(cityPath, scopeRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving Dolt target: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("scope config is not authoritative; no Dolt endpoint to bind")
	}
	if strings.TrimSpace(target.Port) == "" || strings.TrimSpace(target.Database) == "" {
		return nil, fmt.Errorf("scope Dolt target is incomplete (port=%q database=%q)", target.Port, target.Database)
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
	return liveness.NewSQLStore(db), nil
}

// livenessOpContext bounds one liveness read or write.
func livenessOpContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), livenessOpTimeout)
}
