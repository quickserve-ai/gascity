package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
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
// identity (ga-uf77nc). With neither there is no candidate at all: no "human"
// fallback for a sender (ga-fi21sm).
func TestDefaultMailSenderCandidates_OrderAfterSessionNoHuman(t *testing.T) {
	clearMailIdentityEnv(t)
	if got := defaultMailSenderCandidates(); len(got) != 0 {
		t.Fatalf("no identity: candidates = %q, want none", got)
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

// Neither a session nor an order and no --from: refused, with what is missing
// and both fixes named, and no message written (ga-fi21sm). It used to send
// as human, the operator's own address.
func TestCmdMailSend_NoIdentityRefused(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{seat.ID, "body"}, false, false, "", "", "", "", &stdout, &stderr); code == 0 {
		t.Fatalf("cmdMailSend() = 0, want a refusal; stdout=%s", stdout.String())
	}
	for _, want := range []string{"no sender identity", "GC_AGENT", "GC_ORDER_NAME", "Run it from a seat", "--from"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want it to name %q", stderr.String(), want)
		}
	}
	assertNoMessageBeads(t, cityPath)
}

// The refusal holds on the storeless (exec:) path too, before any write.
func TestDefaultMailSender_NoIdentityRefused(t *testing.T) {
	clearMailIdentityEnv(t)
	var stderr bytes.Buffer
	if got, ok := defaultMailSender(&stderr, "gc mail send"); ok || got != "" {
		t.Fatalf("defaultMailSender() = %q, %v; want refused", got, ok)
	}
	if !strings.Contains(stderr.String(), "gc mail send: no sender identity") {
		t.Fatalf("stderr = %q, want the refusal", stderr.String())
	}
}

// Each session key alone is still enough to send, as before.
func TestCmdMailSend_EachSessionKeyAloneSends(t *testing.T) {
	for _, key := range []string{"GC_SESSION_ID", "GC_ALIAS", "GC_AGENT"} {
		t.Run(key, func(t *testing.T) {
			clearMailIdentityEnv(t)
			cityPath, seat := orderSenderTestCity(t)
			value := "worker"
			if key == "GC_SESSION_ID" {
				value = seat.ID
			}
			t.Setenv(key, value)

			var stdout, stderr bytes.Buffer
			if code := cmdMailSend([]string{"human", "body"}, false, false, "", "", "", "", &stdout, &stderr); code != 0 {
				t.Fatalf("cmdMailSend() = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if stored := mailSendTestFindMessage(t, cityPath); stored.From == "human" || stored.From == "" {
				t.Fatalf("From = %q, want the seat's mailbox", stored.From)
			}
		})
	}
}

// An explicit --from human with no identity still sends as human: the
// operator's own terminal relies on it.
func TestCmdMailSend_NoIdentityExplicitHumanSends(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{seat.ID, "body"}, false, false, "human", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("cmdMailSend() = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if stored := mailSendTestFindMessage(t, cityPath); stored.From != "human" {
		t.Fatalf("From = %q, want human", stored.From)
	}
}

// gc mail reply falls back the same way: no identity and no --from is
// refused with nothing written; --from human replies as the operator.
func TestCmdMailReply_NoIdentityRefusedExplicitHumanReplies(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, _ := orderSenderTestCity(t)
	t.Setenv("GC_AGENT", "worker")
	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{"human", "original"}, false, false, "", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("seed send = %d; stderr=%s", code, stderr.String())
	}
	orig := mailSendTestFindMessage(t, cityPath)
	clearMailIdentityEnv(t)

	stdout.Reset()
	stderr.Reset()
	if code := cmdMailReplyFromJSON([]string{orig.ID}, "", "", "refused reply", false, false, &stdout, &stderr); code == 0 {
		t.Fatalf("reply with no identity = 0, want a refusal; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc mail reply: no sender identity") {
		t.Fatalf("stderr = %q, want the refusal", stderr.String())
	}
	if n := countMessageBeads(t, cityPath); n != 1 {
		t.Fatalf("message beads after refused reply = %d, want 1 (the original only)", n)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdMailReplyFromJSON([]string{orig.ID}, "human", "", "operator reply", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("reply --from human = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if n := countMessageBeads(t, cityPath); n != 2 {
		t.Fatalf("message beads after --from human reply = %d, want 2", n)
	}
}

func assertNoMessageBeads(t *testing.T, cityPath string) {
	t.Helper()
	if n := countMessageBeads(t, cityPath); n != 0 {
		t.Fatalf("message beads = %d, want none written", n)
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

// A broadcast from an order records the run on every message, as a single
// send does (cross-family review of #199, finding 2).
func TestMailSendAllRecordsOrderRun(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "committer": true, "tester": true}
	run := map[string]string{mail.FromOrderRunMetadataKey: "pc-run1"}

	var stdout, stderr bytes.Buffer
	code := doMailSendAllCoverageRun(mp, events.Discard, recipients, "order:city/deacon-watch", []string{"alert"}, nil, false, nil, run, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doMailSendAllCoverageRun = %d; stderr: %s", code, stderr.String())
	}
	for _, id := range []string{"gc-1", "gc-2"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got := b.Metadata[mail.FromOrderRunMetadataKey]; got != "pc-run1" {
			t.Fatalf("%s: %s = %q, want pc-run1", id, mail.FromOrderRunMetadataKey, got)
		}
	}
}

// cmdMailReplyAsHuman is cmdMailReply with an explicit --from human: the
// operator replying from his own terminal. Tests written before ga-fi21sm
// replied with no identity and relied on the "human" fallback it removed.
func cmdMailReplyAsHuman(args []string, notify bool, stdout, stderr io.Writer) int {
	return cmdMailReplyFromJSON(args, "human", "", "", notify, false, stdout, stderr)
}

// gc handoff --target with no identity is refused before anything is
// written, and the refusal names --from human (TestCmdHandoff_FromNamesTargetSender
// covers that path).
func TestCmdHandoffRemote_NoIdentityRefused(t *testing.T) {
	clearMailIdentityEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
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
	if _, err := store.Create(beads.Bead{
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"alias": "recipient", "session_name": "recipient-gc-42"},
	}); err != nil {
		t.Fatalf("Create recipient: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdHandoffRemote([]string{"context cycle"}, "recipient", &stdout, &stderr); code == 0 {
		t.Fatalf("handoff --target with no identity = 0, want a refusal; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc handoff: no sender identity") || !strings.Contains(stderr.String(), "--from human") {
		t.Fatalf("stderr = %q, want the handoff refusal naming --from human", stderr.String())
	}
	assertNoMessageBeads(t, cityPath)
}
