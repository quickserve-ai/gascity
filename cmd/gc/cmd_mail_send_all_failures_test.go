package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
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

// ga-0ejdbv round 2, finding 1: the send and reply commands returned errExit
// for every non-zero code, so the shell saw 1 for "lost" (5) and
// "unconfirmed" (6) alike. Drive the cobra commands and read the exit code
// the process would use.
func TestMailSendAndReplyCommandsCarryTheVerdictExitCode(t *testing.T) {
	savedSend, savedReply, savedReplyJSON := mailSendRunner, mailReplyRunner, mailReplyJSONRunner
	t.Cleanup(func() { mailSendRunner, mailReplyRunner, mailReplyJSONRunner = savedSend, savedReply, savedReplyJSON })
	for _, want := range []int{0, 1, mailSendNotPersistedExit, mailSendUnconfirmedExit} {
		mailSendRunner = func([]string, bool, bool, string, string, string, string, string, string, bool, io.Writer, io.Writer) int {
			return want
		}
		mailReplyRunner = func([]string, string, string, bool, io.Writer, io.Writer) int { return want }
		var out, errOut bytes.Buffer
		send := newMailSendCmd(&out, &errOut)
		send.SetArgs([]string{"--no-notify", "bob", "hi"})
		send.SilenceErrors, send.SilenceUsage = true, true
		if got := commandExitCode(send.Execute()); got != want {
			t.Errorf("gc mail send exit = %d, want %d", got, want)
		}
		reply := newMailReplyCmd(&out, &errOut)
		reply.SetArgs([]string{"gc-1", "-m", "hi"})
		reply.SilenceErrors, reply.SilenceUsage = true, true
		if got := commandExitCode(reply.Execute()); got != want {
			t.Errorf("gc mail reply exit = %d, want %d", got, want)
		}
	}
}

// ga-0ejdbv round 2, finding 2: a configured seat with an open session whose
// send was unconfirmed was also listed as "had no open session and got
// NOTHING ... mail each by address", which is advice to blind-re-send.
func TestMailSendAll_AttemptedSeatsAreNotListedAsUnreached(t *testing.T) {
	unconfirmedErr := fmt.Errorf("beadmail send: %w: %w", beadmail.ErrUnconfirmed, &mail.DeliveryUnconfirmedError{ID: "gc-w1", Cause: errors.New("i/o timeout")})
	mp := &failingSendProvider{
		Provider: beadmail.New(beads.NewMemStore()),
		fail:     map[string]error{"woodhouse": unconfirmedErr},
	}
	recipients := map[string]bool{"gastown.mayor": true, "woodhouse": true, "katya": true}
	cov := &broadcastCoverage{
		configured: []string{"woodhouse", "katya", "qcore/archer", "gastown.mayor"},
		open: []session.Info{
			{ID: "ga-1", Alias: "woodhouse", ConfiguredNamedIdentity: "woodhouse"},
			{ID: "ga-2", Alias: "katya", ConfiguredNamedIdentity: "katya"},
			{ID: "ga-3", Alias: "gastown.mayor", ConfiguredNamedIdentity: "gastown.mayor"},
		},
	}
	var stdout, stderr bytes.Buffer
	code := doMailSendAllCoverage(mp, events.Discard, recipients, "gastown.mayor", []string{"s", "b"}, nil, false, cov, &stdout, &stderr)
	if code != mailSendUnconfirmedExit {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, mailSendUnconfirmedExit, stderr.String())
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "qcore/archer") {
		t.Fatalf("the genuinely unreached seat is not named:\n%s", errOut)
	}
	for _, line := range strings.Split(errOut, "\n") {
		if strings.Contains(line, "got NOTHING") && strings.Contains(line, "woodhouse") {
			t.Fatalf("an attempted (unconfirmed) seat is listed as unreached: %q", line)
		}
	}
	if !strings.Contains(errOut, "woodhouse (gc-w1)") {
		t.Fatalf("the unconfirmed seat is not named with its ID:\n%s", errOut)
	}
}

// ga-0ejdbv round 3, finding 3: with --json and any failure, the result was
// never written, so a JSON caller could not learn who already has the message
// and would re-run --all, duplicating to them.
func TestMailSendAll_JSONFailureStillReportsDeliveredAndFailed(t *testing.T) {
	lostErr := fmt.Errorf("beadmail send: %w: gc-x", beadmail.ErrNotPersisted)
	unconfirmedErr := fmt.Errorf("beadmail send: %w: %w", beadmail.ErrUnconfirmed, &mail.DeliveryUnconfirmedError{ID: "gc-c1", Cause: beads.ErrVerifyIndeterminate})
	mp := &failingSendProvider{
		Provider: beadmail.New(beads.NewMemStore()),
		fail:     map[string]error{"bravo": lostErr, "charlie": unconfirmedErr},
	}
	recipients := map[string]bool{"alpha": true, "bravo": true, "charlie": true, "sender": true}
	var stdout, stderr bytes.Buffer
	code := doMailSendAllCoverage(mp, events.Discard, recipients, "sender", []string{"s", "b"}, nil, true, nil, &stdout, &stderr)
	if code != mailSendNotPersistedExit {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, mailSendNotPersistedExit, stderr.String())
	}
	var res mailActionResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err != nil {
		t.Fatalf("stdout is not one JSON result: %v\n%s", err, stdout.String())
	}
	if res.OK {
		t.Fatalf("ok = true on a partial failure: %+v", res)
	}
	if len(res.Messages) != 1 || res.Messages[0].To != "alpha" {
		t.Fatalf("messages = %+v, want the one delivered to alpha", res.Messages)
	}
	if strings.Join(res.Lost, ",") != "bravo" || len(res.Unconfirmed) != 1 || res.Unconfirmed[0] != (mailUnconfirmedRecipient{To: "charlie", ID: "gc-c1"}) {
		t.Fatalf("lost=%v unconfirmed=%+v, want lost [bravo] and unconfirmed charlie/gc-c1", res.Lost, res.Unconfirmed)
	}
}
