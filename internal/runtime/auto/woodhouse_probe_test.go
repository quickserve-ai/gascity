package auto

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ---------------------------------------------------------------------------
// WOODHOUSE ADVERSARIAL PROBE (ga-2otk73 iteration 3 review). Not part of the
// change.
//
// The change's WHO-CAN-ATTEST table says "the auto and hybrid routers ...
// forward" the attestation, implying a session routed to a backend that cannot
// attest (acp) is treated as UNKNOWN and never confirmed. It is not: because the
// router falls through to the OTHER backend whenever the routed one reports
// not-running — and "not running" is the only case the ga-2otk73 release ever
// looks at — the attestation that comes back is the OTHER backend's.
//
// Concretely, an ACP session whose ACP probe is degraded gets its death
// "attested" by a healthy tmux probe for a session tmux never managed.
// ---------------------------------------------------------------------------

// nonAttestingBackend implements runtime.Provider and NOT runtime.LivenessAttester
// — the acp/k8s/ssh/herdr shape. It reports the session as not running, which is
// what a degraded ACP probe returns for a session that is actually alive.
type nonAttestingBackend struct {
	runtime.Provider
}

func (b *nonAttestingBackend) IsRunning(string) bool              { return false }
func (b *nonAttestingBackend) ProcessAlive(string, []string) bool { return false }

func TestWoodhouseProbe_AutoAttestsAnACPSessionFromTheTmuxBackend(t *testing.T) {
	// defaultSP is the tmux-like backend: healthy, attests, and has never heard
	// of this session (it is an ACP session).
	healthyDefault := runtime.NewFake()
	// acpSP is the ACP backend: cannot attest, and its probe is degraded, so it
	// reports the LIVE session as not running.
	acp := &nonAttestingBackend{Provider: runtime.NewFake()}

	p := New(healthyDefault, acp)
	p.RouteACP("acp-agent")

	if _, ok := interface{}(acp).(runtime.LivenessAttester); ok {
		t.Fatal("precondition: the acp backend must not implement LivenessAttester")
	}

	got := p.AttestLiveness("acp-agent", []string{"claude"})
	t.Logf("auto.AttestLiveness(acp-routed session) = %+v", got)

	if got.Fresh {
		t.Errorf("FINDING: an ACP-routed session's liveness came back ATTESTED (%+v). "+
			"The attestation is the TMUX backend's — tmux never managed this session — "+
			"so a degraded ACP probe now reads as a confirmed stop and the ga-2otk73 "+
			"release will close a live ACP session's bead after three ticks.", got)
	}
	if got.Running || got.Alive {
		t.Fatalf("probe setup wrong: %+v, expected the stopped reading", got)
	}
}

// The other direction of the same swap, RESOLVED in iteration 4 as a deliberate
// trade-off rather than the reviewer probe's original expectation. A session
// routed to the ATTESTING default backend (tmux) that is genuinely stopped:
// the primary attests the stop, the router then consults the other backend for
// the stale-route recovery, and the final verdict's freshness is the
// CONJUNCTION of both probes. With a counterpart that cannot attest (acp), the
// stop therefore comes back Fresh=false — UNKNOWN — and the ga-2otk73 release
// never confirms on an auto-wrapped city.
//
// That inertness is the ACCEPTED price, chosen over the alternative: taking the
// primary's attestation alone would let a stale route plus a degraded
// counterpart probe attest the death of a session that is actually alive on
// the other backend — a false close, the exact H1 class. Fail closed wins.
// The recovery remains fully operative on cities without the auto wrapper,
// which includes this fleet (city.toml has zero acp targets, so
// resolveSessionTransportProvider never builds the wrapper).
func TestWoodhouseProbe_AutoStoppedVerdictIsUnknownWhenCounterpartCannotAttest(t *testing.T) {
	healthyDefault := runtime.NewFake()
	acp := &nonAttestingBackend{Provider: runtime.NewFake()}
	p := New(healthyDefault, acp)

	direct := runtime.AttestLiveness(healthyDefault, "tmux-agent", []string{"claude"})
	if !direct.Fresh {
		t.Fatalf("precondition: the default backend must attest directly, got %+v", direct)
	}

	got := p.AttestLiveness("tmux-agent", []string{"claude"})
	t.Logf("direct=%+v  through-auto=%+v", direct, got)
	if got.Running || got.Alive {
		t.Fatalf("probe setup wrong: %+v, expected the stopped reading", got)
	}
	if got.Fresh {
		t.Errorf("a stopped verdict through the auto router must be UNKNOWN (Fresh=false) when "+
			"the counterpart backend cannot attest — the conjunction is what stops a stale route "+
			"plus a degraded counterpart from attesting a live session's death (direct=%+v, through-auto=%+v)",
			direct, got)
	}
}
