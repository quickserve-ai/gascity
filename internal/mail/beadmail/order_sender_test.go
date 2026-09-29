package beadmail

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

var _ mail.MetadataSender = (*Provider)(nil)

func TestSendWithMetadataStampsTheMessage(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	msg, err := p.SendWithMetadata("order:city/deacon-watch", "mayor", "s", "b",
		map[string]string{mail.FromOrderRunMetadataKey: "pc-run1"})
	if err != nil {
		t.Fatalf("SendWithMetadata: %v", err)
	}
	b, err := store.Get(msg.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.From != "order:city/deacon-watch" {
		t.Fatalf("From = %q, want order:city/deacon-watch", b.From)
	}
	if got := b.Metadata[mail.FromOrderRunMetadataKey]; got != "pc-run1" {
		t.Fatalf("%s = %q, want pc-run1", mail.FromOrderRunMetadataKey, got)
	}
}

func TestSendDedupedWithMetadataStampsOnlyANewMessage(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	run := func(id string) map[string]string { return map[string]string{mail.FromOrderRunMetadataKey: id} }

	first, suppressed, err := p.SendDedupedWithMetadata("order:city/w", "mayor", "s", "b", "k", run("pc-run1"))
	if err != nil || suppressed {
		t.Fatalf("first send: suppressed=%v err=%v", suppressed, err)
	}
	b, err := store.Get(first.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.Metadata[mail.FromOrderRunMetadataKey] != "pc-run1" || b.Metadata[mail.DedupKeyMetadataKey] != "k" {
		t.Fatalf("metadata = %v, want run pc-run1 and dedup key k", b.Metadata)
	}

	again, suppressed, err := p.SendDedupedWithMetadata("order:city/w", "mayor", "s", "b", "k", run("pc-run2"))
	if err != nil || !suppressed || again.ID != first.ID {
		t.Fatalf("second send: id=%s suppressed=%v err=%v, want the first message suppressed", again.ID, suppressed, err)
	}
	b, _ = store.Get(first.ID)
	if b.Metadata[mail.FromOrderRunMetadataKey] != "pc-run1" {
		t.Fatalf("suppressed send changed the run to %q", b.Metadata[mail.FromOrderRunMetadataKey])
	}
}

// An order has no mailbox, so a reply to its mail is refused with a clear
// error rather than written to an address nobody reads.
func TestReplyToOrderSenderIsRefused(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	sent, err := p.Send("order:city/deacon-watch", "mayor", "alert", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, err = p.Reply(sent.ID, "mayor", "", "thanks")
	if err == nil || !strings.Contains(err.Error(), "an order, which has no mailbox") {
		t.Fatalf("Reply error = %v, want the order-has-no-mailbox refusal", err)
	}
}
