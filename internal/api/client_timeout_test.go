package api

import (
	"testing"
	"time"
)

// TestDefaultClientTimeoutAccommodatesFederatedReads guards the ceiling that
// governs the control-plane read paths. ListBeads/GetBead/GetStatus
// pass context.Background(), so the HTTP client's overall timeout
// is their only deadline. Those endpoints federate the city store plus every
// rig store, and a dolt-backed rig store can take several seconds; a too-tight
// ceiling false-times-out healthy-but-slow federated reads. 10s was too tight
// once a federated read measured ~10s — keep meaningful headroom over the
// federated read cost.
func TestDefaultClientTimeoutAccommodatesFederatedReads(t *testing.T) {
	const minFederatedReadBudget = 30 * time.Second
	if defaultClientTimeout < minFederatedReadBudget {
		t.Fatalf("defaultClientTimeout = %v, want >= %v to cover federated multi-store control-plane reads",
			defaultClientTimeout, minFederatedReadBudget)
	}
}

// TestMailReadDeadlineShorterThanClientTimeout keeps the typed-error ordering
// for mail reads (ga-x49mfh): the server's store deadline must fire strictly
// before the client's mail read deadline, or a slow hub store surfaces as an
// opaque transport timeout instead of store_slow. The client deadline itself
// must not exceed the HTTP ceilings that would otherwise cut the request first.
func TestMailReadDeadlineShorterThanClientTimeout(t *testing.T) {
	if defaultMailReadDeadline >= mailReadClientTimeout {
		t.Fatalf("defaultMailReadDeadline = %v, want < mailReadClientTimeout %v so store_slow reaches the client typed",
			defaultMailReadDeadline, mailReadClientTimeout)
	}
	if mailReadClientTimeout > defaultClientTimeout {
		t.Fatalf("mailReadClientTimeout = %v exceeds defaultClientTimeout %v, which would cut local mail reads first",
			mailReadClientTimeout, defaultClientTimeout)
	}
	if defaultMailReadDeadline >= remoteResponseHeaderTimeout {
		t.Fatalf("defaultMailReadDeadline = %v, want < remoteResponseHeaderTimeout %v so remote mail reads get headers before the transport gives up",
			defaultMailReadDeadline, remoteResponseHeaderTimeout)
	}
}
