package orders

import "testing"

// A condition check evaluated from a seat, or from inside another order's
// run, must not inherit that seat's or that run's identity (ga-s04g7q). The
// check runs as a real subprocess here, so the assertion is on the child's
// environment, not on the overlay.
func TestConditionCheckDropsInheritedIdentity(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "gc-seat")
	t.Setenv("GC_ALIAS", "seat-alias")
	t.Setenv("GC_AGENT", "seat-agent")
	t.Setenv(ExecOrderRunEnv, "parent-run")
	t.Setenv(ExecOrderNameEnv, "parent-order")
	t.Setenv(ExecOrderScopeEnv, "parent-scope")
	a := Order{Name: "cond", Trigger: "condition", Check: `test -z "${GC_SESSION_ID+x}${GC_ALIAS+x}${GC_AGENT+x}${GC_ORDER_RUN+x}" && test "$GC_ORDER_SCOPE/$GC_ORDER_NAME" = "city/cond"`}
	result := checkCondition(a, TriggerOptions{ConditionEnv: []string{ExecOrderScopeEnv + "=city", ExecOrderNameEnv + "=cond"}})
	if !result.Due {
		t.Fatalf("check saw inherited identity or lost the order's own; reason: %s", result.Reason)
	}
}

// Control: the same check fails when the identity is in the overlay, so the
// test above is not passing on a check that cannot fail.
func TestConditionCheckSeesOverlaidIdentity(t *testing.T) {
	a := Order{Name: "cond", Trigger: "condition", Check: `test -z "${GC_SESSION_ID+x}"`}
	result := checkCondition(a, TriggerOptions{ConditionEnv: []string{"GC_SESSION_ID=gc-overlay"}})
	if result.Due {
		t.Fatal("check passed with GC_SESSION_ID in the overlay; the probe cannot fail")
	}
}
