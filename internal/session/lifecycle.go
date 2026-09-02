package session

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	// DefaultGeneration is the first runtime epoch for a newly created session.
	DefaultGeneration = 1

	// DefaultContinuationEpoch is the first conversation identity epoch.
	DefaultContinuationEpoch = 1
)

// NewInstanceToken returns a cryptographically random token for fencing
// drain/stop and async delivery against stale session incarnations.
func NewInstanceToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("session: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// RuntimeEnv returns the per-incarnation environment variables a live session
// runtime should receive from the controller/session manager.
func RuntimeEnv(sessionID, sessionName string, generation, continuationEpoch int, instanceToken string) map[string]string {
	env := map[string]string{
		"GC_SESSION_ID":         sessionID,
		"GC_SESSION_NAME":       sessionName,
		"GC_RUNTIME_EPOCH":      strconv.Itoa(generation),
		"GC_CONTINUATION_EPOCH": strconv.Itoa(continuationEpoch),
		"GC_INSTANCE_TOKEN":     instanceToken,
		// BEADS_HOLDER_TOKEN is the incarnation credential bd records on a claim
		// (ownership-fencing DESIGN §2.4). It IS the instance token — deriving it
		// here, the single authoritative wiring point, keeps the token a session
		// presents to bd identical to the one the controller persists, so a stale
		// incarnation cannot pass as the current owner. Set unconditionally (even
		// when empty, mirroring GC_INSTANCE_TOKEN) so a fresh incarnation never
		// inherits a parent's stale holder token.
		"BEADS_HOLDER_TOKEN": instanceToken,
		// BEADS_ACTOR is the audit identity bd stamps on every write this
		// session makes (issues.created_by, comments.author). bd resolves its
		// actor as $BEADS_ACTOR -> git user.name -> $USER, so a session that
		// reaches the runtime without it writes to the ledger under the repo's
		// git user -- a HUMAN.
		//
		// It belongs HERE, beside BEADS_HOLDER_TOKEN and for the same reason:
		// this is the one wiring point every start path passes through, so the
		// identity a session presents to bd cannot depend on which path started
		// it. The create-time template env seeds the same value
		// (cmd/gc/template_resolve.go), but the resume/restart resolver does
		// not: it rebuilds session env from the provider env plus the city
		// anchors only (cmd/gc/worker_handle.go). On 2026-08-17 the mayor was
		// the one session brought up by resume after a machine restart, ran for
		// ~12 hours with no BEADS_ACTOR, and recorded its rulings as "Cherub
		// Kumar" -- the Overseer (ga-xs28em). Set unconditionally (even when
		// empty, mirroring GC_INSTANCE_TOKEN) so a fresh incarnation never
		// inherits a parent's stale actor.
		"BEADS_ACTOR": sessionName,
	}
	return env
}

// RuntimeEnvWithAlias extends RuntimeEnv with the public session alias.
// Alias-aware commands use GC_ALIAS as their canonical mailbox/target
// identity; an explicit empty value clears stale template defaults.
func RuntimeEnvWithAlias(sessionID, sessionName, alias string, generation, continuationEpoch int, instanceToken string) map[string]string {
	env := RuntimeEnv(sessionID, sessionName, generation, continuationEpoch, instanceToken)
	env["GC_ALIAS"] = alias
	return env
}

// RuntimeEnvWithSessionContext extends RuntimeEnvWithAlias with the
// session-model context shared by controller, CLI, and API starts.
func RuntimeEnvWithSessionContext(sessionID, sessionName, alias, template, origin string, generation, continuationEpoch int, instanceToken string) map[string]string {
	env := RuntimeEnvWithAlias(sessionID, sessionName, alias, generation, continuationEpoch, instanceToken)
	if template != "" {
		env["GC_TEMPLATE"] = template
	}
	if origin != "" {
		env["GC_SESSION_ORIGIN"] = origin
	}
	if alias != "" {
		env["GC_AGENT"] = alias
		// The identity a session presents to bd must be the identity a claim
		// writes as assignee: hook --claim writes the alias-first identity
		// (ga-i44k), and bd's ownership guard compares the closer's
		// BEADS_ACTOR against it. Leaving the base RuntimeEnv's session-name
		// actor in place here split one session into two identities, and its
		// own terminal close was refused (we-m34w5). The session-name value
		// stays for alias-less sessions — the resume-path guarantee
		// (ga-xs28em) is unchanged.
		env["BEADS_ACTOR"] = alias
	} else if sessionName != "" {
		env["GC_AGENT"] = sessionName
	}
	return env
}

// SyncRuntimeAlias updates the live runtime session metadata to reflect the
// current public alias. Clearing the alias removes GC_ALIAS from the runtime.
func SyncRuntimeAlias(sp runtime.Provider, sessionName, alias string) error {
	if sp == nil || sessionName == "" {
		return nil
	}
	if alias == "" {
		return sp.RemoveMeta(sessionName, "GC_ALIAS")
	}
	return sp.SetMeta(sessionName, "GC_ALIAS", alias)
}
