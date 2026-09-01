package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// ga-singc6. The liveness scan is a host-wide process-table enumeration (lsof
// on darwin) that runs INLINE in the controller's reconciler tick — the city's
// clock. It used to run unconditionally on every pass, before the reaper knew
// whether a single reap candidate existed, and on the live fleet that meant
// ~99.9% of ticks paid for a scan whose result nothing consulted. These tests
// pin the fix: the scan is gathered lazily, at most once per pass, and only
// when a candidate actually reaches the liveness gate — while the fail-closed
// contract on an indeterminate scan is unchanged.

// injectCountingLiveness overrides the reaper's process-table scan with a stub
// that returns state and counts how many times the reaper asked for it.
func injectCountingLiveness(t *testing.T, state liveWorktreeState) *int {
	t.Helper()
	calls := 0
	prev := collectLiveWorktreeStateFn
	collectLiveWorktreeStateFn = func() liveWorktreeState {
		calls++
		return state
	}
	t.Cleanup(func() { collectLiveWorktreeStateFn = prev })
	return &calls
}

// With no per-bead worktrees at all there is nothing to protect, so the
// process table must not be enumerated.
func TestReapClosedBeadWorktrees_NoWorktreesSkipsLivenessScan(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	store := beads.NewMemStoreFrom(1, nil, nil)
	cfg := reapTestConfig(rigRoot)
	calls := injectCountingLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, &stderr)

	if *calls != 0 {
		t.Fatalf("liveness scan ran %d time(s) on a pass with zero worktrees; the host-wide scan must not run when there is nothing to gate", *calls)
	}
	if len(report.Reaped) != 0 || len(report.Protected) != 0 {
		t.Fatalf("report = %+v, want empty", report)
	}
}

// The common fleet state: worktrees exist, but none is a reap candidate —
// its bead is still open, or it is inside the freshness quarantine. Both are
// decided BEFORE the liveness gate, so the scan must not run.
func TestReapClosedBeadWorktrees_NoCandidatesSkipsLivenessScan(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	openWT := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-open001")            // old enough, but bead is open
	freshWT := addClosedWorktreeWithAge(t, rigRoot, cityPath, "builder", "ga-fresh02", 0) // closed, but quarantined
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "ga-open001", Status: "open"},
		{ID: "ga-fresh02", Status: "closed"},
	}, nil)
	cfg := reapTestConfig(rigRoot) // default quarantine window applies
	calls := injectCountingLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, &stderr)

	if *calls != 0 {
		t.Fatalf("liveness scan ran %d time(s) with zero reap candidates (one open bead, one quarantined worktree)\nstderr:\n%s", *calls, stderr.String())
	}
	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0", report.Reaped)
	}
	// The quarantine verdict is still recorded; the open bead is a silent skip.
	if len(report.Protected) != 1 || report.Protected[0].BeadID != "ga-fresh02" {
		t.Fatalf("Protected = %+v, want exactly the quarantined ga-fresh02", report.Protected)
	}
	for _, wt := range []string{openWT, freshWT} {
		if _, err := os.Stat(wt); err != nil {
			t.Fatalf("worktree %s was removed or unstattable: %v", wt, err)
		}
	}
}

// When candidates DO exist the scan runs — exactly once for the whole pass,
// however many candidates share it — and its verdict still governs the reap.
func TestReapClosedBeadWorktrees_LivenessGatheredOncePerPass(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt1 := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-once001")
	wt2 := addClosedWorktree(t, rigRoot, cityPath, "builder-2", "ga-once002")
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "ga-once001", Status: "closed"},
		{ID: "ga-once002", Status: "closed"},
	}, nil)
	cfg := reapTestConfig(rigRoot)
	calls := injectCountingLiveness(t, liveWorktreeState{scanned: true}) // scanned, nothing live

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, &stderr)

	if *calls != 1 {
		t.Fatalf("liveness scan ran %d time(s) for two candidates, want exactly 1 (once per pass, not per candidate)", *calls)
	}
	if len(report.Reaped) != 2 {
		t.Fatalf("Reaped = %+v, want both candidates\nstderr:\n%s", report.Reaped, stderr.String())
	}
	for _, wt := range []string{wt1, wt2} {
		if _, err := os.Stat(wt); !os.IsNotExist(err) {
			t.Fatalf("worktree %s still present after reap (stat err=%v)", wt, err)
		}
	}
}

// The fail-closed contract survives the reordering: an indeterminate scan
// protects EVERY candidate, and the reaper does not retry the scan per
// candidate hoping for a different answer.
func TestReapClosedBeadWorktrees_LazyScanStillFailsClosedForAllCandidates(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt1 := addClosedWorktree(t, rigRoot, cityPath, "builder", "ga-fc00001")
	wt2 := addClosedWorktree(t, rigRoot, cityPath, "builder-2", "ga-fc00002")
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "ga-fc00001", Status: "closed"},
		{ID: "ga-fc00002", Status: "closed"},
	}, nil)
	cfg := reapTestConfig(rigRoot)
	calls := injectCountingLiveness(t, liveWorktreeState{scanned: false})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, &stderr)

	if *calls != 1 {
		t.Fatalf("liveness scan ran %d time(s), want exactly 1 even when indeterminate", *calls)
	}
	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 when liveness is indeterminate", report.Reaped)
	}
	if len(report.Protected) != 2 {
		t.Fatalf("Protected = %+v, want both candidates protected", report.Protected)
	}
	for _, p := range report.Protected {
		if !strings.Contains(p.Reason, "liveness scan unavailable") {
			t.Errorf("Protected reason for %s = %q, want the fail-closed liveness reason", p.BeadID, p.Reason)
		}
	}
	for _, wt := range []string{wt1, wt2} {
		if _, err := os.Stat(wt); err != nil {
			t.Fatalf("worktree %s was removed under an indeterminate liveness scan: %v", wt, err)
		}
	}
}
