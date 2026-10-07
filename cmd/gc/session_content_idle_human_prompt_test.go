//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

// Lifecycle operations remain in memory; both pane observations use real tmux.
// Fresh activity isolates the content clock; stale activity also exercises the
// pending-interaction guard. No fake decides whether the fixture is idle.
type contentIdlePaneProvider struct {
	*runtime.Fake
	pane          *tmux.Tmux
	clk           *clock.Fake
	staleActivity bool
}

func (p *contentIdlePaneProvider) SnapshotIdle(name string) (bool, error) {
	return p.pane.SnapshotIdle(name)
}

func (p *contentIdlePaneProvider) Pending(name string) (*runtime.PendingInteraction, error) {
	return p.pane.Pending(name)
}

func (p *contentIdlePaneProvider) GetLastActivity(string) (time.Time, error) {
	if p.staleActivity {
		return p.clk.Now().Add(-time.Hour), nil
	}
	return p.clk.Now(), nil
}

func TestReconcileSessionBeads_ContentIdleTimeoutPreservesQuestionDialog(t *testing.T) {
	// Question rows come from questionDialogFixture in nudge_human_prompt_test.go,
	// captured from a Claude AskUserQuestion dialog, not an invented idle answer.
	const question = `←  ☐ Q1  ☐ Q2  ☐ Q3  ☐ Q4  ✔ Submit  →
Which approach would you prefer for this feature?
❯ 1. Alpha approach (Recommended)
2. Beta variant
3. Gamma variant
4. Type something.
5. Chat about this
Enter to select · Tab/Arrow keys to navigate · Esc to cancel`
	for _, tc := range []struct {
		name          string
		pane          string
		wantAlive     bool
		staleActivity bool
	}{
		{name: "question", pane: question, wantAlive: true},
		{name: "static question", pane: question, wantAlive: true, staleActivity: true},
		{name: "empty-prompt", pane: "❯"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := tmuxtest.NewGuard(t)
			runTmux := func(args ...string) string {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				argv := append([]string{"-u", "-L", guard.SocketName(), "-f", "/dev/null"}, args...)
				out, err := exec.CommandContext(ctx, "tmux", argv...).CombinedOutput()
				if err != nil {
					t.Fatalf("tmux %v: %v: %s", args, err, out)
				}
				return string(out)
			}
			// The producer signals after writing the fixture. wait-for synchronizes
			// capture without sleeps, and cat keeps the detached pane alive.
			script := filepath.Join(t.TempDir(), "pane.sh")
			body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%s'\ntmux -L %s wait-for -S rendered\nexec cat\n", tc.pane, guard.SocketName())
			if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			runTmux("new-session", "-d", "-s", "worker", "-x", "120", "-y", "40", "/bin/sh "+script)
			runTmux("wait-for", "rendered")
			pane := tmux.NewTmuxWithConfig(tmux.Config{SocketName: guard.SocketName()})
			captured, err := pane.CapturePane("worker", 40)
			if err != nil || !strings.Contains(captured, tc.pane) {
				t.Fatalf("fixture not rendered: capture=%q, err=%v", captured, err)
			}

			env := newReconcilerTestEnv()
			rec := events.NewFake()
			env.rec = rec
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", true)
			seat := env.createSessionBead("worker", "worker")
			env.markSessionActive(&seat)
			env.setSessionMetadata(&seat, map[string]string{"provider": "claude", "transport": "tmux"})
			sp := &contentIdlePaneProvider{Fake: env.sp, pane: pane, clk: env.clk, staleActivity: tc.staleActivity}
			it := newIdleTracker()
			it.setTimeout("worker", time.Minute)
			tick := func() {
				t.Helper()
				current, err := env.store.Get(seat.ID)
				if err != nil {
					t.Fatal(err)
				}
				reconcileSessionBeads(
					context.Background(), []beads.Bead{current}, env.desiredState,
					configuredSessionNames(env.cfg, "", env.store), env.cfg, sp, env.store,
					nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
					it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
				)
			}
			tick()
			env.clk.Advance(2 * time.Minute)
			tick()
			if alive := env.sp.IsRunning("worker"); alive != tc.wantAlive {
				t.Fatalf("seat alive=%v after content timeout, want %v; stderr=%s", alive, tc.wantAlive, env.stderr.String())
			}
			if killed := idleTimeoutBackstopKilled(rec); killed == tc.wantAlive {
				t.Fatalf("idle-kill event=%v, want %v", killed, !tc.wantAlive)
			}
		})
	}
}
