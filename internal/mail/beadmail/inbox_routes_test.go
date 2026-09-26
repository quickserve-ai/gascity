package beadmail

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

// Reply pins the reply to the original sender's session bead id (see
// TestReplyUsesStoredSenderSessionIDAfterAliasRename), and seats restart
// several times a day, so a reply to a seat that rolled before reading it is
// assigned to a closed session bead. These tests pin the read side: a named
// seat's next session sees that mail, a pool worker reusing an alias does not,
// and nothing older than predecessorMailWindow comes back as current.

func namedSeatSessionMetadata(identity, sessionName string) map[string]string {
	return map[string]string{
		"alias":                              identity,
		"session_name":                       sessionName,
		session.NamedSessionIdentityMetadata: identity,
		"configured_named_session":           "true",
	}
}

func poolSessionMetadata(alias, sessionName string) map[string]string {
	return map[string]string{
		"alias":        alias,
		"session_name": sessionName,
		"pool_managed": "true",
	}
}

// replyToRetiredSession sends a message from a session built from retiredMetadata,
// closes that session, starts its successor with nextMetadata, and replies to
// the original. It returns the reply and the successor session bead.
func replyToRetiredSession(t *testing.T, store beads.Store, p *Provider, retiredMetadata, nextMetadata map[string]string) (reply mail.Message, next beads.Bead) {
	t.Helper()
	retired := mustCreateSessionBead(t, store, retiredMetadata)
	original, err := p.Send(retiredMetadata["alias"], "human", "Approval", "please approve")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := store.Close(retired.ID); err != nil {
		t.Fatalf("Close retired session: %v", err)
	}
	next = mustCreateSessionBead(t, store, nextMetadata)
	reply, err = p.Reply(original.ID, "human", "approved", "approved")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	b, err := store.Get(reply.ID)
	if err != nil {
		t.Fatalf("Get reply: %v", err)
	}
	if b.Assignee != retired.ID {
		t.Fatalf("reply Assignee = %q, want the retired sender session %q (Reply addressing must not change)", b.Assignee, retired.ID)
	}
	return reply, next
}

func messageIDs(msgs []mail.Message) []string {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return ids
}

func containsMessage(msgs []mail.Message, id string) bool {
	for _, m := range msgs {
		if m.ID == id {
			return true
		}
	}
	return false
}

func TestNamedSeatInboxSurfacesReplyToItsClosedSession(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	reply, next := replyToRetiredSession(t, store, p,
		namedSeatSessionMetadata("rig/seat", "rig--seat"),
		namedSeatSessionMetadata("rig/seat", "rig--seat"))

	for _, recipient := range []string{"rig/seat", next.ID, "rig--seat"} {
		inbox, err := p.Inbox(recipient)
		if err != nil {
			t.Fatalf("Inbox(%s): %v", recipient, err)
		}
		if len(inbox) != 1 || inbox[0].ID != reply.ID {
			t.Fatalf("Inbox(%s) = %v, want the reply %s addressed to the seat's closed session", recipient, messageIDs(inbox), reply.ID)
		}
		checked, err := p.Check(recipient)
		if err != nil {
			t.Fatalf("Check(%s): %v", recipient, err)
		}
		if len(checked) != 1 || checked[0].ID != reply.ID {
			t.Fatalf("Check(%s) = %v, want the reply %s", recipient, messageIDs(checked), reply.ID)
		}
		total, unread, err := p.Count(recipient)
		if err != nil {
			t.Fatalf("Count(%s): %v", recipient, err)
		}
		if total != 1 || unread != 1 {
			t.Fatalf("Count(%s) = (%d, %d), want (1, 1)", recipient, total, unread)
		}
	}

	inbox, err := p.InboxRecipients([]string{"rig/seat", next.ID})
	if err != nil {
		t.Fatalf("InboxRecipients: %v", err)
	}
	if len(inbox) != 1 || inbox[0].ID != reply.ID {
		t.Fatalf("InboxRecipients = %v, want the reply %s once", messageIDs(inbox), reply.ID)
	}
	total, unread, err := p.CountRecipients([]string{"rig/seat", next.ID})
	if err != nil {
		t.Fatalf("CountRecipients: %v", err)
	}
	if total != 1 || unread != 1 {
		t.Fatalf("CountRecipients = (%d, %d), want (1, 1)", total, unread)
	}

	if err := p.MarkRead(reply.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	all, err := p.All("rig/seat")
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 1 || all[0].ID != reply.ID {
		t.Fatalf("All(rig/seat) after read = %v, want the read reply %s", messageIDs(all), reply.ID)
	}
	candidates, err := p.ArchiveCandidates(ArchiveFilter{Recipients: []string{"rig/seat"}, IncludeRead: true})
	if err != nil {
		t.Fatalf("ArchiveCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != reply.ID {
		t.Fatalf("ArchiveCandidates(rig/seat) = %v, want the reply %s the inbox shows", messageIDs(candidates), reply.ID)
	}
}

// TestNamedSeatInboxFindsPredecessorsByIdentityNotAlias pins that the seat is
// keyed on its configured named identity: a seat whose alias differs from that
// identity still reaches its closed sessions' mail.
func TestNamedSeatInboxFindsPredecessorsByIdentityNotAlias(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)
	seat := namedSeatSessionMetadata("rig/seat", "rig--seat")
	seat["alias"] = "seat-alias"

	reply, _ := replyToRetiredSession(t, store, p, seat, seat)

	inbox, err := p.Inbox("seat-alias")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(inbox) != 1 || inbox[0].ID != reply.ID {
		t.Fatalf("Inbox(seat-alias) = %v, want the reply %s addressed to the seat's closed session", messageIDs(inbox), reply.ID)
	}
}

func TestNamedSeatInboxLeavesAnotherSeatsClosedSessionMail(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	otherReply, _ := replyToRetiredSession(t, store, p,
		namedSeatSessionMetadata("rig/other", "rig--other"),
		namedSeatSessionMetadata("rig/other", "rig--other"))
	mustCreateSessionBead(t, store, namedSeatSessionMetadata("rig/seat", "rig--seat"))

	inbox, err := p.Inbox("rig/seat")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if containsMessage(inbox, otherReply.ID) {
		t.Fatalf("Inbox(rig/seat) = %v, surfaced %s addressed to another seat's closed session", messageIDs(inbox), otherReply.ID)
	}
}

// TestPoolInboxDoesNotSurfaceReplyToClosedSessionUnderReusedAlias pins the
// reason Reply addresses a session id at all: a pool alias is reused by later
// workers, and a reply to the retired worker is not the new worker's mail.
func TestPoolInboxDoesNotSurfaceReplyToClosedSessionUnderReusedAlias(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	reply, next := replyToRetiredSession(t, store, p,
		poolSessionMetadata("rig/pool.worker", "rig--pool__worker-1"),
		poolSessionMetadata("rig/pool.worker", "rig--pool__worker-2"))

	for _, recipient := range []string{"rig/pool.worker", next.ID} {
		inbox, err := p.Inbox(recipient)
		if err != nil {
			t.Fatalf("Inbox(%s): %v", recipient, err)
		}
		if containsMessage(inbox, reply.ID) {
			t.Fatalf("Inbox(%s) = %v, surfaced reply %s addressed to a retired pool worker", recipient, messageIDs(inbox), reply.ID)
		}
		total, unread, err := p.Count(recipient)
		if err != nil {
			t.Fatalf("Count(%s): %v", recipient, err)
		}
		if total != 0 || unread != 0 {
			t.Fatalf("Count(%s) = (%d, %d), want (0, 0)", recipient, total, unread)
		}
	}
}

func TestNamedSeatInboxLeavesPredecessorMailOlderThanTheWindow(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	reply, next := replyToRetiredSession(t, store, p,
		namedSeatSessionMetadata("rig/seat", "rig--seat"),
		namedSeatSessionMetadata("rig/seat", "rig--seat"))
	direct, err := p.Send("human", "rig/seat", "direct", "addressed to the seat itself")
	if err != nil {
		t.Fatalf("Send direct: %v", err)
	}

	p.now = func() time.Time { return time.Now().Add(predecessorMailWindow + time.Minute) }

	inbox, err := p.Inbox("rig/seat")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if containsMessage(inbox, reply.ID) {
		t.Fatalf("Inbox(rig/seat) = %v, surfaced predecessor mail %s older than %s as current", messageIDs(inbox), reply.ID, predecessorMailWindow)
	}
	if !containsMessage(inbox, direct.ID) {
		t.Fatalf("Inbox(rig/seat) = %v, dropped %s addressed to the seat itself; the window bounds predecessor mail only", messageIDs(inbox), direct.ID)
	}
	total, unread, err := p.Count(next.ID)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 1 || unread != 1 {
		t.Fatalf("Count(%s) = (%d, %d), want (1, 1): only the seat-addressed message, not the stale predecessor reply", next.ID, total, unread)
	}
}
