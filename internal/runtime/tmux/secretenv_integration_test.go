//go:build integration

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestColdStartServerArgvAndEnvCarryNoSecrets is the end-to-end proof for
// ga-fhbnmz: a cold-started city's tmux SERVER must expose the session's
// secret neither in its process-table argv nor in its global environment,
// while the session itself still receives the var through the session env.
func TestColdStartServerArgvAndEnvCarryNoSecrets(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	const secretVal = "ga-fhbnmz-canary-value-1994"
	t.Setenv("GA_FHBNMZ_CANARY_TOKEN", secretVal) // ambient in this process → must be scrubbed from the server

	cfg := DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gctest-coldstart-%d-%d", os.Getpid(), time.Now().UnixNano())
	tm := NewTmuxWithConfig(cfg)
	t.Cleanup(func() { _ = tm.KillServer() })

	env := map[string]string{
		"AZURE_OPENAI_API_KEY": secretVal,
		"GC_PROVIDER":          "probe",
	}
	if err := tm.NewSessionWithCommandAndEnv("coldstart", t.TempDir(), "sleep 60", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv (cold start): %v", err)
	}

	pidOut, err := tm.run("display-message", "-p", "#{pid}")
	if err != nil {
		t.Fatalf("reading server pid: %v", err)
	}
	pid := strings.TrimSpace(pidOut)

	argv, err := exec.Command("ps", "-o", "command=", "-p", pid).Output()
	if err != nil {
		t.Fatalf("ps -p %s: %v", pid, err)
	}
	if strings.Contains(string(argv), secretVal) {
		t.Fatalf("tmux server argv carries the secret value: %s", string(argv))
	}
	if strings.Contains(string(argv), "-e") && strings.Contains(string(argv), "AZURE_OPENAI_API_KEY") {
		t.Fatalf("tmux server was forked by the -e new-session, not the inert anchor: %s", string(argv))
	}
	if !strings.Contains(string(argv), "gc-srv-anchor-") {
		t.Fatalf("tmux server argv is not the inert anchor's: %s", string(argv))
	}

	// The anchor must be gone once the real session exists.
	sessions, err := tm.run("list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatalf("list-sessions: %v", err)
	}
	if strings.Contains(sessions, "gc-srv-anchor-") {
		t.Fatalf("anchor session survived the real session's creation: %q", sessions)
	}

	globalEnv, err := tm.run("show-environment", "-g")
	if err != nil {
		t.Fatalf("show-environment -g: %v", err)
	}
	if strings.Contains(globalEnv, secretVal) {
		t.Fatal("tmux server global environment carries the secret value")
	}

	// Delivery contract unchanged: the -e value landed in the SESSION env.
	sessionVal, err := tm.GetEnvironment("coldstart", "AZURE_OPENAI_API_KEY")
	if err != nil {
		t.Fatalf("GetEnvironment(session): %v", err)
	}
	if sessionVal != secretVal {
		t.Fatalf("session env AZURE_OPENAI_API_KEY = %q, want %q", sessionVal, secretVal)
	}
}
