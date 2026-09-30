package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

// clearMailIdentityEnv makes this test process neither a session nor an order.
// Empty reads as unset: every identity read is os.Getenv plus a blank check.
func clearMailIdentityEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"GC_SESSION_ID", "GC_ALIAS", "GC_AGENT", "GC_ORDER_SCOPE", "GC_ORDER_NAME", "GC_ORDER_RUN"} {
		t.Setenv(key, "")
	}
}

func setOrderEnv(t *testing.T, scope, name, run string) {
	t.Helper()
	t.Setenv("GC_ORDER_SCOPE", scope)
	t.Setenv("GC_ORDER_NAME", name)
	t.Setenv("GC_ORDER_RUN", run)
}

// An order's default sender is order:<scope>/<name>, tried after any session
// identity and before the human fallback (ga-uf77nc).
func TestDefaultMailSenderCandidates_OrderBeforeHuman(t *testing.T) {
	clearMailIdentityEnv(t)
	if got := defaultMailSenderCandidates(); strings.Join(got, ",") != "human" {
		t.Fatalf("no identity: candidates = %q, want [human]", got)
	}

	setOrderEnv(t, "city", "deacon-watch", "pc-run1")
	if got := defaultMailSenderCandidates(); strings.Join(got, ",") != "order:city/deacon-watch" {
		t.Fatalf("order only: candidates = %q, want [order:city/deacon-watch]", got)
	}
	// Reading a mailbox never uses the order identity: an order has none.
	if got := defaultMailIdentityCandidates(); strings.Join(got, ",") != "human" {
		t.Fatalf("order only: mailbox candidates = %q, want [human]", got)
	}

	t.Setenv("GC_AGENT", "declared-seat")
	if got := defaultMailSenderCandidates(); strings.Join(got, ",") != "declared-seat,order:city/deacon-watch" {
		t.Fatalf("order + session: candidates = %q, want session first", got)
	}
}

func TestResolveDefaultMailSender_AcceptsOrderIdentity(t *testing.T) {
	clearMailIdentityEnv(t)
	setOrderEnv(t, "qcore", "cert-landing-patrol", "qc-run9")

	var stderr bytes.Buffer
	got, ok := resolveDefaultMailSenderForCommand("", nil, nil, &stderr, "gc mail send")
	if !ok || got != "order:qcore/cert-landing-patrol" {
		t.Fatalf("sender = %q, %v (stderr %q), want order:qcore/cert-landing-patrol", got, ok, stderr.String())
	}
}

func TestOrderMailRunMetadata_OnlyForThisOrdersOwnSender(t *testing.T) {
	clearMailIdentityEnv(t)
	setOrderEnv(t, "city", "deacon-watch", "pc-run1")

	if md := orderMailRunMetadata("order:city/deacon-watch"); md[mail.FromOrderRunMetadataKey] != "pc-run1" {
		t.Fatalf("own sender: metadata = %v, want the run id", md)
	}
	if md := orderMailRunMetadata("order:city/other-order"); md != nil {
		t.Fatalf("another order's sender: metadata = %v, want nil", md)
	}
	if md := orderMailRunMetadata("human"); md != nil {
		t.Fatalf("human sender: metadata = %v, want nil", md)
	}
	t.Setenv("GC_ORDER_RUN", "")
	if md := orderMailRunMetadata("order:city/deacon-watch"); md != nil {
		t.Fatalf("untracked run: metadata = %v, want nil", md)
	}
}

func orderSenderTestCity(t *testing.T) (string, beads.Bead) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "")
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	seat, err := store.Create(beads.Bead{
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"alias": "worker", "session_name": "worker-session"},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	return cityPath, seat
}

// End to end through gc mail send: with no --from, an order's mail says which
// order and which run sent it.
func TestCmdMailSend_OrderSendsAsItself(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)
	setOrderEnv(t, "city", "deacon-watch", "pc-run1")

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{seat.ID, "body"}, false, false, "", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("cmdMailSend() = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stored := mailSendTestFindMessage(t, cityPath)
	if stored.From != "order:city/deacon-watch" {
		t.Fatalf("From = %q, want order:city/deacon-watch", stored.From)
	}
	if got := stored.Metadata[mail.FromOrderRunMetadataKey]; got != "pc-run1" {
		t.Fatalf("%s = %q, want pc-run1", mail.FromOrderRunMetadataKey, got)
	}
}

// An explicit --from wins over the order identity and records no run.
func TestCmdMailSend_ExplicitFromWinsOverOrder(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)
	setOrderEnv(t, "city", "deacon-watch", "pc-run1")

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{seat.ID, "body"}, false, false, "human", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("cmdMailSend() = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stored := mailSendTestFindMessage(t, cityPath)
	if stored.From != "human" {
		t.Fatalf("From = %q, want human", stored.From)
	}
	if got, ok := stored.Metadata[mail.FromOrderRunMetadataKey]; ok {
		t.Fatalf("%s = %q, want absent", mail.FromOrderRunMetadataKey, got)
	}
}

// Neither a session nor an order: unchanged, still human (ga-fi21sm changes it).
func TestCmdMailSend_NoIdentityStillHuman(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{seat.ID, "body"}, false, false, "", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("cmdMailSend() = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if stored := mailSendTestFindMessage(t, cityPath); stored.From != "human" {
		t.Fatalf("From = %q, want human", stored.From)
	}
}

// An order has no mailbox: mail addressed to one is refused, bare or
// city-qualified, rather than stored where nobody reads it.
func TestResolveMailRecipient_RefusesOrderAddress(t *testing.T) {
	clearMailIdentityEnv(t)
	store := beads.NewMemStore()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	for _, addr := range []string{"order:city/deacon-watch", "order:qcore/cert-landing-patrol"} {
		_, err := resolveMailRecipientIdentity(t.TempDir(), cfg, store, addr)
		if err == nil || !strings.Contains(err.Error(), "an order, which has no mailbox") {
			t.Fatalf("resolveMailRecipientIdentity(%q) error = %v, want the no-mailbox refusal", addr, err)
		}
		if errors.Is(err, session.ErrSessionNotFound) {
			t.Fatalf("resolveMailRecipientIdentity(%q) = not-found; want the explicit refusal", addr)
		}
	}
}

// A remote (--context) send from an order signs as the order, not human.
func TestRemoteMailIdentity_FallsBackToOrder(t *testing.T) {
	clearMailIdentityEnv(t)
	if got := remoteMailIdentity(); got != "human" {
		t.Fatalf("no identity: %q, want human", got)
	}
	setOrderEnv(t, "city", "deacon-watch", "pc-run1")
	if got := remoteMailIdentity(); got != "order:city/deacon-watch" {
		t.Fatalf("order: %q, want order:city/deacon-watch", got)
	}
}
