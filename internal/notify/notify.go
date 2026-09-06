// Package notify defines the notification plane: a typed wake event that
// sits ABOVE the provider boundary, with pluggable per-seat delivery
// transports. It is the shared foundation for mail-arrival wakes
// (ga-bjbaui), the operator-attention registry (ga-s0fn27), and desktop
// notifications — one contract, N transports, N consumers.
//
// Design: internal/runtime/claudemsg-bridge-design.md (v2, approved
// 2026-09-03). The two invariants every transport and consumer must hold:
//
//  1. A Notification is a WAKE HINT, never authority. The durable record
//     (mail bead, work bead) is written before the notification fires and
//     remains the only thing a recipient may act on.
//  2. The default "session" transport renders and delivers exactly what the
//     pre-plane code path delivered, byte for byte — existing seats see zero
//     behavioral difference from this abstraction existing.
package notify

import (
	"context"
	"fmt"
)

// Kind classifies what durable event produced the notification.
//
// The enum is deliberately small and may grow (ga-9nggf6 consultation
// pending before it freezes); consumers must treat unknown kinds as
// routine wake hints, never as errors.
type Kind string

const (
	// KindMailArrival fires after a mail bead is durably written.
	KindMailArrival Kind = "mail_arrival"
	// KindAttentionRequest fires when a seat blocks on operator input
	// (e.g. a pending AskUserQuestion). Consumed by the attention registry.
	KindAttentionRequest Kind = "attention_request"
	// KindProtocolSignal is an ephemeral coordination signal (MERGE_READY,
	// WORK_DONE, ...) whose durable truth lives on the underlying bead.
	KindProtocolSignal Kind = "protocol_signal"
	// KindOperatorAlert is an alarm-class event addressed to the operator
	// surface (desktop pop-up transport) rather than an agent seat.
	KindOperatorAlert Kind = "operator_alert"
)

// Urgency expresses how soon the recipient should see the hint. Transports
// may use it to choose delivery mode; it never changes the authority model.
type Urgency string

const (
	// UrgencyRoutine surfaces at the recipient's natural next boundary.
	UrgencyRoutine Urgency = "routine"
	// UrgencyDeadline should reach the recipient before their natural next
	// turn (the `gc mail send --notify` contract, ga-bjbaui doctrine).
	UrgencyDeadline Urgency = "deadline"
	// UrgencyInterrupt is interrupt-grade (`--delivery immediate` class).
	UrgencyInterrupt Urgency = "interrupt"
)

// Notification is one typed wake event. It carries references and claims,
// never instructions: Summary is display-only and the Ref names durable
// state the RECIPIENT can reach ("bead://<id>" for seats on the mail plane,
// an https URL for cloud seats whose reachable plane is GitHub).
type Notification struct {
	Kind Kind
	// Recipient is the immutable seat identity the plane routes on —
	// resolution to a live session/binding is the transport's job.
	Recipient string
	// Sender is the CLAIMED sender identity. Informational, not
	// authenticated (same trust class as nudgequeue.Item.Sender).
	Sender string
	// Ref points at the durable record this hint is about.
	Ref string
	// Summary is one render-safe line. NEVER instructions; transports
	// sanitize before interpolating into any prompt surface.
	Summary string
	Urgency Urgency
}

// Outcome is the typed result of one delivery attempt. Delivery is
// at-most-once: an ambiguous outcome is reported, never retried (a
// duplicate wake is worse than a late one — the durable plane holds the
// truth either way).
type Outcome string

const (
	// OutcomeDelivered means the hint reached a live recipient surface.
	OutcomeDelivered Outcome = "delivered"
	// OutcomeQueuedLocal means the hint was durably queued for a local
	// seat's next safe boundary (today's deferred-nudge queue).
	OutcomeQueuedLocal Outcome = "queued_local"
	// OutcomeQueuedRemote means a remote service accepted the hint;
	// acceptance is NOT delivery (the ga-bjbaui spike's headline finding).
	OutcomeQueuedRemote Outcome = "queued_remote"
	// OutcomeRefusedNotFound: the recipient binding does not resolve.
	OutcomeRefusedNotFound Outcome = "refused_not_found"
	// OutcomeRefusedArchived: the remote session exists but is closed.
	OutcomeRefusedArchived Outcome = "refused_archived"
	// OutcomeRefusedPolicy: the transport is disabled by org/account policy.
	OutcomeRefusedPolicy Outcome = "refused_policy"
	// OutcomeAmbiguous: the attempt may or may not have been accepted
	// (e.g. a timeout after the CLI could have queued remotely).
	OutcomeAmbiguous Outcome = "ambiguous"
)

// Transport delivers notifications for the seats bound to it. Selection is
// structural per-seat configuration resolved at load time — never a runtime
// capability probe.
type Transport interface {
	Deliver(ctx context.Context, n Notification) (Outcome, error)
}

// WakeText renders the recipient-facing hint line for a notification on the
// default session transport.
//
// For mail arrivals this MUST stay byte-identical to the pre-plane literal
// ("You have mail from <sender>", cmd/gc mail-notify path) — seats, tests,
// and operator eyes all know that string. Reference and summary content
// deliberately do NOT ride the wake line on the session transport: the
// recipient's own mail hooks surface the record itself.
func WakeText(n Notification) string {
	switch n.Kind {
	case KindMailArrival:
		return fmt.Sprintf("You have mail from %s", n.Sender)
	case KindAttentionRequest:
		return fmt.Sprintf("Attention requested by %s: see %s", n.Sender, n.Ref)
	default:
		if n.Ref != "" {
			return fmt.Sprintf("%s from %s: see %s", string(n.Kind), n.Sender, n.Ref)
		}
		return fmt.Sprintf("%s from %s", string(n.Kind), n.Sender)
	}
}
