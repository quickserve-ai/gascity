package herdr

// Regression guard for ga-fcdvn.
//
// `herdr agent start --timeout` was a hardcoded 60000ms while the caller's ctx
// deadline — [session] startup_timeout — ALSO defaulted to 60s. Two equal
// nested deadlines can never be ordered: whichever fired first killed a
// healthy, still-booting agent, the reconciler restarted it, and the next
// attempt died identically. Observed 2026-08-05: 37 kill/restart cycles in ~35
// minutes on an agent that reached its idle prompt in ~2s (stacked MCP-server
// connects burned the budget), and the qcore merge-queue consumer was
// quarantined by the same loop. The herdr bound is now DERIVED from the ctx
// deadline, minus a margin so herdr reports its own structured timeout instead
// of dying to a SIGKILL, so the two bounds cannot invert.

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// timeoutArgMS extracts the value following --timeout from an assembled
// `agent start` argument list.
func timeoutArgMS(t *testing.T, cli []string) int {
	t.Helper()
	for i, tok := range cli {
		if tok == "--timeout" && i+1 < len(cli) {
			ms, err := strconv.Atoi(cli[i+1])
			if err != nil {
				t.Fatalf("--timeout value %q is not an integer", cli[i+1])
			}
			return ms
		}
	}
	t.Fatalf("no --timeout flag in agent start args: %v", cli)
	return 0
}

// The args the CLI actually receives must carry the ctx-derived timeout — this
// guards the CALL SITE, so quietly pinning startAgentKind back to a constant
// fails here even while the helper function stays correct. (The first version
// of this guard tested only the helper and passed against a re-pinned call
// site.)
func TestAgentStartArgsCarryContextDerivedTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli := agentStartArgs(ctx, "gc_woodhouse", "claude", "%42", []string{"--flag"})

	got := timeoutArgMS(t, cli)
	wantMax := 10*60*1000 - agentStartDeadlineMarginMS
	if got < wantMax-2000 || got > wantMax {
		t.Fatalf("agent start --timeout = %dms under a 10m ctx, want ~%dms — the call site is not using the ctx-derived bound", got, wantMax)
	}
	// The trailing agent args must still arrive after the -- separator.
	if cli[len(cli)-2] != "--" || cli[len(cli)-1] != "--flag" {
		t.Fatalf("agent args not passed through after --: %v", cli)
	}
}

// A ctx with a real deadline must yield herdr's remaining budget minus the
// margin — never the old fixed constant. Uses a 10-minute deadline: bigger
// than the old 60s constant, so a regression to the pinned value is caught as
// an inner bound SMALLER than the outer one (the inversion this guards).
func TestAgentStartTimeoutDerivedFromContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	got := agentStartTimeoutFor(ctx)

	// Allow scheduling slop between WithTimeout and the derivation.
	wantMax := 10*60*1000 - agentStartDeadlineMarginMS
	wantMin := wantMax - 2000
	if got < wantMin || got > wantMax {
		t.Fatalf("agentStartTimeoutFor(10m ctx) = %dms, want ~%dms — the herdr bound is not tracking the outer deadline", got, wantMax)
	}
}

// No deadline (city-less/standalone construction) falls back to the generous
// default, and that default must stay well above the old 60s constant: "start
// took a bit long" must not be treated as "start failed".
func TestAgentStartTimeoutDefaultWithoutDeadlineIsGenerous(t *testing.T) {
	got := agentStartTimeoutFor(context.Background())
	if got != agentStartTimeoutDefaultMS {
		t.Fatalf("agentStartTimeoutFor(no deadline) = %dms, want the default %dms", got, agentStartTimeoutDefaultMS)
	}
	if got <= 60000 {
		t.Fatalf("no-deadline default = %dms, must exceed the old 60000ms constant that killed healthy agents (ga-fcdvn)", got)
	}
}

// A nearly-expired ctx still produces a value herdr accepts (>3000): the start
// is doomed either way, but the request must stay well-formed rather than
// being rejected client-side.
func TestAgentStartTimeoutFloorsNearExpiredDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	got := agentStartTimeoutFor(ctx)
	if got != agentStartTimeoutMinMS {
		t.Fatalf("agentStartTimeoutFor(nearly-expired ctx) = %dms, want the floor %dms", got, agentStartTimeoutMinMS)
	}
	if got <= 3000 {
		t.Fatalf("floor %dms violates herdr's --timeout > 3000 requirement", got)
	}
}

// The margin must leave herdr room to fail FIRST: for any ctx comfortably
// above the floor, inner < outer strictly, so the CLI returns herdr's own
// structured timeout error instead of being SIGKILLed by the outer ctx.
func TestAgentStartTimeoutInnerStrictlyInsideOuter(t *testing.T) {
	for _, outer := range []time.Duration{30 * time.Second, 60 * time.Second, 300 * time.Second} {
		ctx, cancel := context.WithTimeout(context.Background(), outer)
		inner := agentStartTimeoutFor(ctx)
		cancel()
		if time.Duration(inner)*time.Millisecond >= outer {
			t.Fatalf("outer=%v: inner bound %dms >= outer — nested-deadline inversion, herdr can never report its own timeout", outer, inner)
		}
	}
}
