package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

// messageBeadsByBody returns the open message beads whose body contains needle.
func messageBeadsByBody(t *testing.T, cityPath, needle string) []beads.Bead {
	t.Helper()
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all, err := store.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List messages: %v", err)
	}
	var out []beads.Bead
	for _, b := range all {
		if strings.Contains(b.Description, needle) {
			out = append(out, b)
		}
	}
	return out
}

func assertUnverifiedMark(t *testing.T, b beads.Bead, from string) {
	t.Helper()
	if got := b.Metadata[mail.FromUnverifiedMetadataKey]; got != "true" {
		t.Fatalf("%s = %q, want true", mail.FromUnverifiedMetadataKey, got)
	}
	if !strings.HasPrefix(b.Description, mail.UnverifiedSenderBanner(from)+"\n\n") {
		t.Fatalf("body = %q, want it to open with the unverified-sender banner", b.Description)
	}
}

func assertNoUnverifiedMark(t *testing.T, b beads.Bead) {
	t.Helper()
	if got, ok := b.Metadata[mail.FromUnverifiedMetadataKey]; ok {
		t.Fatalf("%s = %q, want absent", mail.FromUnverifiedMetadataKey, got)
	}
	if strings.Contains(b.Description, "[unverified sender:") {
		t.Fatalf("body = %q, want no banner", b.Description)
	}
}

// ga-7lmivm.1: a process with no session identity (a terminal, a systemd unit)
// may still sign as an order, but the claim is marked, never refused.
func TestCmdMailSend_IdentityLessOrderFromIsMarked(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{seat.ID, "pressure WARN"}, false, false, "order:westeros-pressure/check", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("cmdMailSend() = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	got := messageBeadsByBody(t, cityPath, "pressure WARN")
	if len(got) != 1 {
		t.Fatalf("messages = %d, want 1", len(got))
	}
	if got[0].From != "order:westeros-pressure/check" {
		t.Fatalf("From = %q, want the claimed order", got[0].From)
	}
	assertUnverifiedMark(t, got[0], "order:westeros-pressure/check")
}

// The order's own run is verified: neither its implicit sender nor an explicit
// --from naming itself is marked.
func TestCmdMailSend_OwnOrderRunIsNotMarked(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)
	setOrderEnv(t, "city", "deacon-watch", "pc-run1")

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{seat.ID, "implicit"}, false, false, "", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("implicit send = %d; stderr=%s", code, stderr.String())
	}
	if code := cmdMailSend([]string{seat.ID, "explicit"}, false, false, "order:city/deacon-watch", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("explicit own-order send = %d; stderr=%s", code, stderr.String())
	}
	for _, needle := range []string{"implicit", "explicit"} {
		got := messageBeadsByBody(t, cityPath, needle)
		if len(got) != 1 {
			t.Fatalf("%s messages = %d, want 1", needle, len(got))
		}
		assertNoUnverifiedMark(t, got[0])
	}
}

// Another order's name from inside an order run is not that run: marked.
func TestCmdMailSend_OtherOrderFromInsideAnOrderIsMarked(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)
	setOrderEnv(t, "city", "deacon-watch", "pc-run1")

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{seat.ID, "borrowed"}, false, false, "order:city/other", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("cmdMailSend() = %d; stderr=%s", code, stderr.String())
	}
	got := messageBeadsByBody(t, cityPath, "borrowed")
	if len(got) != 1 {
		t.Fatalf("messages = %d, want 1", len(got))
	}
	assertUnverifiedMark(t, got[0], "order:city/other")
}

// A live seat claiming an order is still refused on send, as before.
func TestCmdMailSend_SeatFromOrderStillRefused(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, seat := orderSenderTestCity(t)
	t.Setenv("GC_AGENT", "worker")

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{seat.ID, "forged"}, false, false, "order:city/deacon-watch", "", "", "", &stdout, &stderr); code == 0 {
		t.Fatalf("seat --from order: = 0, want a refusal; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "does not match this session's own identity") {
		t.Fatalf("stderr = %q, want the identity refusal", stderr.String())
	}
	if n := len(messageBeadsByBody(t, cityPath, "forged")); n != 0 {
		t.Fatalf("messages = %d, want none written", n)
	}
}

// Reply does not authorize an explicit --from, so a reply claiming an order is
// marked from any caller, a live seat included.
func TestCmdMailReply_OrderFromIsMarked(t *testing.T) {
	clearMailIdentityEnv(t)
	cityPath, _ := orderSenderTestCity(t)
	t.Setenv("GC_AGENT", "worker")
	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{"human", "original"}, false, false, "", "", "", "", &stdout, &stderr); code != 0 {
		t.Fatalf("seed send = %d; stderr=%s", code, stderr.String())
	}
	orig := messageBeadsByBody(t, cityPath, "original")
	if len(orig) != 1 {
		t.Fatalf("seed messages = %d, want 1", len(orig))
	}

	if code := cmdMailReplyFromJSON([]string{orig[0].ID}, "order:city/deacon-watch", "", "seat reply", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("seat reply --from order: = %d; stderr=%s", code, stderr.String())
	}
	clearMailIdentityEnv(t)
	if code := cmdMailReplyFromJSON([]string{orig[0].ID}, "order:city/deacon-watch", "", "terminal reply", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("identity-less reply --from order: = %d; stderr=%s", code, stderr.String())
	}
	for _, needle := range []string{"seat reply", "terminal reply"} {
		got := messageBeadsByBody(t, cityPath, needle)
		if len(got) != 1 {
			t.Fatalf("%s messages = %d, want 1", needle, len(got))
		}
		assertUnverifiedMark(t, got[0], "order:city/deacon-watch")
	}
}

// The unverified mark rides only on an order: sender this process is not.
func TestUnverifiedSenderMetadata(t *testing.T) {
	clearMailIdentityEnv(t)
	setOrderEnv(t, "city", "deacon-watch", "")
	for _, tc := range []struct {
		sender string
		want   bool
	}{
		{"order:city/deacon-watch", false},
		{"order:city/other", true},
		{"human", false},
		{"worker", false},
		{"controller", false},
	} {
		if got := unverifiedSenderMetadata(tc.sender) != nil; got != tc.want {
			t.Errorf("unverifiedSenderMetadata(%q) marked = %v, want %v", tc.sender, got, tc.want)
		}
	}
}
