package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// A dark rig store must not hold the city order behind it (ga-rs4ddy): it fires
// within about one budget, and the dark store absorbs at most one call per tick.
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
	t.Cleanup(func() { dark.release(); <-passDone })
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

func openTestCircuitStore(t *testing.T, inner beads.Store, log *bytes.Buffer) beads.Store {
	s, err := newOrderStoreCircuit(200*time.Millisecond, log).open("rig\x00/x", func() (beads.Store, error) { return inner, nil })
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func TestStoreCircuitTripsOnTimeoutThenShortCircuits(t *testing.T) {
	dark := newBlackHoleStore(t)
	var log bytes.Buffer
	s := openTestCircuitStore(t, dark, &log)
	if _, err := s.List(beads.ListQuery{AllowScan: true}); !errors.Is(err, errStoreDark) || !beads.IsTimeoutError(err) {
		t.Fatalf("List on a black hole = %v, want errStoreDark of the timeout class", err)
	}
	start := time.Now()
	if _, err := beads.HandlesFor(s).Live.List(beads.ListQuery{AllowScan: true}); !errors.Is(err, errStoreDark) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("Live.List on a dark store = %v after %s, want errStoreDark immediately", err, time.Since(start))
	}
	if got, n := dark.calls.Load(), strings.Count(log.String(), "is dark for the rest"); got != 1 || n != 1 {
		t.Fatalf("black hole saw %d calls and the trip logged %d times, want 1 and 1:\n%s", got, n, log.String())
	}
}

func TestStoreCircuitTripsOnUnreachableNotOnNotFound(t *testing.T) {
	var log bytes.Buffer
	healthy := openTestCircuitStore(t, beads.NewMemStore(), &log)
	if _, err := healthy.Get("missing-1"); !errors.Is(err, beads.ErrNotFound) || log.Len() != 0 {
		t.Fatalf("Get(missing) = %v (log %q), want ErrNotFound and no trip", err, log.String())
	}
	if _, err := healthy.Create(beads.Bead{Title: "still reachable"}); err != nil {
		t.Fatalf("Create after not-found = %v, want success", err)
	}
	down := openTestCircuitStore(t, &getErrStore{beads.NewMemStore(), errors.New("dial tcp 10.0.0.9:3307: connection refused")}, &log)
	if _, err := down.Get("x-1"); err == nil || errors.Is(err, errStoreDark) {
		t.Fatalf("first Get = %v, want the store's own error", err)
	}
	if _, err := down.List(beads.ListQuery{AllowScan: true}); !errors.Is(err, errStoreDark) {
		t.Fatalf("List after an unreachable error = %v, want errStoreDark", err)
	}
	c, opens := newOrderStoreCircuit(time.Second, &log), 0
	open := func() (beads.Store, error) { opens++; return nil, errors.New("dolt server unreachable") }
	if _, err := c.open("k", open); err == nil {
		t.Fatal("failing open succeeded")
	}
	if _, err := c.open("k", open); !errors.Is(err, errStoreDark) || opens != 1 {
		t.Fatalf("second open = %v after %d opens, want errStoreDark and one open", err, opens)
	}
}
