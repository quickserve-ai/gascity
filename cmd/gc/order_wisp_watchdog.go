package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// THE ORDER WISP WATCHDOG (ga-puy7n0). It reports; it never closes.
//
// An order's open-work gate refuses to fire while any open wisp carries the
// order's order-run label. Order-TRACKING beads have an age-out
// (runOrderTrackingSweepWatchdog, two minutes). Order-RUN wisps have none, so a
// wisp whose worker died disabled its order in silence until someone looked.
// Measured: mol-dog-stale-db dead 41h behind one wisp, digest-generate 33h
// behind another.
//
// This watchdog ends the silence. Every orderWispWatchdogInterval it names on
// stderr each order run that has stayed open past its order's run_stale_after,
// with the claim on it and a verdict on that claim. It writes to no store.
// Whether a stale run is dead, and what to do about it, is a judgment, and
// judgment belongs to whoever reads the report (an operator, or a patrol's
// prompt), not to Go ("Keep judgment out of Go", AGENTS.md). gc doctor's
// order-firing-current check judges the run gating a stale order through the
// same judgeOrderWispRun, so the two cannot disagree.
//
// The verdict covers the whole run: the root and every open member of its
// subtree. The strongest claim on any open member decides it:
//
//	held          a live session holds a claim on an open member, or a
//	              configured named session does (a named session's claims
//	              outlive any one of its sessions).
//	unobservable  a claim names an identity or session this city cannot prove
//	              is its own. A rig store shared with another city carries
//	              that city's claims, and their session beads live in its
//	              stores, not ours: "not found here" is not "dead". The pool
//	              orphan sweeper learned this first
//	              (pool_orphan_foreign_identity.go).
//	holder-gone   every claim names this city's own session or agent, and none
//	              of them is live.
//	unclaimed     no open member carries a claim: queued work.
//
// Owner resolution is read-only by construction. It gets and lists session
// beads and reads config. It never goes through a resolver that can
// materialize or reopen a session, because a resolver that creates the session
// it is checking reads every dead owner as alive (ga-isa3j4, ga-mk8tp4,
// ga-ek26cz).

// orderWispWatchdogInterval is the minimum time between watchdog passes. The
// runs it reports are hours old, so a pass every ten minutes costs a few label
// reads per formula order and loses nothing.
const orderWispWatchdogInterval = 10 * time.Minute

// orderWispOwnerState is the watchdog's verdict on who holds an order run. The
// states are ordered by strength: the strongest claim anywhere in a run
// decides the run's verdict.
type orderWispOwnerState int

const (
	// orderWispUnclaimed means nothing claims the bead: it is queued work.
	orderWispUnclaimed orderWispOwnerState = iota
	// orderWispHolderGone means every claim reference names this city's own
	// identity or session, and none of them is live.
	orderWispHolderGone
	// orderWispUnobservable means a claim reference names an identity or a
	// session this city cannot prove is its own.
	orderWispUnobservable
	// orderWispHeld means a live session holds the claim, or a configured
	// named session does (its claims outlive its sessions).
	orderWispHeld
)

// String names the state as the report and gc doctor print it.
func (s orderWispOwnerState) String() string {
	switch s {
	case orderWispHeld:
		return "held"
	case orderWispUnobservable:
		return "unobservable"
	case orderWispHolderGone:
		return "holder-gone"
	default:
		return "unclaimed"
	}
}

// orderWispOwnerVerdict is one owner judgment with the evidence behind it.
type orderWispOwnerVerdict struct {
	State orderWispOwnerState
	// Owner is the claim reference that decided the verdict; "" when unclaimed.
	Owner string
	// Reason says why, in words an operator reads in a log or doctor line.
	Reason string
}

// String renders the verdict as the report and gc doctor print it: the state,
// then the reason.
func (v orderWispOwnerVerdict) String() string {
	return v.State.String() + ": " + v.Reason
}

// orderWispClaimRef is one reference on a bead to the session that claimed it.
type orderWispClaimRef struct {
	value string
	// sessionID marks a gc.session_id reference: an exact session bead ID
	// rather than an identity to match against session beads.
	sessionID bool
}

// orderWispClaimRefs returns the claim references on b: its assignee and the
// session back-references a claim stamps. A bead that shows no claim at all,
// open and unassigned, returns none, even when a session back-reference
// survives: a release reopens the bead as queued work and deliberately leaves
// gc.session_id behind (ga-pzop1c).
func orderWispClaimRefs(b beads.Bead) []orderWispClaimRef {
	assignee := strings.TrimSpace(b.Assignee)
	if assignee == "" && b.Status != "in_progress" {
		return nil
	}
	var refs []orderWispClaimRef
	seen := make(map[string]struct{}, 4)
	add := func(value string, sessionID bool) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, dup := seen[value]; dup {
			return
		}
		seen[value] = struct{}{}
		refs = append(refs, orderWispClaimRef{value: value, sessionID: sessionID})
	}
	add(assignee, false)
	add(b.Metadata[beadmeta.SessionIDMetadataKey], true)
	add(b.Metadata[beadmeta.SessionIDCamelMetadataKey], true)
	add(b.Metadata[beadmeta.SessionNameMetadataKey], false)
	add(b.Metadata[beadmeta.SessionNameCamelMetadataKey], false)
	return refs
}

// orderWispOwnerResolver judges order run claims against this city's config
// and session beads. It reads two stores, and what each one may prove is the
// locality rule:
//
//   - sessions, this city's sessions-class store. A session bead there is this
//     city's own, so it proves liveness and locality both.
//   - run, the store the judged run lives in, where a graph-resident run
//     session bead sits beside the work it drives. A session bead there proves
//     liveness only. On a rig store shared with another city it may be that
//     city's, so finding one never makes a claim this city's own.
//
// A resolver serves one watchdog pass or one doctor lookup. It builds each
// session index once, on first use, and it is not safe for concurrent use.
type orderWispOwnerResolver struct {
	cfg      *config.City
	cityName string
	sessions *orderWispSessionIndex
	run      *orderWispSessionIndex
}

// orderWispSessionIndex is one store's session beads, listed on first use.
type orderWispSessionIndex struct {
	store beads.Store
	open  *orderWispOpenSessions
	// named holds every claim identity any session bead in the store, open or
	// closed, carries.
	named map[string]struct{}
}

// orderWispOpenSessions indexes a store's open session beads: every identity
// a claim could be made under, and every template an open session runs.
type orderWispOpenSessions struct {
	ids       map[string]struct{}
	templates map[string]struct{}
}

func newOrderWispOwnerResolver(cfg *config.City, cityPath string, sessionStore beads.Store) *orderWispOwnerResolver {
	cityName := ""
	if cfg != nil {
		cityName = config.EffectiveCityName(cfg, filepath.Base(cityPath))
	}
	r := &orderWispOwnerResolver{cfg: cfg, cityName: cityName}
	if store := unwrapOrderTrackingSweepStore(sessionStore); store != nil {
		r.sessions = &orderWispSessionIndex{store: store}
	}
	return r
}

// forRunStore returns a resolver for runs that live in store. It shares r's
// session-store index, so a pass lists the session store once however many
// stores it reads, and it reads store for liveness only. A run store that is
// the session store adds nothing.
func (r *orderWispOwnerResolver) forRunStore(store beads.Store) *orderWispOwnerResolver {
	out := &orderWispOwnerResolver{cfg: r.cfg, cityName: r.cityName, sessions: r.sessions}
	store = unwrapOrderTrackingSweepStore(store)
	if store != nil && (r.sessions == nil || r.sessions.store != store) {
		out.run = &orderWispSessionIndex{store: store}
	}
	return out
}

// livenessIndexes returns the indexes a liveness check reads, the session
// store first.
func (r *orderWispOwnerResolver) livenessIndexes() []*orderWispSessionIndex {
	out := make([]*orderWispSessionIndex, 0, 2)
	for _, idx := range []*orderWispSessionIndex{r.sessions, r.run} {
		if idx != nil {
			out = append(out, idx)
		}
	}
	return out
}

// verdict judges who holds b. A held reference wins outright; an unobservable
// one outranks a gone one, because a stale local back-reference beside a
// foreign claim is exactly the shape a cross-city takeover leaves.
func (r *orderWispOwnerResolver) verdict(b beads.Bead) (orderWispOwnerVerdict, error) {
	refs := orderWispClaimRefs(b)
	if len(refs) == 0 {
		return orderWispOwnerVerdict{State: orderWispUnclaimed, Reason: "unclaimed"}, nil
	}
	var decided orderWispOwnerVerdict
	for i, ref := range refs {
		v, err := r.refVerdict(ref)
		if err != nil {
			return orderWispOwnerVerdict{}, err
		}
		if i == 0 || v.State > decided.State {
			decided = v
		}
		if decided.State == orderWispHeld {
			break
		}
	}
	return decided, nil
}

// refVerdict judges one claim reference in two steps, and the order is what
// keeps the report honest. LIVENESS first: a reference to a live session is
// held, whatever else is true of it. LOCALITY second: a reference to no live
// session is holder-gone only if it provably names this city's own session or
// agent. Without the first step a working run reads as dead; without the
// second, another city's run reads as this city's. Each has a test that fails
// without it.
func (r *orderWispOwnerResolver) refVerdict(ref orderWispClaimRef) (orderWispOwnerVerdict, error) {
	live, reason, err := r.isLive(ref)
	if err != nil {
		return orderWispOwnerVerdict{}, err
	}
	if live {
		return orderWispOwnerVerdict{State: orderWispHeld, Owner: ref.value, Reason: reason}, nil
	}
	return r.localityVerdict(ref)
}

// isLive reports whether ref names a session that still holds its claims.
// Positive evidence from any store it reads counts: calling a run held never
// claims it is this city's.
func (r *orderWispOwnerResolver) isLive(ref orderWispClaimRef) (bool, string, error) {
	for _, idx := range r.livenessIndexes() {
		if ref.sessionID {
			sb, found, err := idx.sessionBeadByID(ref.value)
			if err != nil {
				return false, "", err
			}
			if found && sb.Status != "closed" {
				return true, "its session is live", nil
			}
			continue
		}
		open, err := idx.openSessions()
		if err != nil {
			return false, "", err
		}
		if _, ok := open.ids[ref.value]; ok {
			return true, "its session is live", nil
		}
		// Some routing paths write the bare template into the assignee before
		// a session materializes; any open session of that template may be the
		// one running it (liveEphemeralSessionForTemplate).
		if _, ok := open.templates[ref.value]; ok {
			return true, "an open session of that template may be running it", nil
		}
	}
	if ref.sessionID {
		return false, "", nil
	}
	// A configured named session between sessions is its normal state, not an
	// orphan: its claims outlive any one session bead (releasableAssigneeIdentities).
	if _, ok := findNamedSessionSpecForAssignee(r.cfg, r.cityName, ref.value); ok {
		return true, "configured named session with no open session; its claims outlive any one session", nil
	}
	return false, "", nil
}

// localityVerdict judges a reference to no live session: holder-gone when it
// provably names this city's own session or agent, unobservable otherwise.
// Only config and the session store can prove a claim is this city's; the run
// store never does.
func (r *orderWispOwnerResolver) localityVerdict(ref orderWispClaimRef) (orderWispOwnerVerdict, error) {
	if ref.sessionID {
		found := false
		if r.sessions != nil {
			_, ok, err := r.sessions.sessionBeadByID(ref.value)
			if err != nil {
				return orderWispOwnerVerdict{}, err
			}
			found = ok
		}
		if found {
			return orderWispOwnerVerdict{State: orderWispHolderGone, Owner: ref.value, Reason: "its session bead in this city's session store is closed"}, nil
		}
		return orderWispOwnerVerdict{State: orderWispUnobservable, Owner: ref.value, Reason: "no session bead with that ID in this city's session store"}, nil
	}
	roster := poolAssigneeObservability(r.cfg, r.cityName, ref.value)
	if !roster.Local {
		return orderWispOwnerVerdict{State: orderWispUnobservable, Owner: ref.value, Reason: fmt.Sprintf("not in this city's roster (%s)", roster.Reason)}, nil
	}
	switch roster.Reason {
	case poolRosterReasonNoConfig:
		return orderWispOwnerVerdict{State: orderWispUnobservable, Owner: ref.value, Reason: "no city config to resolve it against"}, nil
	case poolRosterReasonNotQualified:
		// A bare runtime session name, alias or bead ID is this city's own
		// naming only if one of this city's session beads carries it.
		found := false
		if r.sessions != nil {
			var err error
			found, err = r.sessions.namesSessionBead(ref.value)
			if err != nil {
				return orderWispOwnerVerdict{}, err
			}
		}
		if !found {
			return orderWispOwnerVerdict{State: orderWispUnobservable, Owner: ref.value, Reason: "names no session in this city's session store"}, nil
		}
		return orderWispOwnerVerdict{State: orderWispHolderGone, Owner: ref.value, Reason: "its session in this city has ended"}, nil
	}
	return orderWispOwnerVerdict{State: orderWispHolderGone, Owner: ref.value, Reason: fmt.Sprintf("agent of this city (%s) with no live session", roster.Reason)}, nil
}

// sessionBeadByID reads the session bead with this exact ID, bypassing any
// cache: a stale cached copy could read a reopened session as closed.
func (idx *orderWispSessionIndex) sessionBeadByID(id string) (beads.Bead, bool, error) {
	sb, err := beads.HandlesFor(idx.store).Live.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return beads.Bead{}, false, nil
	}
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("reading session bead %s: %w", id, err)
	}
	if !isSessionBead(sb) {
		return beads.Bead{}, false, nil
	}
	return sb, true, nil
}

// namesSessionBead reports whether any session bead in the store, open or
// closed, carries identity as one of its claim identities.
func (idx *orderWispSessionIndex) namesSessionBead(identity string) (bool, error) {
	for _, id := range directSessionBeadIDCandidates(identity) {
		sb, found, err := idx.sessionBeadByID(id)
		if err != nil {
			return false, err
		}
		if found && sessionBeadCarriesIdentity(sb, identity) {
			return true, nil
		}
	}
	if idx.named == nil {
		infos, err := orderWispSessionInfos(idx.store, true)
		if err != nil {
			return false, fmt.Errorf("listing session beads: %w", err)
		}
		named := map[string]struct{}{}
		for _, info := range infos {
			for _, id := range session.AssigneeIdentities(info) {
				named[id] = struct{}{}
			}
		}
		idx.named = named
	}
	_, ok := idx.named[identity]
	return ok, nil
}

// openSessions builds the store's open-session index once. A partial listing
// fails the lookup rather than being used: a missing row would report a live
// session as gone.
func (idx *orderWispSessionIndex) openSessions() (*orderWispOpenSessions, error) {
	if idx.open != nil {
		return idx.open, nil
	}
	infos, err := orderWispSessionInfos(idx.store, false)
	if err != nil {
		return nil, fmt.Errorf("listing open session beads: %w", err)
	}
	open := &orderWispOpenSessions{ids: map[string]struct{}{}, templates: map[string]struct{}{}}
	for _, info := range infos {
		if info.Closed {
			continue
		}
		for _, id := range session.AssigneeIdentities(info) {
			open.ids[id] = struct{}{}
		}
		if template := strings.TrimSpace(info.Template); template != "" {
			open.templates[template] = struct{}{}
		}
	}
	idx.open = open
	return open, nil
}

// orderWispSessionInfos lists one store's session beads through the sessions
// class front door. The read is Live: a cached copy could read a reopened
// session as closed, or miss a session created behind the cache.
func orderWispSessionInfos(store beads.Store, includeClosed bool) ([]session.Info, error) {
	return sessionFrontDoor(store).ListAll(session.ListAllOptions{IncludeClosed: includeClosed, Live: true})
}

func sessionBeadCarriesIdentity(sb beads.Bead, identity string) bool {
	for _, id := range sessionBeadAssigneeIdentities(sb) {
		if id == identity {
			return true
		}
	}
	return false
}

// orderWispRunVerdict judges a whole run: root and every open member of
// subtree (which may include root). The strongest claim on any open member
// decides; on a tie the first found, root first. A verdict decided by a member
// other than the root names that member in its reason.
func orderWispRunVerdict(root beads.Bead, subtree []beads.Bead, owners *orderWispOwnerResolver) (orderWispOwnerVerdict, error) {
	decided, err := owners.verdict(root)
	if err != nil {
		return orderWispOwnerVerdict{}, err
	}
	for _, b := range subtree {
		if decided.State == orderWispHeld {
			break
		}
		if b.ID == root.ID || b.Status == "closed" {
			continue
		}
		v, err := owners.verdict(b)
		if err != nil {
			return orderWispOwnerVerdict{}, err
		}
		if v.State > decided.State {
			v.Reason = fmt.Sprintf("open member %s: %s", b.ID, v.Reason)
			decided = v
		}
	}
	if decided.State == orderWispUnclaimed {
		decided.Reason = "no open member of the run is claimed"
	}
	return decided, nil
}

// judgeOrderWispRun collects root's run from store and judges all of it. gc
// doctor judges the bead gating a stale order through here, so its note and
// the watchdog's report weigh the same beads the same way.
func judgeOrderWispRun(store beads.Store, root beads.Bead, owners *orderWispOwnerResolver) (orderWispOwnerVerdict, error) {
	subtree, err := collectOrderWispSubtree(store, root)
	if err != nil {
		return orderWispOwnerVerdict{}, fmt.Errorf("collecting the run under %s: %w", root.ID, err)
	}
	return orderWispRunVerdict(root, subtree, owners)
}

// orderWispWatchdogOrder is one formula order the watchdog reads, with the
// age past which its runs are reported.
type orderWispWatchdogOrder struct {
	order      orders.Order
	scoped     string
	staleAfter time.Duration
}

// orderWispWatchdogOrdersFor returns the orders whose runs the watchdog
// reports: every enabled formula order whose rig is not suspended. A suspended
// rig's orders do not dispatch, so nothing they hold gates a firing.
func orderWispWatchdogOrdersFor(cityPath string, cfg *config.City, all []orders.Order) []orderWispWatchdogOrder {
	suspended := orderWispWatchdogSuspendedRigs(cityPath, cfg)
	var out []orderWispWatchdogOrder
	for _, a := range orders.FilterEnabled(all) {
		if a.IsExec() || suspended[a.Rig] {
			continue
		}
		out = append(out, orderWispWatchdogOrder{order: a, scoped: a.ScopedName(), staleAfter: a.RunStaleAfterOrDefault()})
	}
	return out
}

func orderWispWatchdogSuspendedRigs(cityPath string, cfg *config.City) map[string]bool {
	out := map[string]bool{}
	if cfg == nil {
		return out
	}
	var state suspensionstate.State
	if cityPath != "" {
		state, _ = loadSuspensionState(fsys.OSFS{}, cityPath) //nolint:errcheck // an unreadable state file means no runtime override, as in rigSuspendedByName
	}
	for i := range cfg.Rigs {
		if suspensionstate.EffectiveRigSuspended(state, cfg.Rigs[i].Name, cfg.Rigs[i].EffectiveSuspendedOnStart()) {
			out[cfg.Rigs[i].Name] = true
		}
	}
	return out
}

// orderWispWatchdogLeg is one store and the orders whose runs can live in it.
type orderWispWatchdogLeg struct {
	store  beads.Store
	orders []orderWispWatchdogOrder
}

func (l orderWispWatchdogLeg) watches(scoped string) bool {
	for _, o := range l.orders {
		if o.scoped == scoped {
			return true
		}
	}
	return false
}

// orderWispWatchdogLegs assigns each order to the stores its runs can live in:
// its target scope store (where `gc order run` mints a root, and where a
// single-store city's dispatcher does), the legacy city store for a rig order
// that still falls back to it, and the graph binding a split city's
// dispatcher writes wisp roots into. Reading every order in every store would
// multiply the pass by the number of rigs and find nothing more.
func orderWispWatchdogLegs(cityPath string, cfg *config.City, stores []beads.Store, graphStore beads.Store, watch []orderWispWatchdogOrder) ([]orderWispWatchdogLeg, error) {
	byKey := make(map[string]beads.Store, len(stores))
	for _, store := range stores {
		if key := orderTrackingSweepStoreKey(store); key != "" {
			byKey[key] = store
		}
	}
	var legs []orderWispWatchdogLeg
	add := func(store beads.Store, o orderWispWatchdogOrder) {
		if store == nil {
			return
		}
		for i := range legs {
			if unwrapOrderTrackingSweepStore(legs[i].store) != unwrapOrderTrackingSweepStore(store) {
				continue
			}
			// A city whose graph binding is also the order's target store
			// reaches the same leg twice; read its runs once.
			if !legs[i].watches(o.scoped) {
				legs[i].orders = append(legs[i].orders, o)
			}
			return
		}
		legs = append(legs, orderWispWatchdogLeg{store: store, orders: []orderWispWatchdogOrder{o}})
	}
	var errs []error
	for _, o := range watch {
		target, err := resolveOrderStoreTarget(cityPath, cfg, o.order)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolving the store for order %s: %w", o.scoped, err))
			continue
		}
		add(byKey[orderStoreTargetKey(target)], o)
		if legacyOrderCityFallbackNeeded(cityPath, target) {
			add(byKey[orderStoreTargetKey(legacyOrderCityTarget(cityPath, cfg))], o)
		}
		add(graphStore, o)
	}
	return legs, errors.Join(errs...)
}

// orderWispStaleRun is one stale order run a watchdog pass reports.
type orderWispStaleRun struct {
	rootID     string
	order      string
	createdAt  time.Time
	staleAfter time.Duration
	verdict    orderWispOwnerVerdict
}

// orderWispWatchdogResult is one pass's outcome: every stale run it found.
type orderWispWatchdogResult struct {
	stale []orderWispStaleRun
}

// reportStaleOrderWisps runs one watchdog pass over legs and returns every
// stale run with its verdict. It only reads. Store errors are collected and
// the pass continues, so one unreachable rig cannot hide every other store's
// stale runs.
func reportStaleOrderWisps(legs []orderWispWatchdogLeg, now time.Time, owners *orderWispOwnerResolver) (orderWispWatchdogResult, error) {
	var result orderWispWatchdogResult
	var errs []error
	for _, leg := range legs {
		// Unwrap the sweep's scope label first: the wrapper does not forward
		// Handles(), so a Live read through it would silently be a cached one.
		store := unwrapOrderTrackingSweepStore(leg.store)
		legOwners := owners.forRunStore(store)
		for _, o := range leg.orders {
			cutoff := now.Add(-o.staleAfter)
			roots, err := staleOrderWispRootsForOrder(store, o.scoped, cutoff)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, root := range roots {
				run, stale, err := judgeStaleOrderWispRoot(store, root, o, cutoff, legOwners)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				if stale {
					result.stale = append(result.stale, run)
				}
			}
		}
	}
	return result, errors.Join(errs...)
}

// judgeStaleOrderWispRoot judges one candidate root. It reports false when the
// root is not a stale run: closed, not a run root, or holding an open member
// younger than cutoff (staleOrderWispRootSubtree, the operator sweep's own
// selection).
func judgeStaleOrderWispRoot(store beads.Store, root beads.Bead, o orderWispWatchdogOrder, cutoff time.Time, owners *orderWispOwnerResolver) (orderWispStaleRun, bool, error) {
	subtree, err := staleOrderWispRootSubtree(store, root, cutoff)
	if err != nil || subtree == nil {
		return orderWispStaleRun{}, false, err
	}
	verdict, err := orderWispRunVerdict(root, subtree, owners)
	if err != nil {
		return orderWispStaleRun{}, false, fmt.Errorf("judging the claim on stale order run %s (%s): %w", root.ID, o.scoped, err)
	}
	return orderWispStaleRun{rootID: root.ID, order: o.scoped, createdAt: root.CreatedAt, staleAfter: o.staleAfter, verdict: verdict}, true, nil
}

// reportLines renders a pass for stderr: a count, then one line for every
// stale run. All of it is printed every pass on purpose: a stale run still
// gates its order, and the only way anyone learns that is from a line naming
// it. A watchdog that only speaks when it acts is how the tracking watchdog
// stayed blind for 43 hours (ga-v5vnyp).
func (r orderWispWatchdogResult) reportLines(now time.Time) []string {
	if len(r.stale) == 0 {
		return nil
	}
	stale := append([]orderWispStaleRun(nil), r.stale...)
	sort.Slice(stale, func(i, j int) bool {
		if stale[i].order != stale[j].order {
			return stale[i].order < stale[j].order
		}
		return stale[i].rootID < stale[j].rootID
	})
	counts := map[orderWispOwnerState]int{}
	for _, s := range stale {
		counts[s.verdict.State]++
	}
	lines := make([]string, 0, len(stale)+1)
	lines = append(lines, fmt.Sprintf("order wisp watchdog: %d stale order run(s) still gating their orders (%d held, %d unobservable, %d holder-gone, %d unclaimed); report only, nothing is closed",
		len(stale), counts[orderWispHeld], counts[orderWispUnobservable], counts[orderWispHolderGone], counts[orderWispUnclaimed]))
	for _, s := range stale {
		holder := s.verdict.Owner
		if holder == "" {
			holder = "unassigned"
		}
		// %q on the holder: it is text read off a bead another city may have
		// written, and an unquoted comma would forge structure in this line.
		lines = append(lines, fmt.Sprintf("order wisp watchdog: stale run %s of order %s, open %s (run_stale_after=%s), holder %q, %s",
			s.rootID, s.order, now.Sub(s.createdAt).Round(time.Minute), orderWispDurationText(s.staleAfter), holder, s.verdict))
	}
	return lines
}

// orderWispDurationText renders d in the shortest whole unit that states it
// exactly ("6h", "90m"), falling back to Go's own form.
func orderWispDurationText(d time.Duration) string {
	switch {
	case d > 0 && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d > 0 && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default:
		return d.String()
	}
}

// runOrderWispWatchdog reports stale order runs, at most once every
// orderWispWatchdogInterval, on the dispatch path beside
// runOrderTrackingSweepWatchdog.
//
// The first call only arms the clock. The runs this reports are hours old, so
// nothing is lost by waiting one interval, and the boot pass (which holds
// readiness, gastownhall/gascity#6429) pays none of the label reads.
//
// It reads the same stores the tracking watchdog sweeps plus the graph binding
// a split city writes wisp roots into, and judges holders against the
// sessions class store. It writes to none of them.
func (cr *CityRuntime) runOrderWispWatchdog(now time.Time) {
	if cr.orderWispWatchdogLast.IsZero() {
		cr.orderWispWatchdogLast = now
		return
	}
	if now.Sub(cr.orderWispWatchdogLast) < orderWispWatchdogInterval {
		return
	}
	cr.orderWispWatchdogLast = now
	if cr.stderr == nil {
		// The report is the watchdog's only output; with nowhere to print it
		// the pass would be reads for nothing.
		return
	}

	watch := orderWispWatchdogOrdersFor(cr.cityPath, cr.cfg, cr.orderSet)
	if len(watch) == 0 {
		return
	}
	stores, _, closeOpened, storeErr := cr.orderTrackingSweepStores()
	defer closeOpened()
	graphStore := resolveGraphStore(cr.storageRoutes, nil, cr.cfg, cr.cityPath, cr.rec)
	legs, legErr := orderWispWatchdogLegs(cr.cityPath, cr.cfg, stores, graphStore, watch)
	owners := newOrderWispOwnerResolver(cr.cfg, cr.cityPath, cr.sessionsBeadStore().Store)
	result, reportErr := reportStaleOrderWisps(legs, now, owners)
	if err := errors.Join(storeErr, legErr, reportErr); err != nil {
		fmt.Fprintf(cr.stderr, "%s: order wisp watchdog: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
	}
	for _, line := range result.reportLines(now) {
		fmt.Fprintf(cr.stderr, "%s: %s\n", cr.logPrefix, line) //nolint:errcheck // best-effort stderr
	}
}
