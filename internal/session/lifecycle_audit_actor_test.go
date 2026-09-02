package session

import "testing"

// ga-xs28em: bd resolves its audit actor as $BEADS_ACTOR -> git user.name ->
// $USER. gc's session identity is nowhere in that chain, so a session that
// reaches its runtime without BEADS_ACTOR writes to the ledger as the repo's
// git user -- a HUMAN. The create-time template env seeds it, but the
// resume/restart resolver rebuilds session env from the provider env plus the
// city anchors only, so a RESUMED session ran for ~12 hours attributing every
// mayor ruling to the Overseer.
//
// These pin the actor to the same wiring point as BEADS_HOLDER_TOKEN: the
// per-incarnation runtime env that EVERY start path merges, so the identity a
// session presents to bd cannot depend on which path started it.

func TestRuntimeEnvSetsAuditActorFromSessionName(t *testing.T) {
	env := RuntimeEnv("sid", "gastown__mayor", DefaultGeneration, DefaultContinuationEpoch, "tok-123")
	if got := env["BEADS_ACTOR"]; got != "gastown__mayor" {
		t.Errorf("BEADS_ACTOR = %q, want gastown__mayor (bd would otherwise attribute the write to git user.name)", got)
	}
	// The actor names the session, exactly as GC_SESSION_NAME does — a write
	// whose author does not match the session it came from is unauditable.
	if env["BEADS_ACTOR"] != env["GC_SESSION_NAME"] {
		t.Errorf("BEADS_ACTOR %q != GC_SESSION_NAME %q", env["BEADS_ACTOR"], env["GC_SESSION_NAME"])
	}
}

func TestRuntimeEnvVariantsPropagateAuditActor(t *testing.T) {
	alias := RuntimeEnvWithAlias("sid", "qcore--pam", "qcore/pam", DefaultGeneration, DefaultContinuationEpoch, "tok-a")
	if got := alias["BEADS_ACTOR"]; got != "qcore--pam" {
		t.Errorf("WithAlias BEADS_ACTOR = %q, want qcore--pam", got)
	}
	// The restart/resume path (cmd/gc/session_lifecycle_parallel.go) merges this
	// variant LAST over the agent config env, which is what makes it the fix for
	// a resumed session that never got the create-time template env.
	//
	// The context variant follows the alias-first claim identity. This pin
	// previously demanded the session-name form here ("the ledger records
	// session names"), while ga-i44k made hook --claim write the alias — one
	// session owning work under a different exact string than it presented to
	// bd. bd's ownership guard then refused the session's own terminal close
	// (we-m34w5: a clean-drain re-dispatch loop). The ledger attribution for
	// aliased sessions changes to the alias form at this boundary, which is
	// the string their claims already carry.
	ctx := RuntimeEnvWithSessionContext("sid", "gastown__mayor", "gastown.mayor", "mayor", "named", 37, 36, "tok-c")
	if got := ctx["BEADS_ACTOR"]; got != "gastown.mayor" {
		t.Errorf("WithSessionContext BEADS_ACTOR = %q, want the alias-first claim identity gastown.mayor", got)
	}
	if ctx["BEADS_ACTOR"] != ctx["GC_AGENT"] {
		t.Errorf("BEADS_ACTOR = %q but GC_AGENT = %q; the audit actor and the ownership identity must be one string", ctx["BEADS_ACTOR"], ctx["GC_AGENT"])
	}
}

// A nameless session must CLEAR the key rather than leave whatever the parent
// process had: inheriting a stale actor is the same misattribution wearing a
// different name. Mirrors the GC_INSTANCE_TOKEN contract.
func TestRuntimeEnvClearsAuditActorWhenSessionNameEmpty(t *testing.T) {
	env := RuntimeEnv("sid", "", DefaultGeneration, DefaultContinuationEpoch, "tok-123")
	if _, ok := env["BEADS_ACTOR"]; !ok {
		t.Fatal("BEADS_ACTOR key absent; want present-and-empty so the runtime unsets a stale parent value")
	}
	if got := env["BEADS_ACTOR"]; got != "" {
		t.Errorf("BEADS_ACTOR = %q, want empty", got)
	}
}

// The identity a session presents to bd must be the identity a claim writes
// as assignee. hook --claim writes the alias-first identity (ga-i44k), so an
// aliased session whose env still carried the runtime session name owned its
// work under one string and spoke to bd under another — bd's ownership guard
// then refused the session's own terminal close, and under a drain-ack EXIT
// trap the controller saw a clean drain with an open bead and re-dispatched
// forever (we-m34w5: 15 wakes in ~40 minutes on a scan that found nothing).
func TestRuntimeEnvWithSessionContextAlignsAuditActorWithClaimIdentity(t *testing.T) {
	env := RuntimeEnvWithSessionContext("gcs-1", "bd__dog-we-ucuj0", "bd.dog-1", "", "", 1, 0, "tok")
	if got := env["BEADS_ACTOR"]; got != "bd.dog-1" {
		t.Fatalf("BEADS_ACTOR = %q, want the alias-first claim identity %q", got, "bd.dog-1")
	}
	if got := env["GC_AGENT"]; got != "bd.dog-1" {
		t.Fatalf("GC_AGENT = %q, want %q", got, "bd.dog-1")
	}
}

// Without an alias the audit actor keeps the session-name fallback the resume
// path depends on (ga-xs28em).
func TestRuntimeEnvWithSessionContextKeepsSessionNameActorWithoutAlias(t *testing.T) {
	env := RuntimeEnvWithSessionContext("gcs-1", "mayor", "", "", "", 1, 0, "tok")
	if got := env["BEADS_ACTOR"]; got != "mayor" {
		t.Fatalf("BEADS_ACTOR = %q, want session-name fallback %q", got, "mayor")
	}
}
