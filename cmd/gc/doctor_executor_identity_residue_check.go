package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// executorIdentityResidueCheck is a gc doctor check that finds and clears
// stale executor-identity stamp residue left on beads: gc.session_name,
// gc.work_dir, and the legacy work_dir metadata keys can survive after a
// bead moves on from the executor that stamped them (design ref
// ga-cm2o5t.1, PR #6099; amended scope ga-6af29d decision 1).
//
// A rig store may be a hub SHARED with other towns, so this city's config is
// not the authority on every bead the check can list. Three rules keep it to
// the beads it can actually judge (ga-n2f1ph item 2; on 2026-09-22 a --fix run
// blanked ten live stamps on the shared qcore store -- other towns' routes,
// a cert-wait park held by a running session, a local bead held by a live
// session, and work_dir pointers to worktrees still on disk):
//
//  1. A bead whose gc.routed_to is not a route THIS city configures (a
//     foreign town's agent, or a pseudo-route such as cert-wait, human, or
//     admission) is never judged. An empty route keeps its prior handling.
//  2. A gc.session_name naming the runtime of an OPEN session bead is
//     legitimate whatever the route says. If the open sessions cannot be
//     listed, session_name findings are reported as unconfirmed and Fix
//     never clears that key.
//  3. A work_dir disagreement whose gc.work_dir or legacy work_dir path still
//     exists on disk is not residue: that path may be the only pointer to
//     unpushed work.
//  4. A stat here proves a path absent only on THIS machine. A work_dir
//     disagreement is cleared only when the bead is provably this city's: a
//     locally configured route, and both paths absolute and under this
//     city's root or one of its rig paths. Otherwise (an empty route, a
//     relative, ~ or $VAR spelling, or another machine's path) it is
//     reported and never cleared (#123 review, 2026-09-23).
type executorIdentityResidueCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
	// loadOpenSessionNames returns the runtime name of every open session
	// bead in the session-class store. Injectable for tests; any error makes
	// every session_name finding unconfirmed (rule 2 fails closed).
	loadOpenSessionNames func(sessionStore beads.Store) (map[string]struct{}, error)
	// statPath stats a work_dir path (rule 3). Injectable so tests never touch
	// the real filesystem.
	statPath func(string) (os.FileInfo, error)
}

func newExecutorIdentityResidueCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *executorIdentityResidueCheck {
	return &executorIdentityResidueCheck{
		cfg:                  cfg,
		cityPath:             cityPath,
		newStore:             newStore,
		loadOpenSessionNames: residueOpenSessionRuntimeNames,
		statPath:             os.Stat,
	}
}

func (c *executorIdentityResidueCheck) Name() string { return "executor-identity-residue" }

func (c *executorIdentityResidueCheck) CanFix() bool { return true }

type executorIdentityResidueFinding struct {
	label  string
	store  beads.Store
	beadID string
	// keys is the set of metadata keys the triggering condition(s) actually
	// named. Fix() clears exactly these keys -- never a fixed superset --
	// so a trigger that did not fire can never lose state it never
	// inspected.
	keys []string
	// unconfirmed names keys a trigger would have fired on but could not
	// confirm (today only gc.session_name, when the open session beads could
	// not be listed). They are reported, never cleared.
	unconfirmed []string
	// reportOnly names work_dir keys whose disagreement this machine cannot
	// judge because it cannot prove the bead is local (rule 4). They are
	// reported, never cleared.
	reportOnly []string
	// judge is the verdict context this finding was judged against (the
	// executor-identity index, the open-session set, the stat), carried so
	// Fix() can re-run the same predicate on the live bead without rebuilding
	// a different context and reaching a different verdict than the one that
	// produced the finding.
	judge *executorIdentityResidueJudge
}

func (f executorIdentityResidueFinding) describe() string {
	var parts []string
	if len(f.keys) > 0 {
		parts = append(parts, fmt.Sprintf("carries stale executor-identity stamp residue (%s)", strings.Join(f.keys, ", ")))
	}
	if len(f.unconfirmed) > 0 {
		parts = append(parts, fmt.Sprintf("may carry stale %s, UNCONFIRMED (open session beads could not be listed: %v); gc doctor --fix will not clear it",
			strings.Join(f.unconfirmed, ", "), f.judge.openSessions.err))
	}
	if len(f.reportOnly) > 0 {
		parts = append(parts, fmt.Sprintf("has disagreeing %s that this machine cannot judge (empty route, or a path not under this city's root or rig paths); REPORTED ONLY, gc doctor --fix never clears it",
			strings.Join(f.reportOnly, ", ")))
	}
	return fmt.Sprintf("%s bead %s %s", f.label, f.beadID, strings.Join(parts, "; "))
}

func (c *executorIdentityResidueCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	findings, skipped := c.collect()
	var confirmed, unconfirmed, reportOnly int
	for _, f := range findings {
		if len(f.keys) > 0 {
			confirmed++
		}
		if len(f.unconfirmed) > 0 {
			unconfirmed++
		}
		if len(f.reportOnly) > 0 {
			reportOnly++
		}
	}
	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no stale executor-identity stamp residue found")
	}
	details := make([]string, 0, len(findings)+len(skipped))
	for _, f := range findings {
		details = append(details, f.describe())
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("executor-identity-residue check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	var summary, hints []string
	if confirmed > 0 {
		summary = append(summary, fmt.Sprintf("%d bead(s) carry stale executor-identity stamp residue", confirmed))
		hints = append(hints, "run gc doctor --fix to clear the stale gc.session_name/gc.work_dir/work_dir stamps")
	}
	if unconfirmed > 0 {
		summary = append(summary, fmt.Sprintf("%d gc.session_name finding(s) unconfirmed: open session beads could not be listed", unconfirmed))
		hints = append(hints, "fix session bead store access (--fix will not clear an unconfirmed gc.session_name)")
	}
	if reportOnly > 0 {
		summary = append(summary, fmt.Sprintf("%d work_dir finding(s) reported only: locality not provable on this machine", reportOnly))
		hints = append(hints, "review report-only work_dir stamps from the machine that owns them (--fix never clears them)")
	}
	if len(skipped) > 0 {
		summary = append(summary, fmt.Sprintf("%d scope(s) skipped", len(skipped)))
		hints = append(hints, "fix skipped store access")
	}
	return warnCheck(c.Name(),
		strings.Join(summary, "; "),
		strings.Join(hints, ", ")+", then rerun gc doctor",
		details)
}

func (c *executorIdentityResidueCheck) Fix(_ *doctor.CheckContext) error {
	findings, skipped := c.collect()
	var errs []error
	unconfirmed := 0
	for _, f := range findings {
		if len(f.unconfirmed) > 0 {
			unconfirmed++
		}
		if len(f.keys) == 0 {
			continue
		}
		// Re-read the bead immediately before writing. collect()'s listing is
		// a snapshot, and a worker -- usually in another process -- may have
		// claimed, closed, or re-routed the bead in the window since. A claim
		// atomically flips it open->in_progress and consumes gc.routed_to, so
		// clearing the snapshot's keys would strip identity metadata off work
		// in flight. Recompute the predicate on the live row and clear only
		// the keys that still fire; a bead that no longer qualifies is skipped
		// silently, the same guard sweepDetachedHandoffOrphans applies before
		// its own write. An unconfirmed key is never among the keys cleared.
		live, getErr := f.store.Get(f.beadID)
		if getErr != nil {
			errs = append(errs, fmt.Errorf("%s bead %s: re-read before clearing executor-identity stamp: %w", f.label, f.beadID, getErr))
			continue
		}
		liveKeys, _, _ := f.judge.staleKeys(live)
		if len(liveKeys) == 0 {
			continue
		}
		clearKVs := make(map[string]string, len(liveKeys))
		for _, key := range liveKeys {
			clearKVs[key] = ""
		}
		if err := f.store.SetMetadataBatch(f.beadID, clearKVs); err != nil {
			errs = append(errs, fmt.Errorf("%s bead %s: clear executor-identity stamp: %w", f.label, f.beadID, err))
		}
	}
	if unconfirmed > 0 {
		var loadErr error
		for _, f := range findings {
			if len(f.unconfirmed) > 0 {
				loadErr = f.judge.openSessions.err
				break
			}
		}
		errs = append(errs, fmt.Errorf("executor-identity-residue left %d unconfirmed gc.session_name stamp(s) uncleared: open session beads could not be listed: %w", unconfirmed, loadErr))
	}
	if len(skipped) > 0 {
		errs = append(errs, fmt.Errorf("executor-identity-residue skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; ")))
	}
	return errors.Join(errs...)
}

func (c *executorIdentityResidueCheck) collect() (findings []executorIdentityResidueFinding, skipped []string) {
	if c.newStore == nil {
		return nil, nil
	}
	type residueScope struct {
		label string
		store beads.Store
	}
	var scopes []residueScope
	var cityStore beads.Store
	if strings.TrimSpace(c.cityPath) != "" {
		store, err := c.newStore(c.cityPath)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("city skipped: opening bead store: %v", err))
		} else {
			cityStore = store
			scopes = append(scopes, residueScope{label: "city", store: store})
		}
	}
	if c.cfg != nil {
		suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
		for _, rig := range c.cfg.Rigs {
			if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			label := "rig " + rig.Name
			store, err := c.newStore(rig.Path)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", label, err))
				continue
			}
			scopes = append(scopes, residueScope{label: label, store: store})
		}
	}
	if len(scopes) == 0 {
		return nil, skipped
	}

	// Session beads belong to the session coordination class, which resolves to
	// the city store by default and follows a [beads.classes.sessions]
	// relocation otherwise -- never to a rig store. Build the executor-identity
	// index from that one store and share it with every scope, because a rig
	// store holds the claimed WORK beads this check inspects but none of the
	// session beads that say which identities legitimately act for a route.
	// Indexing each scope from its own listing leaves every rig with an empty
	// index, so every pool-slot and alias stamp on rig work reads as a re-route
	// hazard -- exactly the false positive the pool-instance and alias
	// stand-downs exist to prevent.
	//
	// Without a usable index this check cannot tell residue from a legitimate
	// identity, and its Fix() deletes state. So an index that cannot be built
	// stands every scope down rather than scanning blind.
	if cityStore == nil {
		skipped = append(skipped, "all scopes skipped: session-identity index unavailable: city bead store did not open")
		return nil, skipped
	}
	sessionStore := cliSessionStore(cityStore, c.cfg, c.cityPath)
	sessionIdentities, err := buildExecutorRouteIdentityIndex(sessionStore)
	if err != nil {
		skipped = append(skipped, fmt.Sprintf("all scopes skipped: building session-identity index: %v", err))
		return nil, skipped
	}

	// The open-session set (rule 2) is shared by every scope and loaded at
	// most once per collect(), lazily: only a session_name stamp that every
	// other rule already calls stale pays for the listing.
	openSessions := &residueOpenSessionSet{load: func() (map[string]struct{}, error) {
		if c.loadOpenSessionNames == nil {
			return nil, errors.New("no open-session loader configured")
		}
		return c.loadOpenSessionNames(sessionStore)
	}}

	for _, sc := range scopes {
		identities := sessionIdentities
		// Union in a DISTINCT scope store's own session beads, so a rig that
		// does hold session records still contributes them. Interface identity
		// is the right test -- production stores are pointer-backed
		// CachingStores -- and it keeps the default single-store city from
		// scanning the same rows twice. The union is built into the scope's own
		// index rather than into the shared one, so one scope's session beads
		// can never leak into the next scope's verdict.
		if sc.store != sessionStore {
			scopeIdentities, idxErr := buildExecutorRouteIdentityIndex(sc.store)
			if idxErr != nil {
				skipped = append(skipped, fmt.Sprintf("%s skipped: listing session beads: %v", sc.label, idxErr))
				continue
			}
			scopeIdentities.backfill(sessionIdentities)
			identities = scopeIdentities
		}
		judge := &executorIdentityResidueJudge{
			cfg:          c.cfg,
			cityPath:     c.cityPath,
			identities:   identities,
			openSessions: openSessions,
			statPath:     c.statPath,
		}
		scopeFindings, listErr := c.collectStoreFindings(sc.store, sc.label, judge)
		findings = append(findings, scopeFindings...)
		if listErr != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", sc.label, listErr))
		}
	}
	return findings, skipped
}

func (c *executorIdentityResidueCheck) collectStoreFindings(store beads.Store, label string, judge *executorIdentityResidueJudge) ([]executorIdentityResidueFinding, error) {
	items, err := store.List(beads.ListQuery{Status: "open", AllowScan: true, Live: true})
	if err != nil {
		return nil, err
	}
	var findings []executorIdentityResidueFinding
	for _, bd := range items {
		keys, unconfirmed, reportOnly := judge.staleKeys(bd)
		if len(keys) == 0 && len(unconfirmed) == 0 && len(reportOnly) == 0 {
			continue
		}
		findings = append(findings, executorIdentityResidueFinding{label: label, store: store, beadID: bd.ID, keys: keys, unconfirmed: unconfirmed, reportOnly: reportOnly, judge: judge})
	}
	return findings, nil
}

// residueOpenSessionSet memoizes one listing of the open session beads'
// runtime names. err is sticky: a failed load is never retried within the
// same collect(), so every verdict in one run sees the same answer.
type residueOpenSessionSet struct {
	load   func() (map[string]struct{}, error)
	loaded bool
	names  map[string]struct{}
	err    error
}

func (s *residueOpenSessionSet) get() (map[string]struct{}, error) {
	if s == nil {
		return nil, errors.New("open-session set not configured")
	}
	if !s.loaded {
		s.loaded = true
		if s.load == nil {
			s.err = errors.New("open-session set not configured")
		} else {
			s.names, s.err = s.load()
			if s.err == nil && s.names == nil {
				s.names = map[string]struct{}{}
			}
		}
	}
	return s.names, s.err
}

// residueOpenSessionRuntimeNames returns the runtime (tmux) name of every
// session bead in sessStore that is not closed: Info.SessionName (the
// session_name metadata when set, otherwise the s-<id> name the session
// manager starts such a bead under) plus the raw SessionNameMetadata. Any
// listing error, including a partial result, is returned: an incomplete set
// would let a live session's stamp read as residue.
func residueOpenSessionRuntimeNames(sessStore beads.Store) (map[string]struct{}, error) {
	if sessStore == nil {
		return nil, errors.New("no session bead store")
	}
	infos, err := loadOpenSessionInfos(sessStore)
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		if info.Closed {
			continue
		}
		if name := strings.TrimSpace(info.SessionName); name != "" {
			names[name] = struct{}{}
		}
		if name := strings.TrimSpace(info.SessionNameMetadata); name != "" {
			names[name] = struct{}{}
		}
	}
	return names, nil
}

// executorRouteIdentityIndex maps a route to the full set of executor
// identities that legitimately act for it. The base session name a plain
// SessionNameFor(route) encoding would produce is only one member of that
// set when the route runs a pool — each pool slot mints its own concrete
// session name (sessionBeadIdentifier semantics) while still belonging to
// the same route (retiredSessionFallbackRoute semantics). Built fresh from
// the session-class store's session beads on every check run, never
// persisted, so it always reflects the pool's current membership.
type executorRouteIdentityIndex map[string]map[string]struct{}

// add records identity as a legitimate executor for route, ignoring a pair
// with an empty half.
func (idx executorRouteIdentityIndex) add(route, identity string) {
	if route == "" || identity == "" {
		return
	}
	if idx[route] == nil {
		idx[route] = make(map[string]struct{})
	}
	idx[route][identity] = struct{}{}
}

// backfill copies every route/identity pair from other that idx does not
// already carry, mirroring pool_detached_orphan_sweep.go's
// detachedOrphanRouteIndex.backfill. Membership here is a set per route
// rather than a single value, so the union is taken pair by pair: a route
// both indexes know keeps both their identities instead of one shadowing
// the other.
func (idx executorRouteIdentityIndex) backfill(other executorRouteIdentityIndex) {
	for route, identities := range other {
		for identity := range identities {
			idx.add(route, identity)
		}
	}
}

// buildExecutorRouteIdentityIndex indexes store's session beads by route
// (retiredSessionFallbackRoute) and identity (sessionBeadIdentifier), the
// inverse direction of pool_detached_orphan_sweep.go's
// detachedOrphanRouteIndex (session_name -> route, single-valued).
//
// Rows come from the session front door's ListAll, the canonical enumeration:
// it unions the type leg with the label leg, so a crash- or migration-damaged
// session bead that kept its gc:session label but lost its type still
// contributes its identity. Closed session beads are included for the same
// reason the sibling index includes them — the worker session is usually
// already gone by the time a sweep runs, and dropping its bead would make
// every stamp it left read as residue. Route and identity are read off the
// typed Info projection via the retiredSessionFallbackRouteInfo /
// sessionBeadIdentifierInfo mirrors, which are byte-identical to their raw-bead
// forms, so no session bead is cracked open here.
//
// A partial listing yields usable rows and is used as-is; only a hard error
// is returned, because an empty index cannot distinguish residue from a
// legitimate identity and this check's Fix() deletes state.
func buildExecutorRouteIdentityIndex(store beads.Store) (executorRouteIdentityIndex, error) {
	idx := make(executorRouteIdentityIndex)
	all, listErr := sessionFrontDoor(store).ListAll(session.ListAllOptions{IncludeClosed: true})
	if listErr != nil && !beads.IsPartialResult(listErr) {
		return nil, fmt.Errorf("listing session beads: %w", listErr)
	}
	for _, info := range all {
		idx.add(strings.TrimSpace(retiredSessionFallbackRouteInfo(info)), strings.TrimSpace(sessionBeadIdentifierInfo(info)))
	}
	return idx, nil
}

func (idx executorRouteIdentityIndex) legitimate(route, identity string) bool {
	if route == "" || identity == "" {
		return false
	}
	_, ok := idx[route][identity]
	return ok
}

// executorIdentityResidueJudge is everything one verdict reads besides the
// bead itself. One judge is built per scope in collect() and carried on each
// finding, so Fix() re-evaluates a live bead against the same inputs.
type executorIdentityResidueJudge struct {
	cfg        *config.City
	cityPath   string
	identities executorRouteIdentityIndex
	// openSessions is shared by every scope's judge in one collect().
	openSessions *residueOpenSessionSet
	statPath     func(string) (os.FileInfo, error)
}

// staleKeys reports which metadata keys on bd carry stale executor-identity
// stamp residue (keys, which Fix() may clear), and which keys a trigger would
// have named but could not confirm (unconfirmed, which Fix() never clears).
// Closed and in_progress beads are always out of scope, as is workflow
// topology (a run root, scope latch, or formula spec is never itself claimed
// — only its descendant steps are — so a completed step's visibility stamp
// copied onto the root must not read as residue even when it no longer
// matches the root's own gc.routed_to).
//
// Rule 1 (see executorIdentityResidueCheck): a bead with a non-empty
// gc.routed_to that this city does not configure is out of scope entirely.
// On a store shared with other towns, a foreign route's stamps name that
// town's sessions, and a pseudo-route (cert-wait, human, admission) parks a
// bead whose stamp still names its live holder; neither can be judged
// against this city's config.
//
// Two independent triggers follow, each contributing only the key(s) it
// actually fired on to the result — see staleWorkDirStamp and
// staleSessionNameStamp for the trigger conditions and stand-downs. Keeping
// the two disjoint is load-bearing: Fix() clears exactly the returned keys,
// so a trigger that did not fire can never lose state it never inspected.
func (j *executorIdentityResidueJudge) staleKeys(bd beads.Bead) (keys, unconfirmed, reportOnly []string) {
	if bd.Status == "closed" {
		return nil, nil, nil
	}
	if bd.Status == "in_progress" {
		return nil, nil, nil
	}
	if graphroute.IsWorkflowTopologyKind(bd.Metadata[beadmeta.KindMetadataKey]) {
		return nil, nil, nil
	}
	if routedTo := strings.TrimSpace(bd.Metadata[beadmeta.RoutedToMetadataKey]); routedTo != "" && !residueRouteConfiguredLocally(j.cfg, routedTo) {
		return nil, nil, nil
	}

	switch j.staleWorkDirStamp(bd) {
	case residueWorkDirStale:
		keys = append(keys, beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey)
	case residueWorkDirReportOnly:
		reportOnly = append(reportOnly, beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey)
	}
	switch j.staleSessionNameStamp(bd) {
	case residueStale:
		keys = append(keys, beadmeta.SessionNameMetadataKey)
	case residueUnconfirmed:
		unconfirmed = append(unconfirmed, beadmeta.SessionNameMetadataKey)
	}
	return keys, unconfirmed, reportOnly
}

// residueRouteConfiguredLocally reports whether route resolves to an agent or
// named session this city configures, using the same resolvers the
// reconciler and CLI use to attribute a route (findAgentByTemplate with its
// binding-migration fallbacks, a rig-scoped template qualified by a
// configured rig, or a configured named session). Anything else — another
// town's template, or a pseudo-route — is not this city's to judge.
func residueRouteConfiguredLocally(cfg *config.City, route string) bool {
	if cfg == nil || route == "" {
		return false
	}
	if findAgentByTemplate(cfg, route) != nil {
		return true
	}
	if _, ok := agentutil.ResolveQualifiedRigScopedTemplate(cfg, route); ok {
		return true
	}
	if _, ok := findNamedSessionSpec(cfg, cfg.EffectiveCityName(), route); ok {
		return true
	}
	return false
}

// staleWorkDirStamp reports whether bd's legacy work_dir disagrees with its
// canonical gc.work_dir in a way that is genuine residue, rather than a
// repair candidate, an actively worktree-owning bead, or a pointer to a
// directory that still exists.
//
// Three stand-downs guard against clearing state something still relies on:
//
//   - hasWorktreeOwnershipEvidence: worktreeSpecForBead treats a bead
//     carrying worktree ownership metadata as actively managed and fails
//     closed on a canonical/legacy disagreement for it. Clearing both keys
//     here would erase the very evidence that check inspects, silently
//     downgrading its fail-closed conflict error into a "no spec, unmanaged"
//     no-op — the ga-6af29d/#6135 round-2 regression this stand-down closes.
//   - poolSlotWorkDirRepairFor(cfg, bd) != nil: this shape (canonical
//     clobbered with a pool-slot label, legacy still holding real per-bead
//     evidence) is a repair candidate the reconciler's own one-shot sweep
//     restores from legacy. Flagging it here races that repair for the same
//     keys and, if this check's Fix() wins, blanks both instead of
//     restoring the canonical.
//   - rule 3: either path still exists on disk (or cannot be proven absent).
//     A legacy per-bead worktree that is still there may hold unpushed
//     work, and its work_dir key is the only pointer to it (ga-n2f1ph).
//
// A disagreement that survives those is residue only if this machine can
// judge it (rule 4): otherwise it is residueWorkDirReportOnly.
func (j *executorIdentityResidueJudge) staleWorkDirStamp(bd beads.Bead) residueWorkDirVerdict {
	workDir := strings.TrimSpace(bd.Metadata[beadmeta.WorkDirMetadataKey])
	legacyWorkDir := strings.TrimSpace(bd.Metadata[beadmeta.LegacyWorkDirMetadataKey])
	if workDir == "" || legacyWorkDir == "" || workDir == legacyWorkDir {
		return residueWorkDirNotStale
	}
	if hasWorktreeOwnershipEvidence(bd) {
		return residueWorkDirNotStale
	}
	if poolSlotWorkDirRepairFor(j.cfg, bd) != nil {
		return residueWorkDirNotStale
	}
	if j.workDirMayExist(workDir) || j.workDirMayExist(legacyWorkDir) {
		return residueWorkDirNotStale
	}
	// An empty route is ordinary on a store shared with other towns (a
	// post-claim or detached bead), so it says nothing about whose bead this
	// is. Only a locally configured route (rule 1 already refused any other)
	// with both paths under this city's roots is this machine's to clear.
	if strings.TrimSpace(bd.Metadata[beadmeta.RoutedToMetadataKey]) == "" ||
		!j.pathProvablyLocal(workDir) || !j.pathProvablyLocal(legacyWorkDir) {
		return residueWorkDirReportOnly
	}
	return residueWorkDirStale
}

// residueWorkDirVerdict is staleWorkDirStamp's three-way answer.
type residueWorkDirVerdict int

const (
	residueWorkDirNotStale residueWorkDirVerdict = iota
	residueWorkDirStale
	// residueWorkDirReportOnly: every rule calls the pair stale, but this
	// machine cannot prove the bead or its paths are local (rule 4).
	residueWorkDirReportOnly
)

// pathProvablyLocal reports whether path is absolute and lies under this
// city's root or one of its rig paths. Only then does a stat on this machine
// say anything about it: a relative, ~ or $VAR spelling, or another machine's
// worktree path, stats absent here whether or not it exists where it was
// stamped (#123 review, 2026-09-23).
func (j *executorIdentityResidueJudge) pathProvablyLocal(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	path = filepath.Clean(path)
	for _, root := range j.localRoots() {
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// localRoots returns this city's root and its rig paths, absolute and clean.
func (j *executorIdentityResidueJudge) localRoots() []string {
	cityPath := strings.TrimSpace(j.cityPath)
	var roots []string
	if filepath.IsAbs(cityPath) {
		roots = append(roots, filepath.Clean(cityPath))
	}
	if j.cfg == nil {
		return roots
	}
	for _, rig := range j.cfg.Rigs {
		p := strings.TrimSpace(rig.Path)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			if !filepath.IsAbs(cityPath) {
				continue
			}
			p = filepath.Join(cityPath, p)
		}
		roots = append(roots, filepath.Clean(p))
	}
	return roots
}

// workDirMayExist reports whether path could still exist on disk. Only a
// stat that positively reports "does not exist" counts as absent; any other
// outcome (success, permission error, no stat configured) stands the
// work_dir trigger down, because Fix() deletes the only pointer to the
// directory. A relative path is resolved against the city root.
func (j *executorIdentityResidueJudge) workDirMayExist(path string) bool {
	if j.statPath == nil {
		return true
	}
	if !filepath.IsAbs(path) {
		if strings.TrimSpace(j.cityPath) == "" {
			return true
		}
		path = filepath.Join(j.cityPath, path)
	}
	_, err := j.statPath(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// hasWorktreeOwnershipEvidence reports whether bd publishes any of the
// worktree-ownership metadata keys worktreeSpecForBead inspects to tell a
// managed per-bead worktree apart from an unmanaged spawn.
func hasWorktreeOwnershipEvidence(bd beads.Bead) bool {
	for _, key := range []string{
		beadmeta.WorktreeRootMetadataKey,
		beadmeta.WorktreeRepoMetadataKey,
		beadmeta.WorktreeOwnerMetadataKey,
	} {
		if strings.TrimSpace(bd.Metadata[key]) != "" {
			return true
		}
	}
	return false
}

// residueSessionNameVerdict is staleSessionNameStamp's three-way answer.
type residueSessionNameVerdict int

const (
	residueNotStale residueSessionNameVerdict = iota
	residueStale
	// residueUnconfirmed: every config/index rule calls the stamp stale, but
	// the open session beads could not be listed to rule out a live holder.
	residueUnconfirmed
)

// staleSessionNameStamp reports whether bd carries a stale gc.session_name:
// a non-empty stamp on a bead with a non-empty gc.routed_to (an empty route
// is the ordinary post-claim/detached-orphan state, not residue) whose
// stamped session name is neither what the bead's CURRENT gc.routed_to would
// mint today — via agent.SessionNameFor, honoring any configured
// session_template, forward-encoded on every call, never against a fixed
// snapshot — nor a member of that route's legitimate executor-identity set
// (identities; a pool slot's own concrete session name is a legitimate
// stamp against its base route), nor the runtime name of any OPEN session
// bead (rule 2: a running holder's stamp is legitimate whatever the route,
// e.g. a cert-wait park or a crew seat working a pool bead). The open-session
// set is consulted last; if it cannot be loaded the answer is
// residueUnconfirmed, never residueStale.
func (j *executorIdentityResidueJudge) staleSessionNameStamp(bd beads.Bead) residueSessionNameVerdict {
	sessionName := strings.TrimSpace(bd.Metadata[beadmeta.SessionNameMetadataKey])
	if sessionName == "" {
		return residueNotStale
	}
	routedTo := strings.TrimSpace(bd.Metadata[beadmeta.RoutedToMetadataKey])
	if routedTo == "" {
		return residueNotStale
	}
	var sessionTemplate string
	var cityName string
	if j.cfg != nil {
		sessionTemplate = j.cfg.Workspace.SessionTemplate
		cityName = j.cfg.EffectiveCityName()
	}
	if agent.SessionNameFor(cityName, routedTo, sessionTemplate) == sessionName {
		return residueNotStale
	}
	if j.identities.legitimate(routedTo, sessionName) {
		return residueNotStale
	}
	open, err := j.openSessions.get()
	if err != nil {
		return residueUnconfirmed
	}
	if _, ok := open[sessionName]; ok {
		return residueNotStale
	}
	return residueStale
}
