package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
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
// as a store that cannot push does, which the gate reads as not narrowed.
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

// createMoleculeRun opens a molecule run of the "digest" order.
func createMoleculeRun(t *testing.T, store beads.Store) {
	t.Helper()
	root, err := store.Create(beads.Bead{Title: "run", Type: "molecule", Labels: []string{"order-run:digest"}})
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
			store := &pushedListingHidesStore{Store: beads.NewMemStore(), report: beads.PushNotClosedNarrowed}
			createMoleculeRun(t, store.Store)
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
	store := &pushedListingHidesStore{Store: beads.NewMemStore(), liveErr: errors.New("hub unavailable"), report: beads.PushNotClosedNarrowed}
	m := &memoryOrderDispatcher{stderr: io.Discard, rec: events.Discard}
	idx := newOrderDispatchTrackingIndex(io.Discard)
	if !m.openWorkGateShut(context.Background(), gateCandidate(store, false), idx, time.Now()) {
		t.Fatal("gate open on a failed live read, want the order held")
	}
}

// An index positive cannot be wrong about a hidden row, so it costs no live read.
func TestOpenWorkGateTrustsAnIndexPositiveWithoutALiveRead(t *testing.T) {
	store := &recordingLabelReadStore{Store: beads.NewMemStore()}
	createMoleculeRun(t, store.Store)
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
		{"a listing that reported nothing (a store that cannot push) keeps the index answer", beads.PushNotClosedUnreported, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &pushedListingHidesStore{Store: beads.NewMemStore(), report: tc.report}
			createMoleculeRun(t, store.Store)
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

// The fact is the listing's, carried to the gate, in both directions. A store
// that would answer differently now (a proxy demoted to bd after its narrowed
// listing, or one promoted after a listing nothing narrowed) does not change
// what the gate owes that listing. The narrowed direction also fails if the
// index stops keeping the report with the entries it cached: a missing report
// reads as not narrowed, so the read that narrowed listing owes would be lost.
func TestOpenWorkGateUsesTheReportRecordedAtListingTime(t *testing.T) {
	for _, tc := range []struct {
		name          string
		atListing     beads.PushNotClosedOutcome
		later         beads.PushNotClosedOutcome
		wantShut      bool
		wantLiveReads int
	}{
		{"a narrowed listing is confirmed after a demotion", beads.PushNotClosedNarrowed, beads.PushNotClosedNotNarrowed, true, 1},
		{"a listing nothing narrowed stays unread after a promotion", beads.PushNotClosedNotNarrowed, beads.PushNotClosedNarrowed, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &pushedListingHidesStore{Store: beads.NewMemStore(), report: tc.atListing}
			createMoleculeRun(t, store.Store)
			idx := newOrderDispatchTrackingIndex(io.Discard)
			if _, err := idx.entriesForStore(store, "store"); err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			store.report = tc.later
			store.mu.Unlock()
			m := &memoryOrderDispatcher{stderr: io.Discard, rec: events.Discard}
			if got := m.openWorkGateShut(context.Background(), gateCandidate(store, false), idx, time.Now()); got != tc.wantShut {
				t.Fatalf("gate shut = %t, want %t", got, tc.wantShut)
			}
			if got := store.labelReads(); got != tc.wantLiveReads {
				t.Fatalf("live label reads = %d, want %d", got, tc.wantLiveReads)
			}
		})
	}
}

// The mayor's ruling 2026-11-10T08:52:17Z (ga-tyf38i): the confirmation read
// runs only where a leaf REPORTED that the pushdown narrowed its listing. One
// case per leaf class the gate can hold, each asserting whether a confirm read
// happens for a due non-idempotent order the index found no open work for.
// The stores are real where cmd/gc can build them; the native and proxied
// leaves are a double that writes what package beads pins those classes to
// write (TestAPushNotClosedListingReportsWhetherItWasNarrowed: native and a
// proxy serving native say narrowed; native whose set could not be read says
// not narrowed). The storebinding front doors are not gate stores; they pass
// a listing to their inner store, pinned in their own packages.
//
// Before this ruling every store but bd and native reported nothing, the gate
// read that as narrowed, and every due non-idempotent order on such a store
// took the forked read each tick (bisect arm D: removing it ended the M1 and
// suspension quiescence acceptance failures, 0 of 20).
func TestHasOpenWorkConfirmsAbsenceOnlyOnALeafThatReportedNarrowed(t *testing.T) {
	newBd := func() *beads.BdStore {
		return beads.NewBdStore(t.TempDir(), func(string, string, ...string) ([]byte, error) {
			return []byte("[]"), nil
		})
	}
	newExec := func() beads.Store {
		path := filepath.Join(t.TempDir(), "beads-provider")
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho '[]'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return beadsexec.NewStore(path)
	}
	newFile := func() beads.Store {
		store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	newSQLite := func() beads.Store {
		store, err := beads.OpenSQLiteStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	reporting := func(report beads.PushNotClosedOutcome) func() beads.Store {
		return func() beads.Store { return &pushedListingHidesStore{Store: beads.NewMemStore(), report: report} }
	}
	for _, tc := range []struct {
		name       string
		store      func() beads.Store
		wantReads  int
		reportedAs string
	}{
		{"native, narrowed (double)", reporting(beads.PushNotClosedNarrowed), 1, "narrowed"},
		{"a proxy serving native (double)", reporting(beads.PushNotClosedNarrowed), 1, "narrowed"},
		{"native, set unreadable (double)", reporting(beads.PushNotClosedNotNarrowed), 0, "not narrowed"},
		{"a proxy demoted to bd (double)", reporting(beads.PushNotClosedNotNarrowed), 0, "not narrowed"},
		{"bd", func() beads.Store { return newBd() }, 0, "not narrowed"},
		{"the bead policy over bd", func() beads.Store { return &beadPolicyStore{Store: newBd()} }, 0, "not narrowed"},
		{"the bead policy over a cache over bd", func() beads.Store {
			return &beadPolicyStore{Store: beads.NewCachingStoreForTest(newBd(), nil)}
		}, 0, "not narrowed"},
		{"exec (a provider script)", newExec, 0, "nothing"},
		{"the bead policy over a cache over exec", func() beads.Store {
			return &beadPolicyStore{Store: beads.NewCachingStoreForTest(newExec(), nil)}
		}, 0, "nothing"},
		{"file", newFile, 0, "nothing"},
		{"mem", func() beads.Store { return beads.NewMemStore() }, 0, "nothing"},
		{"sqlite", newSQLite, 0, "nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := newOrderDispatchTrackingIndex(io.Discard)
			confirmReads := 0
			open, err := idx.hasOpenWork([]beads.Store{tc.store()}, []string{"store"}, "digest", nil,
				func(beads.Store, string) (bool, error) {
					confirmReads++
					return false, nil
				}, true)
			if err != nil {
				t.Fatalf("hasOpenWork: %v", err)
			}
			if open || confirmReads != tc.wantReads {
				t.Fatalf("a leaf that reported %s: open = %t, confirm reads = %d; want false, %d",
					tc.reportedAs, open, confirmReads, tc.wantReads)
			}
		})
	}
}
