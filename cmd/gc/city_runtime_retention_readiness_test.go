package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// retentionReadBlockingStore blocks the order-tracking retention read until
// released, signaling each attempt on hit. That read (every closed
// order-tracking bead with no limit, orders.Store.ClosedRunsForRetention) is
// made only by the retention sweep: the boot backlog advisory and the doctor
// check read the same set with a Limit, so Limit == 0 isolates the sweep from
// every other read on the run path.
type retentionReadBlockingStore struct {
	beads.Store
	block <-chan struct{}
	hit   chan struct{}
}

func (s *retentionReadBlockingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Status == "closed" && q.Label == labelOrderTracking && q.Limit == 0 {
		select {
		case s.hit <- struct{}{}:
		default:
		}
		<-s.block
	}
	return s.Store.List(q)
}

// TestCityRuntimeRun_ReadyBeforeRetentionSweep verifies that the controller
// reports the city ready before it runs the order-tracking retention sweep.
// run()'s startup-orders step calls dispatchOrders before onStarted, and
// dispatchOrders runs the retention watchdog, whose interval stamp is zero at
// boot: the first pass reads and prunes the whole closed order-tracking set
// while the city is still unready, so a slow store with a large closed backlog
// keeps the API answering 404 for the entire pass. Same shape as the
// gastownhall/gascity#3288 boot hang, fixed by deferring the sweep off the boot
// reconcile: with the retention read blocked the city must still report ready,
// and the first steady-state tick must still reach the read, so the sweep is
// deferred rather than dropped.
func TestCityRuntimeRun_ReadyBeforeRetentionSweep(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	// Beads are 8 days old (> 7d default TTL); the 2 oldest exceed the
	// retain-10 floor, so the pass has deletions to make. The city has no
	// .beads backup state, so the bulk-delete backup gate reads it as safe and
	// lets the pass run.
	now := time.Now()
	seed := make([]beads.Bead, 0, minClosedOrderTrackingRetained+2)
	for i := range minClosedOrderTrackingRetained + 2 {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("ready-%02d", i),
			Title:     "order:ready",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-8*24*time.Hour + time.Duration(i)*time.Minute),
			Labels:    []string{"order-run:ready", labelOrderTracking},
			Ephemeral: true,
		})
	}
	block := make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(block) }) }
	store := &retentionReadBlockingStore{
		Store: beads.NewMemStoreFrom(100, seed, nil),
		block: block,
		hit:   make(chan struct{}, 8),
	}

	sp := runtime.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// onStarted holds the runtime at the readiness point until resume is
	// closed, so no steady-state tick can race the readiness assertion.
	started := make(chan struct{})
	resume := make(chan struct{})
	pokeCh := make(chan struct{}, 1)
	var stderr lockedBuffer
	cr, runtimeErr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		PokeCh: pokeCh,
		OnStarted: func() {
			close(started)
			select {
			case <-resume:
			case <-ctx.Done():
			}
		},
		Stdout: io.Discard,
		Stderr: &stderr,
	})
	if runtimeErr != nil {
		t.Fatalf("building the city runtime: %v", runtimeErr)
	}

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = store
	cr.setControllerState(cs)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		cr.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		unblock() // free the runtime if it is parked on the retention read
		<-runDone
	})

	// Readiness must not wait on the retention read.
	select {
	case <-started:
	case <-store.hit:
		t.Fatalf("order-tracking retention read reached before the city reported ready; the startup pass was NOT deferred\nstderr:\n%s", stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("city did not report ready within 10s\nstderr:\n%s", stderr.String())
	}

	// The first steady-state tick MUST reach the retention read.
	close(resume)
	select {
	case pokeCh <- struct{}{}:
	default:
	}
	select {
	case <-store.hit:
		// good: the sweep ran after readiness.
	case <-time.After(10 * time.Second):
		t.Fatalf("first steady-state tick did not reach the order-tracking retention read; the sweep was dropped, not deferred\nstderr:\n%s", stderr.String())
	}
	unblock()
}
