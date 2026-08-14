package tmux

import (
	"errors"
	"testing"
	"time"

	gcruntime "github.com/gastownhall/gascity/internal/runtime"
)

// ---------------------------------------------------------------------------
// ga-2otk73 HOLE 1. StateCache answers "not running" for two completely
// different reasons and, before the attestation, could not tell them apart:
//
//   - a successful fetch that did not list the session — it really is gone;
//   - the DEGRADED empty snapshot it serves once more than staleTTL has passed
//     since the last successful refresh, for as long as the fetch keeps failing.
//
// Every caller that destroys state on "stopped" (the named-session name release)
// needs the second case to read as UNKNOWN. These tests pin the cache's own
// answer to that question, since the whole chain above it is built on it.
// ---------------------------------------------------------------------------

// A healthy cache attests. If this ever fails, the fail-closed callers above it
// stop working entirely — the recovery would never fire on a healthy fleet.
func TestStateCache_HealthySnapshotIsAttested(t *testing.T) {
	f := &mockFetcher{sessions: map[string]bool{"agent-1": true}}
	cache := NewStateCache(f, time.Hour)

	running, _, attested := cache.observeAttested("agent-1", nil)
	if !running {
		t.Fatalf("observeAttested running = false, want true for a live session")
	}
	if !attested {
		t.Fatalf("observeAttested attested = false on a healthy cache; a successful probe MUST attest or no caller can ever act")
	}
	running, _, attested = cache.observeAttested("agent-2", nil)
	if running {
		t.Fatalf("observeAttested running = true for an absent session")
	}
	if !attested {
		t.Fatalf("attested = false for a genuine absence; a successful fetch that did not list the session is real evidence")
	}
}

// The decisive one. Past staleTTL with a failing fetch the cache serves the
// degraded empty snapshot: every session reads not-running. That answer must NOT
// be attested, or a caller cannot distinguish a dead fleet from a blind one.
func TestStateCache_DegradedSnapshotIsNotAttested(t *testing.T) {
	f := &mockFetcher{sessions: map[string]bool{"agent-1": true}}
	cache := NewStateCache(f, 10*time.Millisecond)
	cache.staleTTL = 50 * time.Millisecond

	if running, _, attested := cache.observeAttested("agent-1", nil); !running || !attested {
		t.Fatalf("prime: running=%v attested=%v, want true/true", running, attested)
	}

	f.setResult(nil, errors.New("tmux fetch wedged"))
	time.Sleep(80 * time.Millisecond)

	running, alive, attested := cache.observeAttested("agent-1", []string{"claude"})
	if running || alive {
		t.Fatalf("observeAttested = running:%v alive:%v; the degraded snapshot is expected to read stopped (that is the hazard)", running, alive)
	}
	if attested {
		t.Fatalf("attested = true for the DEGRADED empty snapshot — 'the cache has nothing' would be indistinguishable from 'the session is dead'")
	}
}

// Inside staleTTL the cache keeps serving last-known-good, which is the right
// answer for "is it running" (a blip must not drain healthy sessions) and the
// WRONG basis for a destructive decision: the fetch is failing, so the rows are
// frozen, not current. Attestation must follow the failing fetch, not the age.
func TestStateCache_LastKnownGoodWithinStaleTTLIsNotAttested(t *testing.T) {
	f := &mockFetcher{sessions: map[string]bool{"agent-1": true}}
	// TTL 0 forces every read to refresh, matching
	// TestStateCache_NoServerRefreshPreservesLastKnownGood's note about coarse
	// monotonic clocks making a nanosecond TTL flaky.
	cache := NewStateCache(f, 0)

	if _, _, attested := cache.observeAttested("agent-1", nil); !attested {
		t.Fatalf("prime: attested = false, want true")
	}

	f.setResult(nil, errors.New("tmux fetch wedged"))

	running, _, attested := cache.observeAttested("agent-1", nil)
	if !running {
		t.Fatalf("running = false inside staleTTL; last-known-good preservation (#4082) was lost")
	}
	if attested {
		t.Fatalf("attested = true while the refresh is FAILING — the snapshot is frozen, not current")
	}
}

// An invalidation that the following refresh could not satisfy leaves the cache
// knowingly out of date. Same rule: UNKNOWN, not "fine".
func TestStateCache_DirtyCacheWithFailedRefreshIsNotAttested(t *testing.T) {
	f := &mockFetcher{sessions: map[string]bool{"agent-1": true}}
	cache := NewStateCache(f, time.Hour)
	if _, _, attested := cache.observeAttested("agent-1", nil); !attested {
		t.Fatalf("prime: attested = false, want true")
	}

	f.setResult(nil, errors.New("tmux fetch wedged"))
	cache.Invalidate()

	if _, _, attested := cache.observeAttested("agent-1", nil); attested {
		t.Fatalf("attested = true after an invalidation whose refresh failed")
	}
}

// The provider-level contract the callers actually consume: AttestLiveness must
// carry the cache's verdict, and ObserveLiveness must keep returning exactly the
// same Liveness it always did (it now delegates, so the two cannot drift).
func TestProvider_AttestLivenessCarriesCacheFreshness(t *testing.T) {
	f := &mockFetcher{
		state: runtimeStateSnapshot{
			Sessions: map[string]sessionRuntimeState{
				"agent-1": {Running: true, Panes: []paneRuntimeState{{Command: "claude", PID: "101"}}},
			},
			ProcessesAvailable: true,
		},
	}
	provider := &Provider{cache: NewStateCache(f, 10*time.Millisecond)}
	provider.cache.staleTTL = 50 * time.Millisecond

	attested := provider.AttestLiveness("agent-1", []string{"claude"})
	if !attested.Running || !attested.Alive || !attested.Fresh {
		t.Fatalf("AttestLiveness = %+v, want running+alive+fresh from a healthy cache", attested)
	}
	if got := provider.ObserveLiveness("agent-1", []string{"claude"}); got != attested.Liveness {
		t.Fatalf("ObserveLiveness = %+v, want the same Liveness AttestLiveness reports (%+v)", got, attested.Liveness)
	}

	f.mu.Lock()
	f.state = runtimeStateSnapshot{}
	f.err = errors.New("tmux fetch wedged")
	f.mu.Unlock()
	time.Sleep(80 * time.Millisecond)

	degraded := provider.AttestLiveness("agent-1", []string{"claude"})
	if degraded.Running || degraded.Alive {
		t.Fatalf("AttestLiveness = %+v, want the degraded stopped reading", degraded)
	}
	if degraded.Fresh {
		t.Fatalf("AttestLiveness Fresh = true on a wedged fetch — BOTH halves of Liveness come from that one snapshot, so this is the only signal that says 'we did not look'")
	}
	// And the same through the generic entry point every caller uses.
	if got := gcruntime.AttestLiveness(provider, "agent-1", []string{"claude"}); got.Fresh {
		t.Fatalf("runtime.AttestLiveness Fresh = true; the LivenessAttester fast-path was lost")
	}
}
