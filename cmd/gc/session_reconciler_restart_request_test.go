package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

type restartRequestTestEnv struct {
	store        beads.Store
	sp           *runtime.Fake
	dt           *drainTracker
	clk          *clock.Fake
	rec          events.Recorder
	cfg          *config.City
	desiredState map[string]TemplateParams
	stdout       bytes.Buffer
	stderr       bytes.Buffer
	startOptions []startExecutionOption
}

func newRestartRequestTestEnv() *restartRequestTestEnv {
	return &restartRequestTestEnv{
		store:        beads.NewMemStore(),
		sp:           runtime.NewFake(),
		dt:           newDrainTracker(),
		clk:          &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)},
		rec:          events.Discard,
		cfg:          &config.City{},
		desiredState: make(map[string]TemplateParams),
		startOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
		},
	}
}

func (e *restartRequestTestEnv) createSessionBead(name string) beads.Bead {
	b, err := e.store.Create(beads.Bead{
		Title:  name,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":   name,
			"agent_name":     name,
			"template":       "worker",
			"generation":     "1",
			"instance_token": "test-token",
			"state":          "asleep",
		},
	})
	if err != nil {
		panic("creating test bead: " + err.Error())
	}
	return b
}

func (e *restartRequestTestEnv) setSessionMetadata(session *beads.Bead, kvs map[string]string) {
	for key, value := range kvs {
		_ = e.store.SetMetadata(session.ID, key, value)
		session.Metadata[key] = value
	}
}

func (e *restartRequestTestEnv) reconcile(sessions []beads.Bead) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	e.reconcileWithPoolDesiredAndDrainOps(sessions, poolDesired, nil)
}

func (e *restartRequestTestEnv) reconcileWithPoolDesiredAndDrainOps(sessions []beads.Bead, poolDesired map[string]int, dops drainOps) {
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	_ = reconcileSessionBeads(
		context.Background(),
		sessions,
		e.desiredState,
		cfgNames,
		e.cfg,
		e.sp,
		e.store,
		dops,
		nil,
		nil,
		e.dt,
		poolDesired,
		false,
		nil,
		"",
		nil,
		e.clk,
		e.rec,
		0,
		0,
		&e.stdout,
		&e.stderr,
		e.startOptions...,
	)
}

type clearRestartErrorDrainOps struct {
	*fakeDrainOps
	err error
}

func (d *clearRestartErrorDrainOps) clearRestartRequested(string) error {
	return d.err
}

func newLiveRestartRequestScenario(t *testing.T) (*restartRequestTestEnv, beads.Bead, string) {
	t.Helper()

	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "active",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_RESTART_REQUESTED", "1"); err != nil {
		t.Fatalf("SetMeta(GC_RESTART_REQUESTED): %v", err)
	}

	return env, session, sessionName
}

func TestReconcileSessionBeads_RestartRequestRotatesKeyForSessionIDProviders(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})

	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["session_key"] == "" {
		t.Fatal("session_key = empty, want rotated key for SessionIDFlag provider")
	}
	if got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want empty", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

func TestReconcileSessionBeads_RestartRequestClearsKeyForResumeOnlyProviders(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			ResumeFlag:  "--resume",
			ResumeStyle: "flag",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})

	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["session_key"] != "" {
		t.Fatalf("session_key = %q, want empty for resume-only provider", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want empty", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

func TestReconcileSessionBeads_RestartRequestPreservesLiveHashesDuringHandoff(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "active",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
		"started_live_hash":          "live-before-restart",
		"live_hash":                  "live-before-restart",
		"startup_dialog_verified":    "true",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want empty", got.Metadata["started_config_hash"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["started_live_hash"] != "live-before-restart" {
		t.Fatalf("started_live_hash = %q, want preserved until next successful start", got.Metadata["started_live_hash"])
	}
	if got.Metadata["live_hash"] != "live-before-restart" {
		t.Fatalf("live_hash = %q, want preserved until next successful start", got.Metadata["live_hash"])
	}
	if got.Metadata["startup_dialog_verified"] != "true" {
		t.Fatalf("startup_dialog_verified = %q, want preserved until next successful start", got.Metadata["startup_dialog_verified"])
	}
}

func TestReconcileSessionBeads_RestartRequestReportsClearRestartRequestedError(t *testing.T) {
	env, session, sessionName := newLiveRestartRequestScenario(t)
	env.sp.RemoveMetaErrors[sessionName] = map[string]error{
		"GC_RESTART_REQUESTED": errors.New("permission denied"),
	}

	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 0}, newDrainOps(env.sp))

	got := env.stderr.String()
	if !strings.Contains(got, "clearing restart-requested marker") ||
		!strings.Contains(got, sessionName) ||
		!strings.Contains(got, session.ID) ||
		!strings.Contains(got, "permission denied") {
		t.Fatalf("stderr = %q, want contextual clearRestartRequested diagnostic", got)
	}
}

func TestReconcileSessionBeads_RestartRequestSuppressesGoneClearRestartRequestedError(t *testing.T) {
	env, session, sessionName := newLiveRestartRequestScenario(t)
	dops := &clearRestartErrorDrainOps{
		fakeDrainOps: newFakeDrainOps(),
		err:          fmt.Errorf("%w: %s", runtime.ErrSessionNotFound, sessionName),
	}
	dops.restartRequested[sessionName] = true

	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 0}, dops)

	if got := env.stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want no diagnostic for gone clearRestartRequested error", got)
	}
}

func TestReconcileSessionBeads_RestartRequestPreservesIntentWhenKillFails(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "active",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	env.sp.StopErrors[sessionName] = errors.New("kill denied")

	env.reconcile([]beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("session should remain running when kill fails")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata["restart_requested"] != "true" {
		t.Fatalf("restart_requested = %q, want preserved", got.Metadata["restart_requested"])
	}
	if got.Metadata["session_key"] != "original-key" {
		t.Fatalf("session_key = %q, want original-key", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "hash-before-restart" {
		t.Fatalf("started_config_hash = %q, want preserved", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty until kill succeeds", got.Metadata["continuation_reset_pending"])
	}
	if got := env.stderr.String(); !strings.Contains(got, "stopping restart-requested") || !strings.Contains(got, "kill denied") {
		t.Fatalf("stderr = %q, want kill failure diagnostic", got)
	}
}

func TestReconcileSessionBeads_RestartRequestClearsCircuitBreakerForNextWake(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon: config.DaemonConfig{
			SessionCircuitBreaker:            true,
			SessionCircuitBreakerMaxRestarts: restartRequestTestIntPtr(3),
			SessionCircuitBreakerWindow:      "30m",
		},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	const identity = "worker"
	cb := breakerAt(30*time.Minute, 3)
	base := env.clk.Now().UTC()
	for i := 0; i < 4; i++ {
		cb.RecordRestart(identity, base.Add(time.Duration(i)*time.Second))
	}
	if !cb.IsOpen(identity, base.Add(time.Minute)) {
		t.Fatalf("precondition: breaker should be OPEN for %q", identity)
	}
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: identity,
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := persistSessionCircuitBreakerMetadata(sessionFrontDoor(env.store), session.ID, cb, identity, base); err != nil {
		t.Fatalf("persist circuit metadata: %v", err)
	}
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running after restart-requested kill", sessionName)
	}
	if cb.IsOpen(identity, base.Add(time.Minute)) {
		t.Fatalf("breaker still OPEN for %q after restart-requested kill", identity)
	}
	stopped, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got := stopped.Metadata[sessionCircuitStateMetadata]; got != "" {
		t.Fatalf("persisted circuit state = %q, want cleared", got)
	}
	if got := stopped.Metadata[sessionCircuitRestartsMetadata]; got != "" {
		t.Fatalf("persisted restart history = %q, want cleared", got)
	}
	if got := stopped.Metadata[sessionCircuitResetGenerationMetadata]; got == "" {
		t.Fatal("persisted reset generation is empty, want explicit reset generation")
	}

	env.stdout.Reset()
	env.stderr.Reset()
	env.reconcile([]beads.Bead{stopped})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q did not wake after explicit reset cleared the circuit breaker", sessionName)
	}
	if got := env.stderr.String(); strings.Contains(got, "CIRCUIT_OPEN") {
		t.Fatalf("stderr = %q, want no circuit-open block after explicit reset", got)
	}
}

func TestReconcileSessionBeads_RestartRequestPreemptsRateLimitGate(t *testing.T) {
	env, session, sessionName := newRestartRequestedZombieSession(t)
	env.sp.SetPeekOutput(sessionName, "You've hit your limit, Pro plan\n\n/rate-limit-options")
	env.setSessionMetadata(&session, map[string]string{
		"last_woke_at": env.clk.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
	})

	env.reconcile([]beads.Bead{session})

	assertRestartRequestStoppedBeforeAutonomousGate(t, env, session.ID, sessionName)
	got, _ := env.store.Get(session.ID)
	if got.Metadata["sleep_reason"] == "rate_limit" {
		t.Fatalf("sleep_reason = %q, want explicit reset to preempt rate-limit gate", got.Metadata["sleep_reason"])
	}
}

func TestReconcileSessionBeads_RestartRequestPreemptsStabilityGate(t *testing.T) {
	env, session, sessionName := newRestartRequestedZombieSession(t)
	env.setSessionMetadata(&session, map[string]string{
		"last_woke_at": env.clk.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
	})

	env.reconcile([]beads.Bead{session})

	assertRestartRequestStoppedBeforeAutonomousGate(t, env, session.ID, sessionName)
	got, _ := env.store.Get(session.ID)
	if got.Metadata["last_woke_at"] != "" {
		t.Fatalf("last_woke_at = %q, want restart patch to clear crash-tracker lease", got.Metadata["last_woke_at"])
	}
	if got.Metadata["sleep_reason"] == "rate_limit" {
		t.Fatalf("sleep_reason = %q, want explicit reset to preempt stability gate", got.Metadata["sleep_reason"])
	}
}

func TestReconcileSessionBeads_RestartRequestPreemptsChurnGate(t *testing.T) {
	env, session, sessionName := newRestartRequestedZombieSession(t)
	env.setSessionMetadata(&session, map[string]string{
		"last_woke_at": env.clk.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339),
	})

	env.reconcile([]beads.Bead{session})

	assertRestartRequestStoppedBeforeAutonomousGate(t, env, session.ID, sessionName)
	got, _ := env.store.Get(session.ID)
	if got.Metadata["churn_count"] != "" {
		t.Fatalf("churn_count = %q, want explicit reset to preempt churn gate", got.Metadata["churn_count"])
	}
}

func newRestartRequestedZombieSession(t *testing.T) (*restartRequestTestEnv, beads.Bead, string) {
	t.Helper()
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		Hints:        agent.StartupHints{ProcessNames: []string{"agent-cli"}},
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}
	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "active",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true", ProcessNames: []string{"agent-cli"}}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	env.sp.Zombies[sessionName] = true
	return env, session, sessionName
}

func assertRestartRequestStoppedBeforeAutonomousGate(t *testing.T, env *restartRequestTestEnv, sessionID, sessionName string) {
	t.Helper()
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running; explicit reset should stop it before autonomous gates", sessionName)
	}
	got, _ := env.store.Get(sessionID)
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared after explicit reset patch", got.Metadata["restart_requested"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want cleared", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got := env.stdout.String(); !strings.Contains(got, "Stopped restart-requested session") {
		t.Fatalf("stdout = %q, want stop diagnostic", got)
	}
}

// TestReconcileSessionBeads_RestartRequestNamedAlwaysWakesSameTick guards the
// fix for gastownhall/gascity#2345. A `mode = "always"` named session whose
// tmux was killed out of band (for example, by `gc handoff --target`) before
// the bead's restart_requested flag was processed must wake on the SAME
// reconciler tick, not on the next patrol interval. Before this fix the
// restart_requested branch unconditionally continued past the wake decision,
// imposing a patrol_interval-sized post-handoff wake delay (and, combined
// with watchdog-driven `gc session reset` calls during the gap, sometimes
// multiple patrol cycles).
func TestReconcileSessionBeads_RestartRequestNamedAlwaysWakesSameTick(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})

	// Runtime is NOT started — the tmux session was killed externally
	// (e.g., gc handoff --target) before this reconciler tick. The bead's
	// restart_requested flag was set by `gc session reset` afterwards.
	if env.sp.IsRunning(sessionName) {
		t.Fatal("test fixture wrong: session should not be running")
	}

	env.reconcile([]beads.Bead{session})

	// Same-tick wake: the wake decision must have fired and started the
	// runtime on this same reconcile pass. Before the #2345 fix the
	// restart_requested branch continued past the wake loop, so the fake
	// provider would NOT be running here.
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q did not wake on the same reconciler tick; restart_requested branch skipped the wake decision", sessionName)
	}

	got, _ := env.store.Get(session.ID)
	// The RestartRequestPatch must still run: session_key rotates,
	// restart_requested clears. PreWakePatch (applied by the same-tick wake)
	// subsequently writes last_woke_at and clears continuation_reset_pending,
	// so we assert on the post-wake observable state.
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared after patch applied", got.Metadata["restart_requested"])
	}
	if got.Metadata["last_woke_at"] == "" {
		t.Fatal("last_woke_at = empty, want timestamp from same-tick wake")
	}
}

func restartRequestTestIntPtr(n int) *int { return &n }

func TestReconcileSessionBeads_RestartRequestSkipsCollateralKillForPinnedNamedSession(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"pin_awake":                  "true",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("pinned named session %q was killed by collateral restart request", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared after deferring collateral kill", got.Metadata["restart_requested"])
	}
	if got.Metadata["session_key"] != "original-key" {
		t.Fatalf("session_key = %q, want preserved", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "hash-before-restart" {
		t.Fatalf("started_config_hash = %q, want preserved", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want untouched", got.Metadata["continuation_reset_pending"])
	}
	if got := env.stderr.String(); !strings.Contains(got, "skipping abrupt restart-requested kill for pinned named session") {
		t.Fatalf("stderr = %q, want pinned restart deferral diagnostic", got)
	}
}

// TestReconcileSessionBeads_PinnedGuardClearsAPairedTerminationIntent is the
// backstop for a handoff whose pinned persist failed (Codex #106 r4). The guard
// drops the restart flag UNCONSUMED, so no restart follows; an intent left
// behind would relabel the seat's next unrelated ending as a handoff. The
// handoff's own unwind is best-effort, so the guard must retire the intent too.
func TestReconcileSessionBeads_PinnedGuardClearsAPairedTerminationIntent(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"pin_awake":                  "true",
		"restart_requested":          "true",
		"termination.intent":         "handoff",
		"termination.intent_at":      "2026-09-22T03:00:00Z",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("pinned named session %q was killed; the guard must still defer the kill", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared", got.Metadata["restart_requested"])
	}
	if v := got.Metadata["termination.intent"]; v != "" {
		t.Fatalf("termination.intent = %q after the guard dropped its restart unconsumed, want cleared", v)
	}
	if v := got.Metadata["termination.intent_at"]; v != "" {
		t.Fatalf("termination.intent_at = %q, want cleared with the intent", v)
	}
}

// TestReconcileSessionBeads_PinnedGuardKeepsIntentWhenResetLandsAfterSnapshot
// is the race Codex #106 r5 named. A pinned handoff stamps the intent, arms
// the flag and THEN persists its explicit reset. A tick whose snapshot predates
// that persist must not clear the intent: the reset is consumed next tick, and
// without the intent that ending would record as restart-in-place, dropping a
// real handoff from the numerator.
func TestReconcileSessionBeads_PinnedGuardKeepsIntentWhenResetLandsAfterSnapshot(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"pin_awake":                  "true",
		"restart_requested":          "true",
		"termination.intent":         "handoff",
		"termination.intent_at":      "2026-09-22T03:00:00Z",
	})
	// The tick's snapshot is taken HERE, before the handoff's persist lands.
	snapshot := session
	snapshot.Metadata = make(map[string]string, len(session.Metadata))
	for k, v := range session.Metadata {
		snapshot.Metadata[k] = v
	}
	if err := env.store.SetMetadata(session.ID, "continuation_reset_pending", "true"); err != nil {
		t.Fatalf("persisting the reset after the snapshot: %v", err)
	}
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{snapshot})

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if v := got.Metadata["termination.intent"]; v != "handoff" {
		t.Fatalf("termination.intent = %q, want it KEPT: a reset persisted after the snapshot means its restart is still coming", v)
	}
	// And the restart itself must survive (Codex #106 r6): clearing the request
	// while keeping the intent would strand the seat, never stopping, with an
	// intent armed to relabel some later ending.
	if v := got.Metadata["restart_requested"]; v != "true" {
		t.Fatalf("restart_requested = %q, want still true: the guard must touch nothing on a stale snapshot", v)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatal("the guard killed the seat; deferring means doing nothing this tick")
	}
}

// pinnedCollateralRequestEnv builds a pinned, running named seat whose bead
// carries only a COLLATERAL restart request: no intent, no explicit reset. It
// returns the env, the seat's runtime name, and a snapshot copy of the bead
// taken before anything else lands.
func pinnedCollateralRequestEnv(t *testing.T) (*restartRequestTestEnv, string, beads.Bead, beads.Bead) {
	t.Helper()
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}
	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"pin_awake":                  "true",
		"restart_requested":          "true",
	})
	snapshot := session
	snapshot.Metadata = make(map[string]string, len(session.Metadata))
	for k, v := range session.Metadata {
		snapshot.Metadata[k] = v
	}
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	return env, sessionName, session, snapshot
}

// landHandoffBatch writes what RequestFreshRestartWithIntent writes, in one batch.
func landHandoffBatch(t *testing.T, env *restartRequestTestEnv, id string) {
	t.Helper()
	if err := env.store.SetMetadataBatch(id, map[string]string{
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
		"termination.intent":         "handoff",
		"termination.intent_at":      "2026-09-22T17:00:00Z",
	}); err != nil {
		t.Fatalf("landing the handoff batch: %v", err)
	}
}

// TestReconcileSessionBeads_PinnedGuardDefersWhenHandoffLandsOnACollateralSnapshot
// is Codex #106 r11. The snapshot holds a collateral request and NO intent, so
// the r5 re-read (then scoped to a snapshot intent) was skipped, and the guard
// cleared the request the handoff's atomic batch had just written. The seat was
// left with only the reset and the intent, which never restart a pinned seat.
func TestReconcileSessionBeads_PinnedGuardDefersWhenHandoffLandsOnACollateralSnapshot(t *testing.T) {
	env, sessionName, session, snapshot := pinnedCollateralRequestEnv(t)
	landHandoffBatch(t, env, session.ID)

	env.reconcile([]beads.Bead{snapshot})

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if v := got.Metadata["restart_requested"]; v != "true" {
		t.Fatalf("restart_requested = %q, want still true: a stale collateral snapshot must not clear a handoff's request", v)
	}
	if v := got.Metadata["termination.intent"]; v != "handoff" {
		t.Fatalf("termination.intent = %q, want handoff kept", v)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatal("the guard killed the seat on a stale snapshot; deferring means doing nothing this tick")
	}
	// The re-read must catch this BEFORE any clear. The post-clear verify would
	// repair the request, but only after clearing the runtime flag and the
	// request; a deferral touches nothing.
	if stderr := env.stderr.String(); !strings.Contains(stderr, "deferring pinned restart guard") || strings.Contains(stderr, "re-armed restart") {
		t.Fatalf("stderr = %q, want a deferral before any clear, not a clear-and-re-arm", stderr)
	}
}

// TestReconcileSessionBeads_PinnedGuardReArmsWhenHandoffLandsDuringItsClear
// covers the interleaving the re-read cannot see: the batch lands after the
// re-read and before the clear. The clear then removes the handoff's request;
// the guard's post-clear verify must find the explicit reset and re-arm it.
func TestReconcileSessionBeads_PinnedGuardReArmsWhenHandoffLandsDuringItsClear(t *testing.T) {
	env, sessionName, session, snapshot := pinnedCollateralRequestEnv(t)
	pinnedGuardAfterReReadHook = func() { landHandoffBatch(t, env, session.ID) }
	defer func() { pinnedGuardAfterReReadHook = nil }()

	env.reconcile([]beads.Bead{snapshot})
	pinnedGuardAfterReReadHook = nil

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if v := got.Metadata["restart_requested"]; v != "true" {
		t.Fatalf("restart_requested = %q, want re-armed: the reset landed during the clear, and without a request it never restarts the seat", v)
	}
	if v := got.Metadata["continuation_reset_pending"]; v != "true" {
		t.Fatalf("continuation_reset_pending = %q, want the landed reset untouched", v)
	}
	if !strings.Contains(env.stderr.String(), "re-armed restart for pinned named session") {
		t.Fatalf("stderr = %q, want the re-arm diagnostic", env.stderr.String())
	}

	// And the next tick, on fresh state, performs the restart.
	fresh, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	env.reconcile([]beads.Bead{fresh})
	if env.sp.IsRunning(sessionName) {
		t.Fatal("the re-armed handoff did not stop the seat on the next tick")
	}
}

func TestReconcileSessionBeads_RestartRequestAllowsExplicitResetForPinnedNamedSession(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "active",
		"pin_awake":                  "true",
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("explicit reset should still stop pinned named session %q", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared after explicit reset", got.Metadata["restart_requested"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated after explicit reset", got.Metadata["session_key"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true until next start", got.Metadata["continuation_reset_pending"])
	}
}

// TestDoHandoff_PinnedAlwaysSessionPersistsResetAndReconcilerStopsSession
// covers Fix 1 end-to-end: doHandoffWithOutcome on a pinned always-mode
// session with a working persistRestart must land continuation_reset_pending
// on the session bead, and that persisted state must be exactly what lets the
// reconciler's explicit-reset escape hatch actually stop the pinned session
// on its next pass (mirrors
// TestReconcileSessionBeads_RestartRequestAllowsExplicitResetForPinnedNamedSession,
// but drives the persisted state through the CLI instead of setting it
// directly).
func TestDoHandoff_PinnedAlwaysSessionPersistsResetAndReconcilerStopsSession(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "mayor", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "mayor", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "mayor")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "mayor",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "mayor",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"pin_awake":                  "true",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	dops := newDrainOps(env.sp)
	persistCalled := false
	persistRestart := func() error {
		persistCalled = true
		// Stands in for Manager.RequestFreshRestart (out of scope here):
		// what doHandoffWithOutcome actually depends on is that a working
		// persistRestart lands continuation_reset_pending on the bead.
		return env.store.SetMetadata(session.ID, "continuation_reset_pending", "true")
	}

	var stdout, stderr bytes.Buffer
	outcome := doHandoffWithOutcome(env.store, env.store, env.rec, dops, persistRestart,
		sessionName, sessionName, []string{"HANDOFF: context full"}, &stdout, &stderr)
	if outcome.code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", outcome.code, stderr.String())
	}
	if !outcome.restartRequested {
		t.Fatal("restartRequested = false, want true for pinned always-mode session with working persistRestart")
	}
	if !persistCalled {
		t.Fatal("persistRestart was not called for pinned always-mode session")
	}
	if !strings.Contains(stdout.String(), "requesting restart") {
		t.Errorf("stdout = %q, want restart-requested confirmation", stdout.String())
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true after successful pinned handoff", got.Metadata["continuation_reset_pending"])
	}

	// The reconciler is the end-to-end oracle: with continuation_reset_pending
	// now landed, its explicit-reset escape hatch must actually stop the
	// pinned session on the next reconcile pass.
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{got}, map[string]int{"mayor": 1}, dops)
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("pinned session %q still running after reconcile; persisted restart should have let the reconciler stop it", sessionName)
	}
}

// TestReconcileSessionBeads_RestartRequestOnDemandWakesFromResetMarker pins the
// premise that lets gc handoff cycle an on-demand configured named seat
// (ga-cctcju): with NO wake marker and NO pool or assigned-work demand, the
// restart consume kills the runtime and lands RestartRequestPatch, and the
// durable reset-pending marker (continuation_reset_pending + reset_committed_at)
// wakes the seat fresh on the next tick. If this ever stops holding, a
// self-handoff would leave an on-demand seat down, and the handoff must arm an
// explicit wake before requesting the restart.
func TestReconcileSessionBeads_RestartRequestOnDemandWakesFromResetMarker(t *testing.T) {
	env, session, sessionName := newLiveRestartRequestScenario(t)
	env.setSessionMetadata(&session, map[string]string{
		"restart_requested": "true",
	})

	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{}, nil)
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running after the restart-requested kill", sessionName)
	}
	stopped, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got := stopped.Metadata["continuation_reset_pending"]; got != "true" {
		t.Fatalf("continuation_reset_pending = %q after the consume, want the durable reset marker", got)
	}
	if stopped.Metadata["wake_request"] != "" {
		t.Fatalf("wake_request = %q; this case must carry no wake marker", stopped.Metadata["wake_request"])
	}

	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{stopped}, map[string]int{}, nil)
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("on-demand session %q did not wake from its reset marker after the restart kill (ga-cctcju)\nstdout: %s\nstderr: %s", sessionName, env.stdout.String(), env.stderr.String())
	}
}
