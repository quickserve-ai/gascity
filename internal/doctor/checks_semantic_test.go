package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// --- DurationRangeCheck ---

func TestDurationRangeCheck_AllReasonable(t *testing.T) {
	cfg := &config.City{
		Session: config.SessionConfig{
			SetupTimeout:       "10s",
			NudgeReadyTimeout:  "10s",
			NudgeRetryInterval: "500ms",
			NudgeLockTimeout:   "30s",
			StartupTimeout:     "60s",
		},
		Daemon: config.DaemonConfig{
			PatrolInterval:    "30s",
			RestartWindow:     "1h",
			ShutdownTimeout:   "5s",
			DriftDrainTimeout: "2m",
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
}

func TestDurationRangeCheck_TooSmall(t *testing.T) {
	cfg := &config.City{
		Session: config.SessionConfig{
			StartupTimeout: "1ns",
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
	if len(r.Details) == 0 {
		t.Error("expected details about too-small duration")
	}
	found := false
	for _, d := range r.Details {
		if strings.Contains(d, "startup_timeout") && strings.Contains(d, "below minimum") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected startup_timeout below-minimum warning in details: %v", r.Details)
	}
}

func TestDurationRangeCheck_ProgressStallTimeoutTooSmall(t *testing.T) {
	cfg := &config.City{
		Session: config.SessionConfig{
			ProgressStallTimeout: "30s",
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
	found := false
	for _, d := range r.Details {
		if strings.Contains(d, "progress_stall_timeout") && strings.Contains(d, "below minimum") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected progress_stall_timeout below-minimum warning in details: %v", r.Details)
	}
}

func TestDurationRangeCheck_ProgressStallTimeoutZeroDisables(t *testing.T) {
	cfg := &config.City{
		Session: config.SessionConfig{
			ProgressStallTimeout: "0s",
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; details = %v", r.Status, r.Details)
	}
}

func TestDurationRangeCheck_ProgressStallTimeoutNegativeDisables(t *testing.T) {
	cfg := &config.City{
		Session: config.SessionConfig{
			ProgressStallTimeout: "-5m",
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; details = %v", r.Status, r.Details)
	}
}

func TestDurationRangeCheck_ClaimHolderStallTimeout(t *testing.T) {
	t.Run("too small warns", func(t *testing.T) {
		cfg := &config.City{Session: config.SessionConfig{ClaimHolderStallTimeout: "1m"}}
		r := NewDurationRangeCheck(cfg).Run(&CheckContext{})
		if r.Status != StatusWarning {
			t.Errorf("status = %d, want Warning", r.Status)
		}
		if !strings.Contains(strings.Join(r.Details, "\n"), "claim_holder_stall_timeout") {
			t.Errorf("details = %v, want claim_holder_stall_timeout warning", r.Details)
		}
	})

	t.Run("zero disables", func(t *testing.T) {
		cfg := &config.City{Session: config.SessionConfig{ClaimHolderStallTimeout: "0s"}}
		r := NewDurationRangeCheck(cfg).Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Errorf("status = %d, want OK; details = %v", r.Status, r.Details)
		}
	})
}

func TestDurationRangeCheck_TooLarge(t *testing.T) {
	cfg := &config.City{
		Daemon: config.DaemonConfig{
			PatrolInterval: "720h", // 30 days — exceeds 24h max
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
	found := false
	for _, d := range r.Details {
		if strings.Contains(d, "patrol_interval") && strings.Contains(d, "exceeds maximum") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected patrol_interval exceeds-maximum warning in details: %v", r.Details)
	}
}

func TestDurationRangeCheck_EmptySkipped(t *testing.T) {
	cfg := &config.City{} // All empty — nothing to check.
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestDurationRangeCheck_UnparseableSkipped(t *testing.T) {
	// Unparseable durations are handled by ValidateDurations; this check
	// should skip them rather than erroring.
	cfg := &config.City{
		Session: config.SessionConfig{
			StartupTimeout: "5mins", // invalid
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (unparseable skipped); msg = %s", r.Status, r.Message)
	}
}

func TestDurationRangeCheck_AgentIdleTimeout(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", IdleTimeout: "1ms"},
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	found := false
	for _, d := range r.Details {
		if strings.Contains(d, "idle_timeout") && strings.Contains(d, "below minimum") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected idle_timeout warning in details: %v", r.Details)
	}
}

func TestDurationRangeCheck_AgentPoolDrainTimeout(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:         "pool-agent",
				DrainTimeout: "10ns",
			},
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
}

func TestDurationRangeCheck_MultipleIssues(t *testing.T) {
	cfg := &config.City{
		Session: config.SessionConfig{
			StartupTimeout: "1ns",   // too small
			SetupTimeout:   "9999h", // too large
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
	if len(r.Details) < 2 {
		t.Errorf("expected at least 2 issues, got %d: %v", len(r.Details), r.Details)
	}
}

// --- EventLogSizeCheck ---

func TestEventLogSizeCheck_SmallFile(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "events.jsonl"), []byte("small\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewEventLogSizeCheck()
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestEventLogSizeCheck_LargeFile(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Use a small threshold for testing.
	c := &EventLogSizeCheck{MaxSize: 100}
	path := filepath.Join(gcDir, "events.jsonl")
	data := make([]byte, 200) // exceeds 100-byte threshold
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if r.FixHint == "" {
		t.Error("expected fix hint for large event log")
	}
}

func TestEventLogSizeCheck_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	c := NewEventLogSizeCheck()
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (missing file = nothing to check); msg = %s", r.Status, r.Message)
	}
}

func TestEventLogSizeCheck_ExactlyAtThreshold(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	c := &EventLogSizeCheck{MaxSize: 100}
	data := make([]byte, 100) // exactly at threshold
	if err := os.WriteFile(filepath.Join(gcDir, "events.jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (at threshold, not over); msg = %s", r.Status, r.Message)
	}
}

// --- ConfigSemanticsCheck ---

func TestConfigSemanticsCheck_Clean(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	c := NewConfigSemanticsCheck(cfg, "city.toml")
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
}

func TestConfigSemanticsCheck_BadProviderRef(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "nonexistent"},
		},
	}
	c := NewConfigSemanticsCheck(cfg, "city.toml")
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if len(r.Details) == 0 {
		t.Error("expected warning details about bad provider")
	}
}

func TestConfigSemanticsCheck_MultipleWarnings(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{
			Name:     "test",
			Provider: "bogus",
		},
		Agents: []config.Agent{
			{Name: "a1", Provider: "missing1"},
			{Name: "a2", Provider: "missing2"},
		},
	}
	c := NewConfigSemanticsCheck(cfg, "city.toml")
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if len(r.Details) < 2 {
		t.Errorf("expected multiple warnings, got %d: %v", len(r.Details), r.Details)
	}
}

// --- humanSize ---

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}
	for _, tt := range tests {
		got := humanSize(tt.bytes)
		if got != tt.want {
			t.Errorf("humanSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

// --- WorktreeDiskSizeCheck ---

// fakeMeasure returns a deterministic, fully counted byte total per
// directory path so tests don't shell out to du. Returns sizes[path] when
// present; treats missing keys as not-existent.
func fakeMeasure(sizes map[string]int64, errs map[string]error) dirMeasure {
	return func(_ context.Context, path string) (dirSize, error) {
		if e, ok := errs[path]; ok {
			return dirSize{exists: true}, e
		}
		n, ok := sizes[path]
		if !ok {
			return dirSize{}, nil
		}
		return dirSize{bytes: n, exists: true}, nil
	}
}

func TestWorktreeDiskSizeCheck_NoWorktreesDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewWorktreeDiskSizeCheck(config.DoctorConfig{})
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg=%s", r.Status, r.Message)
	}
}

func TestWorktreeDiskSizeCheck_AllUnderThreshold(t *testing.T) {
	dir := t.TempDir()
	rigA := filepath.Join(dir, ".gc", "worktrees", "rig-a")
	rigB := filepath.Join(dir, ".gc", "worktrees", "rig-b")
	if err := os.MkdirAll(rigA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigB, 0o755); err != nil {
		t.Fatal(err)
	}

	c := &WorktreeDiskSizeCheck{
		cfg: config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"},
		measureDir: fakeMeasure(map[string]int64{
			rigA: 1 * 1024 * 1024 * 1024, // 1 GB
			rigB: 500 * 1024 * 1024,      // 500 MB
		}, nil),
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg=%s details=%v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(r.Message, "rig-a") {
		t.Errorf("message should name largest rig (rig-a); got %q", r.Message)
	}
}

func TestWorktreeDiskSizeCheck_UnderThresholdWithMeasurementErrorReturnsWarning(t *testing.T) {
	dir := t.TempDir()
	rigOK := filepath.Join(dir, ".gc", "worktrees", "ok")
	rigBroken := filepath.Join(dir, ".gc", "worktrees", "broken")
	for _, p := range []string{rigOK, rigBroken} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	c := &WorktreeDiskSizeCheck{
		cfg: config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"},
		measureDir: fakeMeasure(map[string]int64{
			rigOK: 1 * 1024 * 1024 * 1024,
		}, map[string]error{
			rigBroken: errors.New("permission denied"),
		}),
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg=%s details=%v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "measure error: broken: permission denied") {
		t.Errorf("details should surface measurement error; got %v", r.Details)
	}
}

func TestWorktreeDiskSizeCheck_OverWarnThreshold(t *testing.T) {
	dir := t.TempDir()
	rigA := filepath.Join(dir, ".gc", "worktrees", "rig-a")
	rigB := filepath.Join(dir, ".gc", "worktrees", "rig-b")
	if err := os.MkdirAll(rigA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigB, 0o755); err != nil {
		t.Fatal(err)
	}

	c := &WorktreeDiskSizeCheck{
		cfg: config.DoctorConfig{WorktreeRigWarnSize: "5GB", WorktreeRigErrorSize: "50GB"},
		measureDir: fakeMeasure(map[string]int64{
			rigA: 8 * 1024 * 1024 * 1024, // 8 GB — over warn
			rigB: 1 * 1024 * 1024 * 1024, // 1 GB — under
		}, nil),
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg=%s details=%v", r.Status, r.Message, r.Details)
	}
	if len(r.Details) != 1 {
		t.Errorf("len(Details) = %d, want 1; details=%v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], "rig-a") {
		t.Errorf("details should flag rig-a; got %q", r.Details[0])
	}
	if strings.Contains(strings.Join(r.Details, "\n"), "rig-b") {
		t.Errorf("details should not flag rig-b (under threshold); got %v", r.Details)
	}
	if r.FixHint == "" {
		t.Error("expected fix hint")
	}
}

func TestWorktreeDiskSizeCheck_OverErrorThreshold(t *testing.T) {
	dir := t.TempDir()
	rig := filepath.Join(dir, ".gc", "worktrees", "huge")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}

	c := &WorktreeDiskSizeCheck{
		cfg: config.DoctorConfig{WorktreeRigWarnSize: "5GB", WorktreeRigErrorSize: "20GB"},
		measureDir: fakeMeasure(map[string]int64{
			rig: 100 * 1024 * 1024 * 1024, // 100 GB
		}, nil),
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error", r.Status)
	}
	if !strings.Contains(r.Details[0], "error threshold") {
		t.Errorf("details should mention error threshold; got %q", r.Details[0])
	}
}

func TestWorktreeDiskSizeCheck_DetailsSortedDescending(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"small", "huge", "medium"} {
		if err := os.MkdirAll(filepath.Join(dir, ".gc", "worktrees", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	c := &WorktreeDiskSizeCheck{
		cfg: config.DoctorConfig{WorktreeRigWarnSize: "1GB", WorktreeRigErrorSize: "100GB"},
		measureDir: fakeMeasure(map[string]int64{
			filepath.Join(dir, ".gc", "worktrees", "small"):  500 * 1024 * 1024,
			filepath.Join(dir, ".gc", "worktrees", "medium"): 5 * 1024 * 1024 * 1024,
			filepath.Join(dir, ".gc", "worktrees", "huge"):   30 * 1024 * 1024 * 1024,
		}, nil),
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; details=%v", r.Status, r.Details)
	}
	// The largest should appear first in details. The "small" rig is
	// under threshold and should not appear at all.
	if !strings.HasPrefix(r.Details[0], `rig "huge"`) {
		t.Errorf("details[0] should start with huge rig; got %q", r.Details[0])
	}
	if strings.Contains(strings.Join(r.Details, "\n"), `rig "small"`) {
		t.Errorf("under-threshold rig should be omitted from details; got %v", r.Details)
	}
}

// TestWorktreeDiskSizeCheck_CountExcludesMeasurementErrors pins the
// fix for a count bug: the message reports "<N> rig(s) over threshold"
// where N must be the threshold-violation count, NOT
// `len(details)` (which also includes measurement errors).
func TestWorktreeDiskSizeCheck_CountExcludesMeasurementErrors(t *testing.T) {
	dir := t.TempDir()
	rigOver := filepath.Join(dir, ".gc", "worktrees", "over")
	rigBroken := filepath.Join(dir, ".gc", "worktrees", "broken")
	for _, p := range []string{rigOver, rigBroken} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	c := &WorktreeDiskSizeCheck{
		cfg: config.DoctorConfig{WorktreeRigWarnSize: "5GB", WorktreeRigErrorSize: "100GB"},
		measureDir: fakeMeasure(map[string]int64{
			rigOver: 8 * 1024 * 1024 * 1024,
		}, map[string]error{
			rigBroken: errors.New("permission denied"),
		}),
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning", r.Status)
	}
	// Exactly one rig is over threshold; the broken one is a
	// measurement error, not a threshold violation.
	if !strings.Contains(r.Message, "1 rig(s)") {
		t.Errorf("message should report 1 rig over threshold (not 2); got %q", r.Message)
	}
}

func TestWorktreeDiskSizeCheck_AllMeasurementsFailedReturnsWarning(t *testing.T) {
	// "We can't tell" must not look like "we're fine". When every rig
	// fails to measure (e.g. permission denied), the check escalates
	// to Warning and surfaces the errors — matches DoltNomsSize policy.
	dir := t.TempDir()
	rigA := filepath.Join(dir, ".gc", "worktrees", "broken-a")
	rigB := filepath.Join(dir, ".gc", "worktrees", "broken-b")
	if err := os.MkdirAll(rigA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigB, 0o755); err != nil {
		t.Fatal(err)
	}

	c := &WorktreeDiskSizeCheck{
		cfg: config.DoctorConfig{},
		measureDir: fakeMeasure(nil, map[string]error{
			rigA: errors.New("permission denied"),
			rigB: errors.New("io error"),
		}),
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
	if r.FixHint == "" {
		t.Error("expected fix hint pointing at filesystem permissions")
	}
	if len(r.Details) != 2 {
		t.Errorf("len(Details) = %d, want 2 (one per failed rig)", len(r.Details))
	}
}

// --- NestedWorktreePruneCheck ---

// fakeGitWorktree implements gitWorktree for tests. Behaves like the
// shared admin dir of a multi-worktree repo: list returns the same
// entries regardless of which path is used to construct it. Per-path
// "uncommitted/unpushed/stashed" flags drive classifyNested.
var _ gitWorktree = (*fakeGitWorktree)(nil)

type fakeGitWorktree struct {
	listResp    []git.Worktree
	listErr     error
	notRepo     map[string]bool // paths where IsRepo returns false
	uncommitted map[string]bool
	unpushed    map[string]bool
	unpushedErr map[string]error
	stashed     map[string]bool
	stashedErr  map[string]error
	removeCalls *[]string // path argument of each WorktreeRemove call
	removeFrom  *[]string // currentPath (cwd-equivalent) at each remove call
	removeErr   map[string]error
	currentPath string
	onList      func(callerPath string) // optional probe; fires per WorktreeList call
}

func (f *fakeGitWorktree) IsRepo() bool { return !f.notRepo[f.currentPath] }
func (f *fakeGitWorktree) WorktreeList() ([]git.Worktree, error) {
	if f.onList != nil {
		f.onList(f.currentPath)
	}
	return f.listResp, f.listErr
}
func (f *fakeGitWorktree) HasUncommittedWork() bool { return f.uncommitted[f.currentPath] }
func (f *fakeGitWorktree) HasUnpushedCommitsResult() (bool, error) {
	if err := f.unpushedErr[f.currentPath]; err != nil {
		return false, err
	}
	return f.unpushed[f.currentPath], nil
}

func (f *fakeGitWorktree) HasStashesResult() (bool, error) {
	if err := f.stashedErr[f.currentPath]; err != nil {
		return false, err
	}
	return f.stashed[f.currentPath], nil
}

func (f *fakeGitWorktree) WorktreeRemove(path string, _ bool) error {
	if f.removeCalls != nil {
		*f.removeCalls = append(*f.removeCalls, path)
	}
	if f.removeFrom != nil {
		*f.removeFrom = append(*f.removeFrom, f.currentPath)
	}
	if f.removeErr != nil {
		if e, ok := f.removeErr[path]; ok {
			return e
		}
	}
	return nil
}

// makeAgentHome creates dir/.gc/worktrees/rig-a/<agent>/ with a stub
// .git file so isGitWorktreePath returns true. Returns the agent home
// path (canonicalized via pathutil.NormalizePathForCompare to match
// what the check stores). The .git stub uses a shared gitdir so all
// homes created via this helper appear to belong to the same admin
// dir; tests that need distinct admin dirs should use
// makeAgentHomeAdmin.
func makeAgentHome(t *testing.T, dir, agent string) string {
	t.Helper()
	return makeAgentHomeAdmin(t, dir, "rig-a", agent, "/tmp/none")
}

// makeAgentHomeAdmin is like makeAgentHome but lets the test specify
// the gitdir admin root, so two homes can simulate distinct repos.
func makeAgentHomeAdmin(t *testing.T, dir, rig, agent, adminRoot string) string {
	t.Helper()
	home := filepath.Join(dir, ".gc", "worktrees", rig, agent)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := adminRoot + "/worktrees/" + agent
	if err := os.WriteFile(filepath.Join(home, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return pathutil.NormalizePathForCompare(home)
}

func TestNestedWorktreePruneCheck_NoWorktreesDir(t *testing.T) {
	dir := t.TempDir()
	c := NewNestedWorktreePruneCheck(config.DoctorConfig{})
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK", r.Status)
	}
}

func TestNestedWorktreePruneCheck_NoNestedWorktrees(t *testing.T) {
	dir := t.TempDir()
	home := makeAgentHome(t, dir, "polecat-1")
	var removes []string
	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: home, Branch: "home-branch"},
					// sibling worktree at unrelated path — not nested
					{Path: filepath.Join(dir, "external"), Branch: "external"},
				},
				removeCalls: &removes,
				currentPath: path,
			}
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg=%s", r.Status, r.Message)
	}
	if len(c.findings) != 0 {
		t.Errorf("findings = %d, want 0", len(c.findings))
	}
}

func TestNestedWorktreePruneCheck_ClassifiesSafeAndUnsafe(t *testing.T) {
	dir := t.TempDir()
	home := makeAgentHome(t, dir, "polecat-1")
	safe := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "task-clean"))
	dirty := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "task-dirty"))
	unpushed := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "task-unpushed"))
	stashed := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "task-stashed"))
	if err := os.MkdirAll(safe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unpushed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stashed, 0o755); err != nil {
		t.Fatal(err)
	}

	var removes []string
	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: home, Branch: "home-branch"},
					{Path: safe, Branch: "task-clean"},
					{Path: dirty, Branch: "task-dirty"},
					{Path: unpushed, Branch: "task-unpushed"},
					{Path: stashed, Branch: "task-stashed"},
				},
				uncommitted: map[string]bool{dirty: true},
				unpushed:    map[string]bool{unpushed: true},
				stashed:     map[string]bool{stashed: true},
				removeCalls: &removes,
				currentPath: path,
			}
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg=%s details=%v", r.Status, r.Message, r.Details)
	}

	var safeCount, unsafeCount int
	for _, f := range c.findings {
		if f.safeToRm {
			safeCount++
		} else {
			unsafeCount++
		}
	}
	if safeCount != 1 {
		t.Errorf("safeCount = %d, want 1", safeCount)
	}
	if unsafeCount != 3 {
		t.Errorf("unsafeCount = %d, want 3", unsafeCount)
	}

	for _, f := range c.findings {
		if f.path == home {
			t.Errorf("agent home %q should not be a nested finding", home)
		}
	}

	// Fix removes only the safe one.
	if err := c.Fix(&CheckContext{CityPath: dir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(removes) != 1 {
		t.Fatalf("removes = %v, want exactly one (the safe entry)", removes)
	}
	if removes[0] != safe {
		t.Errorf("removed %q, want %q", removes[0], safe)
	}
}

func TestNestedWorktreePruneCheck_PruneTrueEscalatesSeverity(t *testing.T) {
	dir := t.TempDir()
	home := makeAgentHome(t, dir, "polecat-1")
	safe := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "task-clean"))
	if err := os.MkdirAll(safe, 0o755); err != nil {
		t.Fatal(err)
	}

	var removes []string
	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{NestedWorktreePrune: true},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: home, Branch: "home-branch"},
					{Path: safe, Branch: "task-clean"},
				},
				removeCalls: &removes,
				currentPath: path,
			}
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error (NestedWorktreePrune=true escalates)", r.Status)
	}
}

func TestNestedWorktreePruneCheck_AllUnsafeReturnsOK(t *testing.T) {
	dir := t.TempDir()
	home := makeAgentHome(t, dir, "polecat-1")
	dirty := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "task"))
	if err := os.MkdirAll(dirty, 0o755); err != nil {
		t.Fatal(err)
	}

	var removes []string
	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: home, Branch: "home-branch"},
					{Path: dirty, Branch: "task"},
				},
				uncommitted: map[string]bool{dirty: true},
				removeCalls: &removes,
				currentPath: path,
			}
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (nothing safely prunable)", r.Status)
	}
	if !strings.Contains(r.Message, "none safely prunable") {
		t.Errorf("message should say 'none safely prunable'; got %q", r.Message)
	}
}

func TestNestedWorktreePruneCheck_AllUnsafeWithListingErrorReturnsWarning(t *testing.T) {
	dir := t.TempDir()
	homeA := makeAgentHomeAdmin(t, dir, "rig-a", "agent-1", "/repo-a/.git")
	homeB := makeAgentHomeAdmin(t, dir, "rig-b", "agent-2", "/repo-b/.git")
	dirty := pathutil.NormalizePathForCompare(filepath.Join(homeA, "worktrees", "task"))
	if err := os.MkdirAll(dirty, 0o755); err != nil {
		t.Fatal(err)
	}

	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			switch path {
			case homeB:
				return &fakeGitWorktree{
					listErr:     errors.New("cannot list worktrees"),
					currentPath: path,
				}
			default:
				return &fakeGitWorktree{
					listResp: []git.Worktree{
						{Path: homeA, Branch: "home-a"},
						{Path: dirty, Branch: "task"},
					},
					uncommitted: map[string]bool{dirty: true},
					currentPath: path,
				}
			}
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg=%s details=%v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "cannot list worktrees") {
		t.Errorf("details should include listing error; got %v", r.Details)
	}
}

func TestNestedWorktreePruneCheck_DeduplicatesAcrossHomes(t *testing.T) {
	// Two agent homes that share the same git repo would each list the
	// same nested worktree. The check must not classify or remove it
	// twice.
	dir := t.TempDir()
	homeA := makeAgentHome(t, dir, "polecat-1")
	homeB := makeAgentHome(t, dir, "polecat-2")

	// Nested under homeA. homeB will also list it because they share a repo.
	nested := pathutil.NormalizePathForCompare(filepath.Join(homeA, "worktrees", "task"))
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	var removes []string
	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: homeA, Branch: "a"},
					{Path: homeB, Branch: "b"},
					{Path: nested, Branch: "task"},
				},
				removeCalls: &removes,
				currentPath: path,
			}
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning", r.Status)
	}
	if len(c.findings) != 1 {
		t.Errorf("findings = %d, want 1 (deduplicated)", len(c.findings))
	}

	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(removes) != 1 {
		t.Errorf("removes = %v, want exactly one", removes)
	}
}

// TestNestedWorktreePruneCheck_FixContinuesPastError pins the
// reclaim-as-much-as-possible semantic: a single locked worktree must
// not strand later safe entries. The returned error joins all per-entry
// failures so the operator sees what was missed.
func TestNestedWorktreePruneCheck_FixContinuesPastError(t *testing.T) {
	dir := t.TempDir()
	home := makeAgentHome(t, dir, "polecat-1")
	first := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "first"))
	second := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "second"))
	third := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "third"))
	for _, p := range []string{first, second, third} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var removes []string
	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: home, Branch: "home"},
					{Path: first, Branch: "first"},
					{Path: second, Branch: "second"},
					{Path: third, Branch: "third"},
				},
				removeCalls: &removes,
				removeErr:   map[string]error{second: errors.New("git locked")},
				currentPath: path,
			}
		},
	}
	if r := c.Run(&CheckContext{CityPath: dir}); r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning", r.Status)
	}
	err := c.Fix(&CheckContext{})
	if err == nil {
		t.Fatal("Fix should surface the remove error")
	}
	if !strings.Contains(err.Error(), "git locked") {
		t.Errorf("error should wrap original; got %v", err)
	}
	// All three were attempted; only the failing one is missing from a
	// successful-removal perspective — but accumulator records every
	// call.
	if len(removes) != 3 {
		t.Errorf("removes = %v, want all three attempted", removes)
	}
}

func TestNestedWorktreePruneCheck_FixRevalidatesBeforeRemove(t *testing.T) {
	dir := t.TempDir()
	home := makeAgentHome(t, dir, "agent-1")
	nested := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "task"))
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	var removes []string
	uncommitted := map[string]bool{}
	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: home, Branch: "h"},
					{Path: nested, Branch: "task"},
				},
				uncommitted: uncommitted,
				removeCalls: &removes,
				currentPath: path,
			}
		},
	}
	if r := c.Run(&CheckContext{CityPath: dir}); r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning", r.Status)
	}
	uncommitted[nested] = true
	err := c.Fix(&CheckContext{})
	if err == nil {
		t.Fatal("Fix should fail closed when revalidation finds new local work")
	}
	if len(removes) != 0 {
		t.Errorf("removes = %v, want none after failed revalidation", removes)
	}
}

func TestNestedWorktreePruneCheck_ProbeErrorsAreUnsafe(t *testing.T) {
	dir := t.TempDir()
	home := makeAgentHome(t, dir, "agent-1")
	unpushedErr := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "unpushed-error"))
	stashErr := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "stash-error"))
	for _, p := range []string{unpushedErr, stashErr} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var removes []string
	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: home, Branch: "h"},
					{Path: unpushedErr, Branch: "unpushed-error"},
					{Path: stashErr, Branch: "stash-error"},
				},
				unpushedErr: map[string]error{unpushedErr: errors.New("log failed")},
				stashedErr:  map[string]error{stashErr: errors.New("stash failed")},
				removeCalls: &removes,
				currentPath: path,
			}
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning because probe errors are inspection failures; msg=%s details=%v", r.Status, r.Message, r.Details)
	}
	if len(c.findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(c.findings))
	}
	for _, f := range c.findings {
		if f.safeToRm {
			t.Fatalf("%s should not be safe after probe error", f.path)
		}
		if !strings.Contains(f.reason, "probe failed") {
			t.Errorf("reason for %s = %q, want probe failure", f.path, f.reason)
		}
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix should skip unsafe probe-error findings without error: %v", err)
	}
	if len(removes) != 0 {
		t.Errorf("removes = %v, want none", removes)
	}
}

func TestReadGitAdminDir_RepoPathContainsWorktreesSegment(t *testing.T) {
	// Regression: if the repo's own path contains "/worktrees/" as a
	// literal segment (e.g. user keeps repos under ~/worktrees/), the
	// admin-dir extraction must still find the LAST "/worktrees/"
	// (the one git inserts before the per-worktree subdir), not the
	// user's path component.
	dir := t.TempDir()
	tricky := filepath.Join(dir, "worktrees", "myproj")
	if err := os.MkdirAll(tricky, 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := tricky + "/.git/worktrees/agentA"
	if err := os.WriteFile(filepath.Join(tricky, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readGitAdminDir(tricky)
	want := pathutil.NormalizePathForCompare(tricky + "/.git")
	if got != want {
		t.Errorf("readGitAdminDir = %q, want %q (must use LastIndex of /worktrees/)", got, want)
	}
}

// TestNestedWorktreePruneCheck_DedupsWorktreeListAcrossSharedAdminDir
// pins the optimization that skips redundant `git worktree list` calls
// for agent homes that share a single admin dir. Two homes pointing at
// the same admin dir must trigger exactly one WorktreeList call; two
// homes pointing at distinct admin dirs must trigger two.
func TestNestedWorktreePruneCheck_DedupsWorktreeListAcrossSharedAdminDir(t *testing.T) {
	dir := t.TempDir()
	homeA := makeAgentHomeAdmin(t, dir, "rig-a", "polecat-1", "/repo/.git")
	homeB := makeAgentHomeAdmin(t, dir, "rig-a", "polecat-2", "/repo/.git")
	homeC := makeAgentHomeAdmin(t, dir, "rig-b", "polecat-3", "/other/.git")

	var listCalls []string
	var removes []string
	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: homeA, Branch: "a"},
					{Path: homeB, Branch: "b"},
					{Path: homeC, Branch: "c"},
				},
				removeCalls: &removes,
				currentPath: path,
				onList:      func(p string) { listCalls = append(listCalls, p) },
			}
		},
	}
	if r := c.Run(&CheckContext{CityPath: dir}); r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg=%s", r.Status, r.Message)
	}
	if len(listCalls) != 2 {
		t.Errorf("WorktreeList calls = %v, want 2 (one per distinct admin dir; homeA and homeB share)", listCalls)
	}
}

// TestNestedWorktreePruneCheck_DedupCoversNestedUnderEveryHome pins the
// fix for a correctness bug introduced by the admin-dir dedup: when
// homes A and B share an admin dir, only A's WorktreeList runs, but
// nested entries living under B must still be classified. Iterating
// the shared list against EVERY home in the admin group preserves
// coverage; the previous implementation only checked containment
// against the source home and silently dropped B's nested entries.
func TestNestedWorktreePruneCheck_DedupCoversNestedUnderEveryHome(t *testing.T) {
	dir := t.TempDir()
	homeA := makeAgentHomeAdmin(t, dir, "rig-a", "polecat-1", "/repo/.git")
	homeB := makeAgentHomeAdmin(t, dir, "rig-a", "polecat-2", "/repo/.git")
	nestedUnderA := pathutil.NormalizePathForCompare(filepath.Join(homeA, "worktrees", "task-a"))
	nestedUnderB := pathutil.NormalizePathForCompare(filepath.Join(homeB, "worktrees", "task-b"))
	for _, p := range []string{nestedUnderA, nestedUnderB} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var listCalls []string
	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: homeA, Branch: "a"},
					{Path: homeB, Branch: "b"},
					{Path: nestedUnderA, Branch: "task-a"},
					{Path: nestedUnderB, Branch: "task-b"},
				},
				currentPath: path,
				onList:      func(p string) { listCalls = append(listCalls, p) },
			}
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning", r.Status)
	}
	if len(listCalls) != 1 {
		t.Errorf("WorktreeList calls = %v, want 1 (admin-dir dedup)", listCalls)
	}
	if len(c.findings) != 2 {
		t.Errorf("findings = %d, want 2 (one nested under each home, even though only one WorktreeList ran)",
			len(c.findings))
	}
	parents := map[string]bool{}
	for _, f := range c.findings {
		parents[f.parent] = true
	}
	if !parents[homeA] || !parents[homeB] {
		t.Errorf("findings should attribute parents to both homes; got %v", parents)
	}
}

// TestNestedWorktreePruneCheck_FixUsesParentForGitContext pins the fix
// for the cwd-removal pattern: WorktreeRemove must run from the parent
// home, not from the worktree being removed.
func TestNestedWorktreePruneCheck_FixUsesParentForGitContext(t *testing.T) {
	dir := t.TempDir()
	home := makeAgentHome(t, dir, "polecat-1")
	nested := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "task"))
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	var removes, removeFrom []string
	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: home, Branch: "h"},
					{Path: nested, Branch: "task"},
				},
				removeCalls: &removes,
				removeFrom:  &removeFrom,
				currentPath: path,
			}
		},
	}
	if r := c.Run(&CheckContext{CityPath: dir}); r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning", r.Status)
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(removeFrom) != 1 || removeFrom[0] != home {
		t.Errorf("WorktreeRemove ran from %v, want exactly [%q] (parent home, not the worktree being removed)",
			removeFrom, home)
	}
}

// TestNestedWorktreePruneCheck_BrokenRepoGate pins the IsRepo gate that
// defends against fail-open semantics in HasUnpushedCommits / HasStashes
// (which return false on git error). A candidate whose admin dir is
// corrupt must not be classified as safe to remove.
func TestNestedWorktreePruneCheck_BrokenRepoGate(t *testing.T) {
	dir := t.TempDir()
	home := makeAgentHome(t, dir, "polecat-1")
	broken := pathutil.NormalizePathForCompare(filepath.Join(home, "worktrees", "broken"))
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}

	var removes []string
	c := &NestedWorktreePruneCheck{
		cfg: config.DoctorConfig{},
		newGit: func(path string) gitWorktree {
			return &fakeGitWorktree{
				listResp: []git.Worktree{
					{Path: home, Branch: "h"},
					{Path: broken, Branch: "broken"},
				},
				notRepo:     map[string]bool{broken: true},
				removeCalls: &removes,
				currentPath: path,
			}
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK (broken candidate marked unsafe)", r.Status)
	}
	if len(c.findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(c.findings))
	}
	if c.findings[0].safeToRm {
		t.Error("broken candidate should NOT be safeToRm")
	}
	if c.findings[0].reason != "git status unreadable" {
		t.Errorf("reason = %q, want %q", c.findings[0].reason, "git status unreadable")
	}
}

func TestPathStrictlyInside(t *testing.T) {
	tests := []struct {
		child, parent string
		want          bool
	}{
		{"/a/b/c", "/a/b", true},
		{"/a/b", "/a/b", false},  // equal — strict
		{"/a/b", "/a/bc", false}, // prefix-but-not-subpath
		{"/x/y", "/a/b", false},
		{"/a/b/c/d", "/a/b", true},
	}
	for _, tt := range tests {
		got := pathStrictlyInside(tt.child, tt.parent)
		if got != tt.want {
			t.Errorf("pathStrictlyInside(%q, %q) = %v, want %v", tt.child, tt.parent, got, tt.want)
		}
	}
}

// A rig the check could not size must be the headline, by name. On 2026-08-25
// this check read 'largest measured: "astro" at 252.9 MB' while the qcore tree
// it gave up on held 180 GB, and the verdict looked healthy (ga-hyhccs).
func TestWorktreeDiskSizeCheck_UnmeasuredRigIsNamedNotHiddenBehindLargestMeasured(t *testing.T) {
	dir := t.TempDir()
	astro := filepath.Join(dir, ".gc", "worktrees", "astro")
	qcore := filepath.Join(dir, ".gc", "worktrees", "qcore")
	for _, p := range []string{astro, qcore} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := &WorktreeDiskSizeCheck{
		cfg: config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"},
		measureDir: fakeMeasure(map[string]int64{
			astro: 253 * 1024 * 1024,
		}, map[string]error{
			qcore: fmt.Errorf("measure directory: du -sk timed out after 1m0s; fallback walk: %w", context.DeadlineExceeded),
		}),
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg=%s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, `"qcore"`) {
		t.Errorf("message must name the unmeasured rig; got %q", r.Message)
	}
	if strings.Contains(r.Message, "largest: ") || strings.Contains(r.Message, "largest measured: ") {
		t.Errorf("message presents a measured size as the answer while a rig is unmeasured; got %q", r.Message)
	}
	if strings.Contains(r.FixHint, "permission") {
		t.Errorf("a measurement that ran out of time must not blame permissions; hint=%q", r.FixHint)
	}
}

func TestWorktreeDiskSizeCheck_OverThresholdStillNamesUnmeasuredRig(t *testing.T) {
	dir := t.TempDir()
	over := filepath.Join(dir, ".gc", "worktrees", "over")
	qcore := filepath.Join(dir, ".gc", "worktrees", "qcore")
	for _, p := range []string{over, qcore} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := &WorktreeDiskSizeCheck{
		cfg: config.DoctorConfig{WorktreeRigWarnSize: "5GB", WorktreeRigErrorSize: "100GB"},
		measureDir: fakeMeasure(map[string]int64{
			over: 8 * 1024 * 1024 * 1024,
		}, map[string]error{
			qcore: fmt.Errorf("measure directory: du -sk timed out; fallback walk: %w", context.DeadlineExceeded),
		}),
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if !strings.Contains(r.Message, `"qcore"`) {
		t.Errorf("message must name the unmeasured rig even when another rig is over threshold; got %q", r.Message)
	}
}

func TestWorktreeDiskSizeCheck_PermissionErrorKeepsPermissionHint(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, ".gc", "worktrees", "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	c := &WorktreeDiskSizeCheck{
		cfg: config.DoctorConfig{},
		measureDir: fakeMeasure(nil, map[string]error{
			locked: &fs.PathError{Op: "open", Path: locked, Err: fs.ErrPermission},
		}),
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if !strings.Contains(r.FixHint, "permission") {
		t.Errorf("a real permission error should keep the permissions hint; hint=%q", r.FixHint)
	}
	if !strings.Contains(r.Message, `"locked"`) {
		t.Errorf("message must name the unmeasured rig; got %q", r.Message)
	}
}

// The production measurer must carry a real permission failure through to the
// hint: du exits 1 and names the cause only on stderr.
func TestDuDirBytesReportsUnreadableTreeAsPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-000 directory")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	_, _, err := duDirBytes(root)
	if err == nil {
		t.Fatal("duDirBytes on a tree with an unreadable directory returned no error")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("error does not wrap fs.ErrPermission: %v", err)
	}
	if !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("error does not carry du's stderr: %v", err)
	}
}

// Several unreadable directories fill any stderr prefix: the permission cause
// must still be found, and the error must stay on one line for doctor's
// single-line message.
func TestDuDirBytesFindsPermissionPastLongStderrAndStaysOneLine(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-000 directory")
	}
	root := t.TempDir()
	long := strings.Repeat("a-very-long-worktree-directory-name-", 4)
	for i := 0; i < 4; i++ {
		locked := filepath.Join(root, fmt.Sprintf("%s%d", long, i))
		if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	}
	_, _, err := duDirBytes(root)
	if err == nil {
		t.Fatal("duDirBytes on unreadable directories returned no error")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("error does not wrap fs.ErrPermission: %v", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error spans lines: %q", err.Error())
	}
}

// --- worktree-disk-size inside its budget (pl-59k) ---

// sizeWithNextWord finds a printed byte count and the word after it.
var sizeWithNextWord = regexp.MustCompile(`(\d+(?:\.\d+)? (?:B|KB|MB|GB))(?: (\w+))?`)

// assertDuSizesLabeledLogical fails when text prints a byte count that is
// neither labeled logical nor a configured threshold. Thresholds are always
// printed followed by "warn" or "error".
func assertDuSizesLabeledLogical(t *testing.T, text string) {
	t.Helper()
	for _, m := range sizeWithNextWord.FindAllStringSubmatch(text, -1) {
		switch m[2] {
		case "logical", "warn", "error":
		default:
			t.Errorf("byte count %q printed without the logical label: %q", m[1], text)
		}
	}
}

// makeRigDirs creates .gc/worktrees/<name> under dir for each name and
// returns their paths by name.
func makeRigDirs(t *testing.T, dir string, names ...string) map[string]string {
	t.Helper()
	paths := make(map[string]string, len(names))
	for _, name := range names {
		p := filepath.Join(dir, ".gc", "worktrees", name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		paths[name] = p
	}
	return paths
}

// The discharge for pl-59k: with a rig the walk cannot finish inside the
// budget, the check (1) completes inside the budget, (2) names that rig,
// (3) reports a labeled lower bound, never a bare number, and (4) does not
// present another rig's size as its verdict.
func TestWorktreeDiskSizeCheck_UnfinishedWalkIsNamedLowerBoundInsideBudget(t *testing.T) {
	dir := t.TempDir()
	rigs := makeRigDirs(t, dir, "astro", "qcore")
	const budget = 100 * time.Millisecond
	c := &WorktreeDiskSizeCheck{
		cfg:    config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"},
		budget: budget,
		measureDir: func(ctx context.Context, root string) (dirSize, error) {
			if root == rigs["astro"] {
				return dirSize{bytes: 253 * 1024 * 1024, exists: true}, nil
			}
			// A tree too large to finish: the walk stops only at the deadline.
			<-ctx.Done()
			return dirSize{bytes: 3 * 1024 * 1024 * 1024, exists: true, lowerBound: true}, nil
		},
	}
	d := &Doctor{CheckTimeout: 5 * time.Second}
	d.Register(c)

	start := time.Now()
	report := d.RunCollect(&CheckContext{CityPath: dir}, false)
	elapsed := time.Since(start)
	d.Wait()

	r := report.Results[0]
	if r.TimedOut {
		t.Fatalf("check was abandoned at doctor's per-check timeout: %s", r.Message)
	}
	if elapsed > time.Second {
		t.Errorf("check took %s, want it to stop soon after its %s budget", elapsed, budget)
	}
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg=%s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, `"qcore" at least 3.0 GB logical (lower bound: the size walk did not finish within 100ms)`) {
		t.Errorf("message must name the unfinished rig with its labeled lower bound; got %q", r.Message)
	}
	if strings.Contains(r.Message, `"astro"`) || strings.Contains(r.Message, "253.0 MB") {
		t.Errorf("message presents another rig's size as the verdict; got %q", r.Message)
	}
	if !strings.Contains(r.FixHint, "ran out of time") {
		t.Errorf("hint must say the walk ran out of time; got %q", r.FixHint)
	}
	assertDuSizesLabeledLogical(t, r.Message)
	assertDuSizesLabeledLogical(t, strings.Join(r.Details, "\n"))
}

// A measurer that does not stop at the deadline (du stuck in the kernel on a
// hung mount) is abandoned: the check still returns inside its budget and
// names the rig it gave up on.
func TestWorktreeDiskSizeCheck_MeasurerIgnoringDeadlineIsAbandoned(t *testing.T) {
	dir := t.TempDir()
	rigs := makeRigDirs(t, dir, "astro", "stuck")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c := &WorktreeDiskSizeCheck{
		cfg:    config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"},
		budget: 100 * time.Millisecond,
		measureDir: func(_ context.Context, root string) (dirSize, error) {
			if root == rigs["astro"] {
				return dirSize{bytes: 253 * 1024 * 1024, exists: true}, nil
			}
			<-release
			return dirSize{}, nil
		},
	}

	start := time.Now()
	r := c.Run(&CheckContext{CityPath: dir})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("check took %s, want it to abandon the stuck measurer at its budget", elapsed)
	}
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg=%s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, `"stuck" (size UNKNOWN)`) {
		t.Errorf("message must name the abandoned rig; got %q", r.Message)
	}
	// A walk that survives being stopped is stuck in the kernel, not merely
	// large: the prune-it advice would send the operator the wrong way.
	if !strings.Contains(r.FixHint, "hung mount") {
		t.Errorf("hint must point at a stuck walk; got %q", r.FixHint)
	}
	if strings.Contains(r.FixHint, "not unreadable") {
		t.Errorf("hint blames tree size for a walk that would not stop; got %q", r.FixHint)
	}
}

// The budget follows doctor's own per-check timeout: with --check-timeout
// lowered below the default budget, the check still returns its verdict
// instead of being abandoned as "outcome unknown".
func TestWorktreeDiskSizeCheck_FinishesInsideALoweredCheckTimeout(t *testing.T) {
	dir := t.TempDir()
	makeRigDirs(t, dir, "qcore")
	c := &WorktreeDiskSizeCheck{
		cfg: config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"},
		measureDir: func(ctx context.Context, _ string) (dirSize, error) {
			<-ctx.Done()
			return dirSize{bytes: 1024 * 1024 * 1024, exists: true, lowerBound: true}, nil
		},
	}
	d := &Doctor{CheckTimeout: 900 * time.Millisecond}
	d.Register(c)
	report := d.RunCollect(&CheckContext{CityPath: dir}, false)
	d.Wait()

	r := report.Results[0]
	if r.TimedOut {
		t.Fatalf("check was abandoned at a 900ms --check-timeout: %s", r.Message)
	}
	if !strings.Contains(r.Message, `"qcore" at least 1.0 GB logical (lower bound`) {
		t.Errorf("message must name the rig with its labeled lower bound; got %q", r.Message)
	}
}

// More rigs than measuring slots, every walk running to the deadline: the rig
// that never got a slot is named, with a hint that says why, rather than the
// "tree is very large" advice that fits the walks that did run.
func TestWorktreeDiskSizeCheck_StarvedRigIsNamedWithItsOwnHint(t *testing.T) {
	dir := t.TempDir()
	makeRigDirs(t, dir, "r1", "r2", "r3", "r4", "r5")
	c := &WorktreeDiskSizeCheck{
		cfg:    config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"},
		budget: 100 * time.Millisecond,
		measureDir: func(ctx context.Context, _ string) (dirSize, error) {
			<-ctx.Done()
			return dirSize{bytes: 1024 * 1024 * 1024, exists: true, lowerBound: true}, nil
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if got := strings.Count(r.Message, "(size UNKNOWN)"); got != 1 {
		t.Errorf("want exactly the one starved rig of five reported UNKNOWN, got %d; msg=%q", got, r.Message)
	}
	if got := strings.Count(r.Message, "at least 1.0 GB logical"); got != 4 {
		t.Errorf("want the four measured rigs as lower bounds, got %d; msg=%q", got, r.Message)
	}
	if !strings.Contains(r.FixHint, "measuring slot") {
		t.Errorf("hint must explain the starved rig; got %q", r.FixHint)
	}
}

// Measuring runs on goroutines of its own, outside the runner's panic
// recovery: a panic there must fail that rig, not the whole doctor process.
func TestWorktreeDiskSizeCheck_PanickingMeasurerFailsOnlyItsRig(t *testing.T) {
	dir := t.TempDir()
	rigs := makeRigDirs(t, dir, "astro", "boom")
	c := &WorktreeDiskSizeCheck{
		cfg:    config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"},
		budget: time.Second,
		measureDir: func(_ context.Context, root string) (dirSize, error) {
			if root == rigs["boom"] {
				panic("walker exploded")
			}
			return dirSize{bytes: 253 * 1024 * 1024, exists: true}, nil
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg=%s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, `"boom" (size UNKNOWN)`) {
		t.Errorf("message must name the rig whose walk panicked; got %q", r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "walker exploded") {
		t.Errorf("details must carry the panic; got %v", r.Details)
	}
}

// A lower bound already past the error threshold is a definite verdict: the
// tree holds at least that much.
func TestWorktreeDiskSizeCheck_LowerBoundPastErrorThresholdIsAnError(t *testing.T) {
	dir := t.TempDir()
	makeRigDirs(t, dir, "qcore")
	c := &WorktreeDiskSizeCheck{
		cfg:    config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"},
		budget: 50 * time.Millisecond,
		measureDir: func(ctx context.Context, _ string) (dirSize, error) {
			<-ctx.Done()
			return dirSize{bytes: 60 * 1024 * 1024 * 1024, exists: true, lowerBound: true}, nil
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg=%s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, `"qcore" at least 60.0 GB logical`) {
		t.Errorf("message must carry the labeled lower bound; got %q", r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "at least 60.0 GB logical") ||
		!strings.Contains(r.Details[0], "exceeds 50.0 GB error threshold") {
		t.Errorf("details = %v, want the lower bound flagged against the error threshold", r.Details)
	}
	assertDuSizesLabeledLogical(t, r.Message)
	assertDuSizesLabeledLogical(t, strings.Join(r.Details, "\n"))
}

// Rigs are sized side by side under one deadline, so a tree that cannot be
// finished does not use up the time the other rigs needed.
func TestWorktreeDiskSizeCheck_UnfinishableRigDoesNotStarveTheOthers(t *testing.T) {
	dir := t.TempDir()
	rigs := makeRigDirs(t, dir, "aaa-huge", "zzz-small")
	c := &WorktreeDiskSizeCheck{
		cfg:    config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"},
		budget: 100 * time.Millisecond,
		measureDir: func(ctx context.Context, root string) (dirSize, error) {
			if root == rigs["aaa-huge"] {
				<-ctx.Done()
				return dirSize{bytes: 1024 * 1024 * 1024, exists: true, lowerBound: true}, nil
			}
			if ctx.Err() != nil {
				// Started after the budget was spent: nothing counted.
				return dirSize{exists: true, lowerBound: true}, nil
			}
			return dirSize{bytes: 1024 * 1024 * 1024, exists: true}, nil
		},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if !strings.Contains(r.Message, `"aaa-huge" at least`) {
		t.Errorf("message must name the unfinished rig; got %q", r.Message)
	}
	if strings.Contains(r.Message, `"zzz-small"`) {
		t.Errorf("the small rig was starved of its measuring time; got %q", r.Message)
	}
}

// pl-59k part (d): du charges every APFS clone at full size, so each size
// this check prints is labeled logical, on every path.
func TestWorktreeDiskSizeCheck_EveryPrintedSizeIsLabeledLogical(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	cases := map[string]map[string]dirSize{
		"under thresholds":             {"a": {bytes: 1 * gb, exists: true}},
		"over warn":                    {"a": {bytes: 20 * gb, exists: true}, "b": {bytes: 1 * gb, exists: true}},
		"over error":                   {"a": {bytes: 60 * gb, exists: true}, "b": {bytes: 20 * gb, exists: true}},
		"lower bound under warn":       {"a": {bytes: 1 * gb, exists: true, lowerBound: true}, "b": {bytes: 2 * gb, exists: true}},
		"lower bound over warn":        {"a": {bytes: 20 * gb, exists: true, lowerBound: true}, "b": {bytes: 2 * gb, exists: true}},
		"over warn beside lower bound": {"a": {bytes: 20 * gb, exists: true}, "b": {bytes: 2 * gb, exists: true, lowerBound: true}},
	}
	for name, sizes := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			names := make([]string, 0, len(sizes))
			for n := range sizes {
				names = append(names, n)
			}
			makeRigDirs(t, dir, names...)
			c := &WorktreeDiskSizeCheck{
				cfg:    config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"},
				budget: time.Second,
				measureDir: func(_ context.Context, root string) (dirSize, error) {
					return sizes[filepath.Base(root)], nil
				},
			}
			r := c.Run(&CheckContext{CityPath: dir})
			if !strings.Contains(r.Message, " logical") {
				t.Errorf("message prints no labeled size; got %q", r.Message)
			}
			assertDuSizesLabeledLogical(t, r.Message)
			assertDuSizesLabeledLogical(t, strings.Join(r.Details, "\n"))
		})
	}
}

// End to end through the production measurer: du on the huge rig is still
// walking at the deadline, du on the small rig finishes. The huge rig's
// finished subtrees become its labeled lower bound.
func TestWorktreeDiskSizeCheck_ProductionMeasurerStopsDuAtBudget(t *testing.T) {
	dir := t.TempDir()
	makeRigDirs(t, dir, "astro", "qcore")
	installFakeDu(t, `case "$2" in
*/qcore) printf '2097152\t%s/polecats/a\n' "$2"; exec sleep 30 ;;
*) printf '1024\t%s\n' "$2" ;;
esac
`)
	c := NewWorktreeDiskSizeCheck(config.DoctorConfig{WorktreeRigWarnSize: "10GB", WorktreeRigErrorSize: "50GB"})
	c.budget = 300 * time.Millisecond

	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg=%s details=%v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(r.Message, `"qcore" at least 2.0 GB logical (lower bound: the size walk did not finish within 300ms)`) {
		t.Errorf("message must carry du's counted subtrees as the lower bound; got %q", r.Message)
	}
	if strings.Contains(r.Message, `"astro"`) {
		t.Errorf("message presents another rig's size as the verdict; got %q", r.Message)
	}
}
