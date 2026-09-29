package mail

import "testing"

func TestIsOrderSender(t *testing.T) {
	for addr, want := range map[string]bool{
		"order:city/deacon-watch":           true,
		"  order:qcore/patrol ":             true,
		"peer-city/order:city/deacon-watch": true,
		"human":                             false,
		"mayor":                             false,
		"qcore/worker":                      false,
		"template:qcore/order:x":            false,
		"a/b/order:city/x":                  false,
		"":                                  false,
	} {
		if got := IsOrderSender(addr); got != want {
			t.Errorf("IsOrderSender(%q) = %v, want %v", addr, got, want)
		}
	}
}
