package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Review round 5 (ga-yuiof4 item 3): the git calls the reaper runs inside the
// session-start fence are bounded and run no hooks, a pass whose ctx is done
// runs none, and a panicked pass reports no counts.

// TestSessionStartFence_RunIfQuietRefusesADoneCtx: once the pass ctx is done
// (controller shutdown), runIfQuiet runs nothing, even on a quiet fence.
func TestSessionStartFence_RunIfQuietRefusesADoneCtx(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name  string
		fence *sessionStartFence
	}{
		{"fence", &sessionStartFence{}},
		{"nil fence", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gen := tc.fence.generation()
			ran := false
			if tc.fence.runIfQuiet(done, gen, func() { ran = true }) || ran {
				t.Fatalf("runIfQuiet with a done ctx ran fn (ran=%t), want it refused", ran)
			}
			// Control: the same quiet fence with a live ctx runs fn.
			if !tc.fence.runIfQuiet(context.Background(), gen, func() { ran = true }) || !ran {
				t.Fatalf("control: runIfQuiet on a quiet fence with a live ctx did not run fn")
			}
		})
	}
}

// TestWorktreeReaperLane_PanickedPassReportsNoCounts: a panicked pass returned
// no result, so the next trigger's trace must not carry last_pass_reaped /
// last_pass_protected (or timings) as though a zero were observed.
func TestWorktreeReaperLane_PanickedPassReportsNoCounts(t *testing.T) {
	prev := runWorktreeReaperPassFn
	runWorktreeReaperPassFn = func(worktreeReaperPassInput) worktreeReaperPassResult { panic("boom") }
	t.Cleanup(func() { runWorktreeReaperPassFn = prev })
	cr := newReapTickRuntime(t.TempDir(), &config.City{}, beads.NewMemStore(), io.Discard)

	cr.triggerWorktreeReaperPass(context.Background(), cr.cfg, true, nil)
	waitWorktreeReaperIdle(t, cr)
	next := cr.triggerWorktreeReaperPass(context.Background(), cr.cfg, true, nil)
	waitWorktreeReaperIdle(t, cr)

	f := next.fields()
	if f["last_pass_panicked"] != true {
		t.Fatalf("fields = %v, want last_pass_panicked", f)
	}
	for _, k := range []string{"last_pass_reaped", "last_pass_protected", "last_pass_reap_ms", "last_pass_agent_homes_reset"} {
		if v, ok := f[k]; ok {
			t.Errorf("fields[%q] = %v after a panicked pass, want it absent (unknown, not observed)", k, v)
		}
	}
}

// deadlineRecordingProbe records the ctx the agent-home reset's detach got.
type deadlineRecordingProbe struct {
	fakeAgentWorktreeGit
	deadline    time.Time
	hasDeadline bool
}

func (p *deadlineRecordingProbe) CheckoutDetachNoHooksCtx(ctx context.Context, ref string) error {
	p.deadline, p.hasDeadline = ctx.Deadline()
	return p.fakeAgentWorktreeGit.CheckoutDetachNoHooksCtx(ctx, ref)
}

// TestCleanupAgentHomeDetachIsBounded: the reset's detach holds the
// session-start fence, so it runs under a reaperGitTimeout deadline.
func TestCleanupAgentHomeDetachIsBounded(t *testing.T) {
	cityPath, builderWTPath, _ := setupAgentHomeWorktreeCleanupTest(t)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)
	if err := os.WriteFile(filepath.Join(builderWTPath, worktreeStaleFileName), []byte("branch=builder/ga-abc123\n"), 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}
	probe := &deadlineRecordingProbe{fakeAgentWorktreeGit: fakeAgentWorktreeGit{isRepo: true, currentBranch: "builder/ga-abc123"}}
	orig := newAgentWorktreeGitProbe
	newAgentWorktreeGitProbe = func(string) agentWorktreeGitProbe { return probe }
	t.Cleanup(func() { newAgentWorktreeGitProbe = orig })

	before := time.Now()
	cleaned := cleanupClosedBeadAgentHomeWorktreesGuarded(cityPath, agentHomeConfig(), map[string]beads.Store{"ga-rig": store}, io.Discard,
		&agentHomeResetGuard{ctx: context.Background(), stillEnabled: func() bool { return true }, startFence: &sessionStartFence{}})
	if cleaned != 1 || probe.checkoutDetachRef == "" {
		t.Fatalf("cleaned=%d detach=%q, want the home reset", cleaned, probe.checkoutDetachRef)
	}
	if !probe.hasDeadline {
		t.Fatal("detach ran with no deadline, want reaperGitTimeout")
	}
	if d := probe.deadline.Sub(before); d <= 0 || d > reaperGitTimeout+time.Minute {
		t.Fatalf("detach deadline %s after the call, want about reaperGitTimeout (%s)", d, reaperGitTimeout)
	}
}

// TestCleanupAgentHomeDetachDisablesHooks drives the reset through the real
// git.Git probe against a fake `git` that records its argv: the detach must
// carry -c core.hooksPath=/dev/null ahead of the subcommand, so the rig's
// post-checkout hook (`bd hooks run`) cannot block inside the fence.
func TestCleanupAgentHomeDetachDisablesHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git is a POSIX shell script")
	}
	argvLog := filepath.Join(t.TempDir(), "argv")
	bin := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + argvLog + "'\n" +
		"case \"$*\" in\n" +
		"  'rev-parse --abbrev-ref HEAD') echo builder/ga-abc123 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cityPath, builderWTPath, _ := setupAgentHomeWorktreeCleanupTest(t)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-abc123", Status: "closed"}}, nil)
	if err := os.WriteFile(filepath.Join(builderWTPath, worktreeStaleFileName), []byte("branch=builder/ga-abc123\n"), 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	cleaned := cleanupClosedBeadAgentHomeWorktreesGuarded(cityPath, agentHomeConfig(), map[string]beads.Store{"ga-rig": store}, io.Discard,
		&agentHomeResetGuard{ctx: context.Background(), stillEnabled: func() bool { return true }, startFence: &sessionStartFence{}})

	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	var checkouts []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.Contains(line, "checkout") {
			checkouts = append(checkouts, line)
		}
	}
	const want = "-c core.hooksPath=/dev/null checkout --detach origin/main"
	if cleaned != 1 || len(checkouts) != 1 || checkouts[0] != want {
		t.Fatalf("cleaned=%d checkout calls=%q, want exactly %q (all git calls: %q)", cleaned, checkouts, want, string(raw))
	}
}
