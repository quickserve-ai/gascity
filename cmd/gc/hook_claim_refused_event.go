package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/events"
)

// hookEmitClaimRefused is the emitter seam, replaced in tests.
var hookEmitClaimRefused = emitHookClaimRefused

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
		// openCityRecorderAt("") would resolve .gc/events.jsonl against the
		// working directory, which is not a city.
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	rec := openCityRecorderAt(cityPath, io.Discard)
	if closer, ok := rec.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // best-effort event recorder cleanup
	}
	subject := payload.Template
	if subject == "" {
		subject = payload.SessionID
	}
	rec.Record(events.Event{
		Type:      events.HookClaimRefused,
		Actor:     eventActor(),
		Subject:   subject,
		Message:   message,
		Payload:   body,
		SessionID: payload.SessionID,
	})
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
