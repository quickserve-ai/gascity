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
	// a resumed session that never got the create-time template env. Its actor
	// is the alias-first ownership identity (session.AssigneeIdentifier, #4981),
	// the same string the session presents as GC_AGENT, so the resumed session
	// never falls through to git user.name.
	info := Info{
		ID:                  "sid",
		SessionName:         "gastown__mayor",
		SessionNameMetadata: "gastown__mayor",
		Alias:               "gastown.mayor",
		Template:            "mayor",
		SessionOrigin:       "named",
	}
	ctx := RuntimeEnvWithSessionContext(info, 37, 36, "tok-c")
	if got := ctx["BEADS_ACTOR"]; got == "" || got != AssigneeIdentifier(info) {
		t.Errorf("WithSessionContext BEADS_ACTOR = %q, want the ownership identity %q", got, AssigneeIdentifier(info))
	}
	if ctx["BEADS_ACTOR"] != ctx["GC_AGENT"] {
		t.Errorf("BEADS_ACTOR %q != GC_AGENT %q", ctx["BEADS_ACTOR"], ctx["GC_AGENT"])
	}
	// With no alias or configured identity the actor is the session name.
	info.Alias = ""
	if got := RuntimeEnvWithSessionContext(info, 37, 36, "tok-c")["BEADS_ACTOR"]; got != "gastown__mayor" {
		t.Errorf("alias-less WithSessionContext BEADS_ACTOR = %q, want gastown__mayor", got)
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
