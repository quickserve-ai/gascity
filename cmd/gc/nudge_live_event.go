package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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
// builds a file recorder from the target's already-loaded config: re-loading
// city config (openCityRecorderAt) on every nudge would put a config parse on
// the hot path of the most frequent command in the fleet.
var liveNudgeRecorder = func(target nudgeTarget) events.Recorder {
	if target.cityPath == "" {
		return events.Discard
	}
	eventsCfg := config.EventsConfig{}
	if target.cfg != nil {
		eventsCfg = target.cfg.Events
	}
	rec, err := newFileEventsRecorder(filepath.Join(target.cityPath, ".gc", "events.jsonl"), eventsCfg, io.Discard)
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
		payload.Error = deliverErr.Error()
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
