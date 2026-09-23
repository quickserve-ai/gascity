package main

import (
	"context"
	"fmt"
	"io"
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
// session.CurrentAssigneeIdentities: the orphan-release reader's set MINUS
// alias_history, because a reader avoiding a wrong strip must cast wide while
// a writer vouching for liveness must not vouch through a name a later
// session may have reused (the asymmetry is documented on that function).
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

// hookHeartbeatIdentities resolves the identifiers the calling session
// answers to RIGHT NOW (session bead id, session_name,
// configured_named_identity, current alias) — deliberately WITHOUT
// alias_history, which orphan-release must include but a heartbeat must not:
// a prior alias can be reused by a later live session, and a heartbeat
// matched through history would keep the successor's claims looking alive
// after the successor dies. Overridable in tests.
var hookHeartbeatIdentities = func(sessionID string) ([]string, error) {
	front, err := hookCurrentSessionFrontDoor()
	if err != nil {
		return nil, err
	}
	info, err := front.Get(sessionID)
	if err != nil {
		return nil, err
	}
	return hookHeartbeatEligibleIdentities(info, os.Getenv("GC_INSTANCE_TOKEN"))
}

// hookHeartbeatEligibleIdentities fences the write-authorizing identity set
// to the CURRENT incarnation of the session (codex round-6 P1). A provider
// process that survived a restart or adoption keeps GC_SESSION_ID but carries
// a stale GC_INSTANCE_TOKEN; resolving identities by ID alone would let it
// enumerate the replacement incarnation's identities and heartbeat that
// session's work indefinitely, masking the replacement's death. So the same
// closed-and-instance-token fence the claim path applies
// (hookClaimSessionEligibility) gates every heartbeat: a closed bead, a
// missing or superseded token, or a non-eligible state yields NO identities
// and an error the caller reports as a miss.
func hookHeartbeatEligibleIdentities(info session.Info, instanceToken string) ([]string, error) {
	verdict, reason, _ := hookClaimSessionEligibility(info, strings.TrimSpace(instanceToken))
	if verdict != hookClaimSessionEligible {
		return nil, fmt.Errorf("session is not heartbeat-eligible: %s (a stale incarnation must not refresh a successor's claims)", reason)
	}
	return session.CurrentAssigneeIdentities(info), nil
}

// hookHeartbeatStartOffset picks where in the deduplicated row list a run
// starts beating (codex round-6 P2). The rows are gathered in a deterministic
// order, and a board large enough for the sequential bd invocations to
// outrun hookHeartbeatTimeout would otherwise cancel the SAME tail on every
// run, so those claims could expire while the session keeps taking turns.
// Rotating the start by wall-clock seconds makes each run begin elsewhere, so
// every row is refreshed within a few ticks. Overridable in tests.
var hookHeartbeatStartOffset = func(n int) int {
	if n <= 0 {
		return 0
	}
	return int(time.Now().Unix() % int64(n))
}

func newHookHeartbeatCmd(stdout, stderr io.Writer) *cobra.Command {
	var strict bool
	var beadID string
	cmd := &cobra.Command{
		Use:   "heartbeat",
		Short: "Refresh the claim leases on this session's in-progress work",
		Long: `Refreshes the claim lease on every in_progress bead assigned to the calling
session under an identity it answers to right now (session bead id, session
name, configured named identity, current alias — never alias history, which a
later session may have reused). Each row is heartbeated under its own assignee
spelling, because bd's owner check is exact and a cross-spelling heartbeat is
refused.

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
	identities, err := hookHeartbeatIdentities(sessionID)
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
