package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
)

func enableCrossCity(state *fakeState) {
	state.cfg.Mail.CrossCity = &config.MailCrossCityConfig{
		City:   "test-city",
		Cities: []string{"gastown", "westeros"},
	}
}

// A fresh send to a peer city resolves from the roster alone and stores the
// address canonical as written — the fresh-send gate is the one place the
// city-qualified namespace was refused.
func TestMailSendCrossCityForeignRecipient(t *testing.T) {
	state := newFakeState(t)
	enableCrossCity(state)
	h := newTestCityHandler(t, state)

	body := `{"from":"mayor","to":"gastown/mayor","subject":"cutover","body":"leg is green"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/mail"), bytes.NewBufferString(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("send status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var sent mail.Message
	json.NewDecoder(rec.Body).Decode(&sent) //nolint:errcheck
	if sent.To != "gastown/mayor" {
		t.Errorf("To = %q, want canonical %q", sent.To, "gastown/mayor")
	}
	if sent.From != "test-city/mayor" {
		t.Errorf("From = %q, want city-qualified %q (a plain reply must resolve back)", sent.From, "test-city/mayor")
	}
}

// A reply that crosses cities stores this city's sender city-qualified, so
// the far side's plain reply resolves back here — on the API surface
// exactly as on the CLI.
func TestMailReplyCrossCityQualifiesSender(t *testing.T) {
	state := newFakeState(t)
	enableCrossCity(state)
	h := newTestCityHandler(t, state)

	seeded, err := state.cityMailProv.Send("gastown/mayor", "myrig/worker", "cutover", "leg is green")
	if err != nil {
		t.Fatalf("seed Send: %v", err)
	}

	body := `{"from":"worker","subject":"re: cutover","body":"received"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/mail/")+seeded.ID+"/reply", bytes.NewBufferString(body)))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("reply status = %d, want success; body: %s", rec.Code, rec.Body.String())
	}
	var reply mail.Message
	json.NewDecoder(rec.Body).Decode(&reply) //nolint:errcheck
	if reply.To != "gastown/mayor" {
		t.Errorf("To = %q, want %q", reply.To, "gastown/mayor")
	}
	if reply.From != "test-city/worker" {
		t.Errorf("From = %q, want city-qualified %q", reply.From, "test-city/worker")
	}
}

// <local city>/<addr> and <addr> are one mailbox: a local-qualified fresh
// send canonicalizes exactly as the bare form does.
func TestMailSendCrossCityLocalQualifiedRecipient(t *testing.T) {
	state := newFakeState(t)
	enableCrossCity(state)
	h := newTestCityHandler(t, state)

	body := `{"from":"mayor","to":"test-city/worker","subject":"s","body":"b"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/mail"), bytes.NewBufferString(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("send status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var sent mail.Message
	json.NewDecoder(rec.Body).Decode(&sent) //nolint:errcheck
	if sent.To != "myrig/worker" {
		t.Errorf("To = %q, want %q (local-qualified resolves like the bare form)", sent.To, "myrig/worker")
	}
}

// An unknown city-shaped segment refuses with the typed unknown-city message
// naming the roster, never a session lookup failure.
func TestMailSendCrossCityUnknownCityRefused(t *testing.T) {
	state := newFakeState(t)
	enableCrossCity(state)
	h := newTestCityHandler(t, state)

	body := `{"from":"mayor","to":"gastwn/mayor","subject":"s","body":"b"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/mail"), bytes.NewBufferString(body)))
	if rec.Code == http.StatusCreated {
		t.Fatalf("send status = %d, want refusal; body: %s", rec.Code, rec.Body.String())
	}
	respBody := rec.Body.String()
	if !strings.Contains(respBody, "unknown city") || !strings.Contains(respBody, "gastwn") {
		t.Errorf("body = %q, want typed unknown-city refusal naming %q", respBody, "gastwn")
	}
	if strings.Contains(respBody, "session") {
		t.Errorf("body = %q: unknown-city refusal must not be spelled as a session lookup failure", respBody)
	}
}

// Without [mail.crosscity], a city-shaped recipient keeps today's refusal.
func TestMailSendCrossCityNoRosterUnchanged(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	body := `{"from":"mayor","to":"gastown/mayor","subject":"s","body":"b"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/mail"), bytes.NewBufferString(body)))
	if rec.Code == http.StatusCreated {
		t.Fatalf("send status = %d, want refusal without a roster; body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "unknown city") {
		t.Errorf("body = %q: no roster means no unknown-city semantics", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no bead store available") {
		t.Errorf("body = %q: the no-roster refusal must keep today's exact shape", rec.Body.String())
	}
}

// Delivery is a read: a message addressed to the reader's city-qualified
// form is served by the reader's ordinary inbox query.
func TestMailInboxCrossCityServesCityQualifiedDelivery(t *testing.T) {
	state := newFakeState(t)
	enableCrossCity(state)
	h := newTestCityHandler(t, state)

	if _, err := state.cityMailProv.Send("gastown/mayor", "test-city/myrig/worker", "hello", "over the shared store"); err != nil {
		t.Fatalf("seed Send: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/mail?agent=myrig/worker"), nil))
	var inbox struct {
		Items []mail.Message `json:"items"`
		Total int            `json:"total"`
	}
	json.NewDecoder(rec.Body).Decode(&inbox) //nolint:errcheck
	if inbox.Total != 1 {
		t.Fatalf("inbox Total = %d, want 1 (city-qualified delivery must be a read); body: %+v", inbox.Total, inbox.Items)
	}
	if inbox.Items[0].To != "test-city/myrig/worker" {
		t.Errorf("To = %q, want %q", inbox.Items[0].To, "test-city/myrig/worker")
	}
}

// flakyGetMailProvider answers Get for the provider lookup, then fails every
// later Get, so the reply handler's own origin read fails while Reply would
// still succeed — the fail-open shape the cross-city rules must refuse.
type flakyGetMailProvider struct {
	mail.Provider
	okGets int
	gets   int
}

func (p *flakyGetMailProvider) Get(id string) (mail.Message, error) {
	p.gets++
	if p.gets <= p.okGets {
		return p.Provider.Get(id)
	}
	return mail.Message{}, fmt.Errorf("store_slow: transient read failure")
}

// With the roster enabled, a reply whose thread origin cannot be read fails
// closed on the API exactly as on the CLI: never a reply across cities with
// a bare sender.
func TestMailReplyCrossCityFailsClosedWhenOriginUnreadable(t *testing.T) {
	state := newFakeState(t)
	enableCrossCity(state)
	seeded, err := state.cityMailProv.Send("gastown/mayor", "myrig/worker", "cutover", "leg is green")
	if err != nil {
		t.Fatalf("seed Send: %v", err)
	}
	flaky := &flakyGetMailProvider{Provider: state.cityMailProv, okGets: 1}
	state.cityMailProv = flaky
	h := newTestCityHandler(t, state)

	body := `{"from":"worker","subject":"re: cutover","body":"received"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/mail/")+seeded.ID+"/reply", bytes.NewBufferString(body)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("reply status = %d, want 500 (origin unreadable is an internal fail-closed, not a client error); body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cross_city_origin_unverified") {
		t.Errorf("body = %q, want the cross_city_origin_unverified refusal", rec.Body.String())
	}
	inbox, _ := flaky.Provider.Inbox("gastown/mayor")
	for _, m := range inbox {
		if m.ID != seeded.ID {
			t.Errorf("a reply %q was written despite the refusal", m.ID)
		}
	}
}

// A reply into a thread whose origin names a city outside the roster is
// refused with the typed unknown-city message, never written to a literal
// mailbox nobody polls.
func TestMailReplyCrossCityUnknownOriginRefused(t *testing.T) {
	state := newFakeState(t)
	enableCrossCity(state)
	h := newTestCityHandler(t, state)

	seeded, err := state.cityMailProv.Send("gastwn/mayor", "myrig/worker", "cutover", "typo city")
	if err != nil {
		t.Fatalf("seed Send: %v", err)
	}
	body := `{"from":"worker","subject":"re: cutover","body":"received"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/mail/")+seeded.ID+"/reply", bytes.NewBufferString(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reply status = %d, want 400 (a reply the roster disallows is a client error); body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown city") || !strings.Contains(rec.Body.String(), "gastwn") {
		t.Errorf("body = %q, want typed unknown-city refusal naming gastwn", rec.Body.String())
	}
	inbox, _ := state.cityMailProv.Inbox("gastwn/mayor")
	if len(inbox) != 0 {
		t.Errorf("reply written to the literal mailbox gastwn/mayor: %+v", inbox)
	}
}

// The API roster names this city from the configured [mail.crosscity] city,
// never from the supervisor's registered workspace name, so the CLI (which
// reads the same field) and the API stamp one spelling on cross-city mail.
func TestMailCityRosterUsesConfiguredCity(t *testing.T) {
	state := newFakeState(t)
	enableCrossCity(state)
	state.cfg.ResolvedWorkspaceName = "registered-name"
	state.cfg.Workspace.Name = "workspace-name"
	srv := &Server{state: state}
	if got := srv.mailCityRoster().Local; got != "test-city" {
		t.Errorf("roster.Local = %q, want the configured city %q", got, "test-city")
	}
}
