package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// blackHoleWriteStore is a rig store whose server stopped answering writes:
// reads still return, and every close or delete blocks for the store's own
// budget and then fails, as a remote store does when its link black-holes.
type blackHoleWriteStore struct {
	beads.Store
	budget time.Duration
	writes atomic.Int32
}

func (s *blackHoleWriteStore) blackHole() error {
	s.writes.Add(1)
	time.Sleep(s.budget)
	return fmt.Errorf("remote write: %w", context.DeadlineExceeded)
}

func (s *blackHoleWriteStore) CloseAll([]string, map[string]string) (int, error) {
	return 0, s.blackHole()
}

func (s *blackHoleWriteStore) Delete(string) error { return s.blackHole() }

// ga-yuiof4: the order-tracking watchdogs run ahead of dispatch in the same
// pass and walk every store. One black-holed rig store holding a retention
// backlog cost the pass one store budget per write — 100 deletes, hours at the
// production 90-120 s budgets — and every city order waited behind it. The
// pass must stop using a store after its first failure and still dispatch.
func TestOrdersPassDispatchesPastABlackHoledRigStore(t *testing.T) {
	now := time.Now()
	cityPath := t.TempDir()
	backupDir := filepath.Join(cityPath, ".beads", "backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	stateJSON := fmt.Sprintf(`{"timestamp":%q}`, now.Add(-time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(backupDir, "backup_state.json"), []byte(stateJSON), 0o644); err != nil {
		t.Fatalf("write backup_state.json: %v", err)
	}

	var seed []beads.Bead
	for i := range 3 {
		seed = append(seed, beads.Bead{
			ID: fmt.Sprintf("stale-%02d", i), Title: "order:rig-poll", Status: "open", Type: "task",
			CreatedAt: now.Add(-time.Hour + time.Duration(i)*time.Second),
			Labels:    []string{"order-run:rig-poll", labelOrderTracking}, Ephemeral: true,
		})
	}
	const candidates = orderTrackingRetentionWatchdogDeleteBudget
	for i := range candidates + minClosedOrderTrackingRetained {
		seed = append(seed, beads.Bead{
			ID: fmt.Sprintf("closed-%03d", i), Title: "order:rig-poll", Status: "closed", Type: "task",
			CreatedAt: now.Add(-8*24*time.Hour + time.Duration(i)*time.Minute),
			Labels:    []string{"order-run:rig-poll", labelOrderTracking}, Ephemeral: true,
		})
	}
	rig := &blackHoleWriteStore{Store: beads.NewMemStoreFrom(1000, seed, nil), budget: 40 * time.Millisecond}

	var dispatchedAfter atomic.Int64
	start := time.Now()
	od := &recordingOrderDispatcher{onDispatch: func(context.Context, string, time.Time) {
		dispatchedAfter.Store(int64(time.Since(start)))
	}}
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityName: "test-city",
		cityPath: cityPath,
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Rigs:      []config.Rig{{Name: "frontend", Path: filepath.Join(cityPath, "frontend")}},
		},
		standaloneCityStore: beads.NewMemStore(),
		standaloneRigStores: map[string]beads.Store{"frontend": rig},
		od:                  od,
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              &stderr,
		logPrefix:           "gc test",
	}

	cr.dispatchOrders(context.Background(), cityPath, false)

	if od.calls.Load() != 1 {
		t.Fatalf("dispatch calls = %d, want 1\nstderr:\n%s", od.calls.Load(), stderr.String())
	}
	// Each black-holed write costs 40ms; the pre-fix pass spends >4s on them.
	if got := time.Duration(dispatchedAfter.Load()); got > time.Second {
		t.Fatalf("city order dispatched %s after the pass started (rig store absorbed %d black-holed writes first), want <= 1s\nstderr:\n%s",
			got, rig.writes.Load(), stderr.String())
	}
	if got := strings.Count(stderr.String(), `store circuit tripped for rig "frontend"`); got != 1 {
		t.Fatalf("circuit-trip log lines for the rig store = %d, want exactly 1\nstderr:\n%s", got, stderr.String())
	}
}
