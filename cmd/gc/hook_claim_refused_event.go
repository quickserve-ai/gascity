package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// hookEmitClaimRefused is the emitter seam, replaced in tests.
var hookEmitClaimRefused = emitHookClaimRefused

// hookClaimRefusedRecorder opens the event log for one refusal. A seam so a test
// can observe the events.Discard fallback directly.
var hookClaimRefusedRecorder = openHookClaimRefusedRecorder

// hookClaimTokenFingerprintHexLen is how much of a token's SHA-256 a
// hook.claim.refused payload may carry: 8 hex characters (32 bits).
const hookClaimTokenFingerprintHexLen = 8

// emitHookClaimRefused records a hook.claim.refused event for an identity
// refusal of gc hook --claim (ga-cwu447).
//
// Before this event the two identity refusals — stale_session and
// missing_session_registration — wrote their drain result to the seat's own
// stdout and nowhere else. That record dies with the pane, so a pool seat that
// refused and stopped on every spawn left nothing countable: a correct refusal
// read, from outside, exactly like a seat that found no work, and a claim that
// pool seats wedge on an instance-token / runtime-epoch mismatch could be
// neither confirmed nor refuted.
//
// The caller records BEFORE it writes the drain. The drain's --drain-ack pokes
// the controller to stop this session immediately, so recording after it would
// race the teardown of the very seat whose refusal the event exists to keep.
// Recording first cannot change the refusal: the verdict is already decided,
// nothing here writes to stdout or stderr, and nothing is returned.
//
// Best-effort and silent on every failure, like the other hook emitters: a city
// whose event log cannot be opened gets events.Discard, and a marshal failure
// records nothing. The drain that follows is identical either way.
func emitHookClaimRefused(cityPath, message string, payload events.HookClaimRefusedPayload) {
	if strings.TrimSpace(cityPath) == "" {
		// An empty city path would resolve .gc/events.jsonl against the working
		// directory, which is not a city.
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	rec := hookClaimRefusedRecorder(cityPath)
	if closer, ok := rec.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // best-effort event recorder cleanup
	}
	subject := payload.Template
	if subject == "" {
		subject = payload.SessionID
	}
	rec.Record(events.Event{
		Type:      events.HookClaimRefused,
		Actor:     hookClaimRefusedActor(payload.Template),
		Subject:   subject,
		Message:   message,
		Payload:   body,
		SessionID: payload.SessionID,
	})
}

// openHookClaimRefusedRecorder opens the city's event log the way a transient,
// per-invocation writer must — the pattern class_store_emit.go established —
// rather than through openCityRecorderAt, which is built for a long-lived
// command's recorder.
//
//   - events.WithoutStartupSweep: the orphaned-rotating-file sweep and the
//     NUL-tail repair belong to the supervisor's long-lived recorder. Run from a
//     hook they would race it mid-rotation, under a lock, in front of a drain
//     whose --drain-ack is time-sensitive.
//   - no WithMaxSize, and no city config load: size-triggered rotation stays
//     disabled, so this process never rotates the live log and Close never waits
//     on a background gzip. Rotation remains the supervisor's job.
//
// Any open failure falls back to events.Discard.
func openHookClaimRefusedRecorder(cityPath string) events.Recorder {
	rec, err := events.NewFileRecorder(
		filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl"),
		io.Discard,
		events.WithoutStartupSweep(),
	)
	if err != nil {
		return events.Discard
	}
	return rec
}

// hookClaimRefusedActor is eventActor() for a refusal, except that it never
// credits the refusal to the operator. eventActor falls back to "human" when
// GC_ALIAS, GC_AGENT, GC_SESSION_ID and BEADS_ACTOR are all empty — and a
// missing-registration refusal fires precisely when GC_SESSION_ID is empty — so
// that fallback is replaced with the refusing pool runtime's own name.
func hookClaimRefusedActor(template string) string {
	actor := eventActor()
	if actor != "human" {
		return actor
	}
	if template = strings.TrimSpace(template); template != "" {
		return "gc-hook:" + template
	}
	return "gc-hook"
}

// hookClaimRuntimeGeneration renders a session bead's raw generation metadata
// exactly as a session start renders it into GC_RUNTIME_EPOCH: the parsed
// integer, with an empty, zero, negative or unparseable value started as
// session.DefaultGeneration (session_lifecycle_parallel.go and
// internal/session/chat.go both parse the untrimmed value). bead_epoch uses it so
// the two fields compare as written.
func hookClaimRuntimeGeneration(raw string) string {
	generation, err := strconv.Atoi(raw)
	if err != nil || generation <= 0 {
		generation = session.DefaultGeneration
	}
	return strconv.Itoa(generation)
}

// hookClaimRefusedPayload assembles the hook.claim.refused payload from what the
// identity fence already holds: the refusal reason, the classifier's detail
// (bead-derived), and the refusing runtime's own identity environment.
//
// runtimeToken is the runtime's GC_INSTANCE_TOKEN as the fence compared it. It
// is a credential and is reduced to a fingerprint here; it must never be copied
// into the payload any other way.
func hookClaimRefusedPayload(reason, sessionID, runtimeToken string, detail hookClaimStaleDetail) events.HookClaimRefusedPayload {
	payload := events.HookClaimRefusedPayload{
		Reason:                  reason,
		Detail:                  detail.Detail,
		SessionID:               strings.TrimSpace(sessionID),
		SessionName:             strings.TrimSpace(os.Getenv("GC_SESSION_NAME")),
		Template:                strings.TrimSpace(os.Getenv("GC_TEMPLATE")),
		Agent:                   hookClaimRefusedAgent(),
		RuntimeTokenFingerprint: hookClaimTokenFingerprint(runtimeToken),
		RuntimeEpoch:            strings.TrimSpace(os.Getenv("GC_RUNTIME_EPOCH")),
	}
	if detail.BeadRead {
		matched := detail.TokenMatched
		payload.TokenMatched = &matched
		payload.State = detail.State
		payload.BeadTokenFingerprint = detail.BeadTokenFingerprint
		payload.BeadEpoch = detail.BeadEpoch
	}
	return payload
}

// hookClaimRefusedAgent names the refusing runtime's agent identity: GC_ALIAS,
// else GC_AGENT.
func hookClaimRefusedAgent() string {
	if alias := strings.TrimSpace(os.Getenv("GC_ALIAS")); alias != "" {
		return alias
	}
	return strings.TrimSpace(os.Getenv("GC_AGENT"))
}

// hookClaimTokenFingerprint returns a short one-way fingerprint of an instance
// token — the first 8 hex characters of its SHA-256 — or "" for an empty token.
// Two refusals with equal fingerprints involve the same incarnation's token; the
// fingerprint cannot be turned back into the token or presented in its place.
func hookClaimTokenFingerprint(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:hookClaimTokenFingerprintHexLen]
}
