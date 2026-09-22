package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/notify"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestLiveNudgeIsFindableByTargetAfterwards is ga-qbc7d2's acceptance check,
// aimed at a KNOWN POSITIVE: a nudge delivered live must be findable in the
// event log by target afterwards. Before this, only queued nudges left a
// record, so a negative answer to "was anything injected into X at T" could
// not tell "nothing" from "a live nudge that left no trace".
func TestLiveNudgeIsFindableByTargetAfterwards(t *testing.T) {
	const text = "check deploy status"
	for _, tc := range []struct {
		name        string
		nudgeErr    error
		wantCode    int
		wantOutcome string
	}{
		{name: "delivered", wantCode: 0, wantOutcome: liveNudgeDelivered},
		// The provider error QUOTES the nudge text, as herdr's does: the record
		// must carry the failure, never the text inside it.
		{name: "failed attempt is recorded too", nudgeErr: errors.New("agent prompt [check deploy status]: exit 1"), wantCode: 1, wantOutcome: liveNudgeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_AGENT", "woodhouse")
			t.Setenv("GC_SESSION_ID", "ga-sender-session")
			dir := t.TempDir()
			store := openNudgeBeadStore(dir)
			fake := runtime.NewFake()
			mgr := newSessionManagerWithConfig(dir, store, fake, nil)
			info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if tc.nudgeErr != nil {
				fake.NudgeErrors = map[string]error{info.SessionName: tc.nudgeErr}
			}

			rec := events.NewFake()
			orig := liveNudgeRecorder
			liveNudgeRecorder = func(nudgeTarget) events.Recorder { return rec }
			t.Cleanup(func() { liveNudgeRecorder = orig })

			target := nudgeTarget{cityPath: dir, sessionID: info.ID, sessionName: info.SessionName}
			var stdout, stderr bytes.Buffer
			code := deliverSessionNudgeWithWorker(target, store, fake, text, nudgeDeliveryImmediate, false, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d; stderr: %s", code, tc.wantCode, stderr.String())
			}

			var found []events.SessionNudgedPayload
			for _, ev := range rec.Events {
				if ev.Type != events.SessionNudged || ev.Subject != target.agentKey() {
					continue
				}
				if ev.SessionID != info.ID {
					t.Errorf("event SessionID = %q, want %q", ev.SessionID, info.ID)
				}
				if ev.Ts.IsZero() {
					t.Error("event has no timestamp; it must be findable by time")
				}
				var p events.SessionNudgedPayload
				if err := json.Unmarshal(ev.Payload, &p); err != nil {
					t.Fatalf("decode payload: %v", err)
				}
				if strings.Contains(string(ev.Payload), text) || strings.Contains(ev.Message, text) {
					t.Fatalf("the nudge text leaked into the event: %s / %q", ev.Payload, ev.Message)
				}
				found = append(found, p)
			}
			if len(found) != 1 {
				t.Fatalf("session.nudged events for %s = %d, want exactly 1 (all events: %+v)", target.agentKey(), len(found), rec.Events)
			}
			p := found[0]
			sum := sha256.Sum256([]byte(text))
			if p.Outcome != tc.wantOutcome || p.Delivery != string(nudgeDeliveryImmediate) {
				t.Errorf("outcome/delivery = %q/%q, want %q/%q", p.Outcome, p.Delivery, tc.wantOutcome, nudgeDeliveryImmediate)
			}
			if p.TextSHA256 != hex.EncodeToString(sum[:]) || p.TextBytes != len(text) {
				t.Errorf("text fingerprint = %s/%d, want the SHA-256 and length of the sent text", p.TextSHA256, p.TextBytes)
			}
			if p.Sender != "woodhouse" || p.SenderSession != "ga-sender-session" {
				t.Errorf("sender = %q/%q, want the self-reported woodhouse/ga-sender-session", p.Sender, p.SenderSession)
			}
			if tc.nudgeErr != nil && p.ErrorClass != "error" {
				t.Errorf("error_class = %q, want %q", p.ErrorClass, "error")
			}
		})
	}
}

// TestLiveNotificationAttemptIsRecorded covers the second live-injection path:
// a mail --notify wake tried LIVE into a running session. Every live attempt
// must leave exactly one record, and its outcome must agree with what the
// caller was told: delivered <-> OutcomeDelivered, anything else is an attempt
// that fell through to the queue. (With the fake provider this path lands as
// not_delivered; the delivered branch is the same one-line call as the direct
// path's, which TestLiveNudgeIsFindableByTargetAfterwards exercises.)
func TestLiveNotificationAttemptIsRecorded(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := events.NewFake()
	orig := liveNudgeRecorder
	liveNudgeRecorder = func(nudgeTarget) events.Recorder { return rec }
	t.Cleanup(func() { liveNudgeRecorder = orig })

	target := nudgeTarget{cityPath: dir, sessionID: info.ID, sessionName: info.SessionName}
	outcome, err := deliverSessionNotification(target, store, fake, notify.Notification{
		Kind: notify.KindMailArrival, Recipient: "worker", Sender: "katya", Summary: "new mail",
	})
	if err != nil {
		t.Fatalf("deliverSessionNotification: %v", err)
	}
	var got []events.SessionNudgedPayload
	for _, ev := range rec.Events {
		if ev.Type != events.SessionNudged || ev.Subject != target.agentKey() {
			continue
		}
		var p events.SessionNudgedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		got = append(got, p)
	}
	if len(got) != 1 {
		t.Fatalf("session.nudged events = %d, want exactly 1 for one live attempt (outcome %q)", len(got), outcome)
	}
	if got[0].Delivery != string(nudgeDeliveryWaitIdle) || got[0].Source == "" {
		t.Errorf("delivery/source = %q/%q, want %q and a non-empty source", got[0].Delivery, got[0].Source, nudgeDeliveryWaitIdle)
	}
	if (outcome == notify.OutcomeDelivered) != (got[0].Outcome == liveNudgeDelivered) {
		t.Fatalf("recorded outcome %q disagrees with the caller's outcome %q", got[0].Outcome, outcome)
	}
}

// TestOpenLiveNudgeRecorderUsesConfiguredProvider pins Codex #111 r2: a city
// whose [events].provider (or GC_EVENTS) selects a non-file backend must get
// its session.nudged records there, where the controller and API read, not in
// a .gc/events.jsonl nobody consults.
func TestOpenLiveNudgeRecorderUsesConfiguredProvider(t *testing.T) {
	dir := t.TempDir()
	rec := openLiveNudgeRecorder(dir, config.EventsConfig{Provider: "fake"})
	if _, ok := rec.(*events.Fake); !ok {
		t.Fatalf("recorder = %T, want *events.Fake for provider \"fake\"", rec)
	}
	if _, err := os.Stat(filepath.Join(dir, citylayout.RuntimeRoot, "events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("events.jsonl stat err = %v, want not-exist: a non-file provider must not touch the file log", err)
	}
}

// TestOpenLiveNudgeRecorderIsTransient pins Codex #111 r2: the per-nudge file
// recorder must neither sweep orphaned rotating-* files nor rotate the live log.
// Both belong to the supervisor's long-lived recorder, and a per-nudge copy of
// either races its compressor through the shared .gz.tmp path.
func TestOpenLiveNudgeRecorderIsTransient(t *testing.T) {
	t.Setenv("GC_EVENTS_ROTATION_MAX_SIZE_BYTES", "1")
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, citylayout.RuntimeRoot)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(runtimeDir, "events.jsonl")
	if err := os.WriteFile(live, []byte(`{"seq":1,"type":"seed","ts":"2026-09-22T00:00:00Z","actor":"t"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(runtimeDir, "events.jsonl.rotating-20260922T000000Z")
	if err := os.WriteFile(orphan, []byte(`{"seq":0,"type":"old","ts":"2026-09-21T00:00:00Z","actor":"t"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := openLiveNudgeRecorder(dir, config.EventsConfig{})
	rec.Record(events.Event{Type: events.SessionNudged, Actor: "t", Subject: "worker"})
	if closer, ok := rec.(io.Closer); ok {
		_ = closer.Close()
	}

	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("orphaned rotating file was swept by a per-nudge recorder: %v", err)
	}
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".gz") || strings.HasSuffix(e.Name(), ".gz.tmp") {
			t.Fatalf("per-nudge recorder rotated or compressed the log: found %s", e.Name())
		}
	}
	data, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), string(events.SessionNudged)) {
		t.Fatalf("live log does not hold the nudge event; the recorder rotated it away or wrote elsewhere:\n%s", data)
	}
}
