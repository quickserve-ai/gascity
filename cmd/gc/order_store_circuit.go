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

// errStoreCallAbandoned is circuitRun's answer when it stopped waiting on fn.
var errStoreCallAbandoned = errors.New("store call abandoned")

const (
	// storeReprobeBound bounds the one call a tick spends on a store still hung
	// from an earlier tick. Its probe is the store's cheapest read (bd list
	// --limit 0), which a reachable server answers in well under a second, so
	// this notices recovery the next tick at a few seconds' cost, not a budget.
	storeReprobeBound = 5 * time.Second
	// maxOutstandingStoreCalls caps the abandoned calls a store key may hold
	// before it is not even re-probed. A carried-dark key adds at most one (its
	// probe) per tick, so this bounds that growth; calls drain as they return.
	maxOutstandingStoreCalls = 4
)

// orderStoreCalls is the dispatcher's memory across ticks (ga-rs4ddy): per store
// key, the calls a circuit stopped waiting for that have not returned yet. A slow
// but live store answers inside the budget (vp-gprv), so it never appears here.
type orderStoreCalls struct {
	mu        sync.Mutex
	abandoned map[string]int
}

func (o *orderStoreCalls) outstanding(key string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.abandoned[key]
}

// orderStoreCircuit is one tick's record of dark stores: beads.Store takes no
// context, so this bounds what a dead remote store can cost the serial pass.
type orderStoreCircuit struct {
	budget time.Duration
	stderr io.Writer
	calls  *orderStoreCalls
	mu     sync.Mutex
	dark   map[string]error
	wake   map[string]chan struct{} // closed on trip: calls already waiting return
}

func newOrderStoreCircuit(budget time.Duration, stderr io.Writer) *orderStoreCircuit {
	if budget <= 0 {
		// Past the store's own read bound, so a slow but live store, which answers
		// inside it (vp-gprv dispatches through that), never trips; only a hang does.
		budget = beads.ReadCommandTimeout() + 5*time.Second
	}
	return &orderStoreCircuit{
		budget: budget, stderr: stderr, calls: &orderStoreCalls{},
		dark: make(map[string]error), wake: make(map[string]chan struct{}),
	}
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
		close(c.wakeLocked(key))
		logDispatchError(c.stderr, "gc: order dispatch: store %q is dark for the rest of this tick; skipping its orders: %v", key, reason)
	}
}

func (c *orderStoreCircuit) wakeLocked(key string) chan struct{} {
	if c.wake[key] == nil {
		c.wake[key] = make(chan struct{})
	}
	return c.wake[key]
}

func (c *orderStoreCircuit) tripped(key string) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wakeLocked(key)
}

// circuitRun runs fn on its own goroutine until it returns, bound passes, or key
// trips. Abandoned, it returns errStoreCallAbandoned; the call stays counted
// against key until fn returns, and late, if set, then gets the result.
func circuitRun[T any](c *orderStoreCircuit, key string, bound time.Duration, fn func() (T, error), late func(T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	o, gaveUp := c.calls, false
	done := make(chan result, 1)
	go func() {
		v, err := fn()
		o.mu.Lock()
		if !gaveUp {
			done <- result{v, err}
			o.mu.Unlock()
			return
		}
		if o.abandoned[key]--; o.abandoned[key] <= 0 {
			delete(o.abandoned, key)
		}
		o.mu.Unlock()
		if late != nil {
			late(v, err)
		}
	}()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.v, r.err
	case <-timer.C:
	case <-c.tripped(key):
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	select {
	case r := <-done: // fn returned while we stopped waiting
		return r.v, r.err
	default:
	}
	gaveUp = true
	if o.abandoned == nil {
		o.abandoned = make(map[string]int)
	}
	o.abandoned[key]++
	var zero T
	return zero, errStoreCallAbandoned
}

// open runs the open under the budget and memoizes a failure as dark. A key
// still hung from an earlier tick starts dark but for one short re-probe.
func (c *orderStoreCircuit) open(key string, openFn func() (beads.Store, error)) (beads.Store, error) {
	if err := c.darkErr(key); err != nil {
		return nil, err
	}
	bound := c.budget
	if n := c.calls.outstanding(key); n >= maxOutstandingStoreCalls {
		c.trip(key, fmt.Errorf("%d calls from earlier ticks still outstanding", n))
		return nil, c.darkErr(key)
	} else if n > 0 {
		bound = min(storeReprobeBound, c.budget)
		rawOpen := openFn
		openFn = func() (beads.Store, error) {
			store, err := rawOpen()
			if err == nil {
				if err = store.Ping(); err != nil {
					_ = closeBeadStoreHandle(store)
					return nil, fmt.Errorf("re-probe of a store with %d calls outstanding: %w", n, err)
				}
			}
			return store, err
		}
	}
	store, err := circuitRun(c, key, bound, openFn, func(late beads.Store, err error) {
		if err == nil {
			_ = closeBeadStoreHandle(late) // arrived after its open was abandoned
		}
	})
	if errors.Is(err, errStoreCallAbandoned) {
		c.trip(key, fmt.Errorf("store open timed out after %s", bound))
		return nil, c.darkErr(key)
	}
	if err != nil {
		c.trip(key, err)
		return nil, err
	}
	return &circuitStore{in: store, key: key, c: c}, nil
}

// circuitCall runs fn under the budget; an overrun, a trip by another call, or
// an unreachable-class error leaves the store dark. A late result is dropped.
func circuitCall[T any](s *circuitStore, fn func() (T, error)) (T, error) {
	var zero T
	if err := s.c.darkErr(s.key); err != nil {
		return zero, err
	}
	v, err := circuitRun(s.c, s.key, s.c.budget, fn, nil)
	switch {
	case errors.Is(err, errStoreCallAbandoned):
		// Over budget, or woken by another call's trip (e.g. one the 8s gate
		// abandoned). "timed out after" = beads.IsTimeoutError, the gate's class.
		s.c.trip(s.key, fmt.Errorf("store call timed out after %s", s.c.budget))
		return zero, s.c.darkErr(s.key)
	case err != nil && isBeadStoreUnreachable(err) && !beads.IsTimeoutError(err):
		// A RETURNED query timeout is gate contention, which idempotent orders
		// dispatch through (vp-gprv); only the budget trips on slowness.
		s.c.trip(s.key, err)
	}
	return v, err
}

// circuitStore implements beads.Store in full and deliberately does not embed
// it, so no method can reach the store without the circuit.
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

// Handles keeps beads.HandlesFor routing identical to the raw store, reads budgeted.
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

// circuitWrite runs a write unbudgeted (an abandoned write can still commit, e.g. a
// tracking bead with no run); a dark store refuses it, and tick reads trip first.
func circuitWrite[T any](s *circuitStore, fn func() (T, error)) (T, error) {
	var zero T
	if err := s.c.darkErr(s.key); err != nil {
		return zero, err
	}
	v, err := fn()
	if err != nil && isBeadStoreUnreachable(err) && !beads.IsTimeoutError(err) {
		s.c.trip(s.key, err)
	}
	return v, err
}

func (s *circuitStore) do(fn func() error) error {
	_, err := circuitWrite(s, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}
func (s *circuitStore) Close(id string) error  { return s.do(func() error { return s.in.Close(id) }) }
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
	return circuitWrite(s, func() (beads.Bead, error) { return s.in.Create(b) })
}
func (s *circuitStore) Get(id string) (beads.Bead, error) {
	return circuitCall(s, func() (beads.Bead, error) { return s.in.Get(id) })
}
func (s *circuitStore) CloseAll(ids []string, md map[string]string) (int, error) {
	return circuitWrite(s, func() (int, error) { return s.in.CloseAll(ids, md) })
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
