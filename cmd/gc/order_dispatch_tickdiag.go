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
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type tickDiag struct {
	mu     sync.Mutex
	start  time.Time
	phases []string
	stores map[string]*tickStoreDiag
	self0  syscall.Rusage
	kids0  syscall.Rusage
	ruOK   bool
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
	diagStartProfile()
	d := &tickDiag{start: time.Now(), stores: make(map[string]*tickStoreDiag)}
	d.ruOK = syscall.Getrusage(syscall.RUSAGE_SELF, &d.self0) == nil &&
		syscall.Getrusage(syscall.RUSAGE_CHILDREN, &d.kids0) == nil
	return d
}

// diagCPUProfileEnv names the file the controller writes one CPU profile to,
// covering diagCPUProfileWindow from its first dispatch tick.
const diagCPUProfileEnv = "GC_DIAG_CPUPROFILE"

// diagCPUProfileSecsEnv sets the profile window in seconds (default 120).
const diagCPUProfileSecsEnv = "GC_DIAG_CPUPROFILE_SECS"

func diagCPUProfileWindow() time.Duration {
	if v, err := strconv.Atoi(os.Getenv(diagCPUProfileSecsEnv)); err == nil && v > 0 {
		return time.Duration(v) * time.Second
	}
	return 120 * time.Second
}

var diagProfileOnce sync.Once

func diagStartProfile() {
	diagProfileOnce.Do(func() {
		path := os.Getenv(diagCPUProfileEnv)
		if path == "" {
			return
		}
		f, err := os.Create(path)
		if err != nil {
			return
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			_ = f.Close()
			return
		}
		time.AfterFunc(diagCPUProfileWindow(), func() {
			pprof.StopCPUProfile()
			_ = f.Close()
		})
	})
}

func diagTV(tv syscall.Timeval) time.Duration {
	return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
}

func diagRusageMS(ru syscall.Rusage) (int64, int64) {
	return diagTV(ru.Utime).Milliseconds(), diagTV(ru.Stime).Milliseconds()
}

// diagLoad1 is the 1-minute load average, or -1 where /proc/loadavg is absent.
func diagLoad1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return -1
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return -1
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return -1
	}
	return v
}

// diagProcess is the per-tick process reading: this process's and its reaped
// children's CPU (user+sys) during the tick and since start, the load
// average, goroutines and GC.
func (d *tickDiag) diagProcess() string {
	out := fmt.Sprintf("load1=%.2f goroutines=%d", diagLoad1(), runtime.NumGoroutine())
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out += fmt.Sprintf(" gc_num=%d gc_pause_total_ms=%d", ms.NumGC, time.Duration(ms.PauseTotalNs).Milliseconds())
	if !d.ruOK {
		return out
	}
	var self, kids syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &self) != nil || syscall.Getrusage(syscall.RUSAGE_CHILDREN, &kids) != nil {
		return out
	}
	su, ss := diagRusageMS(self)
	su0, ss0 := diagRusageMS(d.self0)
	ku, ks := diagRusageMS(kids)
	ku0, ks0 := diagRusageMS(d.kids0)
	out += fmt.Sprintf(" self_cpu_ms=%d self_cpu_total_ms=%d kids_cpu_ms=%d kids_cpu_total_ms=%d",
		(su+ss)-(su0+ss0), su+ss, (ku+ks)-(ku0+ks0), ku+ks)
	return out
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
	proc := d.diagProcess()
	logDispatchError(stderr, "gc: order dispatch: DIAG ga-tyf38i tick total_ms=%d %s candidates=%d fired=%d %s stores=%d %s",
		total, phases, candidates, fired, proc, len(parts), strings.Join(parts, " "))
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
