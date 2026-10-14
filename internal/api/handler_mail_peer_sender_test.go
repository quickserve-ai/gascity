package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/mail"
)

// A seat of a peer city mails this city over `gc --context` as
// "<peer city>/<identity>" (cmd/gc remoteMailSender). Upstream #7766 sends only
// as an identity this city configures; the carry (#5386 via the PROP-027
// roster) also accepts a sender qualified by a [mail.crosscity] peer, and
// stores it exactly as sent so the reply resolves back. Drop with the
// exception when upstream binds a sender to its caller (mc-8e9ci).
func TestMailSendFromPeerCitySeatStoresTheSenderVerbatim(t *testing.T) {
	state := newFakeState(t)
	enableCrossCity(state)
	h := newTestCityHandler(t, state)

	const from = "gastown/gascity/navani"
	rec := beadRequest(t, h, "POST", cityURL(state, "/mail"), `{"from":"`+from+`","to":"worker","subject":"hub leg","body":"b"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send from %q: status %d (%s), want 201", from, rec.Code, rec.Body.String())
	}
	var sent mail.Message
	if err := json.NewDecoder(rec.Body).Decode(&sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sent.From != from || sent.To != "myrig/worker" {
		t.Fatalf("sent = from %q to %q, want from %q (verbatim) to myrig/worker", sent.From, sent.To, from)
	}
	inbox, err := state.cityMailProv.Inbox("myrig/worker")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(inbox) != 1 || inbox[0].From != from {
		t.Fatalf("stored inbox = %+v, want one message from %q", inbox, from)
	}
}

func TestMailReplyFromPeerCitySeatIsAccepted(t *testing.T) {
	state := newFakeState(t)
	enableCrossCity(state)
	h := newTestCityHandler(t, state)
	const from = "gastown/gascity/navani"
	msg, err := state.cityMailProv.Send("myrig/worker", from, "status", "where is it")
	if err != nil {
		t.Fatalf("seed Send: %v", err)
	}

	rec := beadRequest(t, h, "POST", cityURL(state, "/mail/")+msg.ID+"/reply", `{"from":"`+from+`","subject":"re: status","body":"landed"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reply from %q: status %d (%s), want 201", from, rec.Code, rec.Body.String())
	}
	var reply mail.Message
	if err := json.NewDecoder(rec.Body).Decode(&reply); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reply.From != from || reply.To != "myrig/worker" {
		t.Fatalf("reply = from %q to %q, want from %q (verbatim) to myrig/worker", reply.From, reply.To, from)
	}
}

// Everything that is not a peer city's seat keeps upstream's refusal: 422 at
// body.from and nothing sent. A bare "<rig>/<name>" is no peer seat: the
// remote sender carries no city only when the sender has no local city, and
// then nothing says which city it came from.
func TestMailSenderOutsideThePeerRosterStillRefuses(t *testing.T) {
	for _, c := range []struct {
		name, from string
		roster     bool
	}{
		{"an unknown city", "gastwn/gascity/navani", true},
		{"no city segment", "gascity/navani", true},
		{"a peer with an empty remainder", "gastown/", true},
		{"this city's own name", "test-city/nobody", true},
		{"unqualified and unconfigured", "mallory", true},
		{"empty", "", true},
		{"a peer-shaped sender with no [mail.crosscity]", "gastown/gascity/navani", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := newFakeState(t)
			if c.roster {
				enableCrossCity(state)
			}
			h := newTestCityHandler(t, state)
			rec := beadRequest(t, h, "POST", cityURL(state, "/mail"), `{"from":"`+c.from+`","to":"worker","subject":"s","body":"b"}`)
			inbox, _ := state.cityMailProv.Inbox("myrig/worker")
			if rec.Code != http.StatusUnprocessableEntity || strings.Join(problemLocations(t, rec), ",") != "body.from" || len(inbox) != 0 {
				t.Fatalf("send from %q: status %d (%s), inbox %d; want 422 at body.from and nothing sent", c.from, rec.Code, rec.Body.String(), len(inbox))
			}

			msg, err := state.cityMailProv.Send("human", "myrig/worker", "Hi", "hello")
			if err != nil {
				t.Fatalf("seed Send: %v", err)
			}
			rec = beadRequest(t, h, "POST", cityURL(state, "/mail/")+msg.ID+"/reply", `{"from":"`+c.from+`","subject":"re","body":"r"}`)
			if rec.Code != http.StatusUnprocessableEntity || strings.Join(problemLocations(t, rec), ",") != "body.from" {
				t.Fatalf("reply from %q: status %d (%s), want 422 at body.from", c.from, rec.Code, rec.Body.String())
			}
		})
	}
}
