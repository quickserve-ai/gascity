package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

type listQueryRecordingStore struct {
	beads.Store

	mu      sync.Mutex
	queries []beads.ListQuery
}

func (s *listQueryRecordingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.queries = append(s.queries, query)
	s.mu.Unlock()
	return s.Store.List(query)
}

// The order-dispatch tracking index is the listing the not-closed pushdown was
// opted in for (ga-tyf38i): without the flag it reads every closed row of a
// shared hub on every tick, and nothing else would notice.
func TestOrderDispatchTrackingIndexListingOptsIntoNotClosedPushdown(t *testing.T) {
	store := &listQueryRecordingStore{Store: beads.NewMemStore()}
	idx := newOrderDispatchTrackingIndex(io.Discard)
	if _, err := idx.hasOpenTracking([]beads.Store{store}, []string{"store"}, "order-x"); err != nil {
		t.Fatalf("hasOpenTracking: %v", err)
	}
	if len(store.queries) != 1 {
		t.Fatalf("listings = %+v, want the one corpus read", store.queries)
	}
	query := store.queries[0]
	if !query.PushNotClosed {
		t.Fatalf("query = %+v, want PushNotClosed", query)
	}
	if query.Status != "" || query.IncludeClosed || query.CompleteActive || !query.AllowScan || query.TierMode != beads.TierBoth {
		t.Fatalf("query = %+v, want the no-status, not-closed, both-tier corpus read the pushdown can serve", query)
	}
}

// pushedListingHidesStore answers an opted-in listing as if every row had left
// the pushed status set between the guard probe and the read, the one residue
// the mayor accepted. Every other read sees the real rows.
//
// report is what the listing says about itself, as the answering store class
// would write it (beads.ListQuery.PushReport): the zero value writes nothing,
// which the gate must read as possibly narrowed.
type pushedListingHidesStore struct {
	beads.Store

	mu        sync.Mutex
	liveReads int
	liveErr   error
	report    beads.PushNotClosedOutcome
}

func (s *pushedListingHidesStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.PushNotClosed {
		s.mu.Lock()
		report := s.report
		s.mu.Unlock()
		if query.PushReport != nil && report != beads.PushNotClosedUnreported {
			*query.PushReport = report
		}
		return nil, nil
	}
	if strings.HasPrefix(query.Label, "order-run:") {
		s.mu.Lock()
		s.liveReads++
		s.mu.Unlock()
		if s.liveErr != nil {
			return nil, s.liveErr
		}
	}
	return s.Store.List(query)
}

func (s *pushedListingHidesStore) labelReads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveReads
}

func createMoleculeRun(t *testing.T, store beads.Store, scoped string) {
	t.Helper()
	root, err := store.Create(beads.Bead{Title: "run", Type: "molecule", Labels: []string{"order-run:" + scoped}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{Title: "step", ParentID: root.ID}); err != nil {
		t.Fatal(err)
	}
}

func gateCandidate(store beads.Store, idempotent bool) *orderDispatchCandidate {
	return &orderDispatchCandidate{
		order:         orders.Order{Name: "digest", Idempotent: idempotent},
		scoped:        "digest",
		gateStores:    []beads.Store{store},
		gateStoreKeys: []string{"store"},
	}
}

// The mayor's condition (a), ruling 2026-11-09 on ga-tyf38i: an order whose
// repeat is not a no-op never relies on the pushed listing for "no open work",
// and an idempotent order keeps the index's answer with no extra hub read.
func TestOpenWorkGateConfirmsAbsenceLiveOnlyForANonIdempotentOrder(t *testing.T) {
	for _, tc := range []struct {
		name          string
		idempotent    bool
		wantShut      bool
		wantLiveReads int
	}{
		{"a non-idempotent order reads live and is held", false, true, 1},
		{"an idempotent order keeps the index answer", true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &pushedListingHidesStore{Store: beads.NewMemStore()}
			createMoleculeRun(t, store.Store, "digest")
			var stderr bytes.Buffer
			m := &memoryOrderDispatcher{stderr: &stderr, rec: events.Discard}
			idx := newOrderDispatchTrackingIndex(&stderr)
			if got := m.openWorkGateShut(context.Background(), gateCandidate(store, tc.idempotent), idx, time.Now()); got != tc.wantShut {
				t.Fatalf("gate shut = %t, want %t; stderr: %s", got, tc.wantShut, stderr.String())
			}
			if got := store.labelReads(); got != tc.wantLiveReads {
				t.Fatalf("live label reads = %d, want %d", got, tc.wantLiveReads)
			}
			if named := strings.Contains(stderr.String(), "digest: order-run index for store store found no open work but the live read did"); named != !tc.idempotent {
				t.Fatalf("disagreement line present = %t, want %t; stderr: %s", named, !tc.idempotent, stderr.String())
			}
		})
	}
}

// A live read that fails gives no proof of absence, so the order is held.
func TestOpenWorkGateHoldsANonIdempotentOrderWhenTheLiveReadFails(t *testing.T) {
	store := &pushedListingHidesStore{Store: beads.NewMemStore(), liveErr: errors.New("hub unavailable")}
	m := &memoryOrderDispatcher{stderr: io.Discard, rec: events.Discard}
	idx := newOrderDispatchTrackingIndex(io.Discard)
	if !m.openWorkGateShut(context.Background(), gateCandidate(store, false), idx, time.Now()) {
		t.Fatal("gate open on a failed live read, want the order held")
	}
}

// An index positive cannot be wrong about a hidden row, so it costs no live read.
func TestOpenWorkGateTrustsAnIndexPositiveWithoutALiveRead(t *testing.T) {
	store := &recordingLabelReadStore{Store: beads.NewMemStore()}
	createMoleculeRun(t, store.Store, "digest")
	m := &memoryOrderDispatcher{stderr: io.Discard, rec: events.Discard}
	idx := newOrderDispatchTrackingIndex(io.Discard)
	if !m.openWorkGateShut(context.Background(), gateCandidate(store, false), idx, time.Now()) {
		t.Fatal("gate open, want it held by the molecule root the index read")
	}
	if store.labelReads != 0 {
		t.Fatalf("live label reads = %d, want 0 on an index positive", store.labelReads)
	}
}

// The mayor's term 3: an order-run root reaches the single-flight answer through
// the fold the opted-in listing feeds, in whatever not-closed status it sits.
func TestAnOrderRunRootHoldsTheGateThroughTheFold(t *testing.T) {
	for _, status := range []string{"", "in_progress"} {
		store := beads.NewMemStore()
		root, err := store.Create(beads.Bead{Title: "run", Type: "molecule", Labels: []string{"order-run:digest"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(beads.Bead{Title: "step", ParentID: root.ID}); err != nil {
			t.Fatal(err)
		}
		if status != "" {
			if err := store.Update(root.ID, beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatal(err)
			}
		}
		idx := newOrderDispatchTrackingIndex(io.Discard)
		entries, err := idx.entriesForStore(store, "store")
		if err != nil {
			t.Fatal(err)
		}
		if got := len(entries["digest"].openRoots); got != 1 {
			t.Fatalf("status %q: open roots = %d, want 1", status, got)
		}
		m := &memoryOrderDispatcher{stderr: io.Discard, rec: events.Discard}
		if !m.openWorkGateShut(context.Background(), gateCandidate(store, true), idx, time.Now()) {
			t.Fatalf("status %q: gate open, want the root to hold single-flight", status)
		}
	}
}

type recordingLabelReadStore struct {
	beads.Store
	labelReads int
}

func (s *recordingLabelReadStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if strings.HasPrefix(query.Label, "order-run:") {
		s.labelReads++
	}
	return s.Store.List(query)
}

// The mayor's ruling A (ga-tyf38i, 2026-11-10T02:52:18Z): the live confirmation
// runs exactly where the pushdown ran, judged by what the listing reported when
// it ran. Each store class's report is pinned in package beads
// (TestAPushNotClosedListingReportsWhetherItWasNarrowed: native and a proxy
// serving native say narrowed; bd, a cache over bd and a proxy demoted to bd say
// not narrowed). Here: the gate on each report, and a real bd store end to end.
func TestOpenWorkGateConfirmsAbsenceOnlyWhereTheListingWasNarrowed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		report        beads.PushNotClosedOutcome
		wantShut      bool
		wantLiveReads int
	}{
		{"a narrowed listing (native) is confirmed live", beads.PushNotClosedNarrowed, true, 1},
		{"a listing nothing narrowed (bd, a demoted proxy) keeps the index answer", beads.PushNotClosedNotNarrowed, false, 0},
		{"a listing that reported nothing is confirmed live", beads.PushNotClosedUnreported, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &pushedListingHidesStore{Store: beads.NewMemStore(), report: tc.report}
			createMoleculeRun(t, store.Store, "digest")
			m := &memoryOrderDispatcher{stderr: io.Discard, rec: events.Discard}
			idx := newOrderDispatchTrackingIndex(io.Discard)
			if got := m.openWorkGateShut(context.Background(), gateCandidate(store, false), idx, time.Now()); got != tc.wantShut {
				t.Fatalf("gate shut = %t, want %t", got, tc.wantShut)
			}
			if got := store.labelReads(); got != tc.wantLiveReads {
				t.Fatalf("live label reads = %d, want %d", got, tc.wantLiveReads)
			}
		})
	}
}

// The fact is the listing's, carried to the gate: a store that would answer
// differently now (a proxy demoted to bd after its narrowed listing) does not
// change what the gate owes that listing.
func TestOpenWorkGateUsesTheReportRecordedAtListingTime(t *testing.T) {
	store := &pushedListingHidesStore{Store: beads.NewMemStore(), report: beads.PushNotClosedNarrowed}
	createMoleculeRun(t, store.Store, "digest")
	idx := newOrderDispatchTrackingIndex(io.Discard)
	if _, err := idx.entriesForStore(store, "store"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.report = beads.PushNotClosedNotNarrowed // the demotion, after the listing
	store.mu.Unlock()
	m := &memoryOrderDispatcher{stderr: io.Discard, rec: events.Discard}
	if !m.openWorkGateShut(context.Background(), gateCandidate(store, false), idx, time.Now()) {
		t.Fatal("gate open, want the narrowed listing confirmed live")
	}
	if got := store.labelReads(); got != 1 {
		t.Fatalf("live label reads = %d, want 1", got)
	}
}

// A real bd store reports its listing not narrowed, so a non-idempotent order
// pays no forked read per firing and cannot be held by a slow one.
func TestHasOpenWorkSkipsTheLiveReadOnABdStore(t *testing.T) {
	store := beads.NewBdStore(t.TempDir(), func(string, string, ...string) ([]byte, error) {
		return []byte("[]"), nil
	})
	idx := newOrderDispatchTrackingIndex(io.Discard)
	liveReads := 0
	open, err := idx.hasOpenWork([]beads.Store{store}, []string{"store"}, "digest", nil,
		func(beads.Store, string) (bool, error) {
			liveReads++
			return false, nil
		}, true)
	if err != nil {
		t.Fatalf("hasOpenWork: %v", err)
	}
	if open || liveReads != 0 {
		t.Fatalf("open = %t, live reads = %d; want the index's answer and no live read", open, liveReads)
	}
}
