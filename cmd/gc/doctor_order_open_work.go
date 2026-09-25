package main

import (
	"fmt"
	"io"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/orders"
)

// doctorOrderFiringCurrentOpenWorkFunc answers "which open bead holds this
// order's single-flight gate" for the order-firing check (ga-puy7n0 ask 2).
//
// It reads the legs the dispatcher's gate reads: the order's scope stores and
// the orders binding (cachedOrderHistoryStoresResolver), plus the graph binding
// a split city writes wisp roots into. It applies the gate's own wisp-root
// predicate through orders.Store.OpenWork, the evidence form of HasOpenWork, so
// the bead doctor names is the bead the gate is refusing on.
//
// The holder verdict comes from the order wisp watchdog's resolver, so the
// detail line also says why the watchdog has not closed the bead: a live
// holder, a claim this city cannot prove is its own, or an unclaimed run the
// watchdog never closes.
//
// Nothing is resolved or opened until the check asks about its first non-OK
// order, so a healthy city pays nothing for the lookup (ga-9ymvnl).
func doctorOrderFiringCurrentOpenWorkFunc(cityPath string, cfg *config.City, stderr io.Writer) doctor.OrderFiringCurrentOpenWorkFunc {
	if stderr == nil {
		stderr = io.Discard
	}
	var (
		setup         sync.Once
		resolveStores orderStoresResolver
		graphStore    beads.Store
	)
	sessions := &doctorOrderWispSessions{cityPath: cityPath, cfg: cfg}
	return func(order orders.Order) (doctor.OrderFiringOpenWork, bool, error) {
		setup.Do(func() {
			resolveStores = cachedOrderHistoryStoresResolver(cityPath, cfg, stderr)
			graphStore = orderWispSweepStore(cityPath, cfg)
		})
		typed, err := resolveStores(order)
		if err != nil {
			return doctor.OrderFiringOpenWork{}, false, err
		}
		stores := rawOrderStores(typed)
		if graphStore != nil && !storeListContains(stores, graphStore) {
			stores = append(stores, graphStore)
		}
		for _, store := range stores {
			front := orders.NewStoreWithGraph(beads.OrdersStore{Store: store}, beads.GraphStore{Store: store})
			b, found, err := front.OpenWork(order.ScopedName(), orderWispRootHasOpenWork)
			if err != nil {
				return doctor.OrderFiringOpenWork{}, false, err
			}
			if found {
				return describeOrderFiringOpenWork(b, order, func(b beads.Bead) (orderWispOwnerVerdict, error) {
					return sessions.verdict(store, b)
				}), true, nil
			}
		}
		return doctor.OrderFiringOpenWork{}, false, nil
	}
}

// describeOrderFiringOpenWork renders the gating bead for doctor's detail line.
// judge is consulted only for a run root: an order-tracking bead holds the gate
// because a dispatch is in flight, and has no claim to judge.
func describeOrderFiringOpenWork(b beads.Bead, order orders.Order, judge func(beads.Bead) (orderWispOwnerVerdict, error)) doctor.OrderFiringOpenWork {
	work := doctor.OrderFiringOpenWork{ID: b.ID, CreatedAt: b.CreatedAt}
	if beadLabelsContain(b.Labels, labelOrderTracking) {
		work.Note = "order-tracking bead: a dispatch is in flight"
		return work
	}
	verdict, err := judge(b)
	if err != nil {
		if refs := orderWispClaimRefs(b); len(refs) > 0 {
			work.Holder = refs[0].value
		}
		work.Note = fmt.Sprintf("holder check failed: %v", err)
		return work
	}
	work.Holder = verdict.Owner
	switch verdict.State {
	case orderWispHeld:
		work.Note = verdict.Reason
	case orderWispUnobservable:
		work.Note = verdict.Reason + "; the watchdog never closes a claim this city cannot prove is its own"
	case orderWispAbandoned:
		work.Note = fmt.Sprintf("%s; the watchdog closes it once it is %s old", verdict.Reason, orderWispDurationText(order.RunStaleAfterOrDefault()))
	}
	return work
}

// doctorOrderWispSessions opens the city's session-class store once, on the
// first holder verdict a doctor run needs, and shares it across the check's
// parallel lookups. A healthy city, or one whose gates are all held by
// tracking beads, never opens it.
type doctorOrderWispSessions struct {
	cityPath string
	cfg      *config.City

	once  sync.Once
	store beads.Store
	err   error
}

// verdict judges who holds b, reading the session store and beadStore, the
// store b lives in (where a graph-resident session bead would be). Each call
// builds its own resolver because a resolver is single-use and the check's
// lookups run in parallel.
func (s *doctorOrderWispSessions) verdict(beadStore beads.Store, b beads.Bead) (orderWispOwnerVerdict, error) {
	s.once.Do(func() {
		cityStore, err := openStoreAtForCity(s.cityPath, s.cityPath)
		if err != nil {
			s.err = fmt.Errorf("opening the city store: %w", err)
			return
		}
		s.store = cliSessionStore(cityStore, s.cfg, s.cityPath)
	})
	if s.err != nil {
		return orderWispOwnerVerdict{}, s.err
	}
	return newOrderWispOwnerResolver(s.cfg, s.cityPath, s.store).forStore(beadStore).verdict(b)
}
