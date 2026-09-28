package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

// countingWriteMailProvider embeds a real provider (so Get works and
// findMailProviderForMessage resolves a seeded message) but answers every
// Send and Reply with writeErr, counting how many times the provider was
// actually asked to write. The count is what proves an Idempotency-Key replay
// did or did not reach the store.
type countingWriteMailProvider struct {
	mail.Provider
	writeErr error

	mu      sync.Mutex
	sends   int
	replies int
}

func (p *countingWriteMailProvider) Send(string, string, string, string) (mail.Message, error) {
	p.mu.Lock()
	p.sends++
	p.mu.Unlock()
	return mail.Message{}, p.writeErr
}

func (p *countingWriteMailProvider) Reply(string, string, string, string) (mail.Message, error) {
	p.mu.Lock()
	p.replies++
	p.mu.Unlock()
	return mail.Message{}, p.writeErr
}

func (p *countingWriteMailProvider) counts() (sends, replies int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sends, p.replies
}

// unconfirmedWriteErr is the error chain beadmail returns for an unconfirmed
// write: ErrUnconfirmed wrapping a DeliveryUnconfirmedError that names the ID.
func unconfirmedWriteErr(op, id string) error {
	return fmt.Errorf("beadmail %s: %w: %w", op, beadmail.ErrUnconfirmed,
		&mail.DeliveryUnconfirmedError{ID: id, Cause: errors.New("i/o timeout")})
}

// lostWriteErr is the error chain beadmail returns for a write VERIFIED absent.
func lostWriteErr(op, id string) error {
	return fmt.Errorf("beadmail %s: %w: %s", op, beadmail.ErrNotPersisted, id)
}

// postMailWithKey drives one mail write with an Idempotency-Key through the
// full Huma stack and returns the recorder.
func postMailWithKey(t *testing.T, h http.Handler, url, body, key string, wantStatus int) *bytes.Buffer {
	t.Helper()
	req := newPostRequest(url, bytes.NewBufferString(body))
	req.Header.Set("Idempotency-Key", key)
	rec := doMailRequest(t, h, req, wantStatus)
	return rec.Body
}

func decodeMailMessage(t *testing.T, body *bytes.Buffer) mail.Message {
	t.Helper()
	var msg mail.Message
	if err := json.NewDecoder(body).Decode(&msg); err != nil {
		t.Fatalf("decode message body: %v; body: %s", err, body.String())
	}
	return msg
}

func mailSentEventCount(t *testing.T, state *fakeState, eventType string) int {
	t.Helper()
	evs, err := state.eventProv.List(events.Filter{Type: eventType})
	if err != nil {
		t.Fatalf("list %s events: %v", eventType, err)
	}
	return len(evs)
}

// ga-nee27h: an UNCONFIRMED send (the store reported created, the read-back
// did not complete) answered 500, and withIdempotency released the
// reservation on that error, so a same-key retry sent a SECOND copy. It must
// answer 202 with the message ID, keep the reservation, and replay the 202
// without a second provider call.
func TestMailSendUnconfirmedReturns202AndReplaysWithoutResend(t *testing.T) {
	state := newFakeState(t)
	prov := &countingWriteMailProvider{Provider: state.cityMailProv, writeErr: unconfirmedWriteErr("send", "gc-unc-send")}
	state.cityMailProv = prov
	h := newTestCityHandler(t, state)
	body := `{"from":"mayor","to":"worker","subject":"s","body":"b"}`

	first := decodeMailMessage(t, postMailWithKey(t, h, cityURL(state, "/mail"), body, "unc-send-1", http.StatusAccepted))
	if first.ID != "gc-unc-send" {
		t.Fatalf("202 body ID = %q, want %q", first.ID, "gc-unc-send")
	}

	replay := decodeMailMessage(t, postMailWithKey(t, h, cityURL(state, "/mail"), body, "unc-send-1", http.StatusAccepted))
	if replay.ID != "gc-unc-send" {
		t.Fatalf("replayed 202 body ID = %q, want %q", replay.ID, "gc-unc-send")
	}
	if sends, _ := prov.counts(); sends != 1 {
		t.Fatalf("provider Send calls = %d, want 1 (a same-key retry of an unconfirmed send must not write a second copy)", sends)
	}
	// No mail.sent event for a write nobody could confirm: the CLI send path
	// records none for an unconfirmed write either.
	if n := mailSentEventCount(t, state, events.MailSent); n != 0 {
		t.Fatalf("mail.sent events = %d, want 0 for an unconfirmed send", n)
	}
}

// The reply twin of the send test above.
func TestMailReplyUnconfirmedReturns202AndReplaysWithoutResend(t *testing.T) {
	state := newFakeState(t)
	orig, err := state.cityMailProv.Send("mayor", "worker", "s", "b")
	if err != nil {
		t.Fatalf("seed Send: %v", err)
	}
	prov := &countingWriteMailProvider{Provider: state.cityMailProv, writeErr: unconfirmedWriteErr("reply", "gc-unc-reply")}
	state.cityMailProv = prov
	h := newTestCityHandler(t, state)
	url := cityURL(state, "/mail/") + orig.ID + "/reply"
	body := `{"from":"worker","subject":"re","body":"r"}`

	first := decodeMailMessage(t, postMailWithKey(t, h, url, body, "unc-reply-1", http.StatusAccepted))
	if first.ID != "gc-unc-reply" {
		t.Fatalf("202 body ID = %q, want %q", first.ID, "gc-unc-reply")
	}

	replay := decodeMailMessage(t, postMailWithKey(t, h, url, body, "unc-reply-1", http.StatusAccepted))
	if replay.ID != "gc-unc-reply" {
		t.Fatalf("replayed 202 body ID = %q, want %q", replay.ID, "gc-unc-reply")
	}
	if _, replies := prov.counts(); replies != 1 {
		t.Fatalf("provider Reply calls = %d, want 1 (a same-key retry of an unconfirmed reply must not write a second copy)", replies)
	}
	if n := mailSentEventCount(t, state, events.MailReplied); n != 0 {
		t.Fatalf("mail.replied events = %d, want 0 for an unconfirmed reply", n)
	}
}

// A LOST write (verified absent) is still a 500, and the reservation is
// released so a same-key retry reaches the provider again: re-sending a
// message that did not land is exactly what the caller should do.
func TestMailSendNotPersistedStays500AndReleasesKey(t *testing.T) {
	state := newFakeState(t)
	prov := &countingWriteMailProvider{Provider: state.cityMailProv, writeErr: lostWriteErr("send", "gc-lost-send")}
	state.cityMailProv = prov
	h := newTestCityHandler(t, state)
	body := `{"from":"mayor","to":"worker","subject":"s","body":"b"}`

	first := postMailWithKey(t, h, cityURL(state, "/mail"), body, "lost-send-1", http.StatusInternalServerError)
	if strings.Contains(first.String(), "mail_unconfirmed") {
		t.Fatalf("a lost write reported as unconfirmed: %s", first.String())
	}
	postMailWithKey(t, h, cityURL(state, "/mail"), body, "lost-send-1", http.StatusInternalServerError)
	if sends, _ := prov.counts(); sends != 2 {
		t.Fatalf("provider Send calls = %d, want 2 (a lost send's reservation must be released so the retry re-sends)", sends)
	}
}

// The reply twin of the lost-send test above.
func TestMailReplyNotPersistedStays500AndReleasesKey(t *testing.T) {
	state := newFakeState(t)
	orig, err := state.cityMailProv.Send("mayor", "worker", "s", "b")
	if err != nil {
		t.Fatalf("seed Send: %v", err)
	}
	prov := &countingWriteMailProvider{Provider: state.cityMailProv, writeErr: lostWriteErr("reply", "gc-lost-reply")}
	state.cityMailProv = prov
	h := newTestCityHandler(t, state)
	url := cityURL(state, "/mail/") + orig.ID + "/reply"
	body := `{"from":"worker","subject":"re","body":"r"}`

	postMailWithKey(t, h, url, body, "lost-reply-1", http.StatusInternalServerError)
	postMailWithKey(t, h, url, body, "lost-reply-1", http.StatusInternalServerError)
	if _, replies := prov.counts(); replies != 2 {
		t.Fatalf("provider Reply calls = %d, want 2 (a lost reply's reservation must be released so the retry re-sends)", replies)
	}
}
