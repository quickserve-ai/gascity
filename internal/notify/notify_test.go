package notify

import "testing"

func TestWakeTextMailArrivalIsBytePreserved(t *testing.T) {
	n := Notification{Kind: KindMailArrival, Sender: "gastown.mayor", Ref: "bead://ga-wisp-x", Summary: "ignored on session transport"}
	// This literal is the pre-plane mail-notify text; seats, tests, and
	// operator eyes key on it. Do not change it without a fleet-wide sweep.
	if got := WakeText(n); got != "You have mail from gastown.mayor" {
		t.Fatalf("WakeText(mail_arrival) = %q", got)
	}
}

func TestWakeTextAttentionAndDefaultKinds(t *testing.T) {
	att := Notification{Kind: KindAttentionRequest, Sender: "woodhouse", Ref: "bead://ga-1"}
	if got := WakeText(att); got != "Attention requested by woodhouse: see bead://ga-1" {
		t.Fatalf("WakeText(attention) = %q", got)
	}
	sig := Notification{Kind: KindProtocolSignal, Sender: "refinery"}
	if got := WakeText(sig); got != "protocol_signal from refinery" {
		t.Fatalf("WakeText(signal) = %q", got)
	}
	unknown := Notification{Kind: Kind("future_kind"), Sender: "x", Ref: "https://example.com/pr/1"}
	if got := WakeText(unknown); got != "future_kind from x: see https://example.com/pr/1" {
		t.Fatalf("WakeText(unknown) = %q", got)
	}
}
