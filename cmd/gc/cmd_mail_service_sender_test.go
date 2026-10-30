package main

import (
	"bytes"
	"strings"
	"testing"
)

// ga-7lmivm.1.1 step 1: a process outside gc (a systemd unit) signs as
// service:<name>. It is accepted from a caller with no session identity, kept
// as the stored From, and never marked unverified: no order run stands behind
// it to check.
func TestCmdMailSend_IdentityLessServiceFromIsAcceptedUnmarked(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{seat.ID, "service page"}, false, false, "service:westeros-pressure", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("cmdMailSend() = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	got := messageBeadsByBody(t, cityPath, "service page")
	if len(got) != 1 {
		t.Fatalf("messages = %d, want 1", len(got))
	}
	if got[0].From != "service:westeros-pressure" {
		t.Fatalf("From = %q, want service:westeros-pressure", got[0].From)
	}
	assertNoUnverifiedMark(t, got[0])
}

// A live seat claiming a service is refused, as any foreign --from is.
func TestCmdMailSend_SeatServiceFromIsRefused(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)
	t.Setenv("GC_AGENT", "worker")

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{seat.ID, "seat as service"}, false, false, "service:westeros-pressure", "", "", "", &stdout, &stderr); code == 0 {
		t.Fatalf("seat --from service: = 0, want a refusal; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "does not match this session's own identity") {
		t.Fatalf("stderr = %q, want the identity refusal", stderr.String())
	}
	if n := len(messageBeadsByBody(t, cityPath, "seat as service")); n != 0 {
		t.Fatalf("messages = %d, want none written", n)
	}
}

// A service has no mailbox: mail to one is refused before anything is
// written, bare or city-qualified (the qualified form must not leave as a
// peer-city address).
func TestCmdMailSend_ToServiceIsRefused(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, _ := orderSenderTestCity(t)

	for _, to := range []string{"service:westeros-pressure", "peer/service:westeros-pressure"} {
		var stdout, stderr bytes.Buffer
		if code := cmdMailSend([]string{to, "to a service " + to}, false, false, "human", "", "", "", &stdout, &stderr); code == 0 {
			t.Fatalf("send to %s = 0, want a refusal; stdout=%s", to, stdout.String())
		}
		if !strings.Contains(stderr.String(), "service sender, which has no mailbox") {
			t.Fatalf("send to %s: stderr = %q, want the no-mailbox refusal", to, stderr.String())
		}
		if n := len(messageBeadsByBody(t, cityPath, "to a service "+to)); n != 0 {
			t.Fatalf("send to %s: messages = %d, want none written", to, n)
		}
	}
}

// Nor can a service's mail be replied to, bare or city-qualified: the reply
// would go nowhere.
func TestCmdMailReply_ToServiceIsRefused(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)
	for _, from := range []string{"service:westeros-verdicts", "peer/service:westeros-verdicts"} {
		var stdout, stderr bytes.Buffer
		if code := cmdMailSend([]string{seat.ID, "page to answer " + from}, false, false, from, "", "", "", &stdout, &stderr); code != 0 {
			t.Fatalf("seed send from %s = %d; stderr=%s", from, code, stderr.String())
		}
		orig := messageBeadsByBody(t, cityPath, "page to answer "+from)
		if len(orig) != 1 {
			t.Fatalf("seed messages from %s = %d, want 1", from, len(orig))
		}

		stdout.Reset()
		stderr.Reset()
		if code := cmdMailReplyFromJSON([]string{orig[0].ID}, "human", "", "a reply to "+from, false, false, &stdout, &stderr); code == 0 {
			t.Fatalf("reply to %s = 0, want a refusal; stdout=%s", from, stdout.String())
		}
		if !strings.Contains(stderr.String(), "no mailbox") {
			t.Fatalf("reply to %s: stderr = %q, want the no-mailbox refusal", from, stderr.String())
		}
		if n := len(messageBeadsByBody(t, cityPath, "a reply to "+from)); n != 0 {
			t.Fatalf("reply to %s: messages = %d, want none written", from, n)
		}
	}
}
