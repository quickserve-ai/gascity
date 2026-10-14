package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/usage"
)

func TestAggregateRunCosts(t *testing.T) {
	facts := []usage.Fact{
		{RunID: "run-a", Kind: usage.KindModel, InputTokens: 100, OutputTokens: 50, CacheReadTokens: 5, CostUSDEstimate: 0.01},
		{RunID: "run-a", Kind: usage.KindModel, InputTokens: 10, Unpriced: true}, // excluded from cost
		{RunID: "run-a", Kind: usage.KindCompute, WallSeconds: 12.5},
		{RunID: "run-b", Kind: usage.KindCompute, WallSeconds: 3},
	}
	rows := aggregateRunCosts(facts)
	if len(rows) != 2 {
		t.Fatalf("want 2 runs, got %d", len(rows))
	}
	// Sorted by run id: run-a, run-b.
	a, b := rows[0], rows[1]
	if a.RunID != "run-a" || b.RunID != "run-b" {
		t.Fatalf("run order wrong: %q, %q", a.RunID, b.RunID)
	}
	if a.Invocations != 2 {
		t.Fatalf("run-a invocations = %d, want 2", a.Invocations)
	}
	if a.InputTokens != 110 || a.OutputTokens != 50 || a.CacheReadTokens != 5 {
		t.Fatalf("run-a tokens wrong: %+v", a)
	}
	if a.WallSeconds != 12.5 || a.ComputeFacts != 1 {
		t.Fatalf("run-a compute wrong: %+v", a)
	}
	if a.Unpriced != 1 {
		t.Fatalf("run-a unpriced = %d, want 1", a.Unpriced)
	}
	if a.CostUSDEstimate != 0.01 {
		t.Fatalf("run-a cost = %v, want 0.01 (unpriced excluded)", a.CostUSDEstimate)
	}
	if b.WallSeconds != 3 || b.ComputeFacts != 1 || b.Invocations != 0 {
		t.Fatalf("run-b wrong: %+v", b)
	}
}

func TestAggregateRunCostsEmpty(t *testing.T) {
	if rows := aggregateRunCosts(nil); len(rows) != 0 {
		t.Fatalf("nil facts must yield no rows, got %d", len(rows))
	}
}

func TestRenderRunCostsMarksWorkWithNoModelUsageNotMeasured(t *testing.T) {
	// ga-hrto3i: an omp run records compute (wall time) but no model facts,
	// because the family has no telemetry. It rendered as "0 invocations,
	// $0.0000, UNPRICED 0": work that looks free. It must read NOT-MEASURED,
	// and the trailer must say its cost is unknown, not zero.
	rows := aggregateRunCosts([]usage.Fact{
		{RunID: "run-a", Kind: usage.KindModel, InputTokens: 100, CostUSDEstimate: 0.01},
		{RunID: "run-a", Kind: usage.KindCompute, WallSeconds: 12.5},
		{RunID: "run-omp", Kind: usage.KindCompute, WallSeconds: 20391},
	})
	var out strings.Builder
	renderRunCosts(&out, rows)
	got := out.String()
	var ompLine, aLine string
	for _, line := range strings.Split(got, "\n") {
		switch {
		case strings.HasPrefix(line, "run-omp"):
			ompLine = line
		case strings.HasPrefix(line, "run-a"):
			aLine = line
		}
	}
	if !strings.Contains(ompLine, "NOT-MEASURED") || strings.Contains(ompLine, "0.0000") {
		t.Fatalf("omp row = %q, want NOT-MEASURED and no $0.0000", ompLine)
	}
	if !strings.Contains(aLine, "0.0100") {
		t.Fatalf("priced row = %q, want its estimate", aLine)
	}
	for _, want := range []string{"NOT MEASURED: 1 run(s)", "20391 wall-seconds", "unknown, not zero"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderRunCostsQuietWhenEverythingIsMeasured(t *testing.T) {
	rows := aggregateRunCosts([]usage.Fact{
		{RunID: "run-a", Kind: usage.KindModel, InputTokens: 1, CostUSDEstimate: 0.5},
		{RunID: "run-a", Kind: usage.KindCompute, WallSeconds: 1},
	})
	var out strings.Builder
	renderRunCosts(&out, rows)
	if strings.Contains(out.String(), "NOT") {
		t.Fatalf("unexpected not-measured output:\n%s", out.String())
	}
}
