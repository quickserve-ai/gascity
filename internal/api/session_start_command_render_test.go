package api

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// ga-b1u4yg: the API resume path must render an agent start_command's
// Go-template placeholders for the TARGET session, the same way the
// reconciler's create path does, instead of launching the raw
// `--follow {{.Agent}}` text.
func templatedStartCommandFakeState(t *testing.T) (*fakeState, session.Info) {
	t.Helper()
	fs := newSessionFakeState(t)
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{{
			Name: "myrig",
			Path: filepath.Join(fs.cityPath, "myrig"),
		}},
		Agents: []config.Agent{{
			Name:         "control-dispatcher",
			Dir:          "myrig",
			StartCommand: "sh -c 'exec gc convoy control --serve --follow {{.Agent}} --rig {{.Rig}}'",
		}},
	}

	info := session.Info{
		ID:          "gc-dispatcher",
		Template:    "myrig/control-dispatcher",
		AgentName:   "myrig/control-dispatcher",
		SessionName: "myrig--control-dispatcher",
		Command:     "sh -c 'exec gc convoy control --serve --follow {{.Agent}} --rig {{.Rig}}'",
		WorkDir:     fs.cityPath,
	}
	return fs, info
}

const renderedAPIDispatcherStartCommand = "sh -c 'exec gc convoy control --serve --follow myrig/control-dispatcher --rig myrig'"

func TestBuildSessionResumeRendersTemplatedStartCommand(t *testing.T) {
	fs, info := templatedStartCommandFakeState(t)
	srv := New(fs)

	cmd, _, err := srv.buildSessionResume(info)
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if strings.Contains(cmd, "{{") {
		t.Fatalf("resume command = %q, still carries an unrendered template placeholder", cmd)
	}
	if got, want := cmd, renderedAPIDispatcherStartCommand; got != want {
		t.Fatalf("resume command = %q, want %q", got, want)
	}
}

// The API server's worker-factory runtime resolver (submit/attach through the
// API) shares resolveSessionRuntimeWithMetadata and must render too.
func TestResolveWorkerSessionRuntimeRendersTemplatedStartCommand(t *testing.T) {
	fs, info := templatedStartCommandFakeState(t)
	srv := New(fs)

	resolved, err := srv.resolveWorkerSessionRuntime(info)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntime: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolveWorkerSessionRuntime() = nil")
	}
	if strings.Contains(resolved.Command, "{{") {
		t.Fatalf("Command = %q, still carries an unrendered template placeholder", resolved.Command)
	}
	if got, want := resolved.Command, renderedAPIDispatcherStartCommand; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

// A configured named session renders with its configured identity, not the
// backing template's name.
func TestResolveWorkerSessionRuntimeRendersNamedSessionIdentity(t *testing.T) {
	fs, info := templatedStartCommandFakeState(t)
	info.AgentName = ""
	info.ConfiguredNamedSession = true
	info.ConfiguredNamedIdentity = "myrig/dispatch-alpha"
	info.SessionName = "myrig--dispatch-alpha"
	srv := New(fs)

	resolved, err := srv.resolveWorkerSessionRuntime(info)
	if err != nil {
		t.Fatalf("resolveWorkerSessionRuntime: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolveWorkerSessionRuntime() = nil")
	}
	want := "sh -c 'exec gc convoy control --serve --follow myrig/dispatch-alpha --rig myrig'"
	if got := resolved.Command; got != want {
		t.Fatalf("Command = %q, want %q", got, want)
	}
}

// The API's named-session materialize path (a submit/nudge to a named session
// with no bead yet) creates AND starts the runtime directly; the started
// command must be the start_command rendered for the named identity.
func TestMaterializeNamedSessionStartsRenderedTemplatedStartCommand(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].StartCommand = "sh -c 'exec gc convoy control --serve --follow {{.Agent}} --rig {{.Rig}}'"
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget: %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec")
	}
	id, err := srv.materializeNamedSession(fs.cityBeadStore, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession: %v", err)
	}
	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	start := fs.sp.LastStartConfig(bead.Metadata["session_name"])
	if start == nil {
		t.Fatalf("Start call not recorded: %#v", fs.sp.Calls)
	}
	want := "sh -c 'exec gc convoy control --serve --follow " + spec.Identity + " --rig myrig'"
	if got := start.Command; got != want {
		t.Fatalf("started command = %q, want %q", got, want)
	}
	if spec.Identity != "myrig/worker" {
		t.Fatalf("spec.Identity = %q, want myrig/worker", spec.Identity)
	}
}
