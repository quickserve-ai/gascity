package beadmail

import (
	"log"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// predecessorMailWindow bounds how old mail addressed to a named seat's closed
// session may be and still surface in that seat's inbox. It matches the window
// the city's stranded-mail patrol uses to point fresh strands at the seat;
// older strands have already been reported to their senders, and surfacing
// them now would deliver stale mail as current.
const predecessorMailWindow = 12 * time.Hour

// inboxRoutes is the read-side address set of one or more mailboxes: every
// route the recipients answer to, plus the bead ids of a named seat's own
// closed sessions. Reply addresses the sender's session bead id, and a seat
// that restarts before reading the reply would otherwise never see it. A pool
// alias is reused by unrelated workers, so only a configured named identity
// links one session to the next.
type inboxRoutes struct {
	// routes lists every assignee whose open mail belongs to the mailboxes.
	// Empty means no recipient was named: every open message is admitted.
	routes []string
	// predecessors is the subset of routes that are closed sessions of a
	// named seat; their mail is admitted only when created at or after since.
	predecessors []string
	since        time.Time
}

// inboxRoutesForAll builds the inbox address set for recipients. A route a
// recipient answers to directly is never demoted to a predecessor route, so
// naming a closed session outright still shows all of its mail.
func (p *Provider) inboxRoutesForAll(recipients []string) inboxRoutes {
	mailbox := inboxRoutes{since: p.currentTime().Add(-predecessorMailWindow)}
	var seats []string
	for _, recipient := range recipients {
		routes, owner, live := p.recipientMailbox(recipient)
		for _, route := range routes {
			mailbox.routes = appendRecipientRoute(mailbox.routes, route)
		}
		if live {
			// An unnamed session's identity is empty, which appendRecipientRoute drops.
			seats = appendRecipientRoute(seats, session.NamedSessionIdentityInfo(owner))
		}
	}
	for _, seat := range seats {
		for _, id := range p.predecessorSessionIDs(seat) {
			if containsRecipientRoute(mailbox.routes, id) {
				continue
			}
			mailbox.routes = append(mailbox.routes, id)
			mailbox.predecessors = append(mailbox.predecessors, id)
		}
	}
	return mailbox
}

// admits reports whether an open message bead belongs to the mailboxes.
func (r inboxRoutes) admits(b beads.Bead) bool {
	if len(r.routes) == 0 {
		return true
	}
	if !matchesRecipientRoute(r.routes, b.Assignee) {
		return false
	}
	if containsRecipientRoute(r.predecessors, b.Assignee) {
		return !b.CreatedAt.Before(r.since)
	}
	return true
}

// predecessorSessionIDs returns the bead ids of the closed sessions that
// carried one configured named identity. A failed lookup degrades to the
// seat's own routes, as a failed route resolution does.
func (p *Provider) predecessorSessionIDs(identity string) []string {
	if p.sessions == nil {
		return nil
	}
	closed, err := p.sessions.ListClosedByNamedIdentity(identity)
	if err != nil {
		log.Printf("beadmail: listing closed sessions of named seat %q: %v", identity, err)
		return nil
	}
	ids := make([]string, 0, len(closed))
	for _, info := range closed {
		ids = append(ids, info.ID)
	}
	return ids
}

func (p *Provider) currentTime() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}
