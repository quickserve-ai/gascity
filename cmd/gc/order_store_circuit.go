package main

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// errStoreDark answers every call on a store already dark this tick (ga-rs4ddy).
var errStoreDark = errors.New("order store dark for the rest of this tick")

// orderStoreCircuit is one tick's record of dark stores: beads.Store takes no
// context, so this bounds what a dead remote store can cost the serial pass.
type orderStoreCircuit struct {
	budget time.Duration
	stderr io.Writer
	mu     sync.Mutex
	dark   map[string]error
}

func newOrderStoreCircuit(budget time.Duration, stderr io.Writer) *orderStoreCircuit {
	if budget <= 0 {
		budget = 10 * time.Second
	}
	return &orderStoreCircuit{budget: budget, stderr: stderr, dark: make(map[string]error)}
}

func (c *orderStoreCircuit) darkErr(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if reason, ok := c.dark[key]; ok {
		return fmt.Errorf("%w: store %q: %w", errStoreDark, key, reason)
	}
	return nil
}

// trip marks key dark, logging once per store per tick.
func (c *orderStoreCircuit) trip(key string, reason error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, already := c.dark[key]; !already {
		c.dark[key] = reason
		logDispatchError(c.stderr, "gc: order dispatch: store %q is dark for the rest of this tick; skipping its orders: %v", key, reason)
	}
}

// open memoizes a failed open as dark and wraps a good handle in the budget.
func (c *orderStoreCircuit) open(key string, openFn func() (beads.Store, error)) (beads.Store, error) {
	if err := c.darkErr(key); err != nil {
		return nil, err
	}
	store, err := openFn()
	if err != nil {
		c.trip(key, err)
		return nil, err
	}
	return &circuitStore{in: store, key: key, c: c}, nil
}

// circuitCall runs fn on its own goroutine under the budget; an overrun or an
// unreachable-class error trips the store. A late result is dropped.
func circuitCall[T any](s *circuitStore, fn func() (T, error)) (T, error) {
	var zero T
	if err := s.c.darkErr(s.key); err != nil {
		return zero, err
	}
	done := make(chan func() (T, error), 1)
	go func() {
		v, err := fn()
		done <- func() (T, error) { return v, err }
	}()
	select {
	case result := <-done:
		v, err := result()
		// A RETURNED query timeout is gate contention, which idempotent orders
		// dispatch through (vp-gprv); only the budget trips on slowness.
		if err != nil && isBeadStoreUnreachable(err) && !beads.IsTimeoutError(err) {
			s.c.trip(s.key, err)
		}
		return v, err
	case <-time.After(s.c.budget):
		// "timed out after" = beads.IsTimeoutError, the gate bound's own class.
		s.c.trip(s.key, fmt.Errorf("store call timed out after %s", s.c.budget))
		return zero, s.c.darkErr(s.key)
	}
}

// circuitStore implements beads.Store in full and deliberately does not embed
// it, so no method can reach the store without the budget.
type circuitStore struct {
	in  beads.Store
	key string
	c   *orderStoreCircuit
}

// unwrapCircuitStore returns the raw handle for work that outlives the tick.
func unwrapCircuitStore(store beads.Store) beads.Store {
	if s, ok := store.(*circuitStore); ok {
		return s.in
	}
	return store
}

// Handles keeps beads.HandlesFor routing identical to the raw store (a
// CachingStore's Live reader reads its backing) with every read budgeted.
func (s *circuitStore) Handles() beads.StoreHandles {
	h := beads.HandlesFor(s.in)
	return beads.StoreHandles{Cached: circuitReader{s, h.Cached}, Live: circuitReader{s, h.Live}, Writer: s}
}

type circuitReader struct {
	s *circuitStore
	r beads.LiveReader
}

func (c circuitReader) Get(id string) (beads.Bead, error) {
	return circuitCall(c.s, func() (beads.Bead, error) { return c.r.Get(id) })
}

func (c circuitReader) List(q beads.ListQuery) ([]beads.Bead, error) {
	return circuitCall(c.s, func() ([]beads.Bead, error) { return c.r.List(q) })
}

func (c circuitReader) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	return circuitCall(c.s, func() ([]beads.Bead, error) { return c.r.Ready(q...) })
}

func (c circuitReader) DepList(id, dir string) ([]beads.Dep, error) {
	return circuitCall(c.s, func() ([]beads.Dep, error) { return c.r.DepList(id, dir) })
}

func (s *circuitStore) do(fn func() error) error {
	_, err := circuitCall(s, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

func (s *circuitStore) Close(id string) error { return s.do(func() error { return s.in.Close(id) }) }

func (s *circuitStore) Reopen(id string) error { return s.do(func() error { return s.in.Reopen(id) }) }

func (s *circuitStore) Delete(id string) error { return s.do(func() error { return s.in.Delete(id) }) }
func (s *circuitStore) Ping() error            { return s.do(s.in.Ping) }

func (s *circuitStore) Update(id string, opts beads.UpdateOpts) error {
	return s.do(func() error { return s.in.Update(id, opts) })
}

func (s *circuitStore) SetMetadata(id, key, value string) error {
	return s.do(func() error { return s.in.SetMetadata(id, key, value) })
}

func (s *circuitStore) SetMetadataBatch(id string, kvs map[string]string) error {
	return s.do(func() error { return s.in.SetMetadataBatch(id, kvs) })
}

func (s *circuitStore) SetLocalString(id, key, value string) error {
	return s.do(func() error { return s.in.SetLocalString(id, key, value) })
}

func (s *circuitStore) Tx(msg string, fn func(tx beads.Tx) error) error {
	return s.do(func() error { return s.in.Tx(msg, fn) })
}

func (s *circuitStore) DepAdd(issue, dependsOn, depType string) error {
	return s.do(func() error { return s.in.DepAdd(issue, dependsOn, depType) })
}

func (s *circuitStore) DepRemove(issue, dependsOn string) error {
	return s.do(func() error { return s.in.DepRemove(issue, dependsOn) })
}

func (s *circuitStore) Create(b beads.Bead) (beads.Bead, error) {
	return circuitCall(s, func() (beads.Bead, error) { return s.in.Create(b) })
}

func (s *circuitStore) Get(id string) (beads.Bead, error) {
	return circuitCall(s, func() (beads.Bead, error) { return s.in.Get(id) })
}

func (s *circuitStore) CloseAll(ids []string, md map[string]string) (int, error) {
	return circuitCall(s, func() (int, error) { return s.in.CloseAll(ids, md) })
}

func (s *circuitStore) GetLocalString(id, key string) (string, error) {
	return circuitCall(s, func() (string, error) { return s.in.GetLocalString(id, key) })
}

func (s *circuitStore) DepList(id, dir string) ([]beads.Dep, error) {
	return circuitCall(s, func() ([]beads.Dep, error) { return s.in.DepList(id, dir) })
}

func (s *circuitStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	return circuitCall(s, func() ([]beads.Bead, error) { return s.in.List(q) })
}

func (s *circuitStore) ListOpen(status ...string) ([]beads.Bead, error) {
	return circuitCall(s, func() ([]beads.Bead, error) { return s.in.ListOpen(status...) })
}

func (s *circuitStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	return circuitCall(s, func() ([]beads.Bead, error) { return s.in.Ready(q...) })
}

func (s *circuitStore) Children(parent string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return circuitCall(s, func() ([]beads.Bead, error) { return s.in.Children(parent, opts...) })
}

func (s *circuitStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return circuitCall(s, func() ([]beads.Bead, error) { return s.in.ListByLabel(label, limit, opts...) })
}

func (s *circuitStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	return circuitCall(s, func() ([]beads.Bead, error) { return s.in.ListByAssignee(assignee, status, limit) })
}

func (s *circuitStore) ListByMetadata(f map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return circuitCall(s, func() ([]beads.Bead, error) { return s.in.ListByMetadata(f, limit, opts...) })
}
