//go:build acceptance_a

package acceptance_test

// DIAGNOSTIC BRANCH ONLY, NEVER MERGED (ga-tyf38i, the mayor's B step 1).
// Identical on the #346 head side and the base side. It prints, through the
// test binary's stdout, the controller's per-tick timing lines and a summary
// of every bd invocation gc traced (GC_BD_TRACE_JSON), so a run shows how
// often the controller reached its stores and how long that took.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
