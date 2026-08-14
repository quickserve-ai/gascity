package main

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A NAMED session's session_name is FIXED by config. A pool worker gets a
// freshly generated name per attempt, so an abandoned half-created bead costs
// it one slot; a named session has exactly one name, so an abandoned
// half-created bead holding that name is a PERMANENT outage for that agent —
// every subsequent create fails "session name already exists" forever, and no
// amount of retrying helps. That is ga-2otk73: qcore/gastown.refinery was down
// 15 minutes behind a start-pending bead that held its name, with no tmux
// session and no process, and the loop never recovered on its own.
//
// This file holds the ONE predicate that decides a named session's
// pending-create claim is abandoned, and the two recovery paths that consume
// it. Both paths route through the same predicate on purpose: two destructive
// decisions computed from two different rules is how a fleet loses a live
// agent.
//
// The predicate is deliberately narrow. It fires only on the exact incident
// shape and fails SAFE (leaves the session alone) on every uncertainty:
//
//   - configured named session bead, never a manual one;
//   - pending_create_claim is set and the bead is in a rollback state
//     (start-pending / creating / asleep);
//   - the pending-create lease has EXPIRED under the reconciler's own
//     predicate (pendingCreateLeaseExpiredForRollbackInfo — a never-started
//     claim gets pendingCreateNeverStartedTimeout, currently 10 minutes);
//   - the runtime is AUTHORITATIVELY stopped (no provider session AND no
//     agent process); an absent provider or an empty session name reads as
//     "cannot tell", which is NOT stopped;
//   - the live cross-store work query confirms no open or in-progress work is
//     assigned to it (closeSessionInfoIfUnassigned, which fails closed on a
//     query error).
//
// A healthy named session cannot satisfy this: once its start commits, the
// pending_create_claim is cleared. A slow start is protected by the lease. A
// live agent is protected by the runtime probe and the work guard.

// namedSessionSquatCloseReason is the close reason recorded on a named session
// bead whose abandoned pending-create claim was holding its session name. It
// matches the reconciler's own rollback action name so the two are greppable
// as one class.
const namedSessionSquatCloseReason = "pending_create_lease_expired"

// staleNamedPendingCreateInfo reports whether info is a configured NAMED
// session bead whose pending-create claim has expired — the "abandoned
// half-create" shape. It does NOT consider the runtime or assigned work;
// callers must add namedSessionRuntimeConfirmedStopped and a work guard before
// acting destructively.
//
// The lease question is delegated verbatim to
// pendingCreateLeaseExpiredForRollbackInfo, the same predicate the reconciler
// uses for its own pending-create rollbacks, so the sweep, the collision
// recovery, and the reconciler can never disagree about whether a claim is
// still live.
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

// namedSessionRuntimeConfirmedStopped reports an AUTHORITATIVE "no runtime"
// observation for info's session name. It fails CLOSED: a nil provider or an
// empty session name means the observation is unavailable, which is reported
// as "not confirmed stopped", never as "stopped".
//
// It reads both halves of runtime.Liveness (provider session AND agent
// process), because a tmux-level false negative with a live agent process is
// exactly the case where closing the bead would strand a working agent. The
// process-name hints come from the bead's own configured agent, matching what
// the pool sweep already does for pool workers.
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

// samePendingCreateAttempt reports whether two reads of the same bead describe
// the SAME pending-create attempt. It is the read-compare-write fence around
// the destructive close: between deciding a claim is abandoned and closing the
// bead, the original creator may have finished spawning and committed. Every
// field the decision consumed is compared, plus the per-attempt identity
// markers (instance_token, generation) a new attempt would rotate.
//
// This is not a store-level CAS (the session front door has no conditional
// close today); it shrinks the window and matches the recheck discipline
// releasePoolAssignmentWithRecheck already uses. The work guard inside
// closeSessionInfoIfUnassigned is the second, independent barrier.
func samePendingCreateAttempt(a, b sessionpkg.Info) bool {
	return a.ID == b.ID &&
		a.Closed == b.Closed &&
		a.PendingCreateClaimMetadata == b.PendingCreateClaimMetadata &&
		a.PendingCreateStartedAt == b.PendingCreateStartedAt &&
		a.MetadataState == b.MetadataState &&
		a.LastWokeAt == b.LastWokeAt &&
		a.CreationCompleteAt == b.CreationCompleteAt &&
		a.InstanceToken == b.InstanceToken &&
		a.Generation == b.Generation
}

// abandonedNamedSessionNameHolder reports whether info is an abandoned
// half-created bead squatting sessionName on behalf of identity. Both the name
// and the owning identity must match EXACTLY: a bead that merely collides on
// an alias, or one whose configured_named_identity is empty or belongs to
// someone else, is never a candidate. That is the ga-841/kg4uh4 phantom class
// — on ambiguity we do nothing.
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

// recoverStaleNamedSessionNameSquatter is the GAP-2 repair: the collision path
// itself.
//
// When a configured named session cannot be materialized because its
// session_name is already taken, the error already names the holder — and the
// holder ID is the ONE handle that resolves through a by-ID store read. That
// matters because the ga-2otk73 holder was invisible to every list the
// controller consults (the reconciler's per-tick feed, syncSessionBeads'
// active list, and findOpenSessionBeadBySessionName all issue
// IncludeClosed=false queries, which a CachingStore answers purely from its
// in-memory map, while the name-uniqueness check issues an IncludeClosed=true
// query that takes a live backing read). The controller could see the name was
// taken and could not see by what, so neither reconciler rollback ever ran on
// it: the loop never iterated a bead it could not list.
//
// Resolving the holder BY ID sidesteps the whole read-tier question. It is
// also correct if the holder was hidden for some entirely different future
// reason — this recovery does not depend on WHY the bead was missing from the
// list, only on what the bead itself says.
//
// Returns true when the name was released. The caller does NOT retry the
// create in the same tick; the next reconciler tick materializes the session
// normally, because a closed configured-named bead releases its session_name
// (ensureSessionNameAvailableForSelfAndOwner skips closed configured-named
// holders).
func recoverStaleNamedSessionNameSquatter(
	store beads.Store,
	rigStores map[string]beads.Store,
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
	// identifier collisions deliberately do not: they name a bead that belongs
	// to another identity.
	holderID := sessionpkg.SessionNameConflictHolderID(nameErr)
	if holderID == "" {
		return false
	}

	var startupTimeout time.Duration
	if cfg != nil {
		startupTimeout = cfg.Session.StartupTimeoutDuration()
	}

	front := sessionFrontDoor(store)
	// By-ID read: the one tier that cannot be served from a stale list.
	holder, err := front.Get(holderID)
	if err != nil {
		// Cannot prove the holder is abandoned — fail safe. A holder that is
		// unreadable, absent, or not a session bead is left alone.
		fmt.Fprintf(stderr, "session beads: session_name %q holder %s unreadable, leaving it alone: %v\n", sessionName, holderID, err) //nolint:errcheck
		return false
	}
	if !abandonedNamedSessionNameHolder(holder, sessionName, identity, clk, startupTimeout) {
		return false
	}
	if !namedSessionRuntimeConfirmedStopped(holder, cfg, sp) {
		return false
	}

	// Read-compare-write fence: re-read by ID and require the same attempt,
	// then re-evaluate BOTH gates against the fresh read before closing.
	fresh, err := front.Get(holderID)
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

	// closeSessionInfoIfUnassigned runs the live cross-store assigned-work
	// query itself and fails closed when that query errors.
	if !closeSessionInfoIfUnassigned(store, rigStores, cfg, fresh, namedSessionSquatCloseReason, now, stderr) {
		fmt.Fprintf(stderr, "session beads: session_name %q still held by %s: release blocked by the work guard\n", sessionName, holderID) //nolint:errcheck
		return false
	}
	fmt.Fprintf(stderr, "session beads: released session_name %q from abandoned pending-create bead %s (%s): lease expired and no live runtime\n", sessionName, holderID, identity) //nolint:errcheck
	return true
}
