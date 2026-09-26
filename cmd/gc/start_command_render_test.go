package main

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// ga-b1u4yg: an agent start_command may carry Go-template placeholders (the
// core control-dispatcher's `--follow {{.Agent}}`). The reconciler create path
// renders it through expandSessionSetup; the CLI/worker-handle resume path and
// buildResumeCommand used the RAW command, so a resumed dispatcher launched
// `gc convoy control --serve --follow {{.Agent}}` and died with
// `agent "{{.Agent}}" not found in config`.

const templatedDispatcherStartCommand = "sh -c 'exec gc convoy control --serve --follow {{.Agent}} --rig {{.Rig}}'"

const renderedDispatcherStartCommand = "sh -c 'exec gc convoy control --serve --follow myrig/control-dispatcher --rig myrig'"

func templatedStartCommandCity(cityDir string) *config.City {
	// A control dispatcher is a singleton: one session per agent, so its
	// session identity is the agent's own qualified name.
	singleton := 1
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{{
			Name: "myrig",
			Path: filepath.Join(cityDir, "myrig"),
		}},
		Agents: []config.Agent{{
			Name:              "control-dispatcher",
			Dir:               "myrig",
			StartCommand:      templatedDispatcherStartCommand,
			MaxActiveSessions: &singleton,
		}},
	}
}

func templatedStartCommandInfo(cityDir, storedCommand string) session.Info {
	return session.Info{
		ID:          "gc-dispatcher",
		Template:    "myrig/control-dispatcher",
		AgentName:   "myrig/control-dispatcher",
		SessionName: "myrig--control-dispatcher",
		Command:     storedCommand,
		WorkDir:     cityDir,
	}
}

func assertRenderedDispatcherCommand(t *testing.T, label, got string) {
	t.Helper()
	if strings.Contains(got, "{{") {
		t.Fatalf("%s = %q, still carries an unrendered template placeholder", label, got)
	}
	if !strings.Contains(got, "--follow myrig/control-dispatcher") {
		t.Fatalf("%s = %q, want the target session's qualified agent name after --follow", label, got)
	}
	if !strings.Contains(got, "--rig myrig") {
		t.Fatalf("%s = %q, want the target session's rig rendered", label, got)
	}
}

// (1) CLI resume: the worker-handle runtime resolver must render the agent's
// start_command for the TARGET session, whatever raw text the bead stored.
func TestResolvedWorkerRuntimeWithConfigRendersTemplatedStartCommand(t *testing.T) {
	cityDir := t.TempDir()
	cfg := templatedStartCommandCity(cityDir)

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, templatedStartCommandInfo(cityDir, templatedDispatcherStartCommand), "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	assertRenderedDispatcherCommand(t, "Command", resolved.Command)
	if got, want := resolved.Command, renderedDispatcherStartCommand; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

// (2) A stored command the reconciler already rendered (possibly with extra
// appended args) must be recognized as current, not discarded in favor of the
// raw template.
func TestResolvedWorkerRuntimeWithConfigPreservesStoredRenderedStartCommand(t *testing.T) {
	cityDir := t.TempDir()
	cfg := templatedStartCommandCity(cityDir)
	stored := renderedDispatcherStartCommand + " --extra-flag"

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, templatedStartCommandInfo(cityDir, stored), "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	if got := resolved.Command; got != stored {
		t.Fatalf("Command = %q, want stored rendered command %q preserved", got, stored)
	}
}

// (3) buildResumeCommand renders the start_command the same way.
func TestBuildResumeCommandRendersTemplatedStartCommand(t *testing.T) {
	cityDir := t.TempDir()
	cfg := templatedStartCommandCity(cityDir)

	cmd, _ := buildResumeCommand(cityDir, cfg, templatedStartCommandInfo(cityDir, templatedDispatcherStartCommand), "", nil, io.Discard)
	assertRenderedDispatcherCommand(t, "resume command", cmd)
}

// (4) End to end through the `gc session submit` path: a suspended session
// whose stored command is the raw template is resumed by handle.Message (what
// cmdSessionSubmit calls), which reaches Manager.ensureRunning -> sp.Start. The
// runtime must be started with the start_command rendered for the target
// session's own qualified name, never the raw `{{.Agent}}` text.
func TestSessionSubmitResumeStartsRenderedTemplatedStartCommand(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"
`)
	cfg := templatedStartCommandCity(cityDir)
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig(cityDir, store, sp, cfg)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "myrig/control-dispatcher",
		Title:    "dispatcher",
		Command:  templatedDispatcherStartCommand,
		WorkDir:  cityDir,
		// agent_name is what `gc session new` and the reconciler stamp; without
		// it a manual session is treated as a provider session and never
		// resolves through the agent template at all.
		ExtraMeta: map[string]string{"session_origin": "manual", "agent_name": "myrig/control-dispatcher"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Suspended, so the submit below takes the resume branch
	// (Manager.submit -> sendLocked -> ensureRunning -> sp.Start).
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	handle, err := workerHandleForSessionWithConfig(cityDir, store, sp, cfg, info.ID)
	if err != nil {
		t.Fatalf("workerHandleForSessionWithConfig: %v", err)
	}
	sp.Calls = nil
	if _, err := handle.Message(context.Background(), worker.MessageRequest{
		Text:     "wake",
		Delivery: workerDeliveryIntentForSubmitIntent(session.SubmitIntentDefault),
	}); err != nil {
		t.Fatalf("handle.Message: %v", err)
	}

	start := sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil; calls = %#v", info.SessionName, sp.Calls)
	}
	assertRenderedDispatcherCommand(t, "started command", start.Command)
	if got, want := start.Command, renderedDispatcherStartCommand; got != want {
		t.Fatalf("started command = %q, want %q", got, want)
	}
}

// (5) A configured named session renders with its own configured identity —
// the identity the reconciler's named-session loop renders it with — not the
// backing template's name.
func TestResolvedWorkerRuntimeWithConfigRendersNamedSessionIdentity(t *testing.T) {
	cityDir := t.TempDir()
	cfg := templatedStartCommandCity(cityDir)
	info := templatedStartCommandInfo(cityDir, templatedDispatcherStartCommand)
	info.AgentName = ""
	info.ConfiguredNamedSession = true
	info.ConfiguredNamedIdentity = "myrig/dispatch-alpha"
	info.SessionName = "myrig--dispatch-alpha"

	resolved, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, info, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	want := "sh -c 'exec gc convoy control --serve --follow myrig/dispatch-alpha --rig myrig'"
	if got := resolved.Command; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}
