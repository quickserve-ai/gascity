package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func listOpenMessagesBothTiers(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	all, err := store.List(beads.ListQuery{
		Type:      "message",
		Status:    "open",
		TierMode:  beads.TierBoth,
		AllowScan: true,
	})
	if err != nil {
		t.Fatalf("List messages: %v", err)
	}
	return all
}

func listOpenMessagesInTier(t *testing.T, store beads.Store, tier beads.TierMode) []beads.Bead {
	t.Helper()
	all, err := store.List(beads.ListQuery{
		Type:      "message",
		Status:    "open",
		TierMode:  tier,
		AllowScan: true,
	})
	if err != nil {
		t.Fatalf("List messages in tier %v: %v", tier, err)
	}
	return all
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func listOpenBeadsBothTiers(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	all, err := store.List(beads.ListQuery{
		Status:    "open",
		TierMode:  beads.TierBoth,
		AllowScan: true,
	})
	if err != nil {
		t.Fatalf("List open beads: %v", err)
	}
	return all
}

func TestHandoffSuccess(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	code := doHandoff(store, store, rec, dops, nil, "mayor", "mayor",
		[]string{"HANDOFF: context full"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Verify mail bead created.
	all := listOpenMessagesBothTiers(t, store)
	if len(all) != 1 {
		t.Fatalf("got %d beads, want 1", len(all))
	}
	b := all[0]
	if b.Title != "HANDOFF: context full" {
		t.Errorf("Title = %q, want %q", b.Title, "HANDOFF: context full")
	}
	if b.Type != "message" {
		t.Errorf("Type = %q, want %q", b.Type, "message")
	}
	if b.Assignee != "mayor" {
		t.Errorf("Assignee = %q, want %q", b.Assignee, "mayor")
	}
	if b.From != "mayor" {
		t.Errorf("From = %q, want %q", b.From, "mayor")
	}
	if b.Description != "" {
		t.Errorf("Description = %q, want empty", b.Description)
	}
	if !b.Ephemeral {
		t.Errorf("Ephemeral = false, want true")
	}
	if issueMessages := listOpenMessagesInTier(t, store, beads.TierIssues); len(issueMessages) != 0 {
		t.Fatalf("issue-tier messages = %#v, want none", issueMessages)
	}

	// Verify restart-requested flag set.
	if !dops.restartRequested["mayor"] {
		t.Error("restart-requested flag not set")
	}

	// Verify events recorded.
	if len(rec.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.Events))
	}
	if rec.Events[0].Type != events.MailSent {
		t.Errorf("event[0].Type = %q, want %q", rec.Events[0].Type, events.MailSent)
	}
	if rec.Events[1].Type != events.SessionDraining {
		t.Errorf("event[1].Type = %q, want %q", rec.Events[1].Type, events.SessionDraining)
	}
	if rec.Events[1].Message != "handoff" {
		t.Errorf("event[1].Message = %q, want %q", rec.Events[1].Message, "handoff")
	}

	// Verify stdout confirmation.
	if !strings.Contains(stdout.String(), "Handoff: sent mail") {
		t.Errorf("stdout = %q, want confirmation message", stdout.String())
	}
}

func TestWaitForControllerRestartHandoffFlagCleared(t *testing.T) {
	dops := &drainOpsWithCountdown{fakeDrainOps: newFakeDrainOps(), remaining: 2}
	if err := dops.setRestartRequested("worker"); err != nil {
		t.Fatalf("setRestartRequested: %v", err)
	}

	var stderr bytes.Buffer
	code := waitForControllerRestart(context.Background(), dops, runtime.NewFake(), "worker", "gc handoff",
		10*time.Millisecond, 5*time.Second, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0 when flag cleared; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	if dops.restartRequested["worker"] {
		t.Error("restart flag should be cleared by the simulated reconciler")
	}
}

// TestWaitForControllerRestartHandoffFlagClearedButSessionStillRunning covers
// Fix 2: the reconciler's pinned-session collateral-skip clears
// GC_RESTART_REQUESTED without stopping the session (see
// pinnedConfiguredNamedSessionKillProtected in session_reconciler.go), so a
// cleared flag alone must not be reported as success while the session is
// still running.
func TestWaitForControllerRestartHandoffFlagClearedButSessionStillRunning(t *testing.T) {
	dops := &drainOpsWithCountdown{fakeDrainOps: newFakeDrainOps(), remaining: 2}
	if err := dops.setRestartRequested("worker"); err != nil {
		t.Fatalf("setRestartRequested: %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	var stderr bytes.Buffer
	code := waitForControllerRestart(context.Background(), dops, sp, "worker", "gc handoff",
		10*time.Millisecond, 50*time.Millisecond, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (flag cleared but session still running is not success); stderr: %s", code, stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "gc handoff: controller did not act within") {
		t.Errorf("stderr = %q, want handoff timeout diagnostic", got)
	}
}

func TestWaitForControllerRestartHandoffTimeout(t *testing.T) {
	dops := newFakeDrainOps()
	if err := dops.setRestartRequested("worker"); err != nil {
		t.Fatalf("setRestartRequested: %v", err)
	}

	var stderr bytes.Buffer
	code := waitForControllerRestart(context.Background(), dops, runtime.NewFake(), "worker", "gc handoff",
		10*time.Millisecond, 25*time.Millisecond, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 on timeout", code)
	}
	if got := stderr.String(); !strings.Contains(got, "gc handoff: controller did not act within") {
		t.Errorf("stderr = %q, want handoff timeout diagnostic", got)
	}
	if !strings.Contains(stderr.String(), "gc dashboard") {
		t.Errorf("stderr = %q, want gc dashboard hint", stderr.String())
	}
}

func TestWaitForControllerRestartHandoffTimeoutReportsLastPollError(t *testing.T) {
	dops := newFakeDrainOps()
	if err := dops.setRestartRequested("worker"); err != nil {
		t.Fatalf("setRestartRequested: %v", err)
	}
	dops.restartReadErr = errors.New("metadata read failed")

	var stderr bytes.Buffer
	code := waitForControllerRestart(context.Background(), dops, runtime.NewFake(), "worker", "gc handoff",
		10*time.Millisecond, 25*time.Millisecond, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 on timeout", code)
	}
	if got := stderr.String(); !strings.Contains(got, "last poll error: metadata read failed") {
		t.Errorf("stderr = %q, want last poll error", got)
	}
}

func TestWaitForControllerRestartHandoffContextCancel(t *testing.T) {
	dops := newFakeDrainOps()
	if err := dops.setRestartRequested("worker"); err != nil {
		t.Fatalf("setRestartRequested: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var stderr bytes.Buffer

	done := make(chan int, 1)
	go func() {
		done <- waitForControllerRestart(ctx, dops, runtime.NewFake(), "worker", "gc handoff",
			10*time.Millisecond, 30*time.Second, &stderr)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, want 0 on context cancel", code)
		}
		if !dops.restartRequested["worker"] {
			t.Error("restart flag should remain set after context cancel")
		}
		if got := stderr.String(); !strings.Contains(got, "gc handoff: signal received; restart request remains set") {
			t.Errorf("stderr = %q, want pending restart warning", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForControllerRestart did not exit on context cancel")
	}
}

func TestCmdHandoffAutoSendsMailWithoutBlocking(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_NAME", "mayor")

	var stdout, stderr bytes.Buffer
	cmd := newHandoffCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--auto", "context cycle"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc handoff --auto failed: %v; stderr=%s", err, stderr.String())
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all := listOpenMessagesBothTiers(t, store)
	if len(all) != 1 {
		t.Fatalf("got %d open beads, want 1", len(all))
	}
	if got := all[0].Title; got != "context cycle" {
		t.Fatalf("mail title = %q, want context cycle", got)
	}
	if got := all[0].Type; got != "message" {
		t.Fatalf("mail type = %q, want message", got)
	}
	for _, want := range []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel} {
		if !hasString(all[0].Labels, want) {
			t.Fatalf("auto handoff mail labels = %#v, want %q", all[0].Labels, want)
		}
	}
	if strings.Contains(stdout.String(), "requesting restart") {
		t.Fatalf("stdout = %q, --auto must not request restart", stdout.String())
	}
	if !strings.Contains(stdout.String(), "auto") {
		t.Fatalf("stdout = %q, want auto handoff confirmation", stdout.String())
	}
}

func TestCmdHandoffAutoHookFormatCodex(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_NAME", "mayor")

	var stdout, stderr bytes.Buffer
	cmd := newHandoffCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--auto", "--hook-format", "codex", "context cycle"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc handoff --auto --hook-format codex failed: %v; stderr=%s", err, stderr.String())
	}

	var payload struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not Codex hook JSON: %v\n%s", err, stdout.String())
	}
	if got, want := payload.HookSpecificOutput.HookEventName, "PreCompact"; got != want {
		t.Fatalf("hookEventName = %q, want %q", got, want)
	}
	if !strings.Contains(payload.HookSpecificOutput.AdditionalContext, "Handoff: sent auto mail") {
		t.Fatalf("additionalContext = %q, want handoff confirmation", payload.HookSpecificOutput.AdditionalContext)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all := listOpenMessagesBothTiers(t, store)
	if len(all) != 1 {
		t.Fatalf("open beads = %d, want handoff mail", len(all))
	}
	if !strings.Contains(payload.HookSpecificOutput.AdditionalContext, all[0].ID) {
		t.Fatalf("additionalContext = %q, want handoff mail id %s", payload.HookSpecificOutput.AdditionalContext, all[0].ID)
	}
}

func TestDoHandoffAutoReportsHookOutputWriteError(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	var stderr bytes.Buffer

	code := doHandoffAuto(store, store, rec, "mayor", []string{"context cycle"}, "codex", errWriter{}, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "writing hook output") {
		t.Fatalf("stderr = %q, want hook output write error", stderr.String())
	}
	all := listOpenMessagesBothTiers(t, store)
	if len(all) != 1 {
		t.Fatalf("open beads = %d, want handoff mail still created", len(all))
	}
}

func TestCmdHandoffAutoUsesDefaultSubject(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_NAME", "mayor")

	var stdout, stderr bytes.Buffer
	cmd := newHandoffCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--auto"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc handoff --auto failed: %v; stderr=%s", err, stderr.String())
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	all := listOpenMessagesBothTiers(t, store)
	if len(all) != 1 {
		t.Fatalf("got %d open beads, want 1", len(all))
	}
	if got := all[0].Title; got != "context cycle" {
		t.Fatalf("mail title = %q, want context cycle", got)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestCmdHandoffAutoRejectsTarget(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cmdHandoff([]string{"context cycle"}, "mayor", true, "", &stdout, &stderr); code == 0 {
		t.Fatal("cmdHandoff returned 0 for --auto with --target")
	}
	if !strings.Contains(stderr.String(), "--auto cannot be used with --target") {
		t.Fatalf("stderr = %q, want --auto/--target conflict", stderr.String())
	}
}

// Regression for gastownhall/gascity#744:
// gc handoff on a named (human-attended) session used to call
// setRestartRequested unconditionally. The controller cannot respawn a
// user-started session, so the PreCompact hook crashed the user to their shell
// on every context compaction. doHandoff must recognize the named-session
// case, still send the handoff mail, and skip both the tmux and bead restart
// flags.
// TestDoHandoff_OnDemandNamedSessionArmsWakeAndRestart supersedes the
// Regression744 skip test (ga-cctcju). gastownhall/gascity#744's hazard was a
// restart kill with no respawn path; the fix back then was to skip the
// restart for named sessions entirely. Always-mode named sessions became
// restartable when the desired-state pass started rewaking them
// unconditionally; on-demand named seats now cycle too, with the missing
// respawn path supplied explicitly: a durable wake_request marker is armed on
// the session bead BEFORE any restart flag, so the reconciler wakes the seat
// fresh after the kill even when it has no assigned-work demand (unread mail
// is not demand — bd ready returns no message beads).
func TestDoHandoff_OnDemandNamedSessionArmsWakeAndRestart(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	b, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, "session_name", "mayor"); err != nil {
		t.Fatalf("set session_name: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_session", "true"); err != nil {
		t.Fatalf("set configured_named_session: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_mode", "on_demand"); err != nil {
		t.Fatalf("set configured_named_mode: %v", err)
	}

	persistCalled := false
	outcome := doHandoffWithOutcome(store, store, rec, dops, func() error {
		persistCalled = true
		return nil
	}, "mayor", "mayor", []string{"HANDOFF: context full"}, &stdout, &stderr)
	if outcome.code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", outcome.code, stderr.String())
	}
	if !outcome.restartRequested {
		t.Fatal("restartRequested = false, want true for on-demand named session (ga-cctcju)")
	}

	mailFound := false
	all := listOpenMessagesBothTiers(t, store)
	for _, got := range all {
		if got.Title == "HANDOFF: context full" && got.Type == "message" {
			mailFound = true
			break
		}
	}
	if !mailFound {
		t.Fatalf("handoff mail not created; beads=%v", all)
	}
	if !dops.restartRequested["mayor"] {
		t.Error("restart-requested flag not set for on-demand named session")
	}
	if !persistCalled {
		t.Error("persistRestart was not called for on-demand named session")
	}
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("fetching seeded bead: %v", err)
	}
	if got := refreshed.Metadata["wake_request"]; got != string(session.WakeCauseExplicit) {
		t.Errorf("bead wake_request = %q, want %q (comeback fence must be armed)", got, session.WakeCauseExplicit)
	}
	if refreshed.Metadata["wake_requested_at"] == "" {
		t.Error("bead wake_requested_at empty, want a timestamp")
	}
	if !strings.Contains(stdout.String(), "explicit wake armed") {
		t.Errorf("stdout = %q, want the on-demand cycle contract named", stdout.String())
	}
	if len(rec.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.Events))
	}
	if rec.Events[1].Type != events.SessionDraining {
		t.Fatalf("event[1].Type = %q, want %q", rec.Events[1].Type, events.SessionDraining)
	}
}

// TestDoHandoff_OnDemandNamedSessionWakeArmFailureRefusesRestart pins the
// fence ordering: when the explicit wake cannot be durably persisted, the
// restart must NOT be requested (a kill without the marker leaves the seat
// down until unrelated work is assigned to it), the failure must be loud,
// and the manual remedy must be named.
func TestDoHandoff_OnDemandNamedSessionWakeArmFailureRefusesRestart(t *testing.T) {
	mem := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	b, err := mem.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	for k, v := range map[string]string{
		"session_name":             "mayor",
		"configured_named_session": "true",
		"configured_named_mode":    "on_demand",
	} {
		if err := mem.SetMetadata(b.ID, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	store := &wakeArmFailStore{Store: mem, failID: b.ID}

	persistCalled := false
	outcome := doHandoffWithOutcome(store, store, rec, dops, func() error {
		persistCalled = true
		return nil
	}, "mayor", "mayor", []string{"HANDOFF: context full"}, &stdout, &stderr)
	if outcome.code != 1 {
		t.Fatalf("code = %d, want 1; stderr: %s", outcome.code, stderr.String())
	}
	if outcome.restartRequested {
		t.Fatal("restartRequested = true, want false when the comeback wake cannot be armed")
	}
	if dops.restartRequested["mayor"] {
		t.Error("restart-requested flag set despite wake-arm failure; the kill must not be armed")
	}
	if persistCalled {
		t.Error("persistRestart called despite wake-arm failure")
	}
	if !strings.Contains(stderr.String(), "cannot arm its comeback wake") {
		t.Errorf("stderr = %q, want the wake-arm failure named", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc session wake mayor") {
		t.Errorf("stderr = %q, want the manual remedy named", stderr.String())
	}
}

// wakeArmFailStore fails metadata batch writes to one bead (the session
// bead), which is the write chokepoint armExplicitWakeForRestart persists the
// wake marker through; mail-bead writes pass through untouched.
type wakeArmFailStore struct {
	beads.Store
	failID string
}

func (s *wakeArmFailStore) SetMetadataBatch(id string, meta map[string]string) error {
	if id == s.failID {
		return errors.New("metadata write refused")
	}
	return s.Store.SetMetadataBatch(id, meta)
}

// TestDoHandoff_OnDemandNamedSessionRestartFlagFailureIsLoud pins the fence
// ordering from the other side: the wake marker is armed BEFORE the runtime
// restart flag, so when the flag write fails the wake is already durable and
// the command fails loudly without promising a restart.
func TestDoHandoff_OnDemandNamedSessionRestartFlagFailureIsLoud(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	dops.err = errors.New("tmux borked")
	var stdout, stderr bytes.Buffer

	b, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, "session_name", "mayor"); err != nil {
		t.Fatalf("set session_name: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_session", "true"); err != nil {
		t.Fatalf("set configured_named_session: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_mode", "on_demand"); err != nil {
		t.Fatalf("set configured_named_mode: %v", err)
	}

	outcome := doHandoffWithOutcome(store, store, rec, dops, nil, "mayor", "mayor",
		[]string{"HANDOFF: context full"}, &stdout, &stderr)
	if outcome.code != 1 {
		t.Fatalf("code = %d, want 1", outcome.code)
	}
	if outcome.restartRequested {
		t.Fatal("restartRequested = true, want false")
	}
	if !strings.Contains(stderr.String(), "setting restart flag") {
		t.Fatalf("stderr = %q, want the restart-flag failure named", stderr.String())
	}
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("fetching seeded bead: %v", err)
	}
	if got := refreshed.Metadata["wake_request"]; got != string(session.WakeCauseExplicit) {
		t.Errorf("bead wake_request = %q, want %q (wake arms before the restart flag)", got, session.WakeCauseExplicit)
	}
	if strings.Contains(stdout.String(), "requesting restart") {
		t.Fatalf("stdout = %q, must not promise a restart the flag write refused", stdout.String())
	}
}

func TestDoHandoff_NamedAlwaysSessionRequestsRestart(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	b, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, "session_name", "mayor"); err != nil {
		t.Fatalf("set session_name: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_session", "true"); err != nil {
		t.Fatalf("set configured_named_session: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_mode", "always"); err != nil {
		t.Fatalf("set configured_named_mode: %v", err)
	}

	persistCalled := false
	outcome := doHandoffWithOutcome(store, store, rec, dops, func() error {
		persistCalled = true
		return nil
	}, "mayor", "mayor", []string{"HANDOFF: context full"}, &stdout, &stderr)
	if outcome.code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", outcome.code, stderr.String())
	}
	if !outcome.restartRequested {
		t.Fatal("restartRequested = false, want true for always-mode named session")
	}
	if !dops.restartRequested["mayor"] {
		t.Error("restart-requested flag not set for always-mode named session")
	}
	if !persistCalled {
		t.Error("persistRestart was not called for always-mode named session")
	}
	if len(rec.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.Events))
	}
	if rec.Events[1].Type != events.SessionDraining {
		t.Fatalf("event[1].Type = %q, want %q", rec.Events[1].Type, events.SessionDraining)
	}
}

// TestDoHandoff_PinnedAlwaysSessionRequiresPersistRestart covers Fix 1: a
// pinned named session (pin_awake == "true") is kill-protected by the
// reconciler unless an explicit controller reset is persisted, so
// persistRestart is mandatory rather than best-effort for pinned sessions.
// When it is unavailable or fails, doHandoffWithOutcome must report failure
// and must not promise a restart it cannot guarantee.
func TestDoHandoff_PinnedAlwaysSessionRequiresPersistRestart(t *testing.T) {
	for _, tc := range []struct {
		name           string
		persistRestart func() error
	}{
		{name: "nil persistRestart"},
		{name: "persistRestart error", persistRestart: func() error { return errors.New("worker boundary unavailable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			rec := events.NewFake()
			dops := newFakeDrainOps()
			var stdout, stderr bytes.Buffer

			b, err := store.Create(beads.Bead{
				Type:   sessionBeadType,
				Labels: []string{"gc:session"},
			})
			if err != nil {
				t.Fatalf("seeding session bead: %v", err)
			}
			if err := store.SetMetadata(b.ID, "session_name", "mayor"); err != nil {
				t.Fatalf("set session_name: %v", err)
			}
			if err := store.SetMetadata(b.ID, "configured_named_session", "true"); err != nil {
				t.Fatalf("set configured_named_session: %v", err)
			}
			if err := store.SetMetadata(b.ID, "configured_named_mode", "always"); err != nil {
				t.Fatalf("set configured_named_mode: %v", err)
			}
			if err := store.SetMetadata(b.ID, "pin_awake", "true"); err != nil {
				t.Fatalf("set pin_awake: %v", err)
			}

			outcome := doHandoffWithOutcome(store, store, rec, dops, tc.persistRestart, "mayor", "mayor",
				[]string{"HANDOFF: context full"}, &stdout, &stderr)
			if outcome.code != 1 {
				t.Fatalf("code = %d, want 1; stderr: %s", outcome.code, stderr.String())
			}
			if outcome.restartRequested {
				t.Fatal("restartRequested = true, want false when persistRestart is unavailable for a pinned session")
			}
			if strings.Contains(stdout.String(), "requesting restart") {
				t.Errorf("stdout = %q, must not promise a restart when persistRestart is unavailable for a pinned session", stdout.String())
			}
			if len(rec.Events) != 1 {
				t.Fatalf("got %d events, want 1 (mail only, no SessionDraining); events=%v", len(rec.Events), rec.Events)
			}
			if rec.Events[0].Type != events.MailSent {
				t.Fatalf("event[0].Type = %q, want %q", rec.Events[0].Type, events.MailSent)
			}
		})
	}
}

func TestHandoffWithMessage(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	code := doHandoff(store, store, rec, dops, nil, "polecat-1", "gc-city-polecat-1",
		[]string{"HANDOFF: PR review needed", "PR #42 is open, tests passing, needs review from refinery"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	all := listOpenMessagesBothTiers(t, store)
	if len(all) != 1 {
		t.Fatalf("got %d beads, want 1", len(all))
	}
	b := all[0]
	if b.Description != "PR #42 is open, tests passing, needs review from refinery" {
		t.Errorf("Description = %q, want body text", b.Description)
	}
}

// TestCmdHandoff_OnDemandNamedSessionArmsWakeWithoutHanging is the cmd-level
// guard descended from the Regression744 non-blocking test. An on-demand
// named self-handoff now arms the comeback wake and requests a restart
// (ga-cctcju); with no live runtime session backing the name the restart-flag
// write fails fast and loud. Either way the command must return promptly —
// never hang — and the wake marker must already be durable on the session
// bead (the fence arms before the flag).
func TestCmdHandoff_OnDemandNamedSessionArmsWakeWithoutHanging(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_NAME", "mayor")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	b, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, "session_name", "mayor"); err != nil {
		t.Fatalf("set session_name: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_session", "true"); err != nil {
		t.Fatalf("set configured_named_session: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_mode", "on_demand"); err != nil {
		t.Fatalf("set configured_named_mode: %v", err)
	}

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cmdHandoff([]string{"HANDOFF: context full"}, "", false, "", &stdout, &stderr)
	}()

	var code int
	select {
	case code = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cmdHandoff blocked for named on-demand session")
	}
	if strings.Contains(stdout.String(), "restart skipped") {
		t.Fatalf("stdout = %q, the silent skip must be gone (ga-cctcju)", stdout.String())
	}
	// No runtime session backs "mayor" here, so the restart arm is expected
	// to fail loudly after the wake was armed; a provider that accepts the
	// flag write instead reports the restart. Both are legal — silence is not.
	if code != 0 && strings.TrimSpace(stderr.String()) == "" {
		t.Fatalf("code = %d with empty stderr, want a loud failure", code)
	}
	if code == 0 && !strings.Contains(stdout.String(), "requesting restart") {
		t.Fatalf("stdout = %q, want the restart promised on success", stdout.String())
	}
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("fetching seeded bead: %v", err)
	}
	if got := refreshed.Metadata["wake_request"]; got != string(session.WakeCauseExplicit) {
		t.Errorf("bead wake_request = %q, want %q (fence arms before the flag)", got, session.WakeCauseExplicit)
	}
}

func TestHandoffMissingSubject(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer

	// Cobra enforces RangeArgs(1, 2), so doHandoff won't be called with 0 args.
	// Test at the cobra level.
	cmd := newHandoffCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("handoff with no args should fail")
	}

	// Verify no side effects.
	all := listOpenBeadsBothTiers(t, store)
	if len(all) != 0 {
		t.Errorf("got %d beads, want 0", len(all))
	}
	if len(rec.Events) != 0 {
		t.Errorf("got %d events, want 0", len(rec.Events))
	}
	if len(dops.restartRequested) != 0 {
		t.Error("restart-requested should not be set")
	}
}

func TestHandoffNotInSessionContext(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newHandoffCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_CITY", "")
	cmd.SetArgs([]string{"HANDOFF: test"})
	err := cmd.Execute()
	if err == nil {
		t.Error("handoff without session context should fail")
	}
	if !strings.Contains(stderr.String(), "not in session context") {
		t.Errorf("stderr = %q, want 'not in session context' error", stderr.String())
	}
}

func TestHandoffRemoteRunning(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	sp := runtime.NewFake()
	// Start the target session.
	if err := sp.Start(context.Background(), "deacon", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := doHandoffRemote(store, store, rec, sp, "deacon", "deacon", "mayor",
		[]string{"Context refresh", "Check beads for current state"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Verify mail sent to target.
	all := listOpenMessagesBothTiers(t, store)
	if len(all) != 1 {
		t.Fatalf("got %d beads, want 1", len(all))
	}
	b := all[0]
	if b.Assignee != "deacon" {
		t.Errorf("Assignee = %q, want %q", b.Assignee, "deacon")
	}
	if b.From != "mayor" {
		t.Errorf("From = %q, want %q", b.From, "mayor")
	}
	if b.Description != "Check beads for current state" {
		t.Errorf("Description = %q, want body text", b.Description)
	}

	// Verify session killed.
	if sp.IsRunning("deacon") {
		t.Error("target session should be stopped")
	}

	// Verify events: MailSent + SessionStopped.
	if len(rec.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.Events))
	}
	if rec.Events[0].Type != events.MailSent {
		t.Errorf("event[0].Type = %q, want %q", rec.Events[0].Type, events.MailSent)
	}
	if rec.Events[1].Type != events.SessionStopped {
		t.Errorf("event[1].Type = %q, want %q", rec.Events[1].Type, events.SessionStopped)
	}

	// Verify stdout says killed.
	if !strings.Contains(stdout.String(), "killed session") {
		t.Errorf("stdout = %q, want 'killed session'", stdout.String())
	}
}

// TestHandoffRemoteNamedOnDemandArmsWakeAndKills supersedes the skip-kill
// test (ga-cctcju): a remote handoff to an on-demand named target now arms
// the durable explicit wake on the session bead FIRST, then kills the target
// so the reconciler restarts it fresh with the handoff mail waiting.
func TestHandoffRemoteNamedOnDemandArmsWakeAndKills(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	b, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, "session_name", "mayor"); err != nil {
		t.Fatalf("set session_name: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_session", "true"); err != nil {
		t.Fatalf("set configured_named_session: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_mode", "on_demand"); err != nil {
		t.Fatalf("set configured_named_mode: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doHandoffRemote(store, store, rec, sp, "mayor", "mayor", "deacon",
		[]string{"Context refresh", "Please pick this up manually"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if sp.IsRunning("mayor") {
		t.Error("named on-demand target should have been killed (ga-cctcju)")
	}
	if len(rec.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.Events))
	}
	if rec.Events[0].Type != events.MailSent {
		t.Fatalf("event[0].Type = %q, want %q", rec.Events[0].Type, events.MailSent)
	}
	if rec.Events[1].Type != events.SessionStopped {
		t.Fatalf("event[1].Type = %q, want %q", rec.Events[1].Type, events.SessionStopped)
	}
	if !strings.Contains(stdout.String(), "killed session") {
		t.Errorf("stdout = %q, want the kill reported", stdout.String())
	}
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("fetching seeded bead: %v", err)
	}
	if got := refreshed.Metadata["wake_request"]; got != string(session.WakeCauseExplicit) {
		t.Errorf("bead wake_request = %q, want %q (comeback fence must be armed before the kill)", got, session.WakeCauseExplicit)
	}
}

// TestHandoffRemoteNamedOnDemandWakeArmFailureLeavesTargetRunning pins the
// remote-side fence: when the wake marker cannot be persisted the target must
// be left running (a kill without the marker has no respawn path) and the
// failure must be loud with the manual remedy named.
func TestHandoffRemoteNamedOnDemandWakeArmFailureLeavesTargetRunning(t *testing.T) {
	mem := beads.NewMemStore()
	rec := events.NewFake()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	b, err := mem.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	for k, v := range map[string]string{
		"session_name":             "mayor",
		"configured_named_session": "true",
		"configured_named_mode":    "on_demand",
	} {
		if err := mem.SetMetadata(b.ID, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	store := &wakeArmFailStore{Store: mem, failID: b.ID}

	var stdout, stderr bytes.Buffer
	code := doHandoffRemote(store, store, rec, sp, "mayor", "mayor", "deacon",
		[]string{"Context refresh"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !sp.IsRunning("mayor") {
		t.Error("target must be left running when the comeback wake cannot be armed")
	}
	if !strings.Contains(stderr.String(), "cannot arm its comeback wake") {
		t.Errorf("stderr = %q, want the wake-arm failure named", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc session wake mayor") {
		t.Errorf("stderr = %q, want the manual remedy named", stderr.String())
	}
}

func TestHandoffRemoteNotRunning(t *testing.T) {
	store := beads.NewMemStore()
	rec := events.NewFake()
	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	code := doHandoffRemote(store, store, rec, sp, "deacon", "deacon", "human",
		[]string{"Please check on PR #42"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Mail still sent even if session not running.
	all := listOpenMessagesBothTiers(t, store)
	if len(all) != 1 {
		t.Fatalf("got %d beads, want 1", len(all))
	}

	// Only MailSent event (no SessionStopped since not running).
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}

	// Stdout mentions not running.
	if !strings.Contains(stdout.String(), "not running") {
		t.Errorf("stdout = %q, want 'not running' mention", stdout.String())
	}
}

func TestCmdHandoffRemoteDefaultSenderFallsBackToGCAliasWhenSessionIDMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_MAIL", "")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)

	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	senderBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "sender",
			"session_name": "sender-gc-42",
		},
	})
	if err != nil {
		t.Fatalf("Create sender: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "recipient",
			"session_name": "recipient-gc-42",
		},
	}); err != nil {
		t.Fatalf("Create recipient: %v", err)
	}

	t.Setenv("GC_SESSION_ID", "gc-does-not-match")
	t.Setenv("GC_ALIAS", "sender")
	_ = os.Unsetenv("GC_AGENT")

	var stdout, stderr bytes.Buffer
	code := cmdHandoffRemote([]string{"Context refresh", "Check current state"}, "recipient", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHandoffRemote() = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	storeAfter, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt after handoff: %v", err)
	}
	all := listOpenMessagesBothTiers(t, storeAfter)
	var msg beads.Bead
	found := false
	for _, b := range all {
		if b.Type == "message" {
			msg = b
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("message bead not found; beads=%#v", all)
	}
	if msg.From != "sender" {
		t.Fatalf("message From = %q, want sender", msg.From)
	}
	if msg.Metadata["mail.from_session_id"] != senderBead.ID {
		t.Fatalf("mail.from_session_id = %q, want %q", msg.Metadata["mail.from_session_id"], senderBead.ID)
	}
	if msg.Metadata["mail.from_display"] != "sender" {
		t.Fatalf("mail.from_display = %q, want sender", msg.Metadata["mail.from_display"])
	}
	if msg.Assignee != "recipient" {
		t.Fatalf("message Assignee = %q, want recipient", msg.Assignee)
	}
}
