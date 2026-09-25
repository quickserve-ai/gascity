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
	"github.com/gastownhall/gascity/internal/beads/closeorder"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// THE ORDER WISP WATCHDOG (ga-puy7n0).
//
// An order's open-work gate refuses to fire while any open wisp carries the
// order's order-run label. Order-TRACKING beads have an age-out
// (runOrderTrackingSweepWatchdog, two minutes). Order-RUN wisps had none, so a
// wisp whose worker died disabled its order until an operator ran
// `gc order sweep-tracking --include-wisps` or closed the wisp by hand.
// Measured: mol-dog-stale-db dead 41h behind one wisp, digest-generate 33h
// behind another.
//
// This watchdog closes a stale order-run wisp only when a session of THIS city
// holds the claim and that session is no longer live. It leaves everything
// else open and reports it:
//
//	unclaimed      queued demand. A slow pool is not a dead one, and closing
//	               the wisp would re-fire the order onto the back of the same
//	               queue. Alarming on an unclaimed wisp is ga-puy7n0 ask 3.
//	held           a live session holds it, however long the run takes.
//	unobservable   the claim names an identity this city cannot prove is its
//	               own. A rig store shared with another city carries that
//	               city's claims, and their session beads live in its stores,
//	               not ours: "not found here" is not "dead". The pool orphan
//	               sweeper learned this first (pool_orphan_foreign_identity.go).
//
// Owner resolution is read-only by construction. It gets and lists session
// beads and reads config. It never goes through a resolver that can
// materialize or reopen a session, because a reaper that creates the session
// it is checking reads every dead owner as alive (ga-isa3j4, ga-mk8tp4,
// ga-ek26cz).

const (
	// orderWispWatchdogInterval is the minimum time between watchdog passes.
	// The wisps it judges are hours old, so a pass every ten minutes costs a
	// few label reads per formula order and loses nothing.
	orderWispWatchdogInterval = 10 * time.Minute
	// orderWispWatchdogMetadataInitiator is stamped as order_tracking_sweep_by
	// on every bead the watchdog closes.
	orderWispWatchdogMetadataInitiator = "order-wisp-watchdog"
	// orderWispWatchdogReportSampleLimit bounds the wisp IDs one log line
	// names. The counts are always exact; only the ID list is sampled.
	orderWispWatchdogReportSampleLimit = 5
)

// orderWispOwnerState is the watchdog's verdict on who holds an order wisp.
type orderWispOwnerState int

const (
	// orderWispUnclaimed means nothing claims the bead: it is queued demand.
	orderWispUnclaimed orderWispOwnerState = iota
	// orderWispHeld means a live session of this city holds the claim, or a
	// configured named session does (its claims outlive its sessions).
	orderWispHeld
	// orderWispUnobservable means a claim reference names an identity or a
	// session this city cannot prove is its own.
	orderWispUnobservable
	// orderWispAbandoned means every claim reference names this city's own
	// identity or session, and none of them is live.
	orderWispAbandoned
)

// orderWispOwnerVerdict is one owner judgment with the evidence behind it.
type orderWispOwnerVerdict struct {
	State orderWispOwnerState
	// Owner is the claim reference that decided the verdict; "" when unclaimed.
	Owner string
	// Reason says why, in words an operator reads in a log or doctor line.
	Reason string
}

// protects reports whether the verdict keeps a stale subtree open.
func (v orderWispOwnerVerdict) protects() bool {
	return v.State == orderWispHeld || v.State == orderWispUnobservable
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
// survives: a release reopens the bead as queued demand and deliberately
// leaves gc.session_id behind (ga-pzop1c).
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

// orderWispClaimFingerprint names the claim state of b, so the watchdog can
// tell whether a bead it judged has been claimed or released since.
func orderWispClaimFingerprint(b beads.Bead) string {
	parts := []string{b.Status, strings.TrimSpace(b.Assignee)}
	for _, key := range []string{
		beadmeta.SessionIDMetadataKey,
		beadmeta.SessionIDCamelMetadataKey,
		beadmeta.SessionNameMetadataKey,
		beadmeta.SessionNameCamelMetadataKey,
	} {
		parts = append(parts, strings.TrimSpace(b.Metadata[key]))
	}
	return strings.Join(parts, "\x00")
}

// orderWispOwnerResolver judges order wisp claims against this city's config
// and session beads. Session beads live in the sessions class store, and a
// graph-resident run session lives in the store of the work it drives, so the
// resolver reads every store it is given; the first is the sessions store.
//
// A resolver serves one watchdog pass or one doctor run. It builds its session
// indexes once each, on first use, and it is not safe for concurrent use.
type orderWispOwnerResolver struct {
	cfg      *config.City
	cityName string
	stores   []beads.Store

	openIndex *orderWispOpenSessions
	// namedIndex holds every claim identity any session bead, open or closed,
	// carries in the resolver's stores.
	namedIndex map[string]struct{}
}

// orderWispOpenSessions indexes the open session beads the resolver's stores
// hold: every identity a claim could be made under, and every template an
// open session runs.
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
	r.addStore(sessionStore)
	return r
}

// forStore returns a resolver that also reads store, sharing nothing mutable
// with r. The watchdog makes one per swept store, because a graph-resident
// session bead lives beside the wisp it drives.
func (r *orderWispOwnerResolver) forStore(store beads.Store) *orderWispOwnerResolver {
	out := &orderWispOwnerResolver{cfg: r.cfg, cityName: r.cityName}
	for _, s := range r.stores {
		out.addStore(s)
	}
	out.addStore(store)
	return out
}

func (r *orderWispOwnerResolver) addStore(store beads.Store) {
	store = unwrapOrderTrackingSweepStore(store)
	if store == nil || storeListContains(r.stores, store) {
		return
	}
	r.stores = append(r.stores, store)
}

// verdict judges who holds b. A held reference wins outright; an unobservable
// one keeps the bead open even when another reference is abandoned, because a
// stale local back-reference beside a foreign claim is exactly the shape a
// cross-city takeover leaves.
func (r *orderWispOwnerResolver) verdict(b beads.Bead) (orderWispOwnerVerdict, error) {
	refs := orderWispClaimRefs(b)
	if len(refs) == 0 {
		return orderWispOwnerVerdict{State: orderWispUnclaimed, Reason: "unclaimed"}, nil
	}
	var decided orderWispOwnerVerdict
	for _, ref := range refs {
		v, err := r.refVerdict(ref)
		if err != nil {
			return orderWispOwnerVerdict{}, err
		}
		if v.State == orderWispHeld {
			return v, nil
		}
		if decided.State == orderWispUnclaimed || (v.State == orderWispUnobservable && decided.State == orderWispAbandoned) {
			decided = v
		}
	}
	return decided, nil
}

// refVerdict judges one claim reference in two steps, and the order is the
// safety argument. LIVENESS first: a reference to a live session is held,
// whatever else is true of it. LOCALITY second: a reference to no live session
// is abandoned only if it provably names this city's own session or agent.
// Deleting the first step closes live work; deleting the second closes other
// cities' work. Each has a test that fails without it.
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
func (r *orderWispOwnerResolver) isLive(ref orderWispClaimRef) (bool, string, error) {
	if ref.sessionID {
		sb, found, err := r.sessionBeadByID(ref.value)
		if err != nil || !found {
			return false, "", err
		}
		return sb.Status != "closed", "its session is live", nil
	}
	index, err := r.openSessions()
	if err != nil {
		return false, "", err
	}
	if _, ok := index.ids[ref.value]; ok {
		return true, "its session is live", nil
	}
	// Some routing paths write the bare template into the assignee before a
	// session materializes; any open session of that template may be the one
	// running it, so it counts as live (liveEphemeralSessionForTemplate).
	if _, ok := index.templates[ref.value]; ok {
		return true, "an open session of that template may be running it", nil
	}
	// A configured named session between sessions is its normal state, not an
	// orphan: its claims outlive any one session bead (releasableAssigneeIdentities).
	if _, ok := findNamedSessionSpecForAssignee(r.cfg, r.cityName, ref.value); ok {
		return true, "configured named session; its claims outlive its sessions", nil
	}
	return false, "", nil
}

// localityVerdict judges a reference to no live session: abandoned when it
// provably names this city's own session or agent, unobservable otherwise.
func (r *orderWispOwnerResolver) localityVerdict(ref orderWispClaimRef) (orderWispOwnerVerdict, error) {
	if ref.sessionID {
		_, found, err := r.sessionBeadByID(ref.value)
		if err != nil {
			return orderWispOwnerVerdict{}, err
		}
		if found {
			return orderWispOwnerVerdict{State: orderWispAbandoned, Owner: ref.value, Reason: "its session bead in this city is closed"}, nil
		}
		return orderWispOwnerVerdict{State: orderWispUnobservable, Owner: ref.value, Reason: "no session bead with that ID in this city's stores"}, nil
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
		found, err := r.namesSessionBead(ref.value)
		if err != nil {
			return orderWispOwnerVerdict{}, err
		}
		if !found {
			return orderWispOwnerVerdict{State: orderWispUnobservable, Owner: ref.value, Reason: "names no session in this city's stores"}, nil
		}
		return orderWispOwnerVerdict{State: orderWispAbandoned, Owner: ref.value, Reason: "its session in this city has ended"}, nil
	}
	return orderWispOwnerVerdict{State: orderWispAbandoned, Owner: ref.value, Reason: fmt.Sprintf("agent of this city (%s) with no live session", roster.Reason)}, nil
}

// sessionBeadByID reads the session bead with this exact ID from the
// resolver's stores, bypassing any cache: a stale cached copy could read a
// reopened session as closed.
func (r *orderWispOwnerResolver) sessionBeadByID(id string) (beads.Bead, bool, error) {
	for _, store := range r.stores {
		sb, err := beads.HandlesFor(store).Live.Get(id)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return beads.Bead{}, false, fmt.Errorf("reading session bead %s: %w", id, err)
		}
		if isSessionBead(sb) {
			return sb, true, nil
		}
	}
	return beads.Bead{}, false, nil
}

// namesSessionBead reports whether any session bead, open or closed, in the
// resolver's stores carries identity as one of its claim identities.
func (r *orderWispOwnerResolver) namesSessionBead(identity string) (bool, error) {
	for _, id := range directSessionBeadIDCandidates(identity) {
		sb, found, err := r.sessionBeadByID(id)
		if err != nil {
			return false, err
		}
		if found && sessionBeadCarriesIdentity(sb, identity) {
			return true, nil
		}
	}
	if r.namedIndex == nil {
		named := map[string]struct{}{}
		for _, store := range r.stores {
			infos, err := orderWispSessionInfos(store, true)
			if err != nil {
				return false, fmt.Errorf("listing session beads: %w", err)
			}
			for _, info := range infos {
				for _, id := range session.AssigneeIdentities(info) {
					named[id] = struct{}{}
				}
			}
		}
		r.namedIndex = named
	}
	_, ok := r.namedIndex[identity]
	return ok, nil
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

// openSessions builds the open-session index once. A partial listing fails
// the pass rather than being used: a missing row would read a live session as
// gone, and gone is what licenses a close.
func (r *orderWispOwnerResolver) openSessions() (*orderWispOpenSessions, error) {
	if r.openIndex != nil {
		return r.openIndex, nil
	}
	index := &orderWispOpenSessions{ids: map[string]struct{}{}, templates: map[string]struct{}{}}
	for _, store := range r.stores {
		open, err := orderWispSessionInfos(store, false)
		if err != nil {
			return nil, fmt.Errorf("listing open session beads: %w", err)
		}
		for _, info := range open {
			if info.Closed {
				continue
			}
			for _, id := range session.AssigneeIdentities(info) {
				index.ids[id] = struct{}{}
			}
			if template := strings.TrimSpace(info.Template); template != "" {
				index.templates[template] = struct{}{}
			}
		}
	}
	r.openIndex = index
	return index, nil
}

// orderWispSubtreeVerdict decides whether a stale subtree may be closed, and
// returns the verdict to report. A held or unobservable claim on any open
// member keeps the whole subtree open. A root-only wisp is itself the unit of
// work, so it must also carry an abandoned claim: unclaimed, it is queued
// demand. A molecule whose open members carry no claim keeps the stale-subtree
// semantics the operator sweep always had.
func orderWispSubtreeVerdict(root beads.Bead, subtree []beads.Bead, owners *orderWispOwnerResolver) (orderWispOwnerVerdict, bool, error) {
	rootVerdict, err := owners.verdict(root)
	if err != nil || rootVerdict.protects() {
		return rootVerdict, false, err
	}
	for _, b := range subtree {
		if b.ID == root.ID || b.Status == "closed" {
			continue
		}
		v, err := owners.verdict(b)
		if err != nil || v.protects() {
			return v, false, err
		}
	}
	if isOrderRootOnlyWispCandidate(root) {
		return rootVerdict, rootVerdict.State == orderWispAbandoned, nil
	}
	return rootVerdict, true, nil
}

// orderWispWatchdogOrder is one formula order the watchdog sweeps, with the
// cutoff its wisps are judged against.
type orderWispWatchdogOrder struct {
	order      orders.Order
	scoped     string
	staleAfter time.Duration
}

// orderWispWatchdogOrdersFor returns the orders whose wisps the watchdog
// judges: every enabled formula order whose rig is not suspended. A suspended
// rig's orders do not dispatch, so nothing they hold gates a firing, and the
// watchdog leaves a frozen rig's state for its resume.
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

// orderWispWatchdogLeg is one store and the orders whose wisps can live in it.
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

// orderWispWatchdogLegs assigns each order to the stores its wisps can live in:
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
			// reaches the same leg twice; judge its wisps once.
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

// orderWispWatchdogResult is one pass's outcome, in the terms its log lines
// report.
type orderWispWatchdogResult struct {
	closed      int
	closedRoots []string
	left        []orderWispLeftOpen
}

// orderWispLeftOpen is a stale root the watchdog left open for an operator.
type orderWispLeftOpen struct {
	rootID  string
	order   string
	verdict orderWispOwnerVerdict
}

// sweepStaleOrderWisps runs one watchdog pass over legs. Store errors are
// collected and the pass continues, so one unreachable rig cannot shield
// every other store's stale wisps.
func sweepStaleOrderWisps(legs []orderWispWatchdogLeg, now time.Time, owners *orderWispOwnerResolver) (orderWispWatchdogResult, error) {
	var result orderWispWatchdogResult
	var errs []error
	for _, leg := range legs {
		// Unwrap the sweep's scope label first: the wrapper does not forward
		// Handles(), so a Live read through it would silently be a cached one.
		store := unwrapOrderTrackingSweepStore(leg.store)
		legOwners := owners.forStore(store)
		for _, o := range leg.orders {
			cutoff := now.Add(-o.staleAfter)
			roots, err := staleOrderWispRootsForOrder(store, o.scoped, cutoff)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, root := range roots {
				if err := sweepStaleOrderWispRoot(store, root, o, cutoff, legOwners, &result); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return result, errors.Join(errs...)
}

// sweepStaleOrderWispRoot judges one candidate root and closes its subtree when
// the claim on it is abandoned. The close is guarded: the root is re-read
// live and its claim compared with the one judged, so a worker that claimed
// it in the meantime keeps it. A claim landing between that re-read and the
// close write is the residual every non-conditional close in this package
// carries (see releasePoolAssignmentWithRecheck); the next evaluation of the
// order sees the closed wisp and the claimant sees its bead closed.
func sweepStaleOrderWispRoot(store beads.Store, root beads.Bead, o orderWispWatchdogOrder, cutoff time.Time, owners *orderWispOwnerResolver, result *orderWispWatchdogResult) error {
	subtree, err := staleOrderWispRootSubtree(store, root, cutoff)
	if err != nil || subtree == nil {
		return err
	}
	verdict, closable, err := orderWispSubtreeVerdict(root, subtree, owners)
	if err != nil {
		return fmt.Errorf("judging the claim on order wisp %s: %w", root.ID, err)
	}
	if !closable {
		if verdict.State != orderWispHeld {
			result.left = append(result.left, orderWispLeftOpen{rootID: root.ID, order: o.scoped, verdict: verdict})
		}
		return nil
	}
	live, err := beads.HandlesFor(store).Live.Get(root.ID)
	if err != nil {
		return fmt.Errorf("re-reading order wisp %s before closing it: %w", root.ID, err)
	}
	if live.Status == "closed" || orderWispClaimFingerprint(live) != orderWispClaimFingerprint(root) {
		return nil
	}
	ordered, err := closeorder.Order(store, staleOrderWispSubtreeCloseIDs(subtree))
	if err != nil {
		return fmt.Errorf("ordering the close of stale order wisp %s: %w", root.ID, err)
	}
	n, err := closeStaleOrderWispIDsWithMetadata(store, ordered, orderWispWatchdogMetadataInitiator, orderWispWatchdogCloseMetadata(o, verdict))
	result.closed += n
	if n > 0 {
		result.closedRoots = append(result.closedRoots, root.ID)
	}
	return err
}

// orderWispWatchdogCloseMetadata is the audit trail a watchdog close leaves:
// the close reason bd shows names the order, the cutoff and the holder, and
// the order and cutoff are also stamped as their own keys for tooling.
func orderWispWatchdogCloseMetadata(o orderWispWatchdogOrder, v orderWispOwnerVerdict) map[string]string {
	holder := "no open member is claimed"
	if v.Owner != "" {
		holder = fmt.Sprintf("its holder %s is not a live session (%s)", v.Owner, v.Reason)
	}
	return map[string]string{
		"close_reason":                 fmt.Sprintf("order wisp watchdog: %s wisp stayed open past run_stale_after=%s and %s", o.scoped, orderWispDurationText(o.staleAfter), holder),
		"order_wisp_sweep_order":       o.scoped,
		"order_wisp_sweep_stale_after": o.staleAfter.String(),
	}
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

// closedSummary renders the pass's close line, or "" when nothing closed.
func (r orderWispWatchdogResult) closedSummary() string {
	if r.closed == 0 {
		return ""
	}
	return fmt.Sprintf("order wisp watchdog closed %d bead(s) under %d stale order wisp(s) whose holder is gone: %s",
		r.closed, len(r.closedRoots), sampleOrderWispIDs(r.closedRoots))
}

// leftSummary renders the stale wisps the pass left open, or "" when none.
// It is printed every pass on purpose: a stale wisp the watchdog will not
// close is still gating its order, and the only way anyone learns that is
// from a line naming it.
func (r orderWispWatchdogResult) leftSummary() string {
	if len(r.left) == 0 {
		return ""
	}
	left := append([]orderWispLeftOpen(nil), r.left...)
	sort.Slice(left, func(i, j int) bool { return left[i].rootID < left[j].rootID })
	parts := make([]string, 0, len(left))
	for i, l := range left {
		if i == orderWispWatchdogReportSampleLimit {
			parts = append(parts, fmt.Sprintf("+%d more", len(left)-i))
			break
		}
		holder := l.verdict.Owner
		if holder == "" {
			holder = "unclaimed"
		}
		// %q on the holder: it is text read off a bead another city may have
		// written, and an unquoted comma would forge structure in this line.
		parts = append(parts, fmt.Sprintf("%s (%s, %q: %s)", l.rootID, l.order, holder, l.verdict.Reason))
	}
	return fmt.Sprintf("order wisp watchdog left %d stale order wisp(s) open, still gating their orders: %s",
		len(left), strings.Join(parts, ", "))
}

func sampleOrderWispIDs(ids []string) string {
	if len(ids) <= orderWispWatchdogReportSampleLimit {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s +%d more", strings.Join(ids[:orderWispWatchdogReportSampleLimit], ", "), len(ids)-orderWispWatchdogReportSampleLimit)
}

// runOrderWispWatchdog closes stale order-run wisps whose holder is gone, at
// most once every orderWispWatchdogInterval, on the dispatch path beside
// runOrderTrackingSweepWatchdog so an order it frees can fire on the same tick.
//
// The first call only arms the clock. The wisps this judges are hours old, so
// nothing is lost by waiting one interval, and the boot pass (which holds
// readiness, gastownhall/gascity#6429) pays none of the label reads.
//
// It reads the same stores the tracking watchdog sweeps plus the graph binding
// a split city writes wisp roots into, and judges holders against the
// sessions class store. Every close and every stale wisp it leaves open is
// named on stderr: a watchdog that only speaks when it acts is how the
// tracking watchdog stayed blind for 43 hours (ga-v5vnyp).
func (cr *CityRuntime) runOrderWispWatchdog(now time.Time) {
	if cr.orderWispWatchdogLast.IsZero() {
		cr.orderWispWatchdogLast = now
		return
	}
	if now.Sub(cr.orderWispWatchdogLast) < orderWispWatchdogInterval {
		return
	}
	cr.orderWispWatchdogLast = now

	watch := orderWispWatchdogOrdersFor(cr.cityPath, cr.cfg, cr.orderSet)
	if len(watch) == 0 {
		return
	}
	stores, _, closeOpened, storeErr := cr.orderTrackingSweepStores()
	defer closeOpened()
	graphStore := resolveGraphStore(cr.storageRoutes, nil, cr.cfg, cr.cityPath, cr.rec)
	legs, legErr := orderWispWatchdogLegs(cr.cityPath, cr.cfg, stores, graphStore, watch)
	owners := newOrderWispOwnerResolver(cr.cfg, cr.cityPath, cr.sessionsBeadStore().Store)
	result, sweepErr := sweepStaleOrderWisps(legs, now, owners)
	if cr.stderr == nil {
		return
	}
	if err := errors.Join(storeErr, legErr, sweepErr); err != nil {
		fmt.Fprintf(cr.stderr, "%s: order wisp watchdog: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
	}
	for _, line := range []string{result.closedSummary(), result.leftSummary()} {
		if line != "" {
			fmt.Fprintf(cr.stderr, "%s: %s\n", cr.logPrefix, line) //nolint:errcheck // best-effort stderr
		}
	}
}
