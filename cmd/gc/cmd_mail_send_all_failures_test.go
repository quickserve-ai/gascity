package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

// failingSendProvider fails Send for chosen recipients with a staged error and
// delegates every other call to a real provider.
type failingSendProvider struct {
	mail.Provider
	fail  map[string]error
	calls []string
}

func (p *failingSendProvider) Send(from, to, subject, body string) (mail.Message, error) {
	p.calls = append(p.calls, to)
	if err, ok := p.fail[to]; ok {
		return mail.Message{}, err
	}
	return p.Provider.Send(from, to, subject, body)
}

// ga-0ejdbv review finding 3: --all stopped at the first failing recipient and
// said "re-send", so a re-run duplicated to everyone before it. Every
// recipient must be attempted, and the summary must name exactly who needs a
// re-send and which IDs to check.
func TestMailSendAll_FailuresDoNotStopTheBroadcastAndAreNamed(t *testing.T) {
	lostErr := fmt.Errorf("beadmail send: %w: gc-x", beadmail.ErrNotPersisted)
	unconfirmedErr := fmt.Errorf("beadmail send: %w: %w", beadmail.ErrUnconfirmed, &mail.DeliveryUnconfirmedError{ID: "gc-c1", Cause: beads.ErrVerifyIndeterminate})
	mp := &failingSendProvider{
		Provider: beadmail.New(beads.NewMemStore()),
		fail:     map[string]error{"bravo": lostErr, "charlie": unconfirmedErr},
	}
	recipients := map[string]bool{"alpha": true, "bravo": true, "charlie": true, "delta": true, "sender": true}

	var stdout, stderr bytes.Buffer
	code := doMailSendAll(mp, events.Discard, recipients, "sender", []string{"subj", "body"}, &stdout, &stderr)
	if code != mailSendNotPersistedExit {
		t.Fatalf("exit = %d, want %d (a verified loss is the most actionable verdict); stderr:\n%s", code, mailSendNotPersistedExit, stderr.String())
	}
	if got := strings.Join(mp.calls, ","); got != "alpha,bravo,charlie,delta" {
		t.Fatalf("Send attempted for %q, want every recipient in order", got)
	}
	out, errOut := stdout.String(), stderr.String()
	for _, want := range []string{"to alpha", "to delta"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q (recipients after a failure must still be sent):\n%s", want, out)
		}
	}
	for _, want := range []string{"do NOT re-run --all", "NOT DELIVERED, re-send to each by address: bravo", "charlie (gc-c1)"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, "re-send to each by address: bravo, charlie") || strings.Contains(errOut, "alpha,") {
		t.Errorf("stderr asks for a re-send it should not:\n%s", errOut)
	}
}

func TestMailSendAll_OnlyUnconfirmedExitsUnconfirmed(t *testing.T) {
	unconfirmedErr := fmt.Errorf("beadmail send: %w: %w", beadmail.ErrUnconfirmed, &mail.DeliveryUnconfirmedError{ID: "gc-b1", Cause: errors.New("i/o timeout")})
	mp := &failingSendProvider{
		Provider: beadmail.New(beads.NewMemStore()),
		fail:     map[string]error{"bravo": unconfirmedErr},
	}
	recipients := map[string]bool{"alpha": true, "bravo": true, "sender": true}
	var stdout, stderr bytes.Buffer
	if code := doMailSendAll(mp, events.Discard, recipients, "sender", []string{"subj", "body"}, &stdout, &stderr); code != mailSendUnconfirmedExit {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, mailSendUnconfirmedExit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "bravo (gc-b1)") {
		t.Fatalf("stderr does not name the ID to check:\n%s", stderr.String())
	}
}
