package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ga-2otk73: a named session's session_name is FIXED by config. A pool worker
// gets a freshly generated name per attempt, so an abandoned half-created bead
// costs it one slot; a named session has exactly ONE name, so an abandoned
// half-created bead holding that name is a PERMANENT outage for that agent —
// every subsequent create fails "session name already exists" forever and no
// amount of retrying helps. qcore/gastown.refinery was down 15 minutes behind a
// start-pending bead that held its name with no tmux session and no process;
// the loop failed 62 consecutive times and never recovered on its own.
//
// ROOT CAUSE (proven, not re-litigated here): CachingStore answers an
// IncludeClosed=FALSE List purely from its in-memory cache while an
// IncludeClosed=TRUE List takes a live backing read
// (internal/beads/caching_store_reads.go:36-66). Every identity-discovery read
// the controller makes is IncludeClosed=false, and the name-uniqueness check is
// IncludeClosed=true. So the name registry could see the holder and the working
// set could not: both reconciler pending-create rollbacks were dead code for a
// bead the per-tick feed never contained.
//
// THE REPAIR happens at the COLLISION SITE, and resolves the holder BY BEAD ID.
// The conflict error already named the holder; a by-ID store read is the one
// tier a stale list cache cannot hide. That also means the repair does not
// depend on WHY the bead was invisible — only on what the bead itself says.
//
// It is deliberately narrow and fails SAFE (leaves the session alone) on every
// uncertainty:
//
//   - the holder must be a configured NAMED session bead, never a manual one;
//   - it must carry pending_create_claim in a rollback state, with the lease
//     EXPIRED under the reconciler's OWN predicate
//     (pendingCreateLeaseExpiredForRollbackInfo; a never-started claim gets
//     pendingCreateNeverStartedTimeout, currently 10 minutes);
//   - its session_name must equal the contested name AND its
//     configured_named_identity must equal the identity being materialized —
//     never another identity's bead, never an identity-less bead (the
//     ga-841/kg4uh4 phantom class: on ambiguity, do nothing);
//   - the runtime must be observed stopped on namedNameReleaseConfirmTicks
//     CONSECUTIVE ticks spanning at least namedNameReleaseConfirmWindow of wall
//     clock (see namedSessionRuntimeConfirmedStopped for why a single probe is
//     not evidence);
//   - every gate is re-evaluated against a FRESH by-ID read immediately before
//     the close commits.
//
// What it deliberately does NOT do:
//
//   - It does not consult the assigned-work guard. For an on_demand named
//     session — the incident's own mode — "this identity has assigned work" is
//     not evidence of life, it is the REASON the wake was requested: an
//     on_demand named session with no visible canonical bead only enters the
//     desired set BECAUSE namedWorkReady[identity] is true
//     (build_desired_state.go). Gating the release on the work guard is
//     therefore self-cancelling for exactly the shape that produced the
//     incident. The holder here has been PROVEN dead — expired lease, no
//     runtime, confirmed over a multi-tick window — and a proven-dead bead
//     cannot be "working on" anything.
//   - It does not release that work either (see
//     closeDeadNamedSessionNameHolder). This mirrors the reconciler's own
//     pending-create rollback, which likewise closes without a work guard and
//     without a work-release cascade.
//   - It does not sweep. An earlier proposal widened
//     sweepUndesiredPoolSessionBeads to admit named beads; that carve-out is
//     NOT part of this change. It could not have fired on this incident (the
//     sweep skips anything in desiredState, and the refinery was desired every
//     tick — and an invisible bead is never iterated regardless), while it
//     admitted ASLEEP named sessions carrying a claim, which are
//     "authoritatively stopped" by construction and so unprotectable by any
//     runtime gate. See the report for the full rationale.

const (
	// namedSessionSquatCloseState is the terminal state stamped on a named
	// session bead whose abandoned pending-create claim was holding its
	// session_name.
	//
	// The value is load-bearing for CONTINUITY, not just for forensics.
	// closedNamedSessionReopenEligible (internal/session/named_config.go)
	// REJECTS "gc_swept", "orphaned", "duplicate", "stale-session",
	// "failed-create" and friends: a named bead closed under any of those is
	// never found by FindClosedNamedSessionBeadForSessionName, so the next tick
	// mints a FRESH bead and the identity's session_key / continuation epoch —
	// its conversation — is silently dropped. This code is outside that reject
	// set on purpose, so the very next tick REOPENS this same bead with its
	// continuity intact (reopenClosedConfiguredNamedSessionBead).
	//
	// CanonicalCloseReason has no entry for it and falls through to
	// "session terminated: <code>", which clears the 20-character floor that
	// validation.on-close=error enforces.
	namedSessionSquatCloseState = "pending_create_lease_expired"

	// namedNameReleaseConfirmTicks is how many CONSECUTIVE collision ticks must
	// observe the holder's runtime as stopped before the name is released.
	// Mirrors namedSuspendConfirmTicks, the reconciler's existing
	// transient-signal buffer for a destructive named-session decision.
	namedNameReleaseConfirmTicks = 3

	// namedNameReleaseConfirmWindow is the minimum wall-clock span the
	// confirming observations must cover. Tick count alone is NOT sufficient:
	// tmux's StateCache reports EVERY session as not-running once its snapshot
	// is older than defaultStaleTTL = 30s (internal/runtime/tmux/state_cache.go),
	// and on a busy box N ticks can all land inside one such stale window, so N
	// identical "stopped" answers can be N reads of the same degraded snapshot.
	// The window is set well clear of that 30s stale TTL so a confirmed
	// sequence must survive at least one successful cache refresh.
	//
	// The cost is bounded and small: the lease is already expired by
	// pendingCreateNeverStartedTimeout (10m) before the first confirming tick,
	// so this adds ~2 minutes to a recovery whose alternative was a permanent
	// outage. At the default 30s patrol_interval that is 5 ticks, so the WINDOW
	// (not the tick count) is the binding gate — as intended.
	namedNameReleaseConfirmWindow = 2 * time.Minute

	// namedNameReleaseProbeMaxGap is how stale the previous confirming
	// observation may be and still CHAIN. A longer gap means ticks were missed
	// (controller restart, the create stopped being attempted, the holder
	// looked alive in between), so the sequence restarts from one. This is what
	// makes the confirmations "consecutive" rather than merely cumulative.
	//
	// It must be generous relative to the ACTUAL tick cadence or the recovery
	// never converges and this whole fix silently does nothing. Ticks are
	// driven by daemon.patrol_interval (default 30s), but a single tick can
	// legitimately block for minutes — a start wave waits up to
	// session.startup_timeout (default 300s) — and a tick that slow is exactly
	// the fleet state in which a half-created bead gets abandoned in the first
	// place. Ten minutes clears that comfortably.
	//
	// A wide gap is safe because it is NOT what makes the decision sound: every
	// link in the chain is a fresh liveness probe, the fence re-probes
	// immediately before the close, and a rotated pending-create attempt
	// (instance_token / pending_create_started_at / state) breaks the chain via
	// the fingerprint regardless of timing. The gap only bounds how long stale
	// bookkeeping may sit on a bead before it stops counting.
	namedNameReleaseProbeMaxGap = 10 * time.Minute
)

// Confirmation-window markers, persisted on the HOLDER bead so the window
// survives a controller restart and is auditable after the fact (`bd show
// <holder>` explains why the name was, or was not, released). They are cleared
// by the close itself.
const (
	namedNameReleaseProbeAttemptKey = "name_release_probe_attempt"
	namedNameReleaseProbeCountKey   = "name_release_probe_count"
	namedNameReleaseProbeFirstAtKey = "name_release_probe_first_at"
	namedNameReleaseProbeLastAtKey  = "name_release_probe_last_at"
)

func namedNameReleaseProbeKeys() []string {
	return []string{
		namedNameReleaseProbeAttemptKey,
		namedNameReleaseProbeCountKey,
		namedNameReleaseProbeFirstAtKey,
		namedNameReleaseProbeLastAtKey,
	}
}

// staleNamedPendingCreateInfo reports whether info is a configured NAMED
// session bead whose pending-create claim has expired — the "abandoned
// half-create" shape. It considers NEITHER the runtime NOR assigned work;
// callers must add the confirmation window before acting destructively.
//
// The lease question is delegated verbatim to
// pendingCreateLeaseExpiredForRollbackInfo, the same predicate the reconciler
// uses for its own pending-create rollbacks, so this recovery and the
// reconciler can never disagree about whether a claim is still live.
func staleNamedPendingCreateInfo(info sessionpkg.Info, clk clock.Clock, startupTimeout time.Duration) bool {
	if info.Closed {
		return false
	}
	// Manual sessions are excluded unconditionally: a human-attached session
	// has no pending-create lease semantics and no owner to recreate it.
	if !isNamedSessionInfo(info) || isManualSessionInfo(info) {
		return false
	}
	if !info.PendingCreateClaim {
		return false
	}
	return pendingCreateLeaseExpiredForRollbackInfo(info, clk, startupTimeout)
}

// abandonedNamedSessionNameHolder reports whether info is an abandoned
// half-created bead squatting sessionName on behalf of identity. Both the name
// and the owning identity must match EXACTLY: a bead that merely collides on an
// alias, or one whose configured_named_identity is empty or belongs to someone
// else, is never a candidate.
func abandonedNamedSessionNameHolder(
	info sessionpkg.Info,
	sessionName string,
	identity string,
	clk clock.Clock,
	startupTimeout time.Duration,
) bool {
	sessionName = strings.TrimSpace(sessionName)
	identity = strings.TrimSpace(identity)
	if sessionName == "" || identity == "" {
		return false
	}
	if strings.TrimSpace(info.SessionNameMetadata) != sessionName {
		return false
	}
	if namedSessionIdentityInfo(info) != identity {
		return false
	}
	return staleNamedPendingCreateInfo(info, clk, startupTimeout)
}

// namedSessionRuntimeConfirmedStopped reports a SINGLE "no runtime" observation
// for info's session name. It fails CLOSED: a nil provider or an empty session
// name means the observation is unavailable, which is reported as "not
// confirmed stopped", never as "stopped".
//
// It reads both halves of runtime.Liveness (provider session AND agent process)
// because a tmux-level false negative with a live agent process is exactly the
// case where closing the bead would strand a working agent.
//
// IMPORTANT — this is one observation, not proof. runtime.Liveness carries no
// error or quality channel, so a provider that is degraded, timing out, or
// serving a stale snapshot is INDISTINGUISHABLE here from one reporting a truly
// dead session; tmux's StateCache in particular reports every session
// not-running once its snapshot passes defaultStaleTTL (30s). Callers must
// therefore never act on one call — route through
// recordNamedNameReleaseConfirmation, which requires
// namedNameReleaseConfirmTicks consecutive confirmations spanning at least
// namedNameReleaseConfirmWindow.
func namedSessionRuntimeConfirmedStopped(info sessionpkg.Info, cfg *config.City, sp runtime.Provider) bool {
	name := strings.TrimSpace(info.SessionNameMetadata)
	if sp == nil || name == "" {
		return false
	}
	var processNames []string
	if cfg != nil {
		template := normalizedSessionTemplateInfo(info, cfg)
		if template == "" {
			template = info.Template
		}
		if agentCfg := findAgentByTemplate(cfg, template); agentCfg != nil {
			processNames = config.AgentProcessNames(cfg, *agentCfg, exec.LookPath)
		}
	}
	obs := runtime.ObserveLiveness(sp, name, processNames)
	return !obs.Running && !obs.Alive
}

// pendingCreateAttemptFingerprint digests the fields that identify ONE
// pending-create attempt. Two reads of a bead that agree on this fingerprint
// describe the same attempt; any disagreement means the creator committed,
// rolled back, slept, or started a new attempt while we were deciding.
//
// It includes every field the abandonment decision consumes plus the
// per-attempt identity markers (instance_token, generation) a new attempt
// rotates. It deliberately does NOT include the confirmation-window markers —
// this recovery writes those itself, and a fingerprint that its own bookkeeping
// invalidated would never converge.
//
// The result is a hex digest, not the joined fields: it is PERSISTED as bead
// metadata, so it must be short and free of control characters and of any
// verbatim copy of session state.
func pendingCreateAttemptFingerprint(info sessionpkg.Info) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		info.ID,
		strconv.FormatBool(info.Closed),
		strings.TrimSpace(info.SessionNameMetadata),
		info.PendingCreateClaimMetadata,
		info.PendingCreateStartedAt,
		info.MetadataState,
		info.LastWokeAt,
		info.CreationCompleteAt,
		info.InstanceToken,
		info.Generation,
	}, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// samePendingCreateAttempt reports whether two reads of the same bead describe
// the SAME pending-create attempt. It is the read-compare-write fence around
// the destructive close: between deciding a claim is abandoned and closing the
// bead, the original creator may have finished spawning and committed.
//
// This is not a store-level CAS (the session front door has no conditional
// close today); it shrinks the window and matches the recheck discipline
// releasePoolAssignmentWithRecheck already uses. The multi-tick confirmation
// window is the independent second barrier.
func samePendingCreateAttempt(a, b sessionpkg.Info) bool {
	return a.ID == b.ID && pendingCreateAttemptFingerprint(a) == pendingCreateAttemptFingerprint(b)
}

// clearNamedNameReleaseConfirmation breaks the confirmation chain after a tick
// that did NOT confirm the holder is stopped — the runtime looked alive, or the
// probe could not tell. Both must reset the sequence: "N consecutive
// confirmations" is the whole point, and a probe that flickers
// stopped/alive/stopped is precisely the transient-degradation signature the
// window exists to reject.
//
// This is an explicit reset rather than letting namedNameReleaseProbeMaxGap
// expire, because that gap is deliberately wide (see its comment) — wide enough
// that a chain would otherwise span a tick that saw the session alive.
//
// It writes only when something is actually set, so the steady state costs no
// writes, and it never touches a bead the caller has not already confirmed is
// this identity's own stale-claim holder.
func clearNamedNameReleaseConfirmation(sessFront *sessionpkg.Store, raw beads.Bead, stderr io.Writer) {
	patch := sessionpkg.MetadataPatch{}
	for _, key := range namedNameReleaseProbeKeys() {
		if strings.TrimSpace(raw.Metadata[key]) != "" {
			patch[key] = ""
		}
	}
	if len(patch) == 0 {
		return
	}
	if err := sessFront.ApplyPatch(raw.ID, patch); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "session beads: clearing dead-holder confirmation on %s: %v\n", raw.ID, err) //nolint:errcheck
	}
}

// namedNameReleaseConfirmation is one tick's answer from the confirmation
// window: whether the release may proceed, and the progress to log if not.
type namedNameReleaseConfirmation struct {
	Confirmed bool
	Count     int
	Elapsed   time.Duration
}

// recordNamedNameReleaseConfirmation advances (or restarts) the multi-tick
// "the holder is dead" confirmation sequence for holder, persisting its state
// on the holder bead, and reports whether the sequence is now complete.
//
// The caller must already have observed the runtime as stopped THIS tick; this
// function owns only the counting. A tick that observed the runtime as alive or
// unobservable resets the sequence through clearNamedNameReleaseConfirmation
// instead, and a tick that never reaches either (the create stopped being
// attempted, the controller restarted) lapses via namedNameReleaseProbeMaxGap.
//
// Completion requires BOTH namedNameReleaseConfirmTicks observations AND
// namedNameReleaseConfirmWindow of wall clock between the first and the current
// one. See namedNameReleaseConfirmWindow for why the count alone is not enough.
func recordNamedNameReleaseConfirmation(
	sessFront *sessionpkg.Store,
	raw beads.Bead,
	holder sessionpkg.Info,
	now time.Time,
	stderr io.Writer,
) namedNameReleaseConfirmation {
	if stderr == nil {
		stderr = io.Discard
	}
	now = now.UTC()
	fingerprint := pendingCreateAttemptFingerprint(holder)

	count := 0
	firstAt := now
	chains := strings.TrimSpace(raw.Metadata[namedNameReleaseProbeAttemptKey]) == fingerprint
	if chains {
		if lastAt, ok := parseRFC3339Metadata(raw.Metadata[namedNameReleaseProbeLastAtKey]); !ok ||
			now.Before(lastAt) || now.Sub(lastAt) > namedNameReleaseProbeMaxGap {
			// Missed ticks, an unparsable marker, or a clock that moved
			// backwards: the observations are no longer consecutive.
			chains = false
		}
	}
	if chains {
		if prev, err := strconv.Atoi(strings.TrimSpace(raw.Metadata[namedNameReleaseProbeCountKey])); err == nil && prev > 0 {
			count = prev
		}
		if parsed, ok := parseRFC3339Metadata(raw.Metadata[namedNameReleaseProbeFirstAtKey]); ok && !parsed.After(now) {
			firstAt = parsed
		}
	}
	count++

	patch := sessionpkg.MetadataPatch{
		namedNameReleaseProbeAttemptKey: fingerprint,
		namedNameReleaseProbeCountKey:   strconv.Itoa(count),
		namedNameReleaseProbeFirstAtKey: firstAt.Format(time.RFC3339),
		namedNameReleaseProbeLastAtKey:  now.Format(time.RFC3339),
	}
	if err := sessFront.ApplyPatch(holder.ID, patch); err != nil {
		// Fail CLOSED. Without a durable counter we cannot prove the
		// observations were consecutive, so this tick contributes nothing.
		fmt.Fprintf(stderr, "session beads: recording dead-holder confirmation on %s: %v\n", holder.ID, err) //nolint:errcheck
		return namedNameReleaseConfirmation{}
	}

	elapsed := now.Sub(firstAt)
	return namedNameReleaseConfirmation{
		Confirmed: count >= namedNameReleaseConfirmTicks && elapsed >= namedNameReleaseConfirmWindow,
		Count:     count,
		Elapsed:   elapsed,
	}
}

// closeDeadNamedSessionNameHolder closes a proven-dead named session bead so it
// stops holding its session_name. Closing IS the release: a closed
// configured-named bead no longer reserves its name
// (ensureSessionNameAvailableForSelfAndOwner skips closed configured-named
// holders), so the next tick materializes the session normally.
//
// It is deliberately shaped like the reconciler's own pending-create rollback
// (rollbackPendingCreateClears) — one Tx, claim and in-flight-lease markers
// cleared, no work guard, no work-release cascade — with two named-session
// differences:
//
//   - The terminal state is namedSessionSquatCloseState, NOT "failed-create".
//     A named bead closed as failed-create (or gc_swept) is not reopen-eligible,
//     so the identity's session_key would be dropped and the agent would come
//     back as a fresh conversation. This close keeps the bead reopen-eligible.
//   - session_name is PRESERVED. The rollback clears it for an explicitly named
//     session, but reopenClosedConfiguredNamedSessionBead requires a closed bead
//     whose session_name still matches in order to revive it — clearing the name
//     here would trade a name squat for a lost conversation, and the name is
//     already released by the close itself.
//
// It runs no work guard and no work-release cascade, on purpose:
//
//   - A guard would be self-cancelling. An on_demand named session with no
//     visible canonical bead reaches the collision site ONLY because work is
//     assigned to its identity; "it owns work" is the reason the wake was
//     requested, not evidence the dead holder is alive.
//   - The cascade would be actively harmful. For a named session the IDENTITY
//     and the session_name are durable config, not properties of this bead: the
//     successor materializes under the same identity and inherits the work.
//     Clearing those assignees would destroy the very demand signal
//     (namedWorkReady) that makes an on_demand named session materialize,
//     converting a name squat into a silent work orphan.
//
// Work assigned to the dead bead's own ID is the one class this leaves behind.
// A named bead in this shape never started (no runtime, claim never committed),
// so nothing ran that could have claimed work as that bead ID; the reconciler's
// orphan-release pass remains the idempotent fallback. Noted as residual risk
// rather than hidden.
func closeDeadNamedSessionNameHolder(sessFront *sessionpkg.Store, holder sessionpkg.Info, now time.Time, stderr io.Writer) bool {
	if sessFront == nil || strings.TrimSpace(holder.ID) == "" {
		return false
	}
	if stderr == nil {
		stderr = io.Discard
	}
	store := sessFront.Store()
	// Idempotence, mirroring closeBead: a bead someone else already closed must
	// not be re-stamped with a second terminal state.
	if snapshot, err := store.Get(holder.ID); err == nil && snapshot.Status == "closed" {
		return false
	}
	patch := sessionpkg.ClosePatch(now.UTC(), namedSessionSquatCloseState)
	// The abandoned claim and its in-flight lease marker must not survive onto
	// the reopened bead, or the successor inherits the wedge we just cleared.
	patch["pending_create_claim"] = ""
	patch["pending_create_started_at"] = ""
	patch["last_woke_at"] = ""
	patch["sleep_intent"] = ""
	for _, key := range namedNameReleaseProbeKeys() {
		patch[key] = ""
	}
	// The metadata batch is ordered before the Close so that on a store whose
	// Tx is not atomic the claim clears still land if the Close then fails —
	// a stale claim on a still-open bead would ping-pong the reconciler. The
	// helper reports failure either way, so the next tick retries.
	txErr := store.Tx("gc: release named session name from dead pending-create holder "+holder.ID, func(tx beads.Tx) error {
		if err := tx.SetMetadataBatch(holder.ID, map[string]string(patch)); err != nil {
			return err
		}
		return tx.Close(holder.ID)
	})
	if txErr != nil {
		fmt.Fprintf(stderr, "session beads: releasing session_name from %s: %v\n", holder.ID, txErr) //nolint:errcheck
		return false
	}
	// Defense in depth, matching every other session-retirement path: a startup
	// race between bead creation and an early bind can leave participant
	// records behind.
	cancelStateAssignedToRetiredSessionBead(store.Store, holder.ID, now, stderr)
	return true
}

// recoverStaleNamedSessionNameSquatter is the repair at the collision site.
//
// When a configured named session cannot be materialized because its
// session_name is already taken, the conflict error names the holder — and the
// holder ID is the ONE handle that resolves through a by-ID store read, the one
// read tier a stale list cache cannot serve.
//
// Returns true when the name was released. The caller does NOT retry the create
// in the same tick: the release is the repair, and re-driving the create inside
// the same critical section buys one tick at the cost of a second decision
// point. The next tick REOPENS the closed bead with its continuity intact.
func recoverStaleNamedSessionNameSquatter(
	store beads.Store,
	cfg *config.City,
	sp runtime.Provider,
	nameErr error,
	sessionName string,
	identity string,
	clk clock.Clock,
	now time.Time,
	stderr io.Writer,
) bool {
	if store == nil {
		return false
	}
	if stderr == nil {
		stderr = io.Discard
	}
	sessionName = strings.TrimSpace(sessionName)
	identity = strings.TrimSpace(identity)
	if sessionName == "" || identity == "" {
		return false
	}
	// Only an OUTRIGHT session_name conflict carries a typed holder. Alias and
	// identifier collisions deliberately do not: those name a bead that belongs
	// to some OTHER identity, and no caller may act destructively on one from a
	// name collision alone.
	holderID := sessionpkg.SessionNameConflictHolderID(nameErr)
	if holderID == "" {
		return false
	}

	var startupTimeout time.Duration
	if cfg != nil {
		startupTimeout = cfg.Session.StartupTimeoutDuration()
	}

	sessFront := sessionFrontDoor(store)
	// By-ID reads: the one tier that cannot be served from a stale list. The
	// raw bead carries the confirmation-window markers (session.Info projects a
	// fixed field set and would drop them); the typed Info carries the fields
	// every shared predicate reads.
	raw, err := store.Get(holderID)
	if err != nil {
		fmt.Fprintf(stderr, "session beads: session_name %q holder %s unreadable, leaving it alone: %v\n", sessionName, holderID, err) //nolint:errcheck
		return false
	}
	holder, err := sessFront.Get(holderID)
	if err != nil {
		fmt.Fprintf(stderr, "session beads: session_name %q holder %s is not a readable session bead, leaving it alone: %v\n", sessionName, holderID, err) //nolint:errcheck
		return false
	}
	if !abandonedNamedSessionNameHolder(holder, sessionName, identity, clk, startupTimeout) {
		return false
	}
	if !namedSessionRuntimeConfirmedStopped(holder, cfg, sp) {
		// Alive, or the probe could not tell. Either way this tick did not
		// confirm anything, so the sequence restarts from zero.
		clearNamedNameReleaseConfirmation(sessFront, raw, stderr)
		return false
	}

	confirmation := recordNamedNameReleaseConfirmation(sessFront, raw, holder, now, stderr)
	if !confirmation.Confirmed {
		fmt.Fprintf(stderr,
			"session beads: session_name %q holder %s looks abandoned (no runtime, lease expired); awaiting confirmation %d/%d over %s of %s\n",
			sessionName, holderID, confirmation.Count, namedNameReleaseConfirmTicks,
			confirmation.Elapsed.Round(time.Second), namedNameReleaseConfirmWindow) //nolint:errcheck
		return false
	}

	// TOCTOU fence. The observations above were taken across ticks against
	// possibly cached runtime state; an async start can commit between the last
	// probe and this close, and closeBead re-checks only Status=="closed". So
	// re-read the holder BY ID and re-evaluate EVERY gate — including the
	// pending-create fence and a fresh liveness probe — immediately before
	// committing.
	fresh, err := sessFront.Get(holderID)
	if err != nil {
		fmt.Fprintf(stderr, "session beads: re-reading session_name %q holder %s before release: %v\n", sessionName, holderID, err) //nolint:errcheck
		return false
	}
	if !samePendingCreateAttempt(holder, fresh) {
		fmt.Fprintf(stderr, "session beads: session_name %q holder %s changed under us, leaving it alone\n", sessionName, holderID) //nolint:errcheck
		return false
	}
	if !abandonedNamedSessionNameHolder(fresh, sessionName, identity, clk, startupTimeout) {
		return false
	}
	if !namedSessionRuntimeConfirmedStopped(fresh, cfg, sp) {
		return false
	}

	if !closeDeadNamedSessionNameHolder(sessFront, fresh, now, stderr) {
		return false
	}
	fmt.Fprintf(stderr,
		"session beads: released session_name %q from abandoned pending-create bead %s (%s): lease expired and no runtime on %d consecutive ticks over %s\n",
		sessionName, holderID, identity, confirmation.Count, confirmation.Elapsed.Round(time.Second)) //nolint:errcheck
	return true
}
