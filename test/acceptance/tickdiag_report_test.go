//go:build acceptance_a

package acceptance_test

// DIAGNOSTIC BRANCH ONLY, NEVER MERGED (ga-tyf38i, the mayor's B step 1).
// Identical on the #346 head side and the base side. It prints, through the
// test binary's stdout, the controller's per-tick timing lines and a summary
// of every bd invocation gc traced (GC_BD_TRACE_JSON), so a run shows how
// often the controller reached its stores and how long that took.

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// diagCPUProfileEnv and diagCPUProfileSecsEnv match cmd/gc's diagnostic
// profile switches (order_dispatch_tickdiag.go).
const (
	diagCPUProfileEnv     = "GC_DIAG_CPUPROFILE"
	diagCPUProfileSecsEnv = "GC_DIAG_CPUPROFILE_SECS"
)

// diagMachineBench is the machine-speed reading the mayor ruled (B step 1,
// widened): a fixed CPU-only workload with no gc, bd or Dolt, timed. 64 SHA-256
// passes over 8 MiB, single goroutine, best of 3.
func diagMachineBench(label string) {
	buf := make([]byte, 8<<20)
	for i := range buf {
		buf[i] = byte(i)
	}
	best := time.Duration(0)
	for round := 0; round < 3; round++ {
		start := time.Now()
		var sum [32]byte
		for i := 0; i < 64; i++ {
			sum = sha256.Sum256(append(buf[:len(buf)-32], sum[:]...))
		}
		if d := time.Since(start); best == 0 || d < best {
			best = d
		}
	}
	diagnosticLine("%s: machine bench: sha256 64x8MiB best of 3 = %d ms", label, best.Milliseconds())
}

// diagGCStartup times 20 runs of one trivial gc command (gc version) with the
// test's environment: the start-up cost of the binary under test.
func diagGCStartup(label string, env *helpers.Env) {
	gcPath, err := helpers.ResolveGCPath(env)
	if err != nil {
		diagnosticLine("%s: gc startup: %v", label, err)
		return
	}
	var times []time.Duration
	for i := 0; i < 20; i++ {
		start := time.Now()
		cmd := exec.Command(gcPath, "version")
		cmd.Env = env.List()
		_ = cmd.Run()
		times = append(times, time.Since(start))
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	total := time.Duration(0)
	for _, d := range times {
		total += d
	}
	diagnosticLine("%s: gc startup: 20x gc version: median %d ms, min %d ms, max %d ms, total %d ms",
		label, times[10].Milliseconds(), times[0].Milliseconds(), times[19].Milliseconds(), total.Milliseconds())
}

// diagPrintProfile waits for the controller's CPU profile to be complete (its
// window closed and the file stopped growing), then prints it base64-encoded
// between markers, in 3000-character lines, so it can be decoded from the CI
// log and read with go tool pprof.
func diagPrintProfile(label, path string, wait time.Duration) {
	deadline := time.Now().Add(wait)
	var last int64 = -1
	for time.Now().Before(deadline) {
		st, err := os.Stat(path)
		if err == nil && st.Size() > 0 && st.Size() == last {
			break
		}
		if err == nil {
			last = st.Size()
		}
		time.Sleep(5 * time.Second)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		diagnosticLine("%s: cpuprofile: %v", label, err)
		return
	}
	enc := base64.StdEncoding.EncodeToString(data)
	diagnosticLine("%s: cpuprofile-begin bytes=%d", label, len(data))
	for i := 0; i < len(enc); i += 3000 {
		j := i + 3000
		if j > len(enc) {
			j = len(enc)
		}
		diagnosticLine("%s: cpuprofile-chunk %s", label, enc[i:j])
	}
	diagnosticLine("%s: cpuprofile-end", label)
}

// diagBDTraceEnv is internal/beads.TraceBDCall's JSONL sink variable.
const diagBDTraceEnv = "GC_BD_TRACE_JSON"

// diagReportTicks prints every controller "DIAG ga-tyf38i tick" line from the
// supervisor log under gcHome, then the bd trace summary.
func diagReportTicks(label, gcHome, tracePath string) {
	printed := 0
	if gcHome != "" {
		if f, err := os.Open(filepath.Join(gcHome, "supervisor.log")); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
			for sc.Scan() {
				line := sc.Text()
				if strings.Contains(line, "DIAG ga-tyf38i tick ") {
					diagnosticLine("%s: tick: %s", label, line)
					printed++
				}
			}
			_ = f.Close()
		} else {
			diagnosticLine("%s: tick: supervisor.log: %v", label, err)
		}
	}
	diagnosticLine("%s: tick lines: %d", label, printed)
	diagReportBDTrace(label, tracePath)
}

// diagReportBDTrace summarizes the bd trace: calls and total milliseconds per
// (source, first bd argument).
func diagReportBDTrace(label, tracePath string) {
	f, err := os.Open(tracePath)
	if err != nil {
		diagnosticLine("%s: bd trace: %v", label, err)
		return
	}
	defer f.Close() //nolint:errcheck // read-only
	type agg struct {
		n  int
		ms int64
	}
	byKey := map[string]*agg{}
	total, totalMS := 0, int64(0)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for sc.Scan() {
		var rec struct {
			Source string   `json:"source"`
			Dir    string   `json:"dir"`
			Scope  string   `json:"scope"`
			Args   []string `json:"args"`
			DurMs  int64    `json:"dur_ms"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		verb := ""
		for _, a := range rec.Args {
			if !strings.HasPrefix(a, "-") {
				verb = a
				break
			}
		}
		where := rec.Scope
		if where == "" {
			where = "dir:" + filepath.Base(rec.Dir)
		}
		k := rec.Source + " " + verb + " scope=" + where
		a := byKey[k]
		if a == nil {
			a = &agg{}
			byKey[k] = a
		}
		a.n++
		a.ms += rec.DurMs
		total++
		totalMS += rec.DurMs
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return byKey[keys[i]].n > byKey[keys[j]].n })
	diagnosticLine("%s: bd trace: %d call(s), %d ms total", label, total, totalMS)
	for i, k := range keys {
		if i == 25 {
			diagnosticLine("%s: bd trace: ... %d more group(s)", label, len(keys)-i)
			break
		}
		diagnosticLine("%s: bd trace: %5d calls %7d ms  %s", label, byKey[k].n, byKey[k].ms, k)
	}
}
