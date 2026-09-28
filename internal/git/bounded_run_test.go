package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// installFakeGit puts an executable `git` running script first on PATH.
func installFakeGit(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake git is a POSIX shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestBoundedGitCallsReturnWhenAGrandchildHoldsThePipe (ga-yuiof4 item 3,
// round 5): the reaper runs `git worktree remove` and `git checkout --detach`
// under the session-start fence, so its deadline must actually return the
// call. Here git leaves a grandchild (a hook's `bd`, in the field) holding the
// output pipe; killing git at the deadline does not close the pipe, and
// without WaitDelay CombinedOutput waits for the grandchild.
func TestBoundedGitCallsReturnWhenAGrandchildHoldsThePipe(t *testing.T) {
	installFakeGit(t, "sleep 30 &\nsleep 30\n")
	const deadline = 200 * time.Millisecond
	bound := deadline + boundedWaitDelay + 3*time.Second

	for _, tc := range []struct {
		name string
		call func(ctx context.Context, g *Git) error
	}{
		{"WorktreeRemoveCtx", func(ctx context.Context, g *Git) error { return g.WorktreeRemoveCtx(ctx, "/nowhere", false) }},
		{"CheckoutDetachCtx", func(ctx context.Context, g *Git) error { return g.CheckoutDetachCtx(ctx, "origin/main") }},
		{"CheckoutDetachNoHooksCtx", func(ctx context.Context, g *Git) error { return g.CheckoutDetachNoHooksCtx(ctx, "origin/main") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			g := New(t.TempDir())
			errc := make(chan error, 1)
			go func() { errc <- tc.call(ctx, g) }()
			select {
			case err := <-errc:
				if err == nil {
					t.Fatal("call past its deadline returned nil, want an error (a failed removal/reset)")
				}
			case <-time.After(bound):
				t.Fatalf("call did not return within %s of a %s deadline: a grandchild holding the output pipe outlives the kill", bound, deadline)
			}
		})
	}
}

// TestCheckoutDetachNoHooksCtxDisablesHooks: the reaper's detach passes
// core.hooksPath=/dev/null ahead of the subcommand; plain CheckoutDetach, used
// by every other caller, does not.
func TestCheckoutDetachNoHooksCtxDisablesHooks(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "argv")
	installFakeGit(t, "printf '%s\\n' \"$@\" >> '"+argvLog+"'\necho '==' >> '"+argvLog+"'\n")

	g := New(t.TempDir())
	if err := g.CheckoutDetachNoHooksCtx(context.Background(), "origin/main"); err != nil {
		t.Fatalf("CheckoutDetachNoHooksCtx: %v", err)
	}
	if err := g.CheckoutDetach("origin/main"); err != nil {
		t.Fatalf("CheckoutDetach: %v", err)
	}
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	calls := strings.Split(strings.TrimSuffix(string(raw), "==\n"), "==\n")
	want := []string{
		"-c\ncore.hooksPath=/dev/null\ncheckout\n--detach\norigin/main\n",
		"checkout\n--detach\norigin/main\n",
	}
	if len(calls) != len(want) {
		t.Fatalf("fake git saw %d call(s) %q, want %d", len(calls), calls, len(want))
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d argv = %q, want %q", i, calls[i], want[i])
		}
	}
}

// TestCheckoutDetachNoHooksCtxSkipsARealHook runs real git with a
// post-checkout hook installed: the hook runs under CheckoutDetach (the
// control) and does not under CheckoutDetachNoHooksCtx.
func TestCheckoutDetachNoHooksCtxSkipsARealHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook is a POSIX shell script")
	}
	dir := initTestRepo(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(dir, ".git", "hooks", "post-checkout")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	// Pin the hooks dir in repo config so a global core.hooksPath on the host
	// cannot hide the hook from the control.
	runGit(t, dir, "config", "core.hooksPath", filepath.Dir(hook))
	g := New(dir)

	if err := g.CheckoutDetachNoHooksCtx(context.Background(), "HEAD"); err != nil {
		t.Fatalf("CheckoutDetachNoHooksCtx: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("post-checkout hook ran under CheckoutDetachNoHooksCtx (stat err=%v)", err)
	}
	if err := g.CheckoutDetach("HEAD"); err != nil {
		t.Fatalf("CheckoutDetach: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: post-checkout hook did not run under CheckoutDetach, so this test cannot see a hook (stat err=%v)", err)
	}
}
