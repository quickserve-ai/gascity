package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Live-nudge outcomes recorded on session.nudged. "not_delivered" is a live
// attempt that did not land (wait-idle found no idle boundary) and fell back
// to the queue; the queued wisp records what happens next.
const (
	liveNudgeDelivered    = "delivered"
	liveNudgeFailed       = "failed"
	liveNudgeNotDelivered = "not_delivered"
)

// liveNudgeRecorder opens the recorder a live-nudge event is written to. It is
// a variable so tests can observe the event without a real city. The default
// reads the target's already-loaded config: re-loading city config
// (openCityRecorderAt) on every nudge would put a config parse on the hot path
// of the most frequent command in the fleet.
var liveNudgeRecorder = func(target nudgeTarget) events.Recorder {
	if target.cityPath == "" {
		return events.Discard
	}
	eventsCfg := config.EventsConfig{}
	if target.cfg != nil {
		eventsCfg = target.cfg.Events
	}
	if v := os.Getenv("GC_EVENTS"); v != "" {
		eventsCfg.Provider = v
	}
	return openLiveNudgeRecorder(target.cityPath, eventsCfg)
}

// openLiveNudgeRecorder writes where the rest of the city reads (Codex #111 r2).
//
//   - A configured non-file provider (exec:, fake, ...) is used as configured.
//     Writing to .gc/events.jsonl regardless left every session.nudged record
//     invisible to a controller and API reading another backend.
//   - The default file backend is opened the way a TRANSIENT, per-invocation
//     writer must (the pattern of class_store_emit.go and
//     hook_claim_refused_event.go): events.WithoutStartupSweep, and no size
//     rotation. A fully configured recorder per nudge could rotate the live log
//     on its first Record and sweep rotating-* files, racing the supervisor's
//     compressor through the shared .gz.tmp path. Rotation stays the
//     supervisor's job.
//
// Any open failure falls back to events.Discard: the audit record is
// best-effort and must never fail the nudge.
func openLiveNudgeRecorder(cityPath string, eventsCfg config.EventsConfig) events.Recorder {
	eventsPath := filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl")
	// Only the non-file backends go through the provider constructor: its
	// default branch builds a fully configured, rotating file recorder, which is
	// exactly what a transient writer must not open.
	if v := strings.TrimSpace(eventsCfg.Provider); strings.HasPrefix(v, "exec:") || v == "fake" || v == "fail" {
		prov, err := newEventsProviderForNameWithConfig(v, eventsPath, io.Discard, eventsCfg)
		if err != nil || prov == nil {
			return events.Discard
		}
		return prov
	}
	rec, err := events.NewFileRecorder(eventsPath, io.Discard, events.WithoutStartupSweep())
	if err != nil {
		return events.Discard
	}
	return rec
}

// recordLiveNudgeEvent writes one session.nudged event for a nudge that went
// through LIVE injection (ga-qbc7d2). Queued nudges are already recorded by
// their nudge:<id> wisp; before this, the live path, which is the ordinary one
// for an idle target, left no target-attributed trace at all.
//
// Best-effort by design: a nudge must never fail because its audit record did.
// The text itself is never written, only its length and SHA-256, so the log can
// confirm a KNOWN payload without holding it.
func recordLiveNudgeEvent(target nudgeTarget, text, delivery, source, outcome string, deliverErr error) {
	sum := sha256.Sum256([]byte(text))
	sender, senderSession := nudgeSenderIdentity()
	payload := events.SessionNudgedPayload{
		TargetAgent:   target.agentKey(),
		SessionID:     target.sessionID,
		SessionName:   target.sessionName,
		Delivery:      delivery,
		Outcome:       outcome,
		Sender:        sender,
		SenderSession: senderSession,
		Source:        source,
		TextBytes:     len(text),
		TextSHA256:    hex.EncodeToString(sum[:]),
	}
	if deliverErr != nil {
		payload.ErrorClass = liveNudgeErrorClass(deliverErr)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	actor := sender
	if actor == "" {
		actor = eventActor()
	}
	rec := liveNudgeRecorder(target)
	rec.Record(events.Event{
		Type:      events.SessionNudged,
		Ts:        time.Now().UTC(),
		Actor:     actor,
		Subject:   target.agentKey(),
		Message:   delivery + " " + outcome,
		Payload:   raw,
		SessionID: target.sessionID,
	})
	if closer, ok := rec.(io.Closer); ok {
		_ = closer.Close() //nolint:errcheck // best-effort audit record; the nudge outcome stands
	}
}

// liveNudgeErrorClass reduces a delivery error to a bounded code. The raw
// error is never recorded: the herdr provider, for one, formats its argv, the
// nudge text included, into returned errors (Codex #111), which would put the
// body the payload promises to omit into a widely readable log.
func liveNudgeErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case runtime.IsSessionGone(err):
		return "session_gone"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}
