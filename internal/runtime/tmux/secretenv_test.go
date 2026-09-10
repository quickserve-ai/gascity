package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestIsSecretEnvKey(t *testing.T) {
	secret := []string{
		// The four ga-fhbnmz production exposures.
		"AZURE_OPENAI_API_KEY",
		"GOOGLE_OAUTH_CLIENT_SECRET",
		"BEADS_HOLDER_TOKEN",
		"GC_INSTANCE_TOKEN",
		// Other live credential shapes on this fleet.
		"CLAUDE_CODE_OAUTH_TOKEN",
		"DOLT_REMOTE_PASSWORD",
		"BEADS_DOLT_PASSWORD",
		"AWS_SECRET_ACCESS_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"GITHUB_TOKEN",
		"GH_TOKEN",
		"GC_WORKER_INFERENCE_CODEX_AUTH_JSON",
		"GOOGLE_OAUTH_CLIENT_ID", // OAUTH marker — over-match, deliberately fine
	}
	for _, key := range secret {
		if !IsSecretEnvKey(key) {
			t.Errorf("IsSecretEnvKey(%q) = false, want true", key)
		}
	}
	plain := []string{
		"PATH", "HOME", "USER", "LANG", "LC_ALL", "TZ",
		"GC_RIG", "GC_RIG_ROOT", "GC_PROVIDER", "GC_SESSION_NAME",
		"GC_ALIAS", "GT_PROCESS_NAMES", "GC_CITY_RUNTIME_DIR",
		"BEADS_ACTOR", "CLAUDE_CONFIG_DIR", "TMUX_TMPDIR",
	}
	for _, key := range plain {
		if IsSecretEnvKey(key) {
			t.Errorf("IsSecretEnvKey(%q) = true, want false", key)
		}
	}
}

func TestScrubSecretEnviron(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"AZURE_OPENAI_API_KEY=sk-live-value",
		"HOME=/Users/x",
		"BEADS_HOLDER_TOKEN=abc123",
		"TMUX_TMPDIR=/tmp/sock-root",
		"malformed-no-equals",
	}
	got := scrubSecretEnviron(in)
	want := []string{"PATH=/usr/bin", "HOME=/Users/x", "TMUX_TMPDIR=/tmp/sock-root", "malformed-no-equals"}
	if len(got) != len(want) {
		t.Fatalf("scrubSecretEnviron = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scrubSecretEnviron[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// fakeEnvExecutor records executeCtxEnv calls alongside the plain ones, so a
// test can assert both that start-server ran first and what environment it
// was given.
type fakeEnvExecutor struct {
	fakeExecutor
	envCalls [][]string // env slice per executeCtxEnv call
}

func (f *fakeEnvExecutor) executeCtxEnv(_ context.Context, args []string, env []string) (string, error) {
	envCp := make([]string, len(env))
	copy(envCp, env)
	f.envCalls = append(f.envCalls, envCp)
	return f.executeCtx(context.Background(), args)
}

// callHasFlagBeforeCommand reports whether flag appears before the
// "new-session" command word of one recorded invocation. A -N after the
// command word would be parsed as a command argument by tmux, not as the
// no-server-start client flag.
func callHasFlagBeforeCommand(call []string, flag string) bool {
	for _, a := range call {
		if a == flag {
			return true
		}
		if a == "new-session" {
			return false
		}
	}
	return false
}

func callIndex(calls [][]string, substr string) int {
	for i, call := range calls {
		for _, a := range call {
			if strings.Contains(a, substr) {
				return i
			}
		}
	}
	return -1
}

func TestColdStartForksServerViaAnchorBeforeEnvArgs(t *testing.T) {
	t.Setenv("GA_FHBNMZ_FAKE_SECRET_TOKEN", "leak-me-not")

	cfg := DefaultConfig()
	cfg.SocketName = "gctest-inert"
	tm := NewTmuxWithConfig(cfg)
	tm.serverSocketObserver = func(context.Context, string) error { return nil }
	fake := &fakeEnvExecutor{}
	// Cold socket: the preflight probe and the inert-fork aliveness check
	// both see no server; everything after succeeds.
	fake.errs = []error{ErrNoServer, ErrNoServer}
	tm.exec = fake

	env := map[string]string{"GC_PROVIDER": "claude", "AZURE_OPENAI_API_KEY": "sk-fake"}
	if err := tm.NewSessionWithCommandAndEnv("gctest-inert-sess", "/work", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}

	anchorIdx := callIndex(fake.calls, "gc-srv-anchor-")
	envIdx := callIndex(fake.calls, "AZURE_OPENAI_API_KEY")
	killIdx := callIndex(fake.calls, "kill-session")
	if anchorIdx == -1 {
		t.Fatalf("no anchor new-session issued on a cold socket; calls: %q", fake.calls)
	}
	if envIdx != -1 && !callHasFlagBeforeCommand(fake.calls[envIdx], "-N") {
		t.Fatalf("the -e new-session must carry the no-server-start flag -N; call: %q", fake.calls[envIdx])
	}
	if envIdx == -1 || anchorIdx > envIdx {
		t.Fatalf("anchor (call %d) must precede the -e new-session (call %d); calls: %q", anchorIdx, envIdx, fake.calls)
	}
	if killIdx == -1 || killIdx < envIdx {
		t.Fatalf("anchor must be killed after the real session exists (kill %d, env %d); calls: %q", killIdx, envIdx, fake.calls)
	}
	for _, a := range fake.calls[anchorIdx] {
		if a == "-e" || strings.Contains(a, "AZURE_OPENAI_API_KEY") {
			t.Fatalf("the server-forking anchor call carries session env: %q", fake.calls[anchorIdx])
		}
	}

	// The anchor — the call that forks the daemon — must receive a
	// secret-scrubbed process environment.
	if len(fake.envCalls) != 1 {
		t.Fatalf("executeCtxEnv calls = %d, want 1", len(fake.envCalls))
	}
	var sawPath bool
	for _, kv := range fake.envCalls[0] {
		if strings.HasPrefix(kv, "GA_FHBNMZ_FAKE_SECRET_TOKEN=") {
			t.Fatalf("scrubbed env still carries the fake secret: %q", kv)
		}
		if strings.HasPrefix(kv, "PATH=") {
			sawPath = true
		}
	}
	if !sawPath {
		t.Fatal("scrubbed env lost PATH — scrub must remove only secret-classified vars")
	}
}

func TestWarmServerSkipsAnchor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SocketName = "gctest-warm"
	tm := NewTmuxWithConfig(cfg)
	fake := &fakeEnvExecutor{}
	// Default fake behavior: every call succeeds — the aliveness check
	// reads as a live server, so no anchor churn.
	tm.exec = fake

	if err := tm.NewSessionWithCommandAndEnv("gctest-warm-sess", "", "claude", map[string]string{"A": "b"}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if idx := callIndex(fake.calls, "gc-srv-anchor-"); idx != -1 {
		t.Fatalf("anchor issued against a live server: %q", fake.calls[idx])
	}
	if len(fake.envCalls) != 0 {
		t.Fatalf("executeCtxEnv calls = %d, want 0", len(fake.envCalls))
	}
}

func TestEmptyServerSkipsAnchor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SocketName = "gctest-empty"
	tm := NewTmuxWithConfig(cfg)
	fake := &fakeEnvExecutor{}
	// A live server holding zero sessions replies ErrNoCurrentTarget — which
	// WRAPS ErrNoServer. It must be read as alive, not cold.
	fake.errs = []error{ErrNoCurrentTarget, ErrNoCurrentTarget}
	tm.exec = fake

	if err := tm.NewSessionWithCommandAndEnv("gctest-empty-sess", "", "claude", map[string]string{"A": "b"}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if idx := callIndex(fake.calls, "gc-srv-anchor-"); idx != -1 {
		t.Fatalf("anchor issued against a live-but-empty server: %q", fake.calls[idx])
	}
}

func TestAnchorFailureFailsClosedNotOpenFork(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SocketName = "gctest-anchorfail"
	tm := NewTmuxWithConfig(cfg)
	tm.serverSocketObserver = func(context.Context, string) error { return nil }
	fake := &fakeEnvExecutor{}
	// Cold socket; the anchor itself fails; the -N'd new-session then finds
	// no server. The spawn must FAIL (retriable ErrNoServer), never succeed
	// by forking a daemon that would carry the -e argv.
	fake.errs = []error{ErrNoServer, ErrNoServer, ErrNoServer, ErrNoServer}
	tm.exec = fake

	err := tm.NewSessionWithCommandAndEnv("gctest-anchorfail-sess", "", "claude", map[string]string{"A": "b"})
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("NewSessionWithCommandAndEnv = %v, want ErrNoServer (fail closed)", err)
	}
	envIdx := callIndex(fake.calls, "A=b")
	if envIdx == -1 {
		t.Fatalf("no -e new-session attempted; calls: %q", fake.calls)
	}
	if !callHasFlagBeforeCommand(fake.calls[envIdx], "-N") {
		t.Fatalf("the -e new-session must carry -N even when the anchor failed; call: %q", fake.calls[envIdx])
	}
}

func TestNewSessionSkipsInertForkWithoutSocketName(t *testing.T) {
	tm := NewTmux() // no SocketName: the user's default server — never touch it
	fake := &fakeEnvExecutor{}
	tm.exec = fake
	if err := tm.NewSessionWithCommandAndEnv("gctest-nosock", "", "claude", map[string]string{"A": "b"}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if idx := callIndex(fake.calls, "gc-srv-anchor-"); idx != -1 {
		t.Fatalf("anchor issued without a socket name: %q", fake.calls[idx])
	}
	if len(fake.envCalls) != 0 {
		t.Fatalf("executeCtxEnv calls = %d, want 0", len(fake.envCalls))
	}
}
