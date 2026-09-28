package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/mail"
)

// ga-nee27h: send-mail and reply-mail answer 202 for an UNCONFIRMED write.
// The client must surface that as an error carrying the message ID in a
// *mail.DeliveryUnconfirmedError — never as success, and never as a bare
// "API returned 202 with no body" that loses the ID to check.
func TestClientMailWriteUnconfirmed202CarriesMessageID(t *testing.T) {
	// In-process transport: the 202 is served without opening a loopback
	// listener (the untagged http_test_server census cannot grow).
	answer202 := rtFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteHeader(http.StatusAccepted)
		json.NewEncoder(rec).Encode(mail.Message{ID: "gc-unc-client", From: "mayor", To: "worker", Subject: "s"}) //nolint:errcheck
		resp := rec.Result()
		resp.Request = r
		return resp, nil
	})
	const baseURL = "http://supervisor.test"
	cw, err := genclient.NewClientWithResponses(
		baseURL,
		genclient.WithHTTPClient(&http.Client{Transport: answer202}),
		genclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("X-GC-Request", "true")
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	c := &Client{cw: cw, baseURL: baseURL, cityName: "alpha"}

	for name, call := range map[string]func() (mail.Message, error){
		"send":  func() (mail.Message, error) { return c.SendMail(MailSendRequest{To: "worker", Subject: "s"}) },
		"reply": func() (mail.Message, error) { return c.ReplyMail("gc-orig", MailReplyRequest{Body: "r"}) },
	} {
		m, err := call()
		if err == nil {
			t.Fatalf("%s: a 202 returned success; want an unconfirmed-delivery error", name)
		}
		id, ok := mail.UnconfirmedMessageID(err)
		if !ok || id != "gc-unc-client" {
			t.Fatalf("%s: UnconfirmedMessageID(%v) = (%q, %v), want (%q, true)", name, err, id, ok, "gc-unc-client")
		}
		if m.ID != "gc-unc-client" {
			t.Fatalf("%s: returned message ID = %q, want %q", name, m.ID, "gc-unc-client")
		}
	}
}
