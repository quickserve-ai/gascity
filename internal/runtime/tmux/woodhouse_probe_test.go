package tmux

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// WOODHOUSE ADVERSARIAL PROBES (ga-2otk73 iteration 3 review). Not part of the
// change. These test the attestation's own claims rather than restating them.
// ---------------------------------------------------------------------------

// PROBE H1-a. The full incident shape, held for the whole confirmation window:
// the session is GENUINELY ALIVE (the last successful fetch saw it), the fetch
// subsystem is wedged, and the cache is past staleTTL. Every read in the window
// must report stopped-and-UNATTESTED. One attested read anywhere in the window
// would let the recovery close a live agent's bead.
func TestWoodhouseProbe_WedgedFetchNeverAttestsAcrossTheWholeConfirmationWindow(t *testing.T) {
	f := &mockFetcher{sessions: map[string]bool{"agent-1": true}}
	cache := NewStateCache(f, 5*time.Millisecond)
	cache.staleTTL = 20 * time.Millisecond
	provider := &Provider{cache: cache}

	if a := provider.AttestLiveness("agent-1", []string{"claude"}); !a.Running || !a.Fresh {
		t.Fatalf("prime: %+v, want running+fresh", a)
	}

	f.setResult(nil, errors.New("tmux fetch wedged"))
	time.Sleep(40 * time.Millisecond) // past staleTTL

	// 25 probes spread across the window — the real chain needs 3.
	for i := 0; i < 25; i++ {
		a := provider.AttestLiveness("agent-1", []string{"claude"})
		if a.Fresh {
			t.Fatalf("probe %d: Fresh=true off a wedged fetch (%+v) — a live agent would be closed", i, a)
		}
		if a.Running || a.Alive {
			t.Fatalf("probe %d: %+v — expected the degraded stopped reading", i, a)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// PROBE H1-b. The recovery path is dead-simple to make INERT by accident. On a
// healthy city whose reconcile ticks are far apart (the real cadence: minutes,
// while ttl is 2s and staleTTL is 30s), every tick must still attest — the read
// itself drives the refresh. If this ever fails, the confirmation chain can
// never converge on the live fleet and ga-2otk73 silently re-opens.
func TestWoodhouseProbe_HealthyCityAttestsAtASlowTickCadence(t *testing.T) {
	f := &mockFetcher{sessions: map[string]bool{"other": true}}
	cache := NewStateCache(f, 5*time.Millisecond)
	// staleTTL deliberately SHORTER than the gap between ticks, mirroring
	// 30s staleTTL vs a minutes-apart reconcile tick.
	cache.staleTTL = 10 * time.Millisecond
	provider := &Provider{cache: cache}

	for i := 0; i < 5; i++ {
		time.Sleep(30 * time.Millisecond) // >> staleTTL: the cache is cold every tick
		a := provider.AttestLiveness("dead-session", []string{"claude"})
		if !a.Fresh {
			t.Fatalf("tick %d: Fresh=false on a HEALTHY cache read past staleTTL (%+v) — the recovery could never converge", i, a)
		}
		if a.Running || a.Alive {
			t.Fatalf("tick %d: %+v, want a genuine absence", i, a)
		}
	}
	if f.getCalls() < 5 {
		t.Fatalf("fetcher calls = %d, want one per tick; the read must drive the refresh", f.getCalls())
	}
}

// PROBE H1-c. The tmux SERVER being gone (the whole box's sessions dead, or the
// documented ga-03ixvj "handoff killed the tmux server" shape) is a permanent
// fetch error on an UNPRIMED cache. Document what that means for the recovery:
// attestation never becomes true, so the name-squat release can never fire while
// the server is down.
func TestWoodhouseProbe_NoTmuxServerNeverAttests(t *testing.T) {
	f := &mockFetcher{err: fmt.Errorf("no server running on /tmp/tmux-501/default")}
	cache := NewStateCache(f, 5*time.Millisecond)
	provider := &Provider{cache: cache}

	for i := 0; i < 5; i++ {
		a := provider.AttestLiveness("agent-1", []string{"claude"})
		if a.Fresh {
			t.Fatalf("tick %d: Fresh=true with no reachable tmux server (%+v)", i, a)
		}
		time.Sleep(8 * time.Millisecond)
	}
	t.Log("RESULT: with no tmux server the cache never attests, so the ga-2otk73 release is inert for the whole outage")
}

// PROBE H1-d. Recovery of the attestation after the fetch heals. If lastError or
// dirty could latch, the recovery would be permanently inert after the first
// wobble — a stall mode as bad as the false close.
func TestWoodhouseProbe_AttestationRecoversAfterTheFetchHeals(t *testing.T) {
	f := &mockFetcher{sessions: map[string]bool{"agent-1": true}}
	cache := NewStateCache(f, 5*time.Millisecond)
	cache.staleTTL = 20 * time.Millisecond
	provider := &Provider{cache: cache}

	if a := provider.AttestLiveness("agent-1", []string{"claude"}); !a.Fresh {
		t.Fatalf("prime: %+v", a)
	}
	f.setResult(nil, errors.New("tmux fetch wedged"))
	cache.Invalidate()
	time.Sleep(40 * time.Millisecond)
	if a := provider.AttestLiveness("agent-1", []string{"claude"}); a.Fresh {
		t.Fatalf("degraded: %+v, want Fresh=false", a)
	}

	f.setResult(map[string]bool{"agent-1": true}, nil)
	time.Sleep(10 * time.Millisecond)
	a := provider.AttestLiveness("agent-1", []string{"claude"})
	if !a.Fresh {
		t.Fatalf("healed: %+v, want Fresh=true — the attestation latched off and the recovery is now permanently inert", a)
	}
	if !a.Running {
		t.Fatalf("healed: %+v, want the live session back", a)
	}
}

// PROBE H1-e. A successful fetch that returns an EMPTY session set is a genuine
// absence and MUST attest — otherwise the recovery can never fire on the one
// shape it exists for (a named session whose runtime really is gone while the
// rest of the fleet is up). Pinning the accept side keeps a future "tighten the
// attestation" change from making the feature vacuous.
func TestWoodhouseProbe_SuccessfulEmptyFetchStillAttests(t *testing.T) {
	f := &mockFetcher{sessions: map[string]bool{}}
	cache := NewStateCache(f, 5*time.Millisecond)
	provider := &Provider{cache: cache}
	a := provider.AttestLiveness("agent-1", []string{"claude"})
	if a.Running || a.Alive {
		t.Fatalf("%+v, want stopped", a)
	}
	if !a.Fresh {
		t.Fatalf("%+v, want Fresh=true for a SUCCESSFUL fetch that simply did not list the session", a)
	}
}
