package main

// DIAGNOSTIC BRANCH ONLY, NEVER MERGED (ga-tyf38i, the mayor's B step 1,
// 2026-11-10T10:13:26Z). Identical on the #346 head side and the base side, so
// the per-tick line reads the same way on both: the tick's phases, and per
// store key the store's type chain, how many listings it served and how long
// they took, and the wisp-root checks and confirmation reads the gate made.
// The test counts bd backend calls per store directory from GC_BD_TRACE_JSON.

import (
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type tickDiag struct {
	mu     sync.Mutex
	start  time.Time
	phases []string
	stores map[string]*tickStoreDiag
}

type tickStoreDiag struct {
	typ        string
	listings   int
	listDur    time.Duration
	rows       int
	history    int
	historyDur time.Duration
	wisp       int
	wispDur    time.Duration
	confirm    int
	confirmDur time.Duration
}

func newTickDiag() *tickDiag {
	return &tickDiag{start: time.Now(), stores: make(map[string]*tickStoreDiag)}
}

func (d *tickDiag) storeLocked(key string, st beads.Store) *tickStoreDiag {
	s, ok := d.stores[key]
	if !ok {
		s = &tickStoreDiag{typ: diagStoreTypeChain(st)}
		d.stores[key] = s
	}
	return s
}

func (d *tickDiag) listed(key string, st beads.Store, dur time.Duration, rows int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.storeLocked(key, st)
	s.listings++
	s.listDur += dur
	s.rows += rows
}

func (d *tickDiag) historyListed(key string, st beads.Store, dur time.Duration) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.storeLocked(key, st)
	s.history++
	s.historyDur += dur
}

func (d *tickDiag) wispChecked(key string, st beads.Store, dur time.Duration) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.storeLocked(key, st)
	s.wisp++
	s.wispDur += dur
}

func (d *tickDiag) confirmed(key string, st beads.Store, dur time.Duration) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.storeLocked(key, st)
	s.confirm++
	s.confirmDur += dur
}

func (d *tickDiag) phase(name string, since time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.phases = append(d.phases, fmt.Sprintf("%s=%dms", name, time.Since(since).Milliseconds()))
	d.mu.Unlock()
}

// log writes the tick's one line. Durations are milliseconds. Backend bd calls
// are counted outside gc, from GC_BD_TRACE_JSON, by the acceptance test.
func (d *tickDiag) log(stderr io.Writer, candidates, fired int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	keys := make([]string, 0, len(d.stores))
	for k := range d.stores {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		s := d.stores[k]
		p := fmt.Sprintf("{key=%s type=%s listings=%d list_ms=%d rows=%d history=%d history_ms=%d wisp=%d wisp_ms=%d confirm=%d confirm_ms=%d",
			diagShortKey(k), s.typ, s.listings, s.listDur.Milliseconds(), s.rows, s.history, s.historyDur.Milliseconds(),
			s.wisp, s.wispDur.Milliseconds(), s.confirm, s.confirmDur.Milliseconds())
		parts = append(parts, p+"}")
	}
	phases := strings.Join(d.phases, " ")
	total := time.Since(d.start).Milliseconds()
	d.mu.Unlock()
	logDispatchError(stderr, "gc: order dispatch: DIAG ga-tyf38i tick total_ms=%d %s candidates=%d fired=%d stores=%d %s",
		total, phases, candidates, fired, len(parts), strings.Join(parts, " "))
}

func diagShortKey(k string) string {
	f := strings.Fields(k)
	if len(f) >= 2 {
		return f[0] + ":" + filepath.Base(f[len(f)-1])
	}
	return filepath.Base(k)
}

// diagStoreTypeChain names the store and every store it wraps, outermost
// first, by walking struct fields whose values are beads.Store implementations.
func diagStoreTypeChain(st beads.Store) string {
	var names []string
	seen := map[uintptr]bool{}
	diagWalk(reflect.ValueOf(st), 0, seen, func(v reflect.Value) bool {
		names = append(names, v.Type().String())
		return false
	})
	return strings.Join(names, ">")
}

var diagStoreIface = reflect.TypeOf((*beads.Store)(nil)).Elem()

// diagWalk visits v and, depth-first, every beads.Store held in its struct
// fields, exported or not, stopping when visit returns true. Unexported fields
// are only inspected (type and pointer), never called.
func diagWalk(v reflect.Value, depth int, seen map[uintptr]bool, visit func(reflect.Value) bool) bool {
	if depth > 8 || !v.IsValid() {
		return false
	}
	for v.Kind() == reflect.Interface {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	if v.Kind() == reflect.Ptr {
		if v.IsNil() || seen[v.Pointer()] {
			return false
		}
		seen[v.Pointer()] = true
	}
	if v.Type().Implements(diagStoreIface) && visit(v) {
		return true
	}
	elem := v
	if elem.Kind() == reflect.Ptr {
		elem = elem.Elem()
	}
	if elem.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < elem.NumField(); i++ {
		f := elem.Field(i)
		switch f.Kind() {
		case reflect.Interface:
			if f.IsNil() || !f.Elem().Type().Implements(diagStoreIface) {
				continue
			}
		case reflect.Ptr:
			if !f.Type().Implements(diagStoreIface) {
				continue
			}
		default:
			continue
		}
		if diagWalk(f, depth+1, seen, visit) {
			return true
		}
	}
	return false
}
