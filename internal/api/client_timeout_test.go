package api

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
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
// for mail reads (ga-x49mfh, pl-lzd): the server's store deadline must fire
// strictly before the client's mail read budget, or a slow store surfaces as
// an opaque transport timeout instead of store_slow. The client budget itself
// must not exceed the HTTP ceilings that would otherwise cut the request first.
func TestMailReadDeadlineShorterThanClientTimeout(t *testing.T) {
	for _, raw := range []string{"", "6s", "30s", "45s", config.MailReadTimeoutCeiling.String()} {
		m := config.MailConfig{ReadTimeout: raw}
		c := NewCityScopedClient("http://127.0.0.1:1", "c")
		c.SetMailReadTimeout(m.EffectiveReadTimeout())
		if m.ReadDeadline() >= c.mailReadBudget() {
			t.Fatalf("read_timeout %q: server deadline %v, want < client budget %v so store_slow reaches the client typed",
				raw, m.ReadDeadline(), c.mailReadBudget())
		}
		if c.mailReadBudget() > defaultClientTimeout {
			t.Fatalf("read_timeout %q: client budget %v exceeds defaultClientTimeout %v, which would cut local mail reads first",
				raw, c.mailReadBudget(), defaultClientTimeout)
		}
	}
	if config.MailReadTimeoutCeiling != defaultClientTimeout {
		t.Fatalf("config.MailReadTimeoutCeiling = %v, want defaultClientTimeout %v: the ceiling config load enforces must be the client's real one",
			config.MailReadTimeoutCeiling, defaultClientTimeout)
	}
	defaultDeadline := config.MailConfig{}.ReadDeadline()
	if defaultDeadline >= remoteResponseHeaderTimeout {
		t.Fatalf("default server deadline %v, want < remoteResponseHeaderTimeout %v so remote mail reads get headers before the transport gives up",
			defaultDeadline, remoteResponseHeaderTimeout)
	}
}

func TestClientMailReadBudget(t *testing.T) {
	c := NewCityScopedClient("http://127.0.0.1:1", "c")
	if got := c.mailReadBudget(); got != config.DefaultMailReadTimeout {
		t.Fatalf("default mailReadBudget() = %v, want %v", got, config.DefaultMailReadTimeout)
	}
	c.SetMailReadTimeout(50 * time.Second)
	if got := c.mailReadBudget(); got != 50*time.Second {
		t.Fatalf("mailReadBudget() after SetMailReadTimeout(50s) = %v, want 50s", got)
	}
	c.SetMailReadTimeout(0)
	if got := c.mailReadBudget(); got != config.DefaultMailReadTimeout {
		t.Fatalf("mailReadBudget() after SetMailReadTimeout(0) = %v, want the default %v", got, config.DefaultMailReadTimeout)
	}
}

func TestRemoteClientMailReadBudgetWidensHeaderTimeout(t *testing.T) {
	got, err := remoteHeaderTimeout(RemoteOptions{})
	if err != nil || got != remoteResponseHeaderTimeout {
		t.Fatalf("remoteHeaderTimeout(default) = %v, %v; want %v, nil", got, err, remoteResponseHeaderTimeout)
	}
	got, err = remoteHeaderTimeout(RemoteOptions{MailReadTimeout: 55 * time.Second})
	if err != nil || got != 55*time.Second {
		t.Fatalf("remoteHeaderTimeout(mail 55s) = %v, %v; want 55s, nil", got, err)
	}
	c, err := NewRemoteCityScopedClient("https://example.test", "c", RemoteOptions{MailReadTimeout: 55 * time.Second})
	if err != nil {
		t.Fatalf("NewRemoteCityScopedClient: %v", err)
	}
	if got := c.mailReadBudget(); got != 55*time.Second {
		t.Fatalf("remote mailReadBudget() = %v, want 55s", got)
	}
}

// A context with a short REST timeout and no mail read budget must keep
// building clients: only an explicit budget over the REST timeout is refused.
func TestRemoteClientShortRESTTimeoutWithoutMailBudget(t *testing.T) {
	if _, err := NewRemoteCityScopedClient("https://example.test", "c", RemoteOptions{RESTTimeout: 10 * time.Second}); err != nil {
		t.Fatalf("NewRemoteCityScopedClient(REST 10s, no mail budget): %v", err)
	}
}

func TestRemoteClientRefusesMailReadBudgetOverRESTTimeout(t *testing.T) {
	_, err := NewRemoteCityScopedClient("https://example.test", "c", RemoteOptions{
		RESTTimeout:     40 * time.Second,
		MailReadTimeout: 50 * time.Second,
	})
	if err == nil {
		t.Fatal("NewRemoteCityScopedClient accepted a mail read budget longer than the REST timeout")
	}
	for _, want := range []string{"50s", "40s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err.Error(), want)
		}
	}
}

// The server's deadline follows the city's [mail] read_timeout (pl-lzd).
func TestServerMailReadDeadlineFollowsConfig(t *testing.T) {
	state := newFakeState(t)
	s := &Server{state: state}
	if got, want := s.mailReadDeadline(), 25*time.Second; got != want {
		t.Fatalf("default mailReadDeadline() = %v, want %v", got, want)
	}
	state.cfg.Mail.ReadTimeout = "50s"
	if got, want := s.mailReadDeadline(), 45*time.Second; got != want {
		t.Fatalf("mailReadDeadline() with read_timeout 50s = %v, want %v", got, want)
	}
	old := mailReadDeadlineOverride
	mailReadDeadlineOverride = 5 * time.Millisecond
	t.Cleanup(func() { mailReadDeadlineOverride = old })
	if got := s.mailReadDeadline(); got != 5*time.Millisecond {
		t.Fatalf("mailReadDeadline() with a test override = %v, want 5ms", got)
	}
}
