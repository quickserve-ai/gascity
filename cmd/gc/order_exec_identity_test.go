package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// envValues returns every value env holds for key, in order, so a test can
// see a key that is present twice as well as one that is absent.
func envValues(env []string, key string) []string {
	var out []string
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if ok && k == key {
			out = append(out, v)
		}
	}
	return out
}

func assertEnvOnce(t *testing.T, env []string, key, want string) {
	t.Helper()
	got := envValues(env, key)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("%s = %q, want exactly [%q]", key, got, want)
	}
}

func assertEnvAbsent(t *testing.T, env []string, key string) {
	t.Helper()
	if got := envValues(env, key); len(got) != 0 {
		t.Fatalf("%s = %q, want absent", key, got)
	}
}

// An exec order learns which order and which run it is, and a dispatch-time
// var cannot replace that identity (ga-s04g7q).
func TestOrderExecEnvForRunSetsOrderIdentity(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	cityDir := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "pc"}

	t.Run("city order with a run", func(t *testing.T) {
		a := orders.Order{Name: "deacon-watch", Trigger: "cooldown", Interval: "1m", Exec: "true"}
		env, err := orderExecEnvForRun(cityDir, nil, target, a, map[string]string{
			orders.ExecOrderNameEnv:  "spoofed",
			orders.ExecOrderScopeEnv: "spoofed",
			orders.ExecOrderRunEnv:   "spoofed",
		}, "pc-run1")
		if err != nil {
			t.Fatalf("orderExecEnvForRun: %v", err)
		}
		assertEnvOnce(t, env, orders.ExecOrderScopeEnv, "city")
		assertEnvOnce(t, env, orders.ExecOrderNameEnv, "deacon-watch")
		assertEnvOnce(t, env, orders.ExecOrderRunEnv, "pc-run1")
	})

	t.Run("rig order", func(t *testing.T) {
		a := orders.Order{Name: "patrol", Rig: "qcore", Trigger: "cooldown", Interval: "1m", Exec: "true"}
		env, err := orderExecEnvForRun(cityDir, nil, target, a, nil, "pc-run2")
		if err != nil {
			t.Fatalf("orderExecEnvForRun: %v", err)
		}
		assertEnvOnce(t, env, orders.ExecOrderScopeEnv, "qcore")
		assertEnvOnce(t, env, orders.ExecOrderNameEnv, "patrol")
	})

	t.Run("untracked run leaves GC_ORDER_RUN unset", func(t *testing.T) {
		a := orders.Order{Name: "deacon-watch", Trigger: "cooldown", Interval: "1m", Exec: "true"}
		env, err := orderExecEnvForRun(cityDir, nil, target, a, map[string]string{orders.ExecOrderRunEnv: "spoofed"}, "")
		if err != nil {
			t.Fatalf("orderExecEnvForRun: %v", err)
		}
		assertEnvAbsent(t, env, orders.ExecOrderRunEnv)
		assertEnvOnce(t, env, orders.ExecOrderNameEnv, "deacon-watch")
	})
}

// A seat's identity in the dispatcher's own environment, and a parent order's
// identity, never reach the order; an order may still set a session key itself.
func TestMergeOrderExecEnvDropsInheritedIdentity(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin:/bin",
		"GC_SESSION_ID=gc-leaked",
		"GC_ALIAS=leaked-alias",
		"GC_AGENT=leaked-agent",
		"GC_ORDER_NAME=parent-order",
		"GC_ORDER_SCOPE=parent-scope",
		"GC_ORDER_RUN=parent-run",
	}

	env := mergeOrderExecEnv(environ, []string{"GC_ORDER_NAME=child"})
	for _, key := range []string{"GC_SESSION_ID", "GC_ALIAS", "GC_AGENT", "GC_ORDER_SCOPE", "GC_ORDER_RUN"} {
		assertEnvAbsent(t, env, key)
	}
	assertEnvOnce(t, env, "GC_ORDER_NAME", "child")
	assertEnvOnce(t, env, "PATH", "/usr/bin:/bin")

	declared := mergeOrderExecEnv(environ, []string{"GC_AGENT=declared-by-order"})
	assertEnvOnce(t, declared, "GC_AGENT", "declared-by-order")
}

// End to end through the real shell runner: the child process sees the order
// identity and none of the dispatcher's session identity.
func TestShellExecRunnerChildSeesOrderIdentityNotSeatIdentity(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "gc-leaked")
	t.Setenv("GC_ALIAS", "leaked-alias")
	t.Setenv("GC_AGENT", "leaked-agent")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := shellExecRunner(ctx, "env", t.TempDir(), []string{
		"GC_ORDER_SCOPE=city",
		"GC_ORDER_NAME=deacon-watch",
		"GC_ORDER_RUN=pc-run1",
	})
	if err != nil {
		t.Fatalf("shellExecRunner: %v; output: %s", err, out)
	}
	child := strings.Split(strings.TrimSpace(string(out)), "\n")
	assertEnvOnce(t, child, "GC_ORDER_SCOPE", "city")
	assertEnvOnce(t, child, "GC_ORDER_NAME", "deacon-watch")
	assertEnvOnce(t, child, "GC_ORDER_RUN", "pc-run1")
	for _, key := range []string{"GC_SESSION_ID", "GC_ALIAS", "GC_AGENT"} {
		assertEnvAbsent(t, child, key)
	}
}

// The controller's dispatch hands the order its own tracking bead as GC_ORDER_RUN.
func TestOrderDispatchExecCarriesOrderIdentity(t *testing.T) {
	store := beads.NewMemStore()
	var gotEnv []string
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		gotEnv = env
		return nil, nil
	}

	aa := []orders.Order{{Name: "poll", Trigger: "cooldown", Interval: "1m", Exec: "true"}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)
	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())

	if gotEnv == nil {
		t.Fatal("exec did not run")
	}
	assertEnvOnce(t, gotEnv, orders.ExecOrderScopeEnv, "city")
	assertEnvOnce(t, gotEnv, orders.ExecOrderNameEnv, "poll")
	runs := envValues(gotEnv, orders.ExecOrderRunEnv)
	if len(runs) != 1 || runs[0] == "" {
		t.Fatalf("GC_ORDER_RUN = %q, want one tracking bead id", runs)
	}
	if _, err := store.Get(runs[0]); err != nil {
		t.Fatalf("GC_ORDER_RUN %q does not name a bead in the order's store: %v", runs[0], err)
	}
}

// Declaring an order identity key in [order.env] is refused at check time,
// like every other controller-owned key.
func TestOrderCheckRejectsDeclaredOrderIdentityKey(t *testing.T) {
	for _, key := range []string{orders.ExecOrderScopeEnv, orders.ExecOrderNameEnv, orders.ExecOrderRunEnv} {
		a := orders.Order{Name: "bad-env", Trigger: "cooldown", Interval: "24h", Exec: "true", Env: map[string]string{key: "x"}}
		err := orders.ValidateExecEnvOverrides(a)
		if err == nil || !strings.Contains(err.Error(), `controller-owned env key "`+key+`"`) {
			t.Fatalf("ValidateExecEnvOverrides(%s) = %v, want controller-owned key error", key, err)
		}
	}
}
