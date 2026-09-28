package main

// The closed-bead worktree reaper, moved off the controller tick
// (ga-yuiof4 item 3).
//
// # What was wrong
//
// reapClosedBeadWorktrees and cleanupClosedBeadAgentHomeWorktrees ran inline
// in the serial reconciler tick. Both read every rig bead store through
// context-free calls, and on 2026-09-27 those stores (native Dolt handles, some
// hub-backed) blocked on a store lock: one reap phase ran 45 minutes in a single
// tick. dispatchOrders runs earlier in the same serial tick, so the wedged pass
// held the NEXT tick's order dispatch, and no order fired city-wide for 48
// minutes. Even on an ordinary tick the phase cost 43-64s of the city's clock.
//
// # The lane
//
// The tick now only TRIGGERS a pass and never waits on one. A trigger either
// starts a pass on its own goroutine or, when one is still running, skips —
// single flight: never two passes at once and never a queue of them — and the
// skip lands in the tick's phase trace with the in-flight pass's age, so a
// wedged pass is visible instead of silently absorbed.
//
// Same shape as the other off-tick lanes in this package (route_recovery.go,
// detached_orphan_lane.go, completions_lane.go): the lane owns its cadence
// state behind a mutex, a single-flight latch admits one pass, the pass runs
// under safeTick so a panic is contained and logged, and it is scoped to the
// controller ctx. Unlike those lanes it has no poll loop of its own: its cadence
// IS the tick's (it ran once per tick before, and still runs at most once per
// tick), so the tick is the trigger.
//
// # What the pass owns, and what it snapshots
//
//   - Inputs are snapshotted on the tick goroutine at trigger time: the config
//     pointer (a reload replaces cr.cfg, it never mutates the old value), the
//     reap/dry-run flags, the rig store map, and the live-session dir set.
//   - Reaper-only state — the skip tracker and the bead-status memo — belongs
//     to the lane, and single flight hands it to exactly one pass at a time. The
//     handoff is ordered through lane.mu, so no pass ever sees another's
//     half-written tracker.
//   - Gate verdicts are as old as the pass, so each real removal is preceded
//     by a fresh re-verification (see the comment at the pre-removal wiring
//     in triggerWorktreeReaperPass), including the LIVE reap flag, read under
//     serviceStateMu, so a reload that disables reaping stops the next
//     removal of an in-flight pass.
//   - cr.rec and cr.stderr are shared with every other lane already;
//     events.Recorder implementations serialize Record.
//
// # Store handles
//
// A reload publishes new rig stores and closes the replaced handles 250ms later
// (scheduleCloseReplacedBeadStoreHandles); CloseStore on a native store is a
// one-way latch (gascity#3157). An inline pass could never straddle a tick-side
// reload; a background pass can. Every store the pass reads is therefore
// wrapped in a fence (reaperFencedStore) checked before AND after every read:
// the read is refused, or its answer discarded, once the controller ctx is
// done or the handle is no longer the one the controller state publishes for
// that rig. A refused read is an error, and every store read in the reaper
// fails CLOSED on error — a Get error skips the candidate (and, in the
// pre-removal re-check, protects it), a borrow-veto List error protects every
// remaining candidate in the rig, and cleanupClosedBeadAgentHomeWorktrees
// skips on a Get error — so a pass that
// outlives its handles protects everything for the rest of its run instead of
// acting on answers from a closed or replaced store. The next trigger snapshots
// the fresh handles.
//
// # Shutdown
//
// The pass goroutine is never joined. A pass blocked inside a context-free
// store call cannot be interrupted, so nothing on the shutdown or reload path
// waits for it; the fence makes any read it attempts after ctx is done fail
// closed, and process exit reaps the goroutine.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// worktreeReaperSlowPassThreshold is how old a pass must be before the lane
// says so on stderr: once when a trigger first finds it still in flight past
// this age, and once when a pass completes having taken at least this long.
// Ordinary passes are silent; the tick trace carries their numbers.
const worktreeReaperSlowPassThreshold = 2 * time.Minute

var (
	errReaperPassCanceled = errors.New("worktree reaper pass: controller context done (failing closed)")
	errReaperStoreRetired = errors.New("worktree reaper pass: rig store handle replaced by a reload since the pass started (failing closed)")
)

// worktreeReaperPassInput is everything one pass reads, snapshotted at trigger
// time on the tick goroutine. The pass touches no CityRuntime field.
type worktreeReaperPassInput struct {
	cityPath string
	cfg      *config.City
	// reapEnabled is real removal. When false the pass is dry-run (the trigger
	// only starts a pass when at least one of the two flags is on); real
	// removal supersedes dry-run when both are set, exactly as inline.
	reapEnabled bool
	// cachedStores are the fenced rig stores with pass-1 Gets memoized
	// through the lane's bead-status cache; rawStores are the fenced rig
	// stores without the memo, for the agent-home cleanup (which inline read
	// cr.rigBeadStores() uncached).
	cachedStores    map[string]beads.Store
	rawStores       map[string]beads.Store
	liveSessionDirs []string
	rec             events.Recorder
	skips           *reapSkipTracker
	stderr          io.Writer
	// pre is the pre-removal re-verification the reaper runs immediately
	// before each real removal; stillEnabled is its live reap-flag read, also
	// consulted before each agent-home reset. See reapPreRemoval.
	pre          *reapPreRemoval
	stillEnabled func() bool
}

// worktreeReaperPassResult is one completed pass, in the terms the tick trace
// reports.
type worktreeReaperPassResult struct {
	report          reapReport
	agentHomesRan   bool
	agentHomesReset int
	reapDuration    time.Duration
	agentDuration   time.Duration
}

// runWorktreeReaperPassFn is the pass the lane runs. Tests replace it to
// inject a pass that blocks or returns on command.
var runWorktreeReaperPassFn = runWorktreeReaperPass

// runWorktreeReaperPass is the work that used to run inline in the tick, in
// the same order and with the same flags.
func runWorktreeReaperPass(in worktreeReaperPassInput) worktreeReaperPassResult {
	var res worktreeReaperPassResult
	started := time.Now()
	res.report = reapClosedBeadWorktreesGuarded(in.cityPath, in.cfg, in.cachedStores, in.liveSessionDirs, !in.reapEnabled, in.rec, in.skips, in.stderr, in.pre)
	res.reapDuration = time.Since(started)
	// Agent-home worktree cleanup performs real removals, so it runs only
	// when real reaping is enabled — never under dry-run.
	if in.reapEnabled {
		started = time.Now()
		res.agentHomesReset = cleanupClosedBeadAgentHomeWorktreesGuarded(in.cityPath, in.cfg, in.rawStores, in.stderr, &agentHomeResetGuard{ctx: in.pre.ctx, stillEnabled: in.stillEnabled, startFence: in.pre.startFence})
		res.agentHomesRan = true
		res.agentDuration = time.Since(started)
	}
	return res
}

// worktreeReaperLane is the single-flight background lane for the reaper.
type worktreeReaperLane struct {
	mu sync.Mutex

	// inflight is the single-flight latch. It is set by a trigger that starts
	// a pass and cleared by that pass's goroutine when it returns.
	inflight      bool
	inflightSince time.Time
	inflightSeq   uint64
	// done is closed when the in-flight pass finishes (tests wait on it).
	done chan struct{}
	// slowLoggedSeq is the seq of the last in-flight pass the wedge line was
	// printed for, so a wedged pass is announced once, not on every tick.
	slowLoggedSeq uint64

	seq          uint64
	skippedTotal uint64

	// latestSessionDirs is the open-session working-dir set from the most
	// recent tick that reached the reap phase with a CLEAN session read —
	// published by every trigger, including one that skips because a pass is
	// in flight — so an in-flight pass's pre-removal check can see sessions
	// opened after it started without reading any store. A failed or partial
	// read never overwrites it; it clears latestSessionDirsValid instead, and
	// while that is false every pre-removal check protects. The next clean
	// read restores both.
	latestSessionDirs      []string
	latestSessionDirsValid bool

	// startFence is the session-start generation fence the controller's start
	// path and this lane's removals share. See sessionStartFence.
	startFence sessionStartFence

	lastDone     bool
	lastSeq      uint64
	lastDoneAt   time.Time
	lastDryRun   bool
	lastPanicked bool
	lastResult   worktreeReaperPassResult

	// Reaper-only state, handed to exactly one pass at a time by single
	// flight. statusCache is nil until first use and after a reload that
	// replaced the store backends (a same-named rig can be a different store).
	skips       *reapSkipTracker
	statusCache *beadStatusCache
}

func newWorktreeReaperLane() *worktreeReaperLane {
	return &worktreeReaperLane{skips: newReapSkipTracker()}
}

// worktreeReaperLaneOf returns this runtime's lane, creating it on first use so
// a directly-constructed CityRuntime (every test, every one-shot) needs no
// wiring.
func (cr *CityRuntime) worktreeReaperLaneOf() *worktreeReaperLane {
	cr.worktreeReaperOnce.Do(func() { cr.worktreeReaper = newWorktreeReaperLane() })
	return cr.worktreeReaper
}

// invalidateStatusCache drops the bead-status memo. Called by reload when the
// store backends were replaced. A pass already in flight keeps the memo it was
// handed — it also keeps the handles it was handed, and its fence refuses those
// once they are retired — so the memo and the stores it describes stay paired.
func (l *worktreeReaperLane) currentSessionDirs() ([]string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.latestSessionDirs...), l.latestSessionDirsValid
}

// publishSessionDirs records one tick's session read. Caller holds l.mu.
func (l *worktreeReaperLane) publishSessionDirsLocked(dirs []string, valid bool) {
	if !valid {
		l.latestSessionDirsValid = false
		return
	}
	l.latestSessionDirs = dirs
	l.latestSessionDirsValid = true
}

// sessionStartFenceOf returns the fence the controller's session-start path
// brackets every runtime start with (withSessionStartFence).
func (cr *CityRuntime) sessionStartFenceOf() *sessionStartFence {
	return &cr.worktreeReaperLaneOf().startFence
}

// sessionStartFence is the session-start generation fence between the
// controller's session starts and the reaper lane's removals (ga-yuiof4 item
// 3, Astra r2 finding 1). A snapshot cannot close the window between a pass's
// liveness scan and its removal — a tick may start a session in the tree in
// between — but exclusion can.
//
// Every in-process runtime creation is bracketed: beginStart (gen++,
// inflight++) before it and endStart (gen++, inflight--, deferred) after it,
// each a short critical section holding nothing else. The brackets:
//   - internal/session.Manager.startRuntime, the one place a Manager asks its
//     provider to start a runtime. The controller registers this fence for its
//     provider (registerSessionStartFence → session.RegisterStartFence), so
//     every Manager built on that provider is covered: the reconcile tick, the
//     control-dispatcher tick, and the in-process API server's session wakes
//     (its worker factory is built on cs.SessionProvider(), the same object).
//   - runFencedPreparedStartCandidate, around every reconciler start, sync or
//     async (async ones outlive the tick). It also covers the runtime-only
//     worker handle (internal/worker RuntimeHandle), which starts through the
//     provider without a Manager. Where both apply the start is bracketed
//     twice, which only over-protects.
//   - the launch-drift Relaunch in relaunchAgentForLaunchDrift.
//
// The source ratchet TestSessionStartFenceRatchet fails on any new
// runtime-creating call that is in none of these brackets and not allowlisted.
//
// A removal reads gen when its liveness scan STARTS (genAtScan). After every
// pre-removal read, it takes the lock, and removes only if gen == genAtScan
// and inflight == 0; nothing else runs inside the lock. The ordering argument:
//   - a start that began (or ended) after the scan started moved gen past
//     genAtScan, so a removal that takes the lock afterwards protects;
//   - a start still in flight holds inflight > 0: protect;
//   - a start whose bracket ENDED before the scan started had its process up
//     before the scan, so the scan sees it (liveness protects);
//   - a start that tries to begin while the removal holds the lock waits for
//     that one `git worktree remove` (at most reaperGitTimeout; the reset's
//     detach runs no hooks): the tree is gone before the session
//     starts, the same outcome as the old inline reaper, where removal always
//     preceded the next tick's starts.
//
// Starvation is acceptable: a busy start path protects removals, which fails
// safe, and the candidate is retried on the next pass.
//
// Not covered: runtime starts that do not go through the controller's start
// path (API-driven session creation, a `gc` CLI process starting a session
// itself). Those raced the inline reaper the same way; the liveness scan
// catches them once their process is up.
//
// A nil *sessionStartFence is a no-op fence: generation 0, never contended.
type sessionStartFence struct {
	mu       sync.Mutex
	gen      uint64
	inflight int
}

// BeginStart and EndStart make the fence a session.StartFence.
func (f *sessionStartFence) BeginStart() { f.beginStart() }

// EndStart ends a start bracket begun with BeginStart.
func (f *sessionStartFence) EndStart() { f.endStart() }

var _ sessionpkg.StartFence = (*sessionStartFence)(nil)

// startFenceRegistration is one provider this runtime registered its
// session-start fence for.
type startFenceRegistration struct {
	sp         runtime.Provider
	unregister func()
}

func sameRuntimeProvider(a, b runtime.Provider) bool {
	if a == nil || b == nil {
		return false
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	return a == b
}

// registerSessionStartFence brackets every runtime start any
// internal/session Manager makes through sp with this runtime's session-start
// fence, and reports whether this call added the registration (false when sp
// was already registered or cannot be keyed). Called on the run goroutine: at
// run start for cr.sp, and on reload for a replacement provider BEFORE it is
// published to the controller state (so no in-process API wake on it is
// unfenced). Registrations live until run() returns — see run() for why a
// reload never drops one.
func (cr *CityRuntime) registerSessionStartFence(sp runtime.Provider) bool {
	if sp == nil {
		return false
	}
	for _, reg := range cr.startFenceRegs {
		if sameRuntimeProvider(reg.sp, sp) {
			return false
		}
	}
	unregister, ok := sessionpkg.RegisterStartFence(sp, cr.sessionStartFenceOf())
	if !ok {
		fmt.Fprintf(cr.stderr, "%s: worktree reaper: session provider %T cannot carry the session-start fence; only reconciler starts are fenced\n", cr.logPrefix, sp) //nolint:errcheck // best-effort stderr
		return false
	}
	cr.startFenceRegs = append(cr.startFenceRegs, startFenceRegistration{sp: sp, unregister: unregister})
	return true
}

// unregisterSessionStartFence drops the registration for sp. Only a failed
// reload uses it, for a replacement provider it registered but never
// published — no consumer can hold a handle on that provider.
func (cr *CityRuntime) unregisterSessionStartFence(sp runtime.Provider) {
	kept := cr.startFenceRegs[:0]
	for _, reg := range cr.startFenceRegs {
		if sameRuntimeProvider(reg.sp, sp) {
			reg.unregister()
			continue
		}
		kept = append(kept, reg)
	}
	cr.startFenceRegs = kept
}

// retireSessionStartFencesExcept unregisters the fence from every provider but
// keep (nil keep retires all). run() calls it with nil on return; nothing
// retires a registration mid-run.
func (cr *CityRuntime) retireSessionStartFencesExcept(keep runtime.Provider) {
	kept := cr.startFenceRegs[:0]
	for _, reg := range cr.startFenceRegs {
		if keep != nil && sameRuntimeProvider(reg.sp, keep) {
			kept = append(kept, reg)
			continue
		}
		reg.unregister()
	}
	cr.startFenceRegs = kept
}

func (f *sessionStartFence) beginStart() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.gen++
	f.inflight++
	f.mu.Unlock()
}

func (f *sessionStartFence) endStart() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.gen++
	f.inflight--
	f.mu.Unlock()
}

// generation reads gen; a scan records it as it starts.
func (f *sessionStartFence) generation() uint64 {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen
}

// runIfQuiet runs fn under the fence iff no start began, ended or is in flight
// since genAtScan and the pass ctx is not done, and reports whether it ran. fn
// must be the removal alone, bounded by reaperGitTimeout: a start waiting in
// beginStart waits for it, and some starts wait on the controller run loop.
// ctx is re-checked inside the lock, so a pass that outlived the controller
// never starts a removal. A nil ctx is never done.
func (f *sessionStartFence) runIfQuiet(ctx context.Context, genAtScan uint64, fn func()) bool {
	if f == nil {
		if ctx != nil && ctx.Err() != nil {
			return false
		}
		fn()
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if f.gen != genAtScan || f.inflight > 0 {
		return false
	}
	fn()
	return true
}

// reaperGitTimeout bounds each git call the reaper runs inside the
// session-start fence (`git worktree remove`, the agent-home detach). The
// fence lock is held for the call, so an unbounded git — a hook blocked on a
// wedged store, say — would hold every session start behind it. A call that
// hits the deadline is a failed removal or reset, retried next pass.
const reaperGitTimeout = 2 * time.Minute

// reaperGitCtx derives the deadline for one fenced reaper git call from the
// pass ctx (nil means none).
func reaperGitCtx(passCtx context.Context) (context.Context, context.CancelFunc) {
	if passCtx == nil {
		passCtx = context.Background()
	}
	return context.WithTimeout(passCtx, reaperGitTimeout)
}

func (l *worktreeReaperLane) invalidateStatusCache() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.statusCache = nil
}

// worktreeReaperTrigger is what one tick's trigger did, for the tick trace.
type worktreeReaperTrigger struct {
	dryRun          bool
	started         bool
	skippedInflight bool
	inflightAge     time.Duration
	seq             uint64

	lastDone     bool
	lastSeq      uint64
	lastAge      time.Duration
	lastDryRun   bool
	lastPanicked bool
	lastResult   worktreeReaperPassResult
}

// fields renders the trigger for the tick's phase record. The last completed
// pass's numbers ride along under last_pass_*; last_pass_seq lets a reader
// count each pass once although several ticks may report it.
func (t worktreeReaperTrigger) fields() map[string]any {
	out := map[string]any{
		"lane":             "background",
		"dry_run":          t.dryRun,
		"triggered":        t.started,
		"skipped_inflight": t.skippedInflight,
	}
	if t.started {
		out["pass_seq"] = t.seq
	}
	if t.skippedInflight {
		out["inflight_pass_seq"] = t.seq
		out["inflight_age_ms"] = t.inflightAge.Milliseconds()
	}
	if t.lastDone {
		out["last_pass_seq"] = t.lastSeq
		out["last_pass_age_ms"] = t.lastAge.Milliseconds()
		out["last_pass_dry_run"] = t.lastDryRun
		if t.lastPanicked {
			// A panicked pass returned no result: its counts are unknown, and
			// the zero result finish stored for it must not read as observed.
			out["last_pass_panicked"] = true
		} else {
			out["last_pass_reaped"] = len(t.lastResult.report.Reaped)
			out["last_pass_protected"] = len(t.lastResult.report.Protected)
			out["last_pass_reap_ms"] = t.lastResult.reapDuration.Milliseconds()
			if t.lastResult.agentHomesRan {
				out["last_pass_agent_homes_reset"] = t.lastResult.agentHomesReset
				out["last_pass_agent_homes_ms"] = t.lastResult.agentDuration.Milliseconds()
			}
		}
	}
	return out
}

// triggerWorktreeReaperPass starts a reaper pass in the background unless one
// is already running. It never blocks on a pass. The caller has already
// established that reaping or dry-run is enabled.
//
// It must be called on the tick goroutine: it reads cr.cfg and the rig store
// map the way every other tick phase does.
func (cr *CityRuntime) triggerWorktreeReaperPass(ctx context.Context, cfg *config.City, reapEnabled bool, sessionBeads *sessionBeadSnapshot) worktreeReaperTrigger {
	lane := cr.worktreeReaperLaneOf()
	now := time.Now()
	// Computed before the single-flight check so a skipping trigger still
	// publishes it to the in-flight pass (see latestSessionDirs). A nil
	// snapshot is a failed session read (loadSessionBeadSnapshot returns nil
	// on error); a snapshot carrying a LoadError is a degraded one. Neither
	// may erase the last clean publication.
	liveSessionDirs := liveSessionWorktreeDirs(sessionBeads)
	sessionsValid := sessionBeads != nil && sessionBeads.LoadError() == nil

	lane.mu.Lock()
	lane.publishSessionDirsLocked(liveSessionDirs, sessionsValid)
	trig := worktreeReaperTrigger{
		dryRun:       !reapEnabled,
		lastDone:     lane.lastDone,
		lastSeq:      lane.lastSeq,
		lastDryRun:   lane.lastDryRun,
		lastPanicked: lane.lastPanicked,
		lastResult:   lane.lastResult,
	}
	if lane.lastDone {
		trig.lastAge = now.Sub(lane.lastDoneAt)
	}
	if lane.inflight {
		lane.skippedTotal++
		trig.skippedInflight = true
		trig.seq = lane.inflightSeq
		trig.inflightAge = now.Sub(lane.inflightSince)
		announce := trig.inflightAge >= worktreeReaperSlowPassThreshold && lane.slowLoggedSeq != lane.inflightSeq
		if announce {
			lane.slowLoggedSeq = lane.inflightSeq
		}
		lane.mu.Unlock()
		if announce {
			fmt.Fprintf(cr.stderr, "%s: worktree reaper: pass %d still running after %s; skipping new passes until it returns (a store call may be wedged)\n", //nolint:errcheck // best-effort stderr
				cr.logPrefix, trig.seq, trig.inflightAge.Round(time.Second))
		}
		return trig
	}
	lane.seq++
	seq := lane.seq
	lane.inflight = true
	lane.inflightSince = now
	lane.inflightSeq = seq
	done := make(chan struct{})
	lane.done = done
	if lane.statusCache == nil {
		lane.statusCache = newBeadStatusCache(reapBeadStatusCacheTTL)
	}
	statusCache := lane.statusCache
	skips := lane.skips
	lane.mu.Unlock()
	trig.started = true
	trig.seq = seq

	// What is fresh and what is not. A pass can run as long as its slowest
	// store call, and ticks keep running (and starting sessions) meanwhile,
	// so the pass's GATE verdicts can be stale by the time a candidate is
	// removed:
	//   - liveSessionDirs is THIS tick's session snapshot, fixed for the pass.
	//   - the process-table cwd scan is gathered once per pass, lazily, at the
	//     first candidate to reach the liveness gate
	//     (reapClosedBeadWorktreesGuarded's liveness closure).
	//   - the pass-1 bead status may be a memo hit up to reapBeadStatusCacheTTL
	//     old; the borrow-veto List is read uncached, once per rig per pass.
	// So immediately before EACH removal the reaper re-verifies (reapPreRemoval,
	// preRemovalReason in bead_worktree_reaper.go): an uncached fenced Get
	// that must still say closed; a process scan that STARTED no more than
	// reapPreRemovalLivenessMaxAge before the decision (re-gathered if older;
	// still too old when it returns protects); the pass's session dirs PLUS
	// the latest set any tick has published to the lane since
	// (latestSessionDirs — read without touching a store; the live session
	// snapshot itself is a store read that could block, so it is not
	// consulted; a degraded publication protects everything); and the live
	// reap flag. Any error or indeterminate answer protects. Then the removal
	// runs under the session-start fence (sessionStartFence): published
	// session dirs are taken BEFORE the tick's reconcile starts sessions, so
	// they cannot see a start that lands between the scan and the removal —
	// the fence can, and that is what closes that window.
	//
	// NOT re-verified before removal: the borrow-veto scan (a full rig List
	// per candidate would multiply the pass's heaviest read). A different bead
	// that starts borrowing the tree mid-pass is caught when its session's
	// process has a cwd in the tree (the fresh scan) or its session is in a
	// tick's published dirs; one that has done neither yet is the residual.
	rawStores, cachedStores := cr.fencedReaperStores(ctx, statusCache)
	stillEnabled := cr.reapStillEnabled
	in := worktreeReaperPassInput{
		cityPath:        cr.cityPath,
		cfg:             cfg,
		reapEnabled:     reapEnabled,
		cachedStores:    cachedStores,
		rawStores:       rawStores,
		liveSessionDirs: liveSessionDirs,
		rec:             cr.rec,
		skips:           skips,
		stderr:          cr.stderr,
		stillEnabled:    stillEnabled,
		pre: &reapPreRemoval{
			ctx:                ctx,
			freshStores:        rawStores,
			stillEnabled:       stillEnabled,
			currentSessionDirs: lane.currentSessionDirs,
			livenessMaxAge:     reapPreRemovalLivenessMaxAge,
			startFence:         &lane.startFence,
		},
	}
	go func() {
		// close(done) is registered first so it runs last, after finish has
		// cleared the latch: a waiter woken by done sees the lane idle.
		defer close(done)
		started := time.Now()
		var res worktreeReaperPassResult
		panicked := cr.safeTick(func() { res = runWorktreeReaperPassFn(in) }, "worktree-reaper")
		took := time.Since(started)
		lane.finish(seq, !reapEnabled, res, panicked, time.Now())
		if took >= worktreeReaperSlowPassThreshold {
			counts := fmt.Sprintf("reaped=%d protected=%d", len(res.report.Reaped), len(res.report.Protected))
			if panicked {
				counts = "panicked, counts unknown"
			}
			fmt.Fprintf(cr.stderr, "%s: worktree reaper: pass %d took %s (%s dry_run=%t)\n", //nolint:errcheck // best-effort stderr
				cr.logPrefix, seq, took.Round(time.Second), counts, !reapEnabled)
		}
	}()
	return trig
}

// reapStillEnabled reads the LIVE real-reap flag from the published config,
// under the lock a reload writes it with, so it is safe from the pass
// goroutine. A reload that turns real reaping off (or leaves only dry-run)
// stops an in-flight pass's next removal.
func (cr *CityRuntime) reapStillEnabled() bool {
	cfg := cr.serviceConfigSnapshot()
	return cfg != nil && cfg.Daemon.AutoReapClosedBeadWorktreesEnabled()
}

// finish records a completed pass and releases the single-flight latch. A
// panicked pass records the zero result; fields() reports no counts for it.
func (l *worktreeReaperLane) finish(seq uint64, dryRun bool, res worktreeReaperPassResult, panicked bool, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inflight = false
	l.lastDone = true
	l.lastSeq = seq
	l.lastDoneAt = at
	l.lastDryRun = dryRun
	l.lastPanicked = panicked
	l.lastResult = res
}

// fencedReaperStores snapshots the rig store map and wraps every handle in the
// reaper fence (see the file comment). cachedStores additionally memoizes
// pass-1 Gets through statusCache; the fence sits outermost so even a memo hit
// is refused once the pass has outlived its handles.
//
// residency:allow — wraps, handle for handle, the same fail-closed rig-store
// enumeration the inline reap phase consumed (it moved here off the tick); it
// resolves no residency and adds, drops or reorders no leg.
func (cr *CityRuntime) fencedReaperStores(ctx context.Context, statusCache *beadStatusCache) (rawStores, cachedStores map[string]beads.Store) {
	rigStores := cr.rigBeadStores() // residency:allow — the same fail-closed rig-store enumeration the inline reap phase used; resolves no residency
	cs := cr.cs
	rawStores = make(map[string]beads.Store, len(rigStores))
	cachedStores = make(map[string]beads.Store, len(rigStores))
	for rigName, rigStore := range rigStores {
		if rigStore == nil {
			rawStores[rigName] = nil
			cachedStores[rigName] = nil
			continue
		}
		fence := reaperStoreFence(ctx, cs, rigName, rigStore)
		rawStores[rigName] = &reaperFencedStore{Store: rigStore, fence: fence}
		cachedStores[rigName] = &reaperFencedStore{Store: statusCache.wrap(rigName, rigStore), fence: fence}
	}
	return rawStores, cachedStores
}

// reaperStoreFence returns the check a fenced store runs before every read.
//
// With controller state, a reload swaps cs.beadStores and closes the replaced
// handles shortly after, so the check is pointer identity against what cs
// publishes now. Without controller state (standalone runtimes), a reload
// rebuilds cr.standaloneRigStores but never closes the old handles, so there
// is nothing to fence beyond ctx — and reading that field from the pass
// goroutine would race the tick's reload.
func reaperStoreFence(ctx context.Context, cs *controllerState, rigName string, handle beads.Store) func() error {
	handleKey, keyed := storePointerKey(handle)
	return func() error {
		if ctx != nil && ctx.Err() != nil {
			return errReaperPassCanceled
		}
		if cs == nil {
			return nil
		}
		if !keyed {
			// Identity cannot be established, so retirement cannot be ruled
			// out: fail closed.
			return errReaperStoreRetired
		}
		current := cs.BeadStore(rigName)
		currentKey, ok := storePointerKey(current)
		if !ok || currentKey != handleKey {
			return errReaperStoreRetired
		}
		return nil
	}
}

// reaperFencedStore refuses the two reads the reaper issues — Get (pass-1
// bead status, pre-removal re-check, agent-home bead status) and List (the
// borrow-veto scan) — once its fence trips. The fence is checked BEFORE the
// call and again AFTER it returns: a call that entered the handle, blocked
// across a reload or shutdown, and then came back is discarded — its answer
// may have come from a handle that has since been retired and closed — and the
// fence error is returned in its place. Every other method passes through; the
// reaper calls none.
type reaperFencedStore struct {
	beads.Store
	fence func() error
}

func (s *reaperFencedStore) Get(id string) (beads.Bead, error) {
	if err := s.fence(); err != nil {
		return beads.Bead{}, err
	}
	bead, err := s.Store.Get(id)
	if fenceErr := s.fence(); fenceErr != nil {
		return beads.Bead{}, fenceErr
	}
	return bead, err
}

func (s *reaperFencedStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if err := s.fence(); err != nil {
		return nil, err
	}
	rows, err := s.Store.List(query)
	if fenceErr := s.fence(); fenceErr != nil {
		return nil, fenceErr
	}
	return rows, err
}
