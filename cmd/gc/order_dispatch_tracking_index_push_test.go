package main

import (
	"io"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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
