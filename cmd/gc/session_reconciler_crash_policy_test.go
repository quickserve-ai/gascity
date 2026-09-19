package main

// Tests for the 2026-07-18 town-crash policy fixes:
//   - ga-9n5hj: prompt-only (FPExtra) config drift applies lazily — never
//     restarts or drains a live session; genuine drift restarts are bounded
//     per tick (stagger) with a wave event as advance notice.
//   - ga-2aq43: a continuation-reset marker whose startup ack never arrived
//     is cleared once the runtime is demonstrably healthy, instead of staying
//     armed and hard-resetting the session hours later.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestConfigDriftLazyApplicable(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		want   bool
	}{
		{"empty is not classifiable", nil, false},
		{"fpextra only is lazy", []string{"FPExtra"}, true},
		{"command is eager", []string{"Command"}, false},
		{"fpextra plus env is eager", []string{"Env", "FPExtra"}, false},
		{"env only is eager", []string{"Env"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := configDriftLazyApplicable(tc.fields); got != tc.want {
				t.Fatalf("configDriftLazyApplicable(%v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}

// resetMarkerTestSetup creates a session bead carrying a committed
// continuation-reset marker aged `age` before the env clock's now.
func resetMarkerTestSetup(env *reconcilerTestEnv, age time.Duration) beads.Bead {
	session := env.createSessionBead("worker", "worker")
	committedAt := env.clk.Now().UTC().Add(-age)
	env.setSessionMetadata(&session, map[string]string{
		"continuation_reset_pending":   "true",
		sessionpkg.ResetCommittedAtKey: committedAt.UTC().Format(time.RFC3339),
	})
	return session
}

func TestClearStaleResetMarkerIfHealthy_ClearsForAttachedSession(t *testing.T) {
	env := newReconcilerTestEnv()
	session := resetMarkerTestSetup(env, time.Hour)
	env.sp.SetAttached("worker", true)

	clearStaleResetMarkerIfHealthy(seedSessionInfo(session), env.store, env.sp, "worker", "worker",
		true, time.Minute, env.clk.Now().UTC(), env.dt, env.rec, &env.stderr, nil)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want cleared (stderr=%s)",
			got.Metadata["continuation_reset_pending"], env.stderr.String())
	}
	if got.Metadata[sessionpkg.ResetCommittedAtKey] != "" {
		t.Fatalf("reset_committed_at = %q, want cleared", got.Metadata[sessionpkg.ResetCommittedAtKey])
	}
	if !strings.Contains(env.stderr.String(), "cleared stale reset marker") {
		t.Fatalf("expected stderr note about the clear, got: %s", env.stderr.String())
	}
}

func TestClearStaleResetMarkerIfHealthy_ClearsForActivityAfterReset(t *testing.T) {
	env := newReconcilerTestEnv()
	session := resetMarkerTestSetup(env, time.Hour)
	// Runtime not attached, but provider observed activity well after the
	// reset commit + startup window — the conversation running now began
	// after the reset, so the reset evidently happened.
	env.sp.SetActivity("worker", env.clk.Now().UTC().Add(-5*time.Minute))

	clearStaleResetMarkerIfHealthy(seedSessionInfo(session), env.store, env.sp, "worker", "worker",
		true, time.Minute, env.clk.Now().UTC(), env.dt, env.rec, &env.stderr, nil)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want cleared (stderr=%s)",
			got.Metadata["continuation_reset_pending"], env.stderr.String())
	}
}

func TestClearStaleResetMarkerIfHealthy_KeepsMarker(t *testing.T) {
	cases := []struct {
		name  string
		age   time.Duration
		alive bool
		setup func(env *reconcilerTestEnv)
	}{
		{
			// Within 2x startupTimeout (floored at 2m) the tracked start
			// path still owns the marker.
			name:  "fresh marker",
			age:   30 * time.Second,
			alive: true,
			setup: func(env *reconcilerTestEnv) { env.sp.SetAttached("worker", true) },
		},
		{
			// A dead runtime is recordResetStallIfDue's territory, never
			// the healthy-clear's.
			name:  "dead runtime",
			age:   time.Hour,
			alive: false,
			setup: func(env *reconcilerTestEnv) { env.sp.SetAttached("worker", true) },
		},
		{
			name:  "no evidence",
			age:   time.Hour,
			alive: true,
			setup: func(_ *reconcilerTestEnv) {},
		},
		{
			// Activity from before the reset commit proves nothing about
			// the reset having happened — could be the stale runtime the
			// reset wants replaced.
			name:  "activity predates reset",
			age:   time.Hour,
			alive: true,
			setup: func(env *reconcilerTestEnv) {
				env.sp.SetActivity("worker", env.clk.Now().UTC().Add(-2*time.Hour))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			session := resetMarkerTestSetup(env, tc.age)
			tc.setup(env)

			clearStaleResetMarkerIfHealthy(seedSessionInfo(session), env.store, env.sp, "worker", "worker",
				tc.alive, time.Minute, env.clk.Now().UTC(), env.dt, env.rec, &env.stderr, nil)

			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Metadata["continuation_reset_pending"] != "true" {
				t.Fatalf("continuation_reset_pending = %q, want kept (stderr=%s)",
					got.Metadata["continuation_reset_pending"], env.stderr.String())
			}
		})
	}
}

// TestReconcileSessionBeads_ConfigDriftFPExtraOnlyIsLazy verifies that drift
// confined to FPExtra (prompt fragments) neither drains nor restarts a live
// session. Regression for the 2026-07-18 crash: a city-wide append_fragments
// change drifted every agent's FPExtra, the guards held for ~2.5h, and on
// guard expiry the reconciler rolled the entire roster mid-work (ga-9n5hj).
func TestReconcileSessionBeads_ConfigDriftFPExtraOnlyIsLazy(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Desired/current config has NO FingerprintExtra; the session started
	// with one — the only drifted field is FPExtra.
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	startedCfg := runtime.Config{Command: "test-cmd", FingerprintExtra: map[string]string{"fragment": "old-prompt"}}
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(startedCfg))
	if err != nil {
		t.Fatalf("marshal breakdown: %v", err)
	}
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(startedCfg),
		"core_hash_breakdown": string(breakdown),
	})

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("FPExtra-only drift must not drain, got drain=%+v stderr=%s", ds, env.stderr.String())
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// State healing may rename active → awake for an alive session; either
	// way it must not be reset toward a restart (start-pending/creating).
	if state := got.Metadata["state"]; state != "active" && state != "awake" {
		t.Fatalf("state = %q, want active/awake (session must keep running)", state)
	}
	if got.Metadata["continuation_reset_pending"] == "true" {
		t.Fatal("lazy drift must not commit a reset")
	}
	if strings.Contains(env.stderr.String(), "config-drift worker") {
		t.Fatalf("lazy drift must not log the eager drift diagnostic, stderr=%s", env.stderr.String())
	}
}

// TestReconcileSessionBeads_ConfigDriftCommandStaysEager guards the lazy
// check's scope: drift that includes a non-lazy field (Command) keeps the
// existing drain behavior even when FPExtra also drifted.
func TestReconcileSessionBeads_ConfigDriftCommandStaysEager(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig() // current Command: new-cmd
	session := env.createSessionBead("worker", "worker")
	startedCfg := runtime.Config{Command: "test-cmd", FingerprintExtra: map[string]string{"fragment": "old-prompt"}}
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(startedCfg))
	if err != nil {
		t.Fatalf("marshal breakdown: %v", err)
	}
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(startedCfg),
		"core_hash_breakdown": string(breakdown),
	})

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("Command drift must still drain (stderr=%s)", env.stderr.String())
	}
	if ds.reason != "config-drift" {
		t.Fatalf("drain reason = %q, want config-drift", ds.reason)
	}
}

// staggerTestNamedSession builds an alive named session whose stored config
// drifted in Command (eager) and whose deferral guards are already exhausted:
// not attached, activity old enough to be outside the recent-activity window.
func staggerTestNamedSession(env *reconcilerTestEnv, name string) beads.Bead {
	tp := TemplateParams{
		Command:      "new-cmd",
		SessionName:  name,
		TemplateName: name,
	}
	env.desiredState[name] = tp
	_ = env.sp.Start(context.Background(), name, runtime.Config{Command: "new-cmd"})
	env.sp.SetActivity(name, env.clk.Now().UTC().Add(-time.Hour))

	session := env.createSessionBead(name, name)
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash":                   runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
		sessionpkg.NamedSessionMetadataKey:      "true",
		sessionpkg.NamedSessionIdentityMetadata: name,
	})
	return session
}

// TestReconcileSessionBeads_ConfigDriftRestartsStaggered verifies the
// per-tick drift-restart budget: with more drifted named sessions than the
// budget allows, one tick restarts at most maxConfigDriftRestartsPerTick and
// defers the rest to later ticks. The 2026-07-18 crash rolled ~17 sessions
// in one sweep, which also cycled the tmux server (ga-9n5hj).
func TestReconcileSessionBeads_ConfigDriftRestartsStaggered(t *testing.T) {
	env := newReconcilerTestEnv()
	names := []string{"crew-a", "crew-b", "crew-c", "crew-d"}
	agents := make([]config.Agent, 0, len(names))
	for _, n := range names {
		agents = append(agents, config.Agent{Name: n})
	}
	env.cfg = &config.City{Agents: agents}
	sessions := make([]beads.Bead, 0, len(names))
	for _, n := range names {
		sessions = append(sessions, staggerTestNamedSession(env, n))
	}

	env.reconcile(sessions)

	// A restarted session commits a NEW started_config_hash (the same tick's
	// start machinery completes the fresh spawn and rewrites it); a deferred
	// session still carries the old stored hash.
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})
	countRestarted := func() int {
		restarted := 0
		for _, s := range sessions {
			got, err := env.store.Get(s.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", s.ID, err)
			}
			if got.Metadata["started_config_hash"] != oldHash {
				restarted++
			}
		}
		return restarted
	}
	if restarted := countRestarted(); restarted != maxConfigDriftRestartsPerTick {
		t.Fatalf("restarted %d sessions in one tick, want exactly %d (stderr=%s)",
			restarted, maxConfigDriftRestartsPerTick, env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "config-drift wave") {
		t.Fatalf("expected wave announcement once budget exceeded, stderr=%s", env.stderr.String())
	}

	// Subsequent ticks pick up the deferred sessions: reload beads and
	// reconcile again — the remaining drifted sessions restart within the
	// next tick's budget.
	env.clk.Time = env.clk.Time.Add(time.Minute)
	reloaded := make([]beads.Bead, 0, len(sessions))
	for _, s := range sessions {
		got, err := env.store.Get(s.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", s.ID, err)
		}
		reloaded = append(reloaded, got)
	}
	env.reconcile(reloaded)

	if restarted := countRestarted(); restarted != len(names) {
		t.Fatalf("after second tick %d/%d sessions restarted, want all (stderr=%s)",
			restarted, len(names), env.stderr.String())
	}
}

// findMessageBeads returns the type=message beads addressed to the given
// assignee, used to assert a config-drift handoff was recorded. Handoff mail
// is persisted Ephemeral (a wisp), so the query must span both storage tiers —
// the zero-value TierIssues would exclude it. Fails the test on a store error
// rather than returning it, matching the fail-fast style of the surrounding
// reconciler tests.
func findMessageBeads(t *testing.T, store beads.Store, assignee string) []beads.Bead {
	t.Helper()
	msgs, err := store.List(beads.ListQuery{Type: "message", Assignee: assignee, AllowScan: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("listing message beads for %q: %v", assignee, err)
	}
	return msgs
}

// TestSendConfigDriftHandoffMail exercises the helper directly: it must
// create a type=message handoff bead addressed to the recipient, and must be a
// no-op (no panic, no bead) on nil store or empty recipient.
//
// THIS TEST USED TO ASSERT ONLY THAT A BEAD EXISTED, which is how ga-68f9qa
// survived: the helper mailed a SUBJECT AND AN EMPTY BODY for as long as it has
// existed, and a count-the-beads assertion cannot see that. The body assertion
// below is the guarantee that was missing, not an extra.
func TestSendConfigDriftHandoffMail(t *testing.T) {
	env := newReconcilerTestEnv()
	body := configDriftHandoffBody("qcore/worker", []string{"model", "prompt"}, time.Unix(1700000000, 0).UTC())
	sendConfigDriftHandoffMail(env.store, env.rec, "qcore/worker", body, &env.stderr)
	msgs := findMessageBeads(t, env.store, "qcore/worker")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 handoff message to qcore/worker, got %d (stderr=%s)", len(msgs), env.stderr.String())
	}
	// THE ASSERTION THE COMMENT ABOVE PROMISED. It was missing until Codex
	// review round 4 on PR #104 pointed out that the comment described a
	// guarantee the test did not implement — which is worse than no comment,
	// because the next reader trusts it and does not add the check.
	//
	// Counting beads cannot see ga-68f9qa. The original defect was
	// createHandoffMail(..., []string{"HANDOFF: config-drift restart"}) with no
	// args[1], so cmd_handoff.go's `if len(args) > 1` left the body EMPTY while
	// the bead, its labels and its subject all looked correct. Only comparing
	// the PERSISTED body against the generated one can fail on that.
	if got := msgs[0].Description; got != body {
		t.Errorf("persisted handoff body does not match the generated body — this is ga-68f9qa itself\n--- persisted (%d bytes) ---\n%s\n--- generated (%d bytes) ---\n%s",
			len(got), got, len(body), body)
	}
	if strings.TrimSpace(msgs[0].Description) == "" {
		t.Error("the persisted handoff body is EMPTY — a seat would receive a subject line and nothing else, and it would still be counted as a handoff in the ratio (ga-ksac39)")
	}
	// Nil store / empty recipient are silent no-ops.
	sendConfigDriftHandoffMail(nil, env.rec, "qcore/worker", body, &env.stderr)
	sendConfigDriftHandoffMail(env.store, env.rec, "", body, &env.stderr)
	if got := len(findMessageBeads(t, env.store, "qcore/worker")); got != 1 {
		t.Fatalf("no-op guards created extra beads: got %d, want 1", got)
	}
}

// TestConfigDriftHandoffBodyCarriesRecovery is the guard for ga-68f9qa: the
// note the restarted seat reads must actually say something. A handoff bead
// that wears AutoHandoffLabel while carrying nothing is worse than no handoff —
// it is counted as a clean ending by anything reading the ratio (ga-ksac39).
func TestConfigDriftHandoffBodyCarriesRecovery(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	body := configDriftHandoffBody("qcore/worker", []string{"model", "prompt"}, at)
	if strings.TrimSpace(body) == "" {
		t.Fatal("config-drift handoff body is empty — the defect ga-68f9qa exists to prevent")
	}
	for _, want := range []string{
		"qcore/worker",          // which seat
		at.Format(time.RFC3339), // when
		"model, prompt",         // what drifted
		"MECHANICAL",            // it must not read as a composed handoff
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config-drift handoff body missing %q\n--- body ---\n%s", want, body)
		}
	}

	// The body must make NO claim about the seat's own context. The reset
	// PRESERVES the conversation: resetConfiguredNamedSessionForConfigDriftInfo's
	// preserveResume gate is true for StateStartPending — the very state this
	// branch passes — so the successor resumes with `--resume <prior-key>`.
	// Telling that agent its recollection is LOST would make it discard exactly
	// what the restart path deliberately kept. Codex review finding, PR #104.
	for _, forbidden := range []string{"LOST", "recollection", "as summarized", "start fresh"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("config-drift handoff body asserts something about the seat's context (%q) that the controller does not know — the reset may have RESUMED the conversation\n--- body ---\n%s", forbidden, body)
		}
	}
	// The body must never DIRECT a restarted seat to a `gc bd list --assignee` lookup.
	// The core claim protocol forbids it (claim-protocol.template.md): an unclaimed
	// routed item has no assignee and an ephemeral wisp is hidden from that query, so
	// it reports "no work" while the seat's own work sits open — the seat then
	// concludes its persisted work is gone, which is the opposite of recovery.
	// Codex review finding, PR #104.
	//
	// This checks the DIRECTIVE form, not the mere mention: the body is expected to
	// name the query in order to warn against it, and a plain strings.Contains cannot
	// tell "run this" from "never run this" (it flagged the warning itself when the
	// guard was first written). A command the seat is meant to run stands alone on its
	// line — that is the shape being forbidden here.
	// The body must not PRESCRIBE a work-lookup or claim procedure at all. Two
	// separate defects sit behind this, both found by Codex review on PR #104:
	//
	//   `gc bd list --assignee` is forbidden outright by the core claim protocol
	//   (claim-protocol.template.md): an unclaimed routed item has no assignee and
	//   an ephemeral wisp is hidden from that query, so it reports "no work" while
	//   the seat's own work sits open (ga-tmzjx6, 41 hours unrun).
	//
	//   `gc hook --claim --drain-ack` is the POOL WORKER protocol, and this mail
	//   goes to NAMED seats (it is sent beside resetConfiguredNamedSessionForConfigDrift).
	//   Under that protocol an `action: drain` result means "your session is done —
	//   exit". Prescribing it here could tell a freshly restarted named seat to quit.
	//
	// The controller does not know which protocol a given pack defines, so it names
	// none: `gc prime` re-renders the seat's own pack-rendered role prompt. Checks
	// the DIRECTIVE position, not the mention — a plain substring check cannot tell
	// "run this" from "never run this", which is how the first version of this guard
	// flagged its own warning text.
	for _, line := range strings.Split(body, "\n") {
		directive := strings.TrimLeft(strings.TrimSpace(line), "0123456789.-* ")
		// `gc prime` is here too: for a named agent whose pack intentionally omits
		// prompt_template (a SUPPORTED minimal config, cmd_prime.go:88-94), prime
		// falls through to defaultPrimePrompt, which itself says "1. Claim work:
		// gc hook --claim --json". So prescribing prime reaches the pool protocol
		// one indirection later. Codex review finding, PR #104.
		for _, forbidden := range []string{"gc bd list", "bd list", "gc hook", "gc bd ready", "gc prime"} {
			if strings.HasPrefix(directive, forbidden) {
				t.Errorf("config-drift handoff body prescribes %q, overriding whatever protocol the seat's pack defines: %q\n--- body ---\n%s", forbidden, line, body)
			}
		}
	}
	// No drifted fields is legal and must not produce a dangling label.
	if got := configDriftHandoffBody("qcore/worker", nil, at); strings.Contains(got, "drifted:") {
		t.Errorf("empty drifted-field list still emitted a drifted: line\n--- body ---\n%s", got)
	}
}

func TestConfigDriftCopyFilesOnly(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		want   bool
	}{
		{"empty", nil, false},
		{"copyfiles only", []string{"CopyFiles"}, true},
		{"fpextra only is not copyfiles", []string{"FPExtra"}, false},
		{"copyfiles plus fpextra is not copyfiles-only", []string{"CopyFiles", "FPExtra"}, false},
		{"copyfiles plus command is not copyfiles-only", []string{"CopyFiles", "Command"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := configDriftCopyFilesOnly(tc.fields); got != tc.want {
				t.Fatalf("configDriftCopyFilesOnly(%v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}

// TestReconcileSessionBeads_ConfigDriftCopyFilesOnlyAcceptedInPlace is the
// hq-wi4ka regression: a drift confined to CopyFiles (a staged file under
// <city>/scripts was edited) must NOT drain the live session — the staged copy
// is never re-read by the running process, so the drain destroys context to
// change a file the agent does not have open. Instead the reconciler accepts
// the drift in place: the session keeps running and started_config_hash is
// rebaselined so the next tick sees no drift. 2026-08-20 the eager path
// drained all 14 crew sessions over a one-line script edit.
func TestReconcileSessionBeads_ConfigDriftCopyFilesOnlyAcceptedInPlace(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Desired config: a probed CopyFiles entry whose content changed. The
	// session started with the OLD content hash; only CopyFiles drifts.
	env.addDesiredCopyFiles("worker", "worker", "new-content-hash")
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	startedCfg := runtime.Config{
		Command:   "test-cmd",
		CopyFiles: []runtime.CopyEntry{{Src: "/city/scripts", RelDst: ".gc/scripts", Probed: true, ContentHash: "old-content-hash"}},
	}
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(startedCfg))
	if err != nil {
		t.Fatalf("marshal breakdown: %v", err)
	}
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(startedCfg),
		"core_hash_breakdown": string(breakdown),
	})

	env.reconcile([]beads.Bead{session})

	// No drain, no restart.
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("CopyFiles-only drift must not drain, got drain=%+v stderr=%s", ds, env.stderr.String())
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state := got.Metadata["state"]; state != "active" && state != "awake" {
		t.Fatalf("state = %q, want active/awake (session must keep running)", state)
	}
	// The drift was accepted in place: started_config_hash rebaselined to the
	// current (new-content) fingerprint so the next tick sees no drift.
	currentHash := runtime.CoreFingerprint(runtime.Config{
		Command:   "test-cmd",
		CopyFiles: []runtime.CopyEntry{{Src: "/city/scripts", RelDst: ".gc/scripts", Probed: true, ContentHash: "new-content-hash"}},
	})
	if got.Metadata["started_config_hash"] != currentHash {
		t.Fatalf("started_config_hash = %q, want rebaselined to current %q", got.Metadata["started_config_hash"], currentHash)
	}
	if strings.Contains(env.stderr.String(), "config-drift worker") {
		t.Fatalf("in-place accept must not log the eager drift diagnostic, stderr=%s", env.stderr.String())
	}
}

// TestReconcileSessionBeads_ConfigDriftCopyFilesPlusCommandStaysEager guards
// the in-place accept's scope: drift that includes a non-CopyFiles field
// (Command) keeps the drain behavior even when CopyFiles also drifted.
func TestReconcileSessionBeads_ConfigDriftCopyFilesPlusCommandStaysEager(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesiredCopyFilesWithCommand("worker", "worker", "new-cmd", "new-content-hash")
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	startedCfg := runtime.Config{
		Command:   "test-cmd",
		CopyFiles: []runtime.CopyEntry{{Src: "/city/scripts", RelDst: ".gc/scripts", Probed: true, ContentHash: "old-content-hash"}},
	}
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(startedCfg))
	if err != nil {
		t.Fatalf("marshal breakdown: %v", err)
	}
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(startedCfg),
		"core_hash_breakdown": string(breakdown),
	})

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("CopyFiles+Command drift must still drain (stderr=%s)", env.stderr.String())
	}
	if ds.reason != "config-drift" {
		t.Fatalf("drain reason = %q, want config-drift", ds.reason)
	}
}

// TestReconcileSessionBeads_ConfigDriftDrainRecordsHandoff is the second
// hq-wi4ka fix: when a config-drift drain DOES fire (an eager field drifted),
// the reconciler records a handoff mail to the session's own identity BEFORE
// draining, so the rebuilt session can recover its context instead of losing
// the conversation. 2026-08-19/20 the fleet drained with no handoff.
func TestReconcileSessionBeads_ConfigDriftDrainRecordsHandoff(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig() // eager Command drift
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	startedCfg := runtime.Config{Command: "test-cmd"}
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(startedCfg))
	if err != nil {
		t.Fatalf("marshal breakdown: %v", err)
	}
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(startedCfg),
		"core_hash_breakdown": string(breakdown),
	})

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds == nil || ds.reason != "config-drift" {
		t.Fatalf("expected a config-drift drain, got %+v (stderr=%s)", ds, env.stderr.String())
	}
	// A handoff mail must have been recorded to the session's identity,
	// carrying the auto-handoff + archive-after-inject labels.
	if strings.Contains(env.stderr.String(), "creating mail") {
		t.Fatalf("handoff mail creation failed, stderr=%s", env.stderr.String())
	}
	handoffs := findMessageBeads(t, env.store, "worker")
	if len(handoffs) == 0 {
		t.Fatalf("expected a handoff mail to worker before drain; none found (stderr=%s)", env.stderr.String())
	}
}
