//go:build integration

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
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

	// GA_FHBNMZ_DELIVERY_TOKEN probes -e delivery to the pane process. The
	// realistic name AZURE_OPENAI_API_KEY cannot be used for THAT assertion
	// on a developer box: tmux runs pane commands via the user's default
	// shell, and a dotfile (~/.zshenv on the machine that found this)
	// re-exporting the real key overwrites the delivered value before the
	// pane command sees it. It still serves the argv/global-env absence
	// assertions below.
	env := map[string]string{
		"AZURE_OPENAI_API_KEY":     secretVal,
		"GA_FHBNMZ_DELIVERY_TOKEN": secretVal,
		"GC_PROVIDER":              "probe",
	}
	workDir := t.TempDir()
	envDump := filepath.Join(workDir, "pane-env")
	paneCmd := "env > " + shellquote.Quote(envDump+".tmp") + " && mv " + shellquote.Quote(envDump+".tmp") + " " + shellquote.Quote(envDump) + "; sleep 60"
	if err := tm.NewSessionWithCommandAndEnv("coldstart", workDir, paneCmd, env); err != nil {
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

	// And in the PANE PROCESS itself — session-env storage alone would not
	// prove the child received it.
	deadline := time.Now().Add(10 * time.Second)
	var paneEnv []byte
	for {
		paneEnv, err = os.ReadFile(envDump)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("pane env dump never appeared at %s: %v", envDump, err)
	}
	if !strings.Contains(string(paneEnv), "GA_FHBNMZ_DELIVERY_TOKEN="+secretVal) {
		t.Fatalf("pane process env lacks the -e value; dump:\n%s", paneEnv)
	}
}
