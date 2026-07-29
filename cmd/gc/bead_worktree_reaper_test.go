package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/runtime"
)

func gaConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
	}
}

func TestExtractBeadIDFromWorktreeNameBareID(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "ga-n0oafq")
	if got != "ga-n0oafq" {
		t.Errorf("got %q, want %q", got, "ga-n0oafq")
	}
}

func TestExtractBeadIDFromWorktreeNameCompound(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "builder-ga-34q3ss")
	if got != "ga-34q3ss" {
		t.Errorf("got %q, want %q", got, "ga-34q3ss")
	}
}

func TestExtractBeadIDFromWorktreeNameNoMatch(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "builder-feature-branch")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractBeadIDFromWorktreeNameSingleSegment(t *testing.T) {
	cfg := gaConfig()
	got := extractBeadIDFromWorktreeName(cfg, "builder")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractBeadIDFromWorktreeNameNilConfig(t *testing.T) {
	got := extractBeadIDFromWorktreeName(nil, "ga-n0oafq")
	if got != "" {
		t.Errorf("got %q, want empty for nil config", got)
	}
}

func TestExtractBeadIDFromWorktreeNameEmptyName(t *testing.T) {
	got := extractBeadIDFromWorktreeName(gaConfig(), "")
	if got != "" {
		t.Errorf("got %q, want empty for empty name", got)
	}
}

func TestIsStrictlyUnderDirSubpath(t *testing.T) {
	dir := filepath.Join("a", "b")
	path := filepath.Join("a", "b", "c")
	if !isStrictlyUnderDir(dir, path) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = false, want true", dir, path)
	}
}

func TestIsStrictlyUnderDirSameDir(t *testing.T) {
	dir := filepath.Join("a", "b")
	if isStrictlyUnderDir(dir, dir) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = true, want false (same dir)", dir, dir)
	}
}

func TestIsStrictlyUnderDirPathTraversal(t *testing.T) {
	dir := filepath.Join("a", "b")
	path := filepath.Join("a", "c") // sibling — relative path starts with ".."
	if isStrictlyUnderDir(dir, path) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = true, want false (path traversal)", dir, path)
	}
}

func TestIsStrictlyUnderDirDeepSubpath(t *testing.T) {
	dir := filepath.Join("root", "worktrees")
	path := filepath.Join("root", "worktrees", "gascity", "builder")
	if !isStrictlyUnderDir(dir, path) {
		t.Errorf("isStrictlyUnderDir(%q, %q) = false, want true", dir, path)
	}
}

func TestDiscoverBeadWorktreeCandidatesFindsDirectAndNestedWorktrees(t *testing.T) {
	cityPath := t.TempDir()
	paths := []string{
		filepath.Join(cityPath, ".gc", "worktrees", "qcore", "qc-direct1"),
		filepath.Join(cityPath, ".gc", "worktrees", "qcore", "polecats", "gastown.furiosa", "worktrees", "qc-nested1"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
		Rigs:      []config.Rig{{Name: "qcore", Prefix: "qc"}},
	}

	got := discoverBeadWorktreeCandidates(cityPath, cfg, "qcore")
	gotIDs := make([]string, 0, len(got))
	for _, candidate := range got {
		gotIDs = append(gotIDs, candidate.BeadID)
	}
	sort.Strings(gotIDs)
	want := []string{"qc-direct1", "qc-nested1"}
	if len(gotIDs) != len(want) || gotIDs[0] != want[0] || gotIDs[1] != want[1] {
		t.Fatalf("candidate IDs = %v, want %v", gotIDs, want)
	}
}

func TestDiscoverBeadWorktreeCandidatesRejectsArbitraryNestedAndProtectedHomes(t *testing.T) {
	cityPath := t.TempDir()
	paths := []string{
		filepath.Join(cityPath, ".gc", "worktrees", "qcore", "refinery", "worktrees", "qc-refine1"),
		filepath.Join(cityPath, ".gc", "worktrees", "qcore", "polecats", "gastown.furiosa", "other", "qc-other1"),
		filepath.Join(cityPath, ".gc", "worktrees", "qcore", "polecats", ".gascity-worktree-stage.abc", "worktrees", "qc-stage1"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
		Rigs:      []config.Rig{{Name: "qcore", Prefix: "qc"}},
		Agents:    []config.Agent{{Name: "refinery", Dir: "qcore"}},
	}

	if got := discoverBeadWorktreeCandidates(cityPath, cfg, "qcore"); len(got) != 0 {
		t.Fatalf("candidates = %#v, want none", got)
	}
}

func TestDiscoverBeadWorktreeCandidatesSkipsSymlinkCandidate(t *testing.T) {
	cityPath := t.TempDir()
	realPath := filepath.Join(cityPath, "outside", "qc-link1")
	if err := os.MkdirAll(realPath, 0o755); err != nil {
		t.Fatal(err)
	}
	worktreesDir := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "polecats", "gastown.furiosa", "worktrees")
	if err := os.MkdirAll(worktreesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, filepath.Join(worktreesDir, "qc-link1")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "test", Prefix: "ga"}, Rigs: []config.Rig{{Name: "qcore", Prefix: "qc"}}}

	if got := discoverBeadWorktreeCandidates(cityPath, cfg, "qcore"); len(got) != 0 {
		t.Fatalf("candidates = %#v, want symlink skipped", got)
	}
}

var errWorktreeProbe = errors.New("probe failed")

type fakeBeadWorktreeGit struct {
	isRepo       bool
	dirty        bool
	unpushed     bool
	unpushedErr  error
	stashes      bool
	stashesErr   error
	branch       string
	removeErr    error
	removedPath  string
	removedForce bool
	movedFrom    string
	movedTo      string
	moveErr      error
	worktrees    []git.Worktree
	worktreeErr  error
}

func (f *fakeBeadWorktreeGit) IsRepo() bool             { return f.isRepo }
func (f *fakeBeadWorktreeGit) HasUncommittedWork() bool { return f.dirty }
func (f *fakeBeadWorktreeGit) HasUnpushedCommitsResult() (bool, error) {
	return f.unpushed, f.unpushedErr
}
func (f *fakeBeadWorktreeGit) HasStashesResult() (bool, error) { return f.stashes, f.stashesErr }
func (f *fakeBeadWorktreeGit) CurrentBranch() (string, error)  { return f.branch, nil }
func (f *fakeBeadWorktreeGit) WorktreeRemove(path string, force bool) error {
	f.removedPath, f.removedForce = path, force
	return f.removeErr
}

func (f *fakeBeadWorktreeGit) WorktreeList() ([]git.Worktree, error) {
	return f.worktrees, f.worktreeErr
}

func (f *fakeBeadWorktreeGit) WorktreeMove(oldPath, newPath string) error {
	f.movedFrom, f.movedTo = oldPath, newPath
	if f.moveErr != nil {
		return f.moveErr
	}
	if _, err := os.Stat(oldPath); err == nil {
		if err := os.Rename(oldPath, newPath); err != nil {
			return err
		}
	}
	for i := range f.worktrees {
		if f.worktrees[i].Path == oldPath {
			f.worktrees[i].Path = newPath
		}
	}
	return nil
}

func TestEvaluateBeadWorktreeCandidateFailsClosedOnProbeErrors(t *testing.T) {
	cityPath := t.TempDir()
	candidatePath := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "qc-closed1")
	if err := os.MkdirAll(candidatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "qc-closed1", Status: "closed"}}, nil)
	candidate := beadWorktreeCandidate{Rig: "qcore", BeadID: "qc-closed1", Path: candidatePath}

	tests := []struct {
		name string
		git  *fakeBeadWorktreeGit
	}{
		{name: "unpushed probe", git: &fakeBeadWorktreeGit{isRepo: true, unpushedErr: errWorktreeProbe}},
		{name: "stash probe", git: &fakeBeadWorktreeGit{isRepo: true, stashesErr: errWorktreeProbe}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decision := evaluateBeadWorktreeCandidate(candidate, cityPath, store, runtime.NewFake(), tc.git)
			if decision.Action != beadWorktreeSkip || decision.Reason == "" {
				t.Fatalf("decision = %+v, want fail-closed skip", decision)
			}
		})
	}
}

func TestEvaluateBeadWorktreeCandidateChoosesRemoveOrSkip(t *testing.T) {
	cityPath := t.TempDir()
	candidatePath := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "qc-closed1")
	if err := os.MkdirAll(candidatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := beadWorktreeCandidate{Rig: "qcore", BeadID: "qc-closed1", Path: candidatePath}

	tests := []struct {
		name string
		bead beads.Bead
		git  *fakeBeadWorktreeGit
		want beadWorktreeAction
	}{
		{name: "safe remove", bead: beads.Bead{ID: "qc-closed1", Status: "closed"}, git: &fakeBeadWorktreeGit{isRepo: true}, want: beadWorktreeRemove},
		{name: "dirty skip", bead: beads.Bead{ID: "qc-closed1", Status: "closed"}, git: &fakeBeadWorktreeGit{isRepo: true, dirty: true}, want: beadWorktreeSkip},
		{name: "unpushed skip", bead: beads.Bead{ID: "qc-closed1", Status: "closed"}, git: &fakeBeadWorktreeGit{isRepo: true, unpushed: true}, want: beadWorktreeSkip},
		{name: "stash skip", bead: beads.Bead{ID: "qc-closed1", Status: "closed"}, git: &fakeBeadWorktreeGit{isRepo: true, stashes: true}, want: beadWorktreeSkip},
		{name: "rejected skip", bead: beads.Bead{ID: "qc-closed1", Status: "closed", Metadata: map[string]string{"rejection_reason": "needs revision"}}, git: &fakeBeadWorktreeGit{isRepo: true}, want: beadWorktreeSkip},
		{name: "open skip", bead: beads.Bead{ID: "qc-closed1", Status: "open"}, git: &fakeBeadWorktreeGit{isRepo: true}, want: beadWorktreeSkip},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStoreFrom(1, []beads.Bead{tc.bead}, nil)
			decision := evaluateBeadWorktreeCandidate(candidate, cityPath, store, runtime.NewFake(), tc.git)
			if decision.Action != tc.want {
				t.Fatalf("decision = %+v, want action %q", decision, tc.want)
			}
		})
	}
}

func TestEvaluateBeadWorktreeCandidateSkipsLiveOwner(t *testing.T) {
	cityPath := t.TempDir()
	candidatePath := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "qc-live1")
	if err := os.MkdirAll(candidatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "qc-live1", Status: "closed", Metadata: map[string]string{"gc.session_name": "worker-live", "gc.work_dir": candidatePath}}}, nil)
	sp := runtime.NewFake()
	if err := sp.Start(t.Context(), "worker-live", runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	decision := evaluateBeadWorktreeCandidate(beadWorktreeCandidate{Rig: "qcore", BeadID: "qc-live1", Path: candidatePath}, cityPath, store, sp, &fakeBeadWorktreeGit{isRepo: true})
	if decision.Action != beadWorktreeSkip {
		t.Fatalf("decision = %+v, want live-owner skip", decision)
	}
}

func TestReapClosedBeadWorktreesRemovesSafeNestedWorktreeWithoutForce(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "rigs", "qcore")
	candidatePath := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "polecats", "gastown.furiosa", "worktrees", "qc-safe1")
	if err := os.MkdirAll(candidatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "qc-safe1", Status: "closed"}}, nil)
	cfg := &config.City{Workspace: config.Workspace{Name: "test", Prefix: "ga"}, Rigs: []config.Rig{{Name: "qcore", Prefix: "qc", Path: rigRoot}}}
	candidateGit := &fakeBeadWorktreeGit{isRepo: true, branch: "polecat/qc-safe1"}
	rigGit := &fakeBeadWorktreeGit{isRepo: true, worktrees: []git.Worktree{{Path: candidatePath, Branch: "refs/heads/polecat/qc-safe1"}}}
	if !branchMatchesBead(cfg, "polecat/qc-safe1", "qc-safe1") {
		t.Fatal("expected polecat branch to match bead")
	}
	orig := newBeadWorktreeGitProbe
	defer func() { newBeadWorktreeGitProbe = orig }()
	newBeadWorktreeGitProbe = func(path string) beadWorktreeGitProbe {
		if path == rigRoot {
			return rigGit
		}
		return candidateGit
	}

	var stderr bytes.Buffer
	got := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"qcore": store}, runtime.NewFake(), nil, &stderr, false, true)
	if got != 1 {
		t.Fatalf("reaped = %d, want 1; stderr=%s", got, stderr.String())
	}
	if rigGit.movedFrom != candidatePath || rigGit.removedForce || !strings.HasPrefix(rigGit.removedPath, candidatePath+".gc-reap-") {
		t.Fatalf("move/remove = (%q -> %q, force=%v), want quarantine then non-force removal", rigGit.movedFrom, rigGit.removedPath, rigGit.removedForce)
	}
}

func TestReapClosedBeadWorktreesPreservesDependenciesAfterRemoveFailure(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "rigs", "qcore")
	candidatePath := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "qc-clean1")
	nodeModules := filepath.Join(candidatePath, "node_modules")
	if err := os.MkdirAll(nodeModules, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeModules, "large.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "qc-clean1", Status: "closed"}}, nil)
	cfg := &config.City{Workspace: config.Workspace{Name: "test", Prefix: "ga"}, Rigs: []config.Rig{{Name: "qcore", Prefix: "qc", Path: rigRoot}}}
	candidateGit := &fakeBeadWorktreeGit{isRepo: true, branch: "polecat/qc-clean1"}
	rigGit := &fakeBeadWorktreeGit{isRepo: true, removeErr: errors.New("worktree busy"), worktrees: []git.Worktree{{Path: candidatePath, Branch: "refs/heads/polecat/qc-clean1"}}}
	orig := newBeadWorktreeGitProbe
	defer func() { newBeadWorktreeGitProbe = orig }()
	newBeadWorktreeGitProbe = func(path string) beadWorktreeGitProbe {
		if path == rigRoot {
			return rigGit
		}
		return candidateGit
	}

	if got := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"qcore": store}, runtime.NewFake(), nil, nil, false, true); got != 0 {
		t.Fatalf("reaped = %d, want 0 whole worktrees", got)
	}
	if _, err := os.Stat(candidatePath); err != nil {
		t.Fatalf("candidate removed: %v", err)
	}
	if _, err := os.Stat(nodeModules); err != nil {
		t.Fatalf("node_modules removed after failed worktree removal: %v", err)
	}
}

func TestReapClosedBeadWorktreesDryRunDoesNotMutate(t *testing.T) {
	cityPath := t.TempDir()
	candidatePath := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "qc-dryrun1")
	nodeModules := filepath.Join(candidatePath, "node_modules")
	if err := os.MkdirAll(nodeModules, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "qc-dryrun1", Status: "closed"}}, nil)
	rigRoot := filepath.Join(cityPath, "rigs", "qcore")
	cfg := &config.City{Workspace: config.Workspace{Name: "test", Prefix: "ga"}, Rigs: []config.Rig{{Name: "qcore", Prefix: "qc", Path: rigRoot}}}
	fakeGit := &fakeBeadWorktreeGit{isRepo: true, branch: "polecat/qc-dryrun1", worktrees: []git.Worktree{{Path: candidatePath, Branch: "refs/heads/polecat/qc-dryrun1"}}}
	orig := newBeadWorktreeGitProbe
	defer func() { newBeadWorktreeGitProbe = orig }()
	newBeadWorktreeGitProbe = func(string) beadWorktreeGitProbe { return fakeGit }

	reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"qcore": store}, runtime.NewFake(), nil, nil, true, true)
	if fakeGit.removedPath != "" {
		t.Fatalf("dry-run removed %q", fakeGit.removedPath)
	}
	if _, err := os.Stat(nodeModules); err != nil {
		t.Fatalf("dry-run pruned dependencies: %v", err)
	}
}

func TestDiscoverBeadWorktreeCandidatesSkipsConfiguredBeadLikeHome(t *testing.T) {
	cityPath := t.TempDir()
	home := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "qc-agent1")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
		Rigs:      []config.Rig{{Name: "qcore", Prefix: "qc"}},
		Agents:    []config.Agent{{Name: "qc-agent1", Dir: "qcore"}},
	}
	if got := discoverBeadWorktreeCandidates(cityPath, cfg, "qcore"); len(got) != 0 {
		t.Fatalf("configured home became candidate: %#v", got)
	}
}

func TestEvaluateBeadWorktreeCandidateSkipsWhenLivenessUnavailable(t *testing.T) {
	cityPath := t.TempDir()
	candidatePath := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "qc-live1")
	if err := os.MkdirAll(candidatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "qc-live1", Status: "closed", Metadata: map[string]string{"gc.session_name": "worker-maybe-live"}}}, nil)
	decision := evaluateBeadWorktreeCandidate(beadWorktreeCandidate{Rig: "qcore", BeadID: "qc-live1", Path: candidatePath}, cityPath, store, nil, &fakeBeadWorktreeGit{isRepo: true})
	if decision.Action != beadWorktreeSkip || !strings.Contains(decision.Reason, "liveness unavailable") {
		t.Fatalf("decision = %+v, want unknown-liveness skip", decision)
	}
}

func TestReapClosedBeadWorktreesReevaluatesGitSafetyBeforeRemoval(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "rigs", "qcore")
	candidatePath := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "qc-race1")
	if err := os.MkdirAll(candidatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "qc-race1", Status: "closed"}}, nil)
	cfg := &config.City{Workspace: config.Workspace{Name: "test", Prefix: "ga"}, Rigs: []config.Rig{{Name: "qcore", Prefix: "qc", Path: rigRoot}}}
	rigGit := &fakeBeadWorktreeGit{isRepo: true, worktrees: []git.Worktree{{Path: candidatePath, Branch: "refs/heads/polecat/qc-race1"}}}
	probeCount := 0
	orig := newBeadWorktreeGitProbe
	defer func() { newBeadWorktreeGitProbe = orig }()
	newBeadWorktreeGitProbe = func(path string) beadWorktreeGitProbe {
		if path == rigRoot {
			return rigGit
		}
		probeCount++
		return &fakeBeadWorktreeGit{isRepo: true, branch: "polecat/qc-race1", dirty: probeCount > 1}
	}
	reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"qcore": store}, runtime.NewFake(), nil, nil, false, true)
	if rigGit.removedPath != "" {
		t.Fatalf("removed candidate after safety changed: %q", rigGit.removedPath)
	}
}

func TestReapClosedBeadWorktreesSkipsUnstampedLiveSessionWorktree(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "rigs", "qcore")
	home := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "polecats", "gastown.live")
	candidatePath := filepath.Join(home, "worktrees", "qc-live2")
	if err := os.MkdirAll(candidatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "qc-live2", Status: "closed"}}, nil)
	cfg := &config.City{Workspace: config.Workspace{Name: "test", Prefix: "ga"}, Rigs: []config.Rig{{Name: "qcore", Prefix: "qc", Path: rigRoot}}}
	rigGit := &fakeBeadWorktreeGit{isRepo: true, worktrees: []git.Worktree{{Path: candidatePath, Branch: "refs/heads/polecat/qc-live2"}}}
	candidateGit := &fakeBeadWorktreeGit{isRepo: true, branch: "polecat/qc-live2"}
	orig := newBeadWorktreeGitProbe
	defer func() { newBeadWorktreeGitProbe = orig }()
	newBeadWorktreeGitProbe = func(path string) beadWorktreeGitProbe {
		if path == rigRoot {
			return rigGit
		}
		return candidateGit
	}
	sp := runtime.NewFake()
	if err := sp.Start(t.Context(), "live-session", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	session := beads.Bead{ID: "session-1", Status: "open", Metadata: map[string]string{"session_name": "live-session", "work_dir": home}}
	reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"qcore": store}, sp, nil, nil, false, true, session)
	if rigGit.movedFrom != "" || rigGit.removedPath != "" {
		t.Fatalf("live unstamped worktree was mutated: move=%q remove=%q", rigGit.movedFrom, rigGit.removedPath)
	}
}

func TestReapClosedBeadWorktreesUnavailableSnapshotWouldSkip(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "rigs", "qcore")
	candidatePath := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "qc-unowned1")
	if err := os.MkdirAll(candidatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "qc-unowned1", Status: "closed"}}, nil)
	cfg := &config.City{Workspace: config.Workspace{Name: "test", Prefix: "ga"}, Rigs: []config.Rig{{Name: "qcore", Prefix: "qc", Path: rigRoot}}}
	rigGit := &fakeBeadWorktreeGit{isRepo: true, worktrees: []git.Worktree{{Path: candidatePath, Branch: "refs/heads/polecat/qc-unowned1"}}}
	orig := newBeadWorktreeGitProbe
	defer func() { newBeadWorktreeGitProbe = orig }()
	newBeadWorktreeGitProbe = func(string) beadWorktreeGitProbe { return rigGit }
	var output bytes.Buffer
	reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{"qcore": store}, nil, nil, &output, true, false)
	if !strings.Contains(output.String(), "action=would-skip") || strings.Contains(output.String(), "indeterminate") {
		t.Fatalf("output = %q, want deterministic would-skip", output.String())
	}
	if rigGit.movedFrom != "" || rigGit.removedPath != "" {
		t.Fatalf("unavailable snapshot mutated worktree: move=%q remove=%q", rigGit.movedFrom, rigGit.removedPath)
	}
}
