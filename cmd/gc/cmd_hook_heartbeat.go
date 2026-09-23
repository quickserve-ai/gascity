package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// gc hook heartbeat — the turn-driven lease refresher (ga-56nq1a stage 1).
//
// A claim carries a lease; today nothing refreshes it, so every lease in the
// fleet expires ~5 minutes after claim and "expired" says nothing about
// liveness (measured 2026-08-27 and re-verified 2026-09-23: heartbeat_at ==
// granted_at on every live lease row). This command is the missing refresh
// leg: it heartbeats every in_progress bead assigned to the calling session
// under any of its identities, so leases track the one thing a lease is for —
// a session that is still taking turns. It is driven by turns, not by a
// timer, so it stops exactly when the work stops (the ga-clzquf constraint:
// an alive-but-wedged process must not keep its claims fresh forever).
//
// KEYED ON ASSIGNED ROWS, NOT THE CLAIM STAMP (hook-seam consult, ga-56nq1a
// 2026-09-23): `gc hook current` only sees `gc hook --claim` records, so
// hand-dole, adoption and hand-claim — most of a named seat's board — are
// invisible to it. The identity set here is
// session.CurrentAssigneeIdentities PLUS the prior aliases no other live
// session answers to (hookHeartbeatUnreusedPriorAliases): the orphan-release
// reader casts over the whole alias history so it never strips live work,
// while a writer vouching for liveness must not vouch through a name a later
// live session has taken (SESSION-RUNTIME-010).
// Each row is heartbeated with the row's OWN assignee spelling as actor,
// because bd's owner check is exact string equality and a cross-spelling
// heartbeat is refused (measured; the refusal would otherwise be swallowed by
// the lenient exit and look armed).
//
// bd's heartbeat self-heals a missing lease for the current assignee, so this
// single tick both ARMS unleased claims (paths that deliberately arm nothing
// at v59) and REFRESHES armed ones.
//
// Protocol contract: this is a hook leg, so by default it NEVER fails the
// turn — no session identity, no rows, store trouble, a lost lease: each
// prints a diagnostic and exits 0. --strict inverts that for canaries and
// the stage-2 both-cities proof, where "the heartbeat happened" is the
// measurement and must fail loudly.
//
// WIRING: a detached, throttled call inside the mail-check --inject path
// (cmd_mail.go, maybeSpawnLeaseHeartbeat) — the start-of-turn leg. gc
// installs no Stop hook anywhere, and this is the per-turn seam the managed
// overlays share: claude, codex, cursor, copilot, gemini, antigravity, kiro,
// opencode, mimocode, pi and omp all run `gc mail check --inject` (or the
// equivalent plugin call) before each turn. KNOWN GAP: the kimi overlay
// registers only a SessionStart hook (gc prime --hook) and no per-turn
// event, so an armed kimi seat's claims get NO turn-driven refresh; its
// leases lapse at the TTL like an unattended seat's. Stage 2 must not reap
// on expiry for a provider with no heartbeat seam (see SESSION-RUNTIME-012).

// hookHeartbeatTimeout bounds the whole run. The command is invoked detached
// from the turn (never synchronously — a bd invocation costs seconds and a
// board can hold many rows), so this budget protects the system from a stuck
// child, not the turn from the tick.
const hookHeartbeatTimeout = 120 * time.Second

// hookHeartbeatStore opens the bead store the calling seat's work lives in,
// resolved the same way the seat's own bd commands resolve it: the rig store
// when the working directory sits inside a rig, the city store otherwise.
// Portfolios spanning BOTH stores are a recorded stage-2 gap: rows in the
// other store are not reached from here. Overridable in tests.
var hookHeartbeatStore = func(ctx context.Context) (hookHeartbeatBeadStore, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if rig, ok := rigForDir(cfg, cityPath, cwd); ok {
		return scopedBdStoreForRig(ctx, cityPath, cfg, rig.Path)
	}
	return scopedBdStoreForCity(ctx, cityPath)
}

// hookHeartbeatBeadStore is the store capability set this command needs.
type hookHeartbeatBeadStore interface {
	List(beads.ListQuery) ([]beads.Bead, error)
	Heartbeat(id, actor string) error
}

var _ hookHeartbeatBeadStore = (*beads.BdStore)(nil)

// hookHeartbeatIdentities resolves the identifiers the calling session may
// vouch for RIGHT NOW: the current set (session bead id, session_name,
// configured_named_identity, current alias) plus every PRIOR alias from
// alias_history that no OTHER live session currently answers to (codex
// round-8 P1-b). A rebranded pool session keeps ownership of work assigned
// under its previous alias — orphan release deliberately skips that work
// (TestReleaseOrphanedPoolAssignments_SkipsLiveSessionAssignedByAliasHistory)
// — so a writer set that stops at the current alias lets exactly that work's
// lease expire while its owner is still taking turns. A prior alias that a
// later live session has taken is still excluded: vouching through it would
// keep the successor's claims looking alive after the successor dies.
// Overridable in tests. diag receives one line per excluded prior alias, so
// an alias the run declined to vouch through is never silent.
var hookHeartbeatIdentities = func(ctx context.Context, sessionID, instanceToken string, diag io.Writer) ([]string, error) {
	front, err := hookHeartbeatSessionFrontDoor(ctx)
	if err != nil {
		return nil, err
	}
	info, err := front.Get(sessionID)
	if err != nil {
		return nil, err
	}
	current, err := hookHeartbeatEligibleIdentities(info, instanceToken)
	if err != nil {
		return nil, err
	}
	return append(current, hookHeartbeatUnreusedPriorAliases(info, current, front.ResolveID, diag)...), nil
}

// hookHeartbeatSessionFrontDoor opens the session-class front door the
// heartbeat resolves its identities through, bound to the run's context
// (codex round-8 P2): sessionCurrentClaimFrontDoor's store runner is fixed to
// context.Background(), so a heartbeat whose session read stalled during a
// store outage would outlive hookHeartbeatTimeout and overlapping detached
// children would accumulate for the length of the outage. Overridable in
// tests.
var hookHeartbeatSessionFrontDoor = func(ctx context.Context) (*session.Store, error) {
	return sessionCurrentClaimFrontDoorContext(ctx)
}

// hookHeartbeatUnreusedPriorAliases returns the entries of info's alias
// history that the session may still vouch for: those already in current
// are dropped (already vouched), and each remaining alias is resolved
// against LIVE sessions through resolveLive (session.Store.ResolveID: exact
// bead id, then live session_name, then live current alias — never alias
// history, so a history-only match cannot answer here). An alias nobody
// live answers to, or that resolves back to this session, is kept. One a
// DIFFERENT live session answers to has been reused and is excluded; an
// ambiguous alias or a failed lookup is excluded too, because the writer
// side fails toward NOT vouching. Every exclusion is written to diag.
//
// The residual asymmetry with the orphan-release reader is deliberate and
// bounded: the reader attributes history-assigned work to this session
// whether or not a successor once held the alias; this writer refreshes it
// only while no live successor holds the alias. Work a since-closed
// successor left under the alias therefore reads as this session's and is
// refreshed as this session's — the same attribution the reader already
// makes, not a new one.
func hookHeartbeatUnreusedPriorAliases(info session.Info, current []string, resolveLive func(string) (string, error), diag io.Writer) []string {
	if diag == nil {
		diag = io.Discard
	}
	have := make(map[string]bool, len(current)+len(info.AliasHistory))
	for _, id := range current {
		have[id] = true
	}
	var kept []string
	for _, prior := range info.AliasHistory {
		prior = strings.TrimSpace(prior)
		if prior == "" || have[prior] {
			continue
		}
		have[prior] = true
		owner, err := resolveLive(prior)
		switch {
		case err == nil && strings.TrimSpace(owner) == strings.TrimSpace(info.ID):
			kept = append(kept, prior)
		case err == nil:
			fmt.Fprintf(diag, "gc hook heartbeat: prior alias %q is now answered to by live session %s; not vouching through it\n", prior, owner) //nolint:errcheck
		case errors.Is(err, session.ErrSessionNotFound):
			kept = append(kept, prior)
		default:
			fmt.Fprintf(diag, "gc hook heartbeat: prior alias %q: %v; not vouching through it\n", prior, err) //nolint:errcheck
		}
	}
	return kept
}

// hookHeartbeatEligibleIdentities fences the CURRENT write-authorizing identity
// set (prior aliases are decided separately, above) to the CURRENT incarnation of the session (codex round-6 P1). A provider
// process that survived a restart or adoption keeps GC_SESSION_ID but carries
// a stale GC_INSTANCE_TOKEN; resolving identities by ID alone would let it
// enumerate the replacement incarnation's identities and heartbeat that
// session's work indefinitely, masking the replacement's death. So the same
// closed-and-instance-token fence the claim path applies
// (hookClaimSessionEligibility) gates every heartbeat: a closed bead, a
// missing or superseded token, or a non-eligible state yields NO identities
// and an error the caller reports as a miss.
//
// The STATE rule differs from the claim path's (codex round-8 P1): a
// DRAINING session may not take new claims but is explicitly allowed to
// finish the work it holds (cmd_runtime_drain.go), and that work's lease must
// keep being refreshed for the length of the graceful-drain timeout or it
// expires mid-finish. So the closed and token checks are the claim path's
// verbatim, and the eligible state set is the claim path's plus draining.
func hookHeartbeatEligibleIdentities(info session.Info, instanceToken string) ([]string, error) {
	if info.Closed {
		return nil, errors.New("session is not heartbeat-eligible: session bead is closed (a stale incarnation must not refresh a successor's claims)")
	}
	storedToken := strings.TrimSpace(info.InstanceToken)
	if storedToken == "" || storedToken != strings.TrimSpace(instanceToken) {
		return nil, errors.New("session is not heartbeat-eligible: runtime instance token does not match the session bead (a stale incarnation must not refresh a successor's claims)")
	}
	switch state := session.State(strings.TrimSpace(info.MetadataState)); state {
	case session.StateNone, session.StateActive, session.StateAwake, session.StateCreating, session.StateStartPending, session.StateDraining:
		return session.CurrentAssigneeIdentities(info), nil
	default:
		return nil, fmt.Errorf("session is not heartbeat-eligible: session state %q holds no live work", state)
	}
}

// hookHeartbeatStartOffset picks where in the deduplicated row list a run
// starts beating (codex round-6 P2, sharpened in round 7). The rows are
// gathered in a deterministic order, and a board large enough for the
// sequential bd invocations to outrun hookHeartbeatTimeout would otherwise
// cancel the SAME tail on every run, so those claims could expire while the
// session keeps taking turns. A wall-clock offset (Unix() % n) repeats under a
// regular turn cadence and a persisted cursor would be a state file, so the
// start is drawn at random: every row is equally likely to lead each run,
// and no tail is starved by construction. Boards that need more than the
// budget every tick are stage 2's batching problem, not a scheduling one.
// Overridable in tests.
var hookHeartbeatStartOffset = func(n int) int {
	if n <= 1 {
		return 0
	}
	return rand.IntN(n)
}

func newHookHeartbeatCmd(stdout, stderr io.Writer) *cobra.Command {
	var strict bool
	var beadID string
	cmd := &cobra.Command{
		Use:   "heartbeat",
		Short: "Refresh the claim leases on this session's in-progress work",
		Long: `Refreshes the claim lease on every in_progress bead assigned to the calling
session under an identity it may vouch for right now: session bead id, session
name, configured named identity, current alias, and any prior alias that no
other live session currently answers to (a rebranded session keeps the work it
was assigned under its old name; an alias a later session took is excluded).
Each row is heartbeated under its own assignee spelling, because bd's owner
check is exact and a cross-spelling heartbeat is refused.

Intended to run detached from a per-turn hook event so leases track a session
that is still taking turns and expire when it stops. bd self-heals a missing
lease for the current assignee, so this both arms unleased claims and
refreshes armed ones.

By default this never exits nonzero: a hook leg must not fail the turn, so
every miss (no session identity, store trouble, a lease lost to another
owner) prints a diagnostic and exits 0. Pass --strict when the heartbeat
itself is the thing under test.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return exitForCode(cmdHookHeartbeat(beadID, strict, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&strict, "strict", false, "exit 1 when any heartbeat does not happen, instead of the lenient hook-leg default")
	cmd.Flags().StringVar(&beadID, "id", "", "heartbeat only this bead, under the ambient actor (canary/proof use)")
	return cmd
}

// cmdHookHeartbeat resolves the calling session's in_progress rows and
// heartbeats each under its own assignee spelling. Lenient exit-0-on-miss
// unless strict.
func cmdHookHeartbeat(beadID string, strict bool, stdout, stderr io.Writer) int {
	code := func(failed bool) int {
		if strict && failed {
			return 1
		}
		return 0
	}
	miss := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "gc hook heartbeat: "+format+"\n", a...) //nolint:errcheck
		return code(true)
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookHeartbeatTimeout)
	defer cancel()

	if beadID = strings.TrimSpace(beadID); beadID != "" {
		store, err := hookHeartbeatStore(ctx)
		if err != nil {
			return miss("opening store: %v", err)
		}
		if err := store.Heartbeat(beadID, ""); err != nil {
			return miss("%v", err)
		}
		fmt.Fprintf(stdout, "heartbeat %s\n", beadID) //nolint:errcheck
		return 0
	}

	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	if sessionID == "" {
		return miss("no session identity (set $GC_SESSION_ID) and no --id; nothing to heartbeat")
	}
	// The instance token is read here, in the command body, never in a
	// top-level initializer: GC_INSTANCE_TOKEN is a leak-vector variable and
	// the init-time guard (internal/testenv) forbids package-init reads.
	identities, err := hookHeartbeatIdentities(ctx, sessionID, strings.TrimSpace(os.Getenv("GC_INSTANCE_TOKEN")), stderr)
	if err != nil {
		return miss("resolving session identities: %v", err)
	}
	if len(identities) == 0 {
		return miss("session %s has no assignment identities; nothing to heartbeat", sessionID)
	}
	store, err := hookHeartbeatStore(ctx)
	if err != nil {
		return miss("opening store: %v", err)
	}

	// Union the rows across identities: several identities can name the same
	// bead, and one bead must be heartbeated once, under its own assignee.
	seen := make(map[string]bool)
	var beat, refused int
	var rows []beads.Bead
	for _, identity := range identities {
		// TierBoth: the durable-issue default (TierIssues) filters out
		// ephemeral rows, and an ephemeral in_progress bead's lease needs
		// beating exactly as much as a durable one's.
		listed, err := store.List(beads.ListQuery{Assignee: identity, Status: "in_progress", TierMode: beads.TierBoth})
		if err != nil {
			// A PartialResultError carries USABLE rows: one tier failed
			// after the other matched, or one entry failed to parse. The
			// healthy claims must still be refreshed — dropping them here
			// would let every lease under this identity lapse over a
			// single malformed row. Count the partial failure as a refusal
			// so it is never silent, then beat what came back.
			fmt.Fprintf(stderr, "gc hook heartbeat: listing %q: %v\n", identity, err) //nolint:errcheck
			refused++
			if !beads.IsPartialResult(err) {
				continue
			}
		}
		for _, row := range listed {
			if seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			rows = append(rows, row)
		}
	}
	// Rotate the start so a timeout never starves the same tail twice.
	if off := hookHeartbeatStartOffset(len(rows)); off > 0 && off < len(rows) {
		rows = append(rows[off:], rows[:off]...)
	}
	for _, row := range rows {
		if err := store.Heartbeat(row.ID, row.Assignee); err != nil {
			// COUNTABLE, never silent (hook-seam consult gap 4): the
			// per-row diagnostic is this leg's whole observability.
			fmt.Fprintf(stderr, "gc hook heartbeat: %v\n", err) //nolint:errcheck
			refused++
			continue
		}
		beat++
	}
	fmt.Fprintf(stdout, "heartbeat: %d refreshed, %d refused, session %s\n", beat, refused, sessionID) //nolint:errcheck
	if beat == 0 {
		// Nothing was refreshed: every list succeeded but matched no row (a
		// claim resolved in another store, an identity or routing regression
		// hiding it), or every row was refused. --strict exists to PROVE a
		// heartbeat happened, so a zero-refresh run is a strict miss with its
		// own diagnostic — never a false-green canary. Lenient stays exit 0.
		return miss("session %s refreshed nothing (%d refused); no live claim was heartbeated", sessionID, refused)
	}
	return code(refused > 0)
}
