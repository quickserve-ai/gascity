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

// A service has no mailbox: mail to one is refused before anything is written.
func TestCmdMailSend_ToServiceIsRefused(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, _ := orderSenderTestCity(t)

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{"service:westeros-pressure", "to a service"}, false, false, "", "", "", "", &stdout, &stderr); code == 0 {
		t.Fatalf("send to service: = 0, want a refusal; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "service sender, which has no mailbox") {
		t.Fatalf("stderr = %q, want the no-mailbox refusal", stderr.String())
	}
	if n := len(messageBeadsByBody(t, cityPath, "to a service")); n != 0 {
		t.Fatalf("messages = %d, want none written", n)
	}
}

// Nor can a service's mail be replied to: the reply would go nowhere.
func TestCmdMailReply_ToServiceIsRefused(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)
	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{seat.ID, "page to answer"}, false, false, "service:westeros-verdicts", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("seed send = %d; stderr=%s", code, stderr.String())
	}
	orig := messageBeadsByBody(t, cityPath, "page to answer")
	if len(orig) != 1 {
		t.Fatalf("seed messages = %d, want 1", len(orig))
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdMailReplyFromJSON([]string{orig[0].ID}, "", "", "a reply", false, false, &stdout, &stderr); code == 0 {
		t.Fatalf("reply to a service = 0, want a refusal; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "no mailbox") {
		t.Fatalf("stderr = %q, want the no-mailbox refusal", stderr.String())
	}
	if n := len(messageBeadsByBody(t, cityPath, "a reply")); n != 0 {
		t.Fatalf("messages = %d, want none written", n)
	}
}
