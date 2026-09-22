package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
		{name: "failed attempt is recorded too", nudgeErr: errors.New("pane gone"), wantCode: 1, wantOutcome: liveNudgeFailed},
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
			if tc.nudgeErr != nil && !strings.Contains(p.Error, tc.nudgeErr.Error()) {
				t.Errorf("error = %q, want it to carry %q", p.Error, tc.nudgeErr)
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
