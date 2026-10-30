package mail

import (
	"strings"
	"testing"
)

func TestIsServiceSender(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"service:westeros-pressure", true},
		{"  service:westeros-verdicts ", true},
		{"service:a.b_c-1", true},
		{"service:", false},
		{"service:has space", false},
		{"service:a/b", false},
		{"service:order:city/x", false},
		{"peer/service:x", false},
		{"order:city/x", false},
		{"human", false},
		{"", false},
	} {
		if got := IsServiceSender(tc.addr); got != tc.want {
			t.Errorf("IsServiceSender(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestServiceNoMailboxErrorNamesTheAddress(t *testing.T) {
	got := ServiceNoMailboxError(" service:westeros-pressure ").Error()
	if !strings.HasPrefix(got, "service:westeros-pressure is a service sender, which has no mailbox") {
		t.Errorf("refusal = %q", got)
	}
}
