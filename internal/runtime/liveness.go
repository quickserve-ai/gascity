package runtime

import "strings"

// Liveness reports both provider-runtime presence and configured agent-process
// presence for a session target.
type Liveness struct {
	Running bool
	Alive   bool
}

// LivenessObserver is implemented by providers that can observe runtime and
// agent-process liveness in one provider-native pass.
type LivenessObserver interface {
	ObserveLiveness(name string, processNames []string) Liveness
}

// AttestedLiveness is a Liveness observation together with the provider's own
// statement about the QUALITY of the probe that produced it.
//
// Liveness on its own is two booleans with no error channel: "not running"
// returned by a provider whose probe FAILED is byte-identical to "not running"
// returned by a provider that looked and genuinely found nothing. That is safe
// for a caller that only wants to know whether to nudge, and unsafe for a caller
// that destroys state when the answer is "stopped" — a wedged provider then
// reads as a fleet-wide death.
//
// tmux is the concrete case. StateCache serves an EMPTY snapshot — every session
// not-running — once more than staleTTL (30s) has passed since its last
// SUCCESSFUL refresh, and keeps serving it for as long as the underlying fetch
// keeps failing (internal/runtime/tmux/state_cache.go). Both halves of Liveness
// read that one snapshot, so a degraded cache zeroes Running and Alive together:
// they are not independent channels.
type AttestedLiveness struct {
	Liveness
	// Fresh reports that the provider stands behind this observation: it came
	// from a probe that actually SUCCEEDED, recently enough to be current.
	//
	// False means UNKNOWN, never "fine". It covers both "the provider knows its
	// data is degraded" and "the provider has no way to tell", so a caller that
	// acts destructively on Running==false must refuse on !Fresh.
	Fresh bool
}

// LivenessAttester is implemented by providers that can report the freshness of
// a liveness observation as well as its content. Providers that do not implement
// it are treated as unable to attest — see AttestLiveness.
type LivenessAttester interface {
	AttestLiveness(name string, processNames []string) AttestedLiveness
}

// AttestLiveness returns the consolidated liveness view for a provider session
// together with a freshness attestation.
//
// The Liveness half is identical to what ObserveLiveness returns for the same
// arguments. The attestation half fails CLOSED: a nil provider, an empty name,
// or a provider that does not implement LivenessAttester all report Fresh=false.
// A caller may therefore treat "not attested" as UNKNOWN without knowing which
// providers can attest.
func AttestLiveness(sp Provider, name string, processNames []string) AttestedLiveness {
	if sp == nil || strings.TrimSpace(name) == "" {
		return AttestedLiveness{}
	}
	if attester, ok := sp.(LivenessAttester); ok {
		attested := attester.AttestLiveness(name, processNames)
		attested.Liveness = normalizeLiveness(attested.Liveness)
		return attested
	}
	return AttestedLiveness{Liveness: ObserveLiveness(sp, name, processNames)}
}

// ObserveLiveness returns the consolidated liveness view for a provider
// session. Providers with native support may use additional persisted runtime
// hints; other providers fall back to IsRunning plus ProcessAlive.
func ObserveLiveness(sp Provider, name string, processNames []string) Liveness {
	if sp == nil || strings.TrimSpace(name) == "" {
		return Liveness{}
	}
	if observer, ok := sp.(LivenessObserver); ok {
		return normalizeLiveness(observer.ObserveLiveness(name, processNames))
	}
	running := sp.IsRunning(name)
	if !hasProcessNameHints(processNames) {
		return Liveness{Running: running, Alive: running}
	}
	alive := sp.ProcessAlive(name, processNames)
	if alive && !running {
		running = true
	}
	return normalizeLiveness(Liveness{Running: running, Alive: alive})
}

func hasProcessNameHints(processNames []string) bool {
	for _, name := range processNames {
		if strings.TrimSpace(name) != "" {
			return true
		}
	}
	return false
}

func normalizeLiveness(obs Liveness) Liveness {
	if obs.Alive && !obs.Running {
		obs.Running = true
	}
	return obs
}
