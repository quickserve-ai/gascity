package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// newRigWorktreesCheckWithMeasure builds the check with an injected
// measurement so tests never shell out to du.
func newRigWorktreesCheckWithMeasure(t *testing.T, rigPath string, cfg config.DoctorConfig, measure dirMeasure) *RigWorktreesCheck {
	t.Helper()
	c := NewRigWorktreesCheck(config.Rig{Name: "testrig", Path: rigPath}, cfg)
	c.measureDir = measure
	return c
}

// fixedSize returns a measurement stub reporting n fully counted bytes
// for an existing directory.
func fixedSize(n int64) dirMeasure {
	return func(context.Context, string) (dirSize, error) { return dirSize{bytes: n, exists: true}, nil }
}

// makeWorktreeDirs creates <rigPath>/worktrees/<name> for each name and
// returns the rig path.
func makeWorktreeDirs(t *testing.T, names ...string) string {
	t.Helper()
	rigPath := t.TempDir()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(rigPath, "worktrees", n), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", n, err)
		}
	}
	return rigPath
}

func TestRigWorktreesCheck_Name(t *testing.T) {
	c := NewRigWorktreesCheck(config.Rig{Name: "eunice", Path: "/tmp/x"}, config.DoctorConfig{})
	if got, want := c.Name(), "rig:eunice:worktrees"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

// The regression this check exists for: a rig holding a large per-bead
// worktree population must be reported, not reported as absent. Before
// this check, every worktree check was scoped to $CITY/.gc/worktrees and
// the board came back green over tens of gigabytes.
func TestRigWorktreesCheck_PopulatedOverErrorThreshold_ReportsCountAndSize(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa", "tlp-bbb", "tlp-ccc")
	cfg := config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"}
	c := newRigWorktreesCheckWithMeasure(t, rigPath, cfg, fixedSize(60*1024*1024*1024))

	r := c.Run(&CheckContext{})

	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "3 per-bead worktree(s)") {
		t.Errorf("message = %q, want the worktree count", r.Message)
	}
	if !strings.Contains(r.Message, "60.0 GB") {
		t.Errorf("message = %q, want the aggregate size", r.Message)
	}
	if strings.Contains(r.Message, "no worktrees") {
		t.Errorf("message = %q, must not claim absence over a populated directory", r.Message)
	}
	if r.FixHint == "" {
		t.Error("FixHint empty; want a pointer at manual remediation")
	}
	// The hint must name a command that exists. An earlier draft pointed
	// at `gc order run worktree-prune`, which is not a registered order —
	// the operator would hit an error at the worst moment.
	if strings.Contains(r.FixHint, "gc order run") {
		t.Errorf("FixHint = %q, must not point at a nonexistent order", r.FixHint)
	}
}

func TestRigWorktreesCheck_OverWarnUnderError_Warns(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa")
	cfg := config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"}
	c := newRigWorktreesCheckWithMeasure(t, rigPath, cfg, fixedSize(20*1024*1024*1024))

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "warn threshold") {
		t.Errorf("message = %q, want the warn threshold named", r.Message)
	}
}

func TestRigWorktreesCheck_UnderThresholds_OKButStillReportsCount(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa", "tlp-bbb")
	cfg := config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"}
	c := newRigWorktreesCheckWithMeasure(t, rigPath, cfg, fixedSize(1024*1024*1024))

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	// An OK status must still be honest about what is there — the
	// original bug was a green line that implied nothing existed.
	if !strings.Contains(r.Message, "2 per-bead worktree(s)") {
		t.Errorf("message = %q, want the count even when under threshold", r.Message)
	}
	if r.FixHint != "" {
		t.Errorf("FixHint = %q, want empty for OK result", r.FixHint)
	}
}

func TestRigWorktreesCheck_NoWorktreesDir_OKAndNamesThePath(t *testing.T) {
	rigPath := t.TempDir()
	c := newRigWorktreesCheckWithMeasure(t, rigPath, config.DoctorConfig{}, fixedSize(0))

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	// Naming the path is the point: "no worktrees directory" unqualified
	// is what made the old message a false claim.
	if !strings.Contains(r.Message, filepath.Join(rigPath, "worktrees")) {
		t.Errorf("message = %q, want the absent path named", r.Message)
	}
}

func TestRigWorktreesCheck_EmptyWorktreesDir_OK(t *testing.T) {
	rigPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigPath, "worktrees"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	c := newRigWorktreesCheckWithMeasure(t, rigPath, config.DoctorConfig{}, fixedSize(0))

	r := c.Run(&CheckContext{})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no per-bead worktrees") {
		t.Errorf("message = %q, want empty-directory wording", r.Message)
	}
}

func TestRigWorktreesCheck_FilesAreNotCountedAsWorktrees(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa")
	if err := os.WriteFile(filepath.Join(rigPath, "worktrees", "README"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := newRigWorktreesCheckWithMeasure(t, rigPath, config.DoctorConfig{}, fixedSize(1))

	r := c.Run(&CheckContext{})

	if !strings.Contains(r.Message, "1 per-bead worktree(s)") {
		t.Errorf("message = %q, want only the directory counted", r.Message)
	}
}

// "We can't tell" must not read as "we're fine" — the same policy
// WorktreeDiskSizeCheck applies to measurement failure.
func TestRigWorktreesCheck_MeasureFails_WarnsAndKeepsTheCount(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa", "tlp-bbb")
	c := newRigWorktreesCheckWithMeasure(t, rigPath, config.DoctorConfig{}, func(context.Context, string) (dirSize, error) {
		return dirSize{}, errors.New("du exploded")
	})

	r := c.Run(&CheckContext{})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "2 per-bead worktree(s)") {
		t.Errorf("message = %q, want the count preserved when size is unknown", r.Message)
	}
	if len(r.Details) == 0 || !strings.Contains(strings.Join(r.Details, " "), "du exploded") {
		t.Errorf("Details = %v, want the underlying measurement error", r.Details)
	}
}

// Nothing reclaims this population automatically — the bead-worktree
// reaper is scoped to .gc/worktrees/<rig>/ — so these trees are removed
// by hand. doctor --fix must not grow its own opinion about when a
// worktree is disposable.
func TestRigWorktreesCheck_IsObservationOnly(t *testing.T) {
	c := NewRigWorktreesCheck(config.Rig{Name: "r", Path: t.TempDir()}, config.DoctorConfig{})
	if c.CanFix() {
		t.Error("CanFix() = true, want false — this check only observes")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Errorf("Fix() = %v, want nil no-op", err)
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
}

// The city-scoped check's message was a positive false claim: it said
// "no worktrees directory" on a box whose rig worktrees/ held 28 GB.
func TestWorktreeCheck_AbsentCityDirMessageNamesGcWorktrees(t *testing.T) {
	c := &WorktreeCheck{}

	r := c.Run(&CheckContext{CityPath: t.TempDir()})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, ".gc/worktrees") {
		t.Errorf("message = %q, want the .gc/worktrees scope named", r.Message)
	}
}

func TestWorktreeCheck_EmptyCityDirMessageNamesGcWorktrees(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "worktrees"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	c := &WorktreeCheck{}

	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, ".gc/worktrees") {
		t.Errorf("message = %q, want the .gc/worktrees scope named", r.Message)
	}
}

// The rig-root population shares the measurer, so it shares the budget: a
// per-bead worktree tree too large to size in time is reported as a labeled
// lower bound, inside the check's budget, with the ran-out-of-time hint.
func TestRigWorktreesCheck_UnfinishedWalkReportsLowerBoundInsideBudget(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa", "tlp-bbb")
	cfg := config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"}
	c := newRigWorktreesCheckWithMeasure(t, rigPath, cfg, func(ctx context.Context, _ string) (dirSize, error) {
		<-ctx.Done()
		return dirSize{bytes: 2 * 1024 * 1024 * 1024, exists: true, lowerBound: true}, nil
	})
	c.budget = 100 * time.Millisecond

	start := time.Now()
	r := c.Run(&CheckContext{})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("check took %s, want it to stop soon after its budget", elapsed)
	}
	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want StatusWarning", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "2 per-bead worktree(s)") {
		t.Errorf("message = %q, want the count", r.Message)
	}
	if !strings.Contains(r.Message, "at least 2.0 GB logical (lower bound: the size walk did not finish within 100ms)") {
		t.Errorf("message = %q, want the labeled lower bound", r.Message)
	}
	if !strings.Contains(r.FixHint, "ran out of time") {
		t.Errorf("FixHint = %q, want the ran-out-of-time hint", r.FixHint)
	}
	assertDuSizesLabeledLogical(t, r.Message)
}

func TestRigWorktreesCheck_LowerBoundPastErrorThresholdIsAnError(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa")
	cfg := config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"}
	c := newRigWorktreesCheckWithMeasure(t, rigPath, cfg, func(ctx context.Context, _ string) (dirSize, error) {
		<-ctx.Done()
		return dirSize{bytes: 60 * 1024 * 1024 * 1024, exists: true, lowerBound: true}, nil
	})
	c.budget = 50 * time.Millisecond

	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d (%s), want StatusError", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "at least 60.0 GB logical") || !strings.Contains(r.Message, "error threshold") {
		t.Errorf("message = %q, want the lower bound flagged against the error threshold", r.Message)
	}
	assertDuSizesLabeledLogical(t, r.Message)
}

// pl-59k part (d): the size is du's LOGICAL count, and says so.
func TestRigWorktreesCheck_SizeIsLabeledLogical(t *testing.T) {
	rigPath := makeWorktreeDirs(t, "tlp-aaa")
	cfg := config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"}
	for _, n := range []int64{1 << 30, 20 << 30, 60 << 30} {
		c := newRigWorktreesCheckWithMeasure(t, rigPath, cfg, fixedSize(n))
		r := c.Run(&CheckContext{})
		if !strings.Contains(r.Message, " logical") {
			t.Errorf("message = %q, want a labeled size", r.Message)
		}
		assertDuSizesLabeledLogical(t, r.Message)
	}
}
