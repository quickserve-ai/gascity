package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
// The conflict error already named the holder, and a by-ID read is what reaches
// the incident's holder: the cached list and CachingStore.Get read the SAME
// in-memory map, and an id ABSENT from that map falls through to the backing
// store (caching_store_reads.go:398-460) — which is exactly the shape of an
// invisible bead. The repair therefore does not depend on WHY the bead was
// invisible.
//
// It is NOT a universally stronger read tier, and this code must not be read as
// if it were: an id that is PRESENT in the cache is answered FROM the cache, so
// a stale-but-present row is served to a by-ID read exactly as stale as it is
// served to a list. Beads this process wrote are marked dirty and re-read live,
// so the staleness that survives is another WRITER's commit that this cache has
// not absorbed yet. That residue is covered by the liveness gate, not by the
// read: a session whose start really did commit elsewhere has a runtime, and the
// confirmation window below refuses to advance on any probe that cannot attest
// it actually looked.
//
// It is deliberately narrow and fails SAFE (leaves the session alone) on every
// uncertainty:
//
//   - the holder must be a configured NAMED session bead, never a manual one;
//   - it must carry pending_create_claim in a rollback state that is NOT
//     StateAsleep, with the lease EXPIRED under the reconciler's OWN predicate
//     (pendingCreateLeaseExpiredForRollbackInfo; a never-started claim gets
//     pendingCreateNeverStartedTimeout, currently 10 minutes);
//   - its session_name must equal the contested name AND its
//     configured_named_identity must equal the identity being materialized —
//     never another identity's bead, never an identity-less bead (the
//     ga-841/kg4uh4 phantom class: on ambiguity, do nothing);
//   - the runtime must be observed stopped on namedNameReleaseConfirmTicks
//     CONSECUTIVE ticks spanning at least namedNameReleaseConfirmWindow of wall
//     clock, and every one of those observations must come from a probe the
//     provider can ATTEST as fresh and successful (see
//     observeNamedSessionLiveness);
//   - every gate is re-evaluated against a fresh by-ID read immediately before
//     the close commits, and that read-compare-close runs inside the per-bead
//     start lock every start-lane writer also takes, so it is a guarded update
//     and not a compare followed by an unprotected write.
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
//   - It does not touch an ASLEEP holder either, and that is the same argument
//     applied to this path (reviewer finding F1). The sweep carve-out was
//     dropped because no runtime gate can protect a shape that is stopped by
//     definition; pendingCreateRollbackState (session_reconciler.go) admits
//     StateAsleep, so without an explicit exclusion HERE the collision path
//     would close exactly the class the sweep was denied — and this path is
//     strictly worse for it, because it clears sleep_intent on the way out, so
//     the successor is force-restarted rather than left asleep. The exclusion
//     lives in staleNamedPendingCreateInfo. It leaves ONE shape unrepaired — an
//     asleep holder that is also invisible to the list tier — which is stated
//     there rather than papered over.

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
	//
	// The window does NOT by itself rule that out — wall-clock spacing only
	// proves the readings were taken apart in time, never that any of them came
	// from a probe that succeeded, and a fetch subsystem wedged for the whole
	// window degrades every reading in it identically. Proving the probe looked
	// is the attestation's job (observeNamedSessionLiveness): an unattested
	// reading cannot advance the chain at all. The window is retained as the
	// second, independent barrier — it bounds how transient a real death may be
	// and still be believed, and it is set clear of the 30s stale TTL so a
	// confirmed sequence spans more than one cache generation.
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
	// ASLEEP is excluded (reviewer finding F1). pendingCreateLeaseExpiredForRollbackInfo
	// runs pendingCreateRollbackState, which accepts StateAsleep as well as
	// start-pending/creating — the reconciler documents the shape ("keep
	// never-started pending-create leases alive after heal has rewritten
	// state=creating to asleep", session_reconcile.go). This change dropped the
	// sweep's named carve-out with the argument that an asleep named session is
	// authoritatively stopped by construction, so no runtime gate can protect it;
	// that argument does not stop being true at this call site. Worse here: the
	// close clears sleep_intent, so the successor comes back AWAKE — a
	// deliberately-sleeping, user-held session would be force-restarted.
	//
	// It costs the incident's own repair nothing (that bead was
	// state=start-pending), but it is not free in general: an asleep holder that
	// is ALSO invisible to the controller's list tier keeps wedging its name,
	// with no automatic repair — the pre-ga-2otk73 status quo for that one shape.
	// That is the accepted price. The alternative is closing a session a user
	// deliberately put to sleep on evidence ("no runtime") that can never
	// distinguish it from a dead one.
	if sessionpkg.State(strings.TrimSpace(info.MetadataState)) == sessionpkg.StateAsleep {
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

// namedSessionLivenessObservation is ONE tick's liveness reading for a holder
// bead, kept as a tri-state on purpose. "Stopped" and "could not tell" are
// different answers, and only code that keeps them apart can fail closed.
type namedSessionLivenessObservation struct {
	// Stopped is the content of the reading: neither the provider session nor
	// the agent process was seen.
	Stopped bool
	// Attested reports that the provider stands behind the reading — it came
	// from a probe that actually SUCCEEDED and is current
	// (runtime.AttestedLiveness.Fresh). False means UNKNOWN.
	Attested bool
}

// ConfirmsStopped reports whether this observation may advance the confirmation
// chain. Both halves are required: an unattested "stopped" is the degraded
// reading this whole mechanism exists to reject.
func (o namedSessionLivenessObservation) ConfirmsStopped() bool {
	return o.Stopped && o.Attested
}

// observeNamedSessionLiveness takes a SINGLE liveness reading for info's session
// name, with the provider's freshness attestation attached. It fails CLOSED: a
// nil provider or an empty session name yields the zero value — not stopped, not
// attested.
//
// It reads both halves of runtime.Liveness (provider session AND agent process)
// because a runtime-level false negative with a live agent process would strand
// a working agent. Those halves are NOT independent evidence, and must not be
// counted as two channels: for tmux they are two lookups into ONE StateCache
// snapshot (internal/runtime/tmux/adapter.go, state_cache.go), so a cache that
// cannot refresh zeroes both together. That is precisely what the attestation is
// for — the two halves answer "is it alive", the attestation answers "did we
// actually look".
//
// WHICH PROVIDERS CAN ATTEST. tmux can (StateCache knows its own fetchedAt,
// staleTTL and last refresh error) and the auto/hybrid routers and the status
// wrapper forward it; runtime.Fake attests, since its map is the truth it
// simulates and a BROKEN fake attests false. Every other provider — acp, k8s,
// ssh, exec, subprocess, herdr, t3bridge, and any pack-declared runtime — does
// not implement runtime.LivenessAttester and is therefore treated as UNABLE to
// attest. For those the observation is permanently UNKNOWN and this recovery
// NEVER releases a name. That is the deliberate trade: the failure it prevents
// (closing a live agent's bead) is unrecoverable, the failure it causes (a named
// session stays wedged until a human intervenes, exactly as it did before this
// change) is not.
//
// IMPORTANT — one attested observation is still not proof. Route through
// recordNamedNameReleaseConfirmation, which requires
// namedNameReleaseConfirmTicks consecutive confirmations spanning at least
// namedNameReleaseConfirmWindow.
func observeNamedSessionLiveness(info sessionpkg.Info, cfg *config.City, sp runtime.Provider) namedSessionLivenessObservation {
	name := strings.TrimSpace(info.SessionNameMetadata)
	if sp == nil || name == "" {
		return namedSessionLivenessObservation{}
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
	obs := runtime.AttestLiveness(sp, name, processNames)
	return namedSessionLivenessObservation{
		Stopped:  !obs.Running && !obs.Alive,
		Attested: obs.Fresh,
	}
}

// namedSessionStartLockPath returns the cityPath the per-bead start lock should
// use for info: the real city path for a CONFIGURED NAMED bead, "" (in-process
// layer only) for everything else.
//
// The file layer of that lock exists for exactly one caller — the ga-2otk73
// name-squat release — and the release only ever targets a configured named
// bead (abandonedNamedSessionNameHolder requires a matching
// configured_named_identity). Taking the file layer for pool beads too would be
// pure cost: a pool worker mints a NEW bead per attempt, so the city's lock
// directory would grow one never-reclaimed file per start, forever. Named beads
// are bounded by config and turn over rarely.
//
// The in-process layer is unconditional, so the controller's own start lane is
// serialized for every bead either way.
func namedSessionStartLockPath(cityPath string, info sessionpkg.Info) string {
	if sessionpkg.NamedSessionIdentityInfo(info) == "" {
		return ""
	}
	return cityPath
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
// the SAME pending-create attempt. It is the comparison half of the guarded
// update around the destructive close: between deciding a claim is abandoned and
// closing the bead, the original creator may have finished spawning and
// committed.
//
// It is the SEMANTIC half of the fence and no longer the whole of it. The close
// itself is now issued as a store-level compare-and-swap — CloseIfMatch against
// the beads revision the gates were read at (beads.ConditionalWriter) — so the
// mechanical question "did this bead change at all" is answered by the store,
// atomically with the close. This compare answers the narrower question the
// recovery actually reasons about ("is it still the same attempt"), it is what
// runs on a store without the capability, and it is what produces a legible
// diagnostic instead of a revision mismatch.
//
// Both run inside sessionpkg.WithSessionBeadStartLock, which every start-lane
// write that can move this fingerprint — the pre-wake mint, the start commit,
// the in-flight lease clear — also takes. The lock keeps the start lane out of
// the window entirely; the revision fence catches everyone else.
func samePendingCreateAttempt(a, b sessionpkg.Info) bool {
	return a.ID == b.ID && pendingCreateAttemptFingerprint(a) == pendingCreateAttemptFingerprint(b)
}

// clearNamedNameReleaseConfirmation breaks the confirmation chain after a tick
// that did NOT confirm the holder is stopped — the runtime looked alive, or the
// probe could not attest that it looked at all. Both must reset the sequence:
// "N consecutive confirmations" is the whole point, and a probe that flickers
// stopped/alive/stopped is precisely the transient-degradation signature the
// window exists to reject. An UNKNOWN tick resets rather than merely holding,
// which is the stricter of the two safe options: a fetch subsystem that is
// wobbling in and out (the ga-03ixvj / ga-0ecw5 tmux failure mode) can then
// never assemble a chain out of the good moments between its bad ones.
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
// The caller must already have taken an ATTESTED stopped observation this tick
// (namedSessionLivenessObservation.ConfirmsStopped); this function owns only the
// counting. A tick that saw the runtime alive, or whose probe could not attest
// itself, resets the sequence through clearNamedNameReleaseConfirmation instead,
// and a tick that never reaches either (the create stopped being attempted, the
// controller restarted) lapses via namedNameReleaseProbeMaxGap.
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
// expectedRevision is the beads revision the caller's gates were evaluated
// against. When the store implements beads.ConditionalWriter the close is issued
// as CloseIfMatch against it — a genuine store-level compare-and-swap: ANY
// user-visible mutation of the bead since that read (not just a pending-create
// fingerprint change) moves the revision and the close is refused. Pass 0 to
// skip the fence; a store without the capability falls back to the Tx form,
// where the caller's fingerprint compare and the per-bead start lock are the
// only fence.
func closeDeadNamedSessionNameHolder(sessFront *sessionpkg.Store, holder sessionpkg.Info, expectedRevision int64, now time.Time, stderr io.Writer) bool {
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
	if writer, ok := beads.ConditionalWriterFor(store.Store); ok && expectedRevision > 0 {
		switch closed, err := closeNamedSessionNameHolderIfCurrent(writer, holder.ID, expectedRevision, stderr); {
		case err == nil && closed:
			// Closed under the fence. The clears follow the close here rather
			// than preceding it, which inverts the Tx form's ordering on
			// purpose: the fence can only be evaluated by the close itself, and
			// of the two partial-failure shapes this is the recoverable one. A
			// failed clear leaves a CLOSED bead still carrying its claim — the
			// name is released (that is the repair), the next tick reopens the
			// bead, the collision recurs and this recovery runs again. The
			// opposite order fails to an OPEN bead whose claim is gone, which
			// staleNamedPendingCreateInfo will never look at again: a permanent
			// wedge.
			if err := store.SetMetadataBatch(holder.ID, map[string]string(patch)); err != nil {
				fmt.Fprintf(stderr, "session beads: clearing the released claim on %s after a fenced close: %v\n", holder.ID, err) //nolint:errcheck
			}
			cancelStateAssignedToRetiredSessionBead(store.Store, holder.ID, now, stderr)
			return true
		case err == nil && !closed:
			// The fence rejected it: the bead moved under us. Diagnosed inside
			// the helper; leave it alone.
			return false
		default:
			// The store advertised the capability but could not use it (an
			// unsupported backing behind a caching wrapper is the documented
			// case). Fall through to the Tx form rather than skipping the
			// repair — the caller still holds the per-bead start lock and has
			// already compared the attempt fingerprint.
		}
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

// closeNamedSessionNameHolderIfCurrent issues the fenced close and classifies
// the three outcomes the caller must tell apart:
//
//	(true,  nil) — closed; the bead had not moved since expectedRevision.
//	(false, nil) — the fence REJECTED it (beads.PreconditionFailedError). The
//	               bead changed under us; do not close it by another route.
//	(false, err) — the capability is not actually usable (or the store failed).
//	               The caller may fall back to the unfenced Tx form.
//
// The rejection is the interesting one, and it is strictly stronger than the
// pending-create fingerprint compare: the fingerprint only notices the fields
// this recovery reasons about, while the revision notices every user-visible
// write. A release refused here is retried from scratch on the next collision
// tick, so a spurious rejection (an unrelated metadata write landing in the same
// instant) costs one tick and never costs safety.
func closeNamedSessionNameHolderIfCurrent(writer beads.ConditionalWriter, holderID string, expectedRevision int64, stderr io.Writer) (bool, error) {
	err := writer.CloseIfMatch(holderID, expectedRevision)
	if err == nil {
		return true, nil
	}
	var precondition *beads.PreconditionFailedError
	if errors.As(err, &precondition) {
		_, _ = fmt.Fprintf(stderr,
			"session beads: holder %s changed under us (revision fence: expected %d, current %d), leaving it alone\n",
			holderID, precondition.Expected, precondition.Current)
		return false, nil
	}
	if errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		return false, err
	}
	fmt.Fprintf(stderr, "session beads: fenced close of %s failed: %v\n", holderID, err) //nolint:errcheck
	return false, err
}

// recoverStaleNamedSessionNameSquatter is the repair at the collision site.
//
// When a configured named session cannot be materialized because its
// session_name is already taken, the conflict error names the holder — and the
// holder ID is the handle that reaches a bead the cached list has dropped, since
// CachingStore.Get falls through to the backing store for an id ABSENT from its
// map. (It does not outrank the cache for an id the cache HAS; see the file
// header.)
//
// cityPath scopes the per-bead start lock the guarded update below takes. It is
// passed straight through rather than via namedSessionStartLockPath because the
// holder is a configured NAMED bead by construction here
// (abandonedNamedSessionNameHolder), which is exactly the condition that helper
// applies on the start-lane side — so both sides take the same lock file. Empty
// (unit tests, cityPath-less callers) degrades the lock to its in-process layer,
// which is still the layer this process's own start lane uses.
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
	cityPath string,
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
	// By-ID reads: the tier that still resolves a bead the cached list dropped.
	// The raw bead carries the confirmation-window markers (session.Info projects
	// a fixed field set and would drop them); the typed Info carries the fields
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
	if obs := observeNamedSessionLiveness(holder, cfg, sp); !obs.ConfirmsStopped() {
		// Alive, or the probe could not attest that it looked. Either way this
		// tick confirmed nothing, so the sequence restarts from zero.
		//
		// The unattested case gets its own diagnostic because it is an
		// OPERATIONAL signal, not a benign one: the runtime provider cannot
		// currently see anything, which on tmux means the fetch subsystem is
		// wedged or the server is gone. Silently logging "awaiting confirmation"
		// forever would hide a fleet-wide probe outage behind a per-session
		// recovery message.
		if !obs.Attested {
			_, _ = fmt.Fprintf(stderr,
				"session beads: session_name %q holder %s: runtime probe cannot attest a fresh reading, treating liveness as UNKNOWN and holding the name\n",
				sessionName, holderID)
		}
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

	if !releaseDeadNamedSessionNameHolder(sessFront, cfg, sp, holder, cityPath, sessionName, identity, clk, startupTimeout, now, stderr) {
		return false
	}
	fmt.Fprintf(stderr,
		"session beads: released session_name %q from abandoned pending-create bead %s (%s): lease expired and no runtime on %d consecutive ticks over %s\n",
		sessionName, holderID, identity, confirmation.Count, confirmation.Elapsed.Round(time.Second)) //nolint:errcheck
	return true
}

// releaseDeadNamedSessionNameHolder is the guarded update: re-read the holder,
// re-evaluate EVERY gate against that read, and close — all inside the per-bead
// start lock, so the comparison cannot be invalidated between the compare and
// the write by any writer that takes the same lock.
//
// It replaces what was a compare-then-write. The compare alone was not a fence:
// the real start-commit path (CommitStartedPatch / ClearPendingCreateClaim in
// commitStartResult) writes through sessFront.ApplyPatch and used to hold no
// lock at all, so it could land after the re-read, after the compare, after the
// liveness re-check, or in the middle of the close. Two mechanisms replace it:
//
//   - a store-level CAS. The close is issued as CloseIfMatch against the beads
//     revision this function read the bead at (beads.ConditionalWriter, via
//     closeDeadNamedSessionNameHolder). The store refuses it if ANY user-visible
//     write touched the bead in between — which is a superset of the
//     fingerprint, and is evaluated atomically with the close rather than before
//     it.
//   - the per-bead start lock, which the pre-wake mint, the start commit and the
//     in-flight-lease clear all take now. The CAS would DETECT those writers;
//     the lock keeps them out of the window, so the common case is a clean
//     release instead of a rejected one retried next tick.
//
// What that closes, and what it does not:
//
//   - CLOSED against any writer, on a store that implements
//     beads.ConditionalWriter — CachingStore (the controller's own store),
//     BdStore, MemStore and FileStore all do. The close cannot land on a bead
//     that moved after the gates read it.
//   - DEGRADED to lock + fingerprint compare on a store without the capability
//     (a caching wrapper over an unsupported backing returns
//     ErrConditionalWriteUnsupported). There the start lane is still excluded by
//     the lock, and every other writer is only DETECTED, with a window between
//     the re-read and the Tx.
//   - NOT a guarantee that the re-read is current. It is a by-ID read through
//     the same CachingStore, so a commit made by ANOTHER process that this cache
//     has not absorbed is invisible to it — though the revision fence is
//     evaluated by the BACKING store, so such a write still rejects the close.
//     The liveness gate covers what remains: a session that really started has a
//     runtime.
func releaseDeadNamedSessionNameHolder(
	sessFront *sessionpkg.Store,
	cfg *config.City,
	sp runtime.Provider,
	holder sessionpkg.Info,
	cityPath string,
	sessionName string,
	identity string,
	clk clock.Clock,
	startupTimeout time.Duration,
	now time.Time,
	stderr io.Writer,
) bool {
	holderID := holder.ID
	released := false
	lockErr := sessionpkg.WithSessionBeadStartLock(cityPath, holderID, func() error {
		// The observations above were taken across ticks against runtime state
		// that may have moved since; an in-flight start can have committed. Every
		// gate is re-evaluated here against a read taken INSIDE the lock.
		//
		// The RAW read comes FIRST and its revision is what the close is fenced
		// on. The order is load-bearing: a write landing between the two reads
		// then makes the typed projection NEWER than the fenced revision, so the
		// close is refused. Reading the revision second would fence on a
		// revision newer than the state the gates judged — a fence that passes
		// precisely when it should fail.
		freshRaw, err := sessFront.Store().Get(holderID)
		if err != nil {
			fmt.Fprintf(stderr, "session beads: re-reading session_name %q holder %s before release: %v\n", sessionName, holderID, err) //nolint:errcheck
			return nil
		}
		fresh, err := sessFront.Get(holderID)
		if err != nil {
			fmt.Fprintf(stderr, "session beads: re-reading session_name %q holder %s before release: %v\n", sessionName, holderID, err) //nolint:errcheck
			return nil
		}
		if !samePendingCreateAttempt(holder, fresh) {
			fmt.Fprintf(stderr, "session beads: session_name %q holder %s changed under us, leaving it alone\n", sessionName, holderID) //nolint:errcheck
			return nil
		}
		if !abandonedNamedSessionNameHolder(fresh, sessionName, identity, clk, startupTimeout) {
			return nil
		}
		if !observeNamedSessionLiveness(fresh, cfg, sp).ConfirmsStopped() {
			return nil
		}
		released = closeDeadNamedSessionNameHolder(sessFront, fresh, freshRaw.Revision, now, stderr)
		return nil
	})
	if lockErr != nil {
		// Fail CLOSED: without the lock the close would be an unguarded write.
		fmt.Fprintf(stderr, "session beads: locking session_name %q holder %s for release: %v\n", sessionName, holderID, lockErr) //nolint:errcheck
		return false
	}
	return released
}
