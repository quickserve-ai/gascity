package mail

import (
	"strings"
	"testing"
)

func TestIsOrderSender(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"order:city/deacon-watch", true},
		{"  order:qcore/patrol ", true},
		{"peer-city/order:city/deacon-watch", true},
		{"human", false},
		{"mayor", false},
		{"qcore/worker", false},
		{"template:qcore/order:x", false},
		{"a/b/order:city/x", false},
		{"", false},
	} {
		addr, want := tc.addr, tc.want
		if got := IsOrderSender(addr); got != want {
			t.Errorf("IsOrderSender(%q) = %v, want %v", addr, got, want)
		}
	}
}

// The refusal names where to write instead, so an operator answering an
// order's alert is not left at a dead end.
func TestOrderNoMailboxErrorNamesWhereToWrite(t *testing.T) {
	bare := OrderNoMailboxError("order:city/deacon-watch").Error()
	if !strings.Contains(bare, "an order, which has no mailbox") || !strings.Contains(bare, "mayor") {
		t.Errorf("bare refusal = %q, want the no-mailbox text and the mayor named", bare)
	}
	qualified := OrderNoMailboxError("gastown/order:city/deacon-watch").Error()
	if !strings.Contains(qualified, "gastown/mayor") {
		t.Errorf("city-qualified refusal = %q, want gastown/mayor named", qualified)
	}
}
