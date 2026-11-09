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
type pushedListingHidesStore struct {
	beads.Store

	mu        sync.Mutex
	liveReads int
	liveErr   error
}

func (s *pushedListingHidesStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.PushNotClosed {
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

// A bd-backed store ignores the pushdown, so its index was never narrowed and a
// non-idempotent order has nothing to confirm: the live read would only add a
// forked read per order per tick, and a hold when that read is slow.
func TestHasOpenWorkSkipsTheLiveReadOnAStoreThatCannotPush(t *testing.T) {
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
