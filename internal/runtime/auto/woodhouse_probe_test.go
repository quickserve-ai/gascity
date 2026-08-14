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

// The other direction of the same swap. A session routed to the ATTESTING
// default backend (tmux) that is genuinely stopped: the primary probe attests,
// but because Running==false the router falls through and returns the ACP
// backend's attestation instead — Fresh=false. So on ANY city that carries the
// auto wrapper, the ga-2otk73 recovery is permanently INERT for tmux sessions
// as well: the exact outage the change exists to repair silently stops being
// repaired.
func TestWoodhouseProbe_AutoLosesTheDefaultBackendAttestationOnAStoppedSession(t *testing.T) {
	healthyDefault := runtime.NewFake()
	acp := &nonAttestingBackend{Provider: runtime.NewFake()}
	p := New(healthyDefault, acp)

	direct := runtime.AttestLiveness(healthyDefault, "tmux-agent", []string{"claude"})
	if !direct.Fresh {
		t.Fatalf("precondition: the default backend must attest directly, got %+v", direct)
	}

	got := p.AttestLiveness("tmux-agent", []string{"claude"})
	t.Logf("direct=%+v  through-auto=%+v", direct, got)
	if !got.Fresh {
		t.Errorf("FINDING: the auto router DROPPED the attesting default backend's Fresh=true "+
			"(direct=%+v, through-auto=%+v) because the stopped reading fell through to the "+
			"non-attesting ACP backend. The recovery can never fire on an auto-wrapped city.", direct, got)
	}
}
