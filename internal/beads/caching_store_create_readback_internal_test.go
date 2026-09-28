package beads

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// readBackFailingStore fails the first Get of each created bead with getErr, the
// read-back createWith does right after the create. Later Gets pass through.
type readBackFailingStore struct {
	Store
	getErr error

	mu     sync.Mutex
	failed map[string]bool
}

func (s *readBackFailingStore) Get(id string) (Bead, error) {
	s.mu.Lock()
	if s.failed == nil {
		s.failed = map[string]bool{}
	}
	first := !s.failed[id]
	s.failed[id] = true
	s.mu.Unlock()
	if first {
		return Bead{}, s.getErr
	}
	return s.Store.Get(id)
}

// TestCachingStoreCreateRecordsEveryFailedReadBack (ga-th31cy): the read-back
// after a create used to be recorded only for non-not-found errors, and an
// incomplete lookup (ErrVerifyIndeterminate wraps ErrNotFound) was absorbed
// CLEAN with no record, so the cache served a row verification could not find.
// Now every failure is recorded; a failure that proves nothing leaves the row
// dirty so the next read goes to storage, and a proven not-found stays clean
// (a store that cannot read back fresh wisps must not have its mail dropped).
func TestCachingStoreCreateRecordsEveryFailedReadBack(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		getErr    error
		wantDirty bool
	}{
		{"indeterminate lookup", fmt.Errorf("getting bead: %w: timeout", ErrVerifyIndeterminate), true},
		{"read error", errors.New("connection reset by peer"), true},
		{"proven not found", fmt.Errorf("getting bead: %w", ErrNotFound), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &readBackFailingStore{Store: NewMemStore(), getErr: tc.getErr}
			cache := NewCachingStoreForTest(backing, nil)

			// A store already in reconcile backoff: the read-back record must not
			// move the backoff anchor, or steady sends keep the retry from ever
			// coming due (review finding on #185; anchor field since ga-yarqx9).
			anchor := time.Now().Add(-time.Minute)
			cache.mu.Lock()
			cache.syncFailures = 3
			cache.lastSyncFailureAt = anchor
			cache.mu.Unlock()

			created, err := cache.Create(Bead{Title: "mail", Type: "message"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			stats := cache.Stats()
			if stats.ProblemCount != 1 || !strings.Contains(stats.LastProblem, "refresh bead after create") {
				t.Fatalf("ProblemCount=%d LastProblem=%q, want the failed read-back recorded", stats.ProblemCount, stats.LastProblem)
			}
			cache.mu.RLock()
			gotAnchor := cache.lastSyncFailureAt
			cache.mu.RUnlock()
			if !gotAnchor.Equal(anchor) {
				t.Fatalf("lastSyncFailureAt moved %v -> %v; it is the reconcile backoff anchor", anchor, gotAnchor)
			}
			cache.mu.RLock()
			_, dirty := cache.dirty[created.ID]
			_, cached := cache.beads[created.ID]
			cache.mu.RUnlock()
			if !cached {
				t.Fatalf("created row not absorbed; the cache must keep it in every case")
			}
			if dirty != tc.wantDirty {
				t.Fatalf("dirty = %v, want %v", dirty, tc.wantDirty)
			}
		})
	}
}
