package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// blackHoleStore is a dark remote store: every bd call it makes blocks until
// test cleanup, so no store method can answer.
type blackHoleStore struct {
	*beads.BdStore
	calls   atomic.Int32
	release func()
}

func newBlackHoleStore(t *testing.T) *blackHoleStore {
	release := make(chan struct{})
	b := &blackHoleStore{release: sync.OnceFunc(func() { close(release) })}
	t.Cleanup(b.release)
	b.BdStore = beads.NewBdStore(t.TempDir(), func(string, string, ...string) ([]byte, error) {
		b.calls.Add(1)
		<-release
		return nil, errors.New("black hole released")
	})
	return b
}

// A dark rig store must not hold the city order behind it (ga-rs4ddy): the
// city order fires within about one store budget of the pass starting, and the
// dark store absorbs at most one blocking call per tick.
func TestOrderDispatchStoreCircuitDarkRigDoesNotStallCityOrder(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dark := newBlackHoleStore(t)
	cityStore := beads.NewMemStore()
	fired := make(chan string, 8)
	rigOrder := func(name string) orders.Order {
		return orders.Order{Name: name, Rig: "frontend", Trigger: "cooldown", Interval: "1m", Exec: "true", Timeout: "1m"}
	}
	m := &memoryOrderDispatcher{
		aa: []orders.Order{
			rigOrder("rig-a"), rigOrder("rig-b"), rigOrder("rig-c"),
			{Name: "city-sweep", Trigger: "cooldown", Interval: "1m", Exec: "true", Timeout: "1m"},
		},
		storeFn: func(target execStoreTarget) (beads.Store, error) {
			if target.ScopeKind == "city" {
				return cityStore, nil
			}
			return dark, nil
		},
		execRun: func(_ context.Context, _ string, dir string, _ []string) ([]byte, error) {
			fired <- dir
			return nil, nil
		},
		rec:             events.Discard,
		stderr:          &bytes.Buffer{},
		storeCallBudget: 200 * time.Millisecond,
		cfg:             &config.City{Rigs: []config.Rig{{Name: "frontend", Path: rigDir}}},
	}

	passDone := make(chan struct{})
	t.Cleanup(func() {
		dark.release()
		select {
		case <-passDone:
		case <-time.After(time.Minute):
			t.Error("dispatch pass did not finish after the dark store was released")
		}
		m.drain(context.Background())
	})
	start := time.Now()
	go func() {
		defer close(passDone)
		m.dispatch(context.Background(), cityDir, start)
	}()

	select {
	case dir := <-fired:
		if dir != cityDir {
			t.Fatalf("first fired order ran in %q, want the city order in %q", dir, cityDir)
		}
	case <-time.After(time.Second):
		t.Fatalf("city order not dispatched within 1s of the pass starting (dark store calls so far: %d)", dark.calls.Load())
	}
	select {
	case <-passDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("dispatch pass still running 5s in (dark store calls: %d)", dark.calls.Load())
	}
	if got := dark.calls.Load(); got > 1 {
		t.Fatalf("dark store received %d blocking calls this tick, want at most 1", got)
	}
}
