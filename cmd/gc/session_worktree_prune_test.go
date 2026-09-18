package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeGitProbe is a hand-rolled gitProbe stub. Each field controls one
// probe; WorktreeRemoveErr controls the destructive call, and removed
// records the (path, force) of every WorktreeRemove invocation so tests
// can assert which directory the removal targeted.
type fakeGitProbe struct {
	isRepo           bool
	currentBranch    string
	currentBranchErr error
	hasUncommitted   bool
	porcelain        string
	porcelainErr     error
	hasUnpushed      bool
	unpushedErr      error
	hasStashes       bool
	stashesErr       error
	worktreeRemove   func(path string, force bool) error
	removedPath      string
	removedForce     bool
	removeInvoked    bool
}

func (f *fakeGitProbe) IsRepo() bool { return f.isRepo }
func (f *fakeGitProbe) CurrentBranch() (string, error) {
	return f.currentBranch, f.currentBranchErr
}

// HasUncommittedWork mirrors git: any porcelain line at all, sediment
// included, reads as dirty. It is what the prune used to call.
func (f *fakeGitProbe) HasUncommittedWork() bool {
	return f.hasUncommitted || f.porcelain != "" || f.porcelainErr != nil
}

// StatusPorcelain reports porcelain when set; otherwise hasUncommitted stands
// for one authored modification, so the older tests keep their meaning.
func (f *fakeGitProbe) StatusPorcelain() (string, error) {
	if f.porcelainErr != nil || f.porcelain != "" {
		return f.porcelain, f.porcelainErr
	}
	if f.hasUncommitted {
		return " M main.go\n", nil
	}
	return "", nil
}

func (f *fakeGitProbe) HasUnpushedCommitsResult() (bool, error) {
	return f.hasUnpushed, f.unpushedErr
}
func (f *fakeGitProbe) HasStashesResult() (bool, error) { return f.hasStashes, f.stashesErr }
func (f *fakeGitProbe) WorktreeRemove(path string, force bool) error {
	f.removeInvoked = true
	f.removedPath = path
	f.removedForce = force
	if f.worktreeRemove != nil {
		return f.worktreeRemove(path, force)
	}
	return nil
}

// assertWorktreeStaleMarker fails the test unless workerDir contains a
// .worktree-stale marker recording the given branch and reason. Shared by
// both the raw and session.Info test forms.
func assertWorktreeStaleMarker(t *testing.T, workerDir, wantBranch, wantReason string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workerDir, worktreeStaleFileName))
	if err != nil {
		t.Fatalf("reading %s marker: %v", worktreeStaleFileName, err)
	}
	content := string(data)
	if !strings.Contains(content, "branch="+wantBranch) {
		t.Errorf("marker content = %q, want to contain %q", content, "branch="+wantBranch)
	}
	if !strings.Contains(content, "reason="+wantReason) {
		t.Errorf("marker content = %q, want to contain %q", content, "reason="+wantReason)
	}
}

// assertNoWorktreeStaleMarker fails the test if workerDir contains a
// .worktree-stale marker.
func assertNoWorktreeStaleMarker(t *testing.T, workerDir string) {
	t.Helper()
	if _, err := os.ReadFile(filepath.Join(workerDir, worktreeStaleFileName)); err == nil {
		t.Errorf("%s marker unexpectedly present in %s", worktreeStaleFileName, workerDir)
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking %s marker: %v", worktreeStaleFileName, err)
	}
}

// pruneTestFixture wires a temp city directory, a writable worker_dir
// under .gc/worktrees with a .git marker, and a config with a rig that
// points at a separate "rig root" directory the prune call uses as the
// git -C target.
type pruneTestFixture struct {
	t          *testing.T
	cityPath   string
	rigRoot    string
	workerDir  string
	cfg        *config.City
	probesByWD map[string]*fakeGitProbe
}

func newPruneFixture(t *testing.T) *pruneTestFixture {
	t.Helper()
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "repos", "demo")
	workerDir := filepath.Join(cityPath, ".gc", "worktrees", "demo", "polecats", "polecat-3")

	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("mkdir rigRoot: %v", err)
	}
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatalf("mkdir workerDir: %v", err)
	}
	// .git marker so the existence check passes; contents don't matter
	// because gitProbe is faked.
	if err := os.WriteFile(filepath.Join(workerDir, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git marker: %v", err)
	}

	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "demo", Path: rigRoot},
		},
	}

	fx := &pruneTestFixture{
		t:          t,
		cityPath:   cityPath,
		rigRoot:    rigRoot,
		workerDir:  workerDir,
		cfg:        cfg,
		probesByWD: make(map[string]*fakeGitProbe),
	}

	orig := newGitProbe
	t.Cleanup(func() { newGitProbe = orig })
	newGitProbe = func(workDir string) gitProbe {
		probe, ok := fx.probesByWD[workDir]
		if !ok {
			// Default: a healthy probe. The WorktreeRemove call goes
			// through this default for the rig-root invocation.
			probe = &fakeGitProbe{isRepo: true}
			fx.probesByWD[workDir] = probe
		}
		return probe
	}

	return fx
}

func (fx *pruneTestFixture) setProbe(workDir string, probe *fakeGitProbe) {
	fx.probesByWD[workDir] = probe
}

func (fx *pruneTestFixture) sessionBead() beads.Bead {
	return beads.Bead{
		ID: "session-1",
		Metadata: map[string]string{
			"worker_dir":   fx.workerDir,
			"template":     "demo/polecat",
			"session_name": "demo/polecat-3",
		},
	}
}

func TestPruneAgentHomeWorktreeIfSafe_DisabledByConfig(t *testing.T) {
	fx := newPruneFixture(t)
	off := false
	fx.cfg.Daemon.AutoPruneWorkerDir = &off

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true while disabled")
	}
	if rigProbe := fx.probesByWD[fx.rigRoot]; rigProbe != nil && rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove called while config disabled prune")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_NoWorkerDir(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	delete(session.Metadata, "worker_dir")

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true with no worker_dir")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_LegacyWorkDirKey(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	delete(session.Metadata, "worker_dir")
	session.Metadata["work_dir"] = fx.workerDir

	probe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.workerDir, probe)
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	if !pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatalf("prune returned false on legacy work_dir; stderr=%s", stderr.String())
	}
	if !rigProbe.removeInvoked || rigProbe.removedPath != fx.workerDir || !rigProbe.removedForce {
		t.Fatalf("expected WorktreeRemove(%q, true) on rig root; got invoked=%v path=%q force=%v",
			fx.workerDir, rigProbe.removeInvoked, rigProbe.removedPath, rigProbe.removedForce)
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_OutsideWorktreesTree(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	outside := filepath.Join(fx.cityPath, "elsewhere", "polecat-3")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	session.Metadata["worker_dir"] = outside

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true for path outside .gc/worktrees")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_RejectsWorktreesRoot(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	wtRoot := filepath.Join(fx.cityPath, ".gc", "worktrees")
	if err := os.WriteFile(filepath.Join(wtRoot, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git on wtRoot: %v", err)
	}
	session.Metadata["worker_dir"] = wtRoot

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true for .gc/worktrees root itself")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_RelativeWorkerDir(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	session.Metadata["worker_dir"] = "relative/path"

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true for relative worker_dir")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_MissingDotGit(t *testing.T) {
	fx := newPruneFixture(t)
	if err := os.Remove(filepath.Join(fx.workerDir, ".git")); err != nil {
		t.Fatalf("remove .git: %v", err)
	}

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true with missing .git pointer")
	}
}

func TestPruneAgentHomeWorktreeIfSafe_NotARepo(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: false})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true when IsRepo=false")
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_HasUncommitted(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasUncommitted: true, currentBranch: "builder/ga-abc123"})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true with uncommitted work")
	}
	if !strings.Contains(stderr.String(), "uncommitted changes") {
		t.Errorf("expected uncommitted-reason log; got %q", stderr.String())
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "builder/ga-abc123", "uncommitted-work")
}

func TestPruneAgentHomeWorktreeIfSafe_HasUnpushed(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, hasUnpushed: true, currentBranch: "builder/ga-def456"})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true with unpushed commits")
	}
	if !strings.Contains(stderr.String(), "unpushed commits") {
		t.Errorf("expected unpushed-reason log; got %q", stderr.String())
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "builder/ga-def456", "unpushed-commits")
}

func TestPruneAgentHomeWorktreeIfSafe_UnpushedProbeError(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, unpushedErr: errors.New("boom")})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true after unpushed probe error")
	}
	if !strings.Contains(stderr.String(), "unpushed probe failed") {
		t.Errorf("expected unpushed-error log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

// A stash in the repo is not a veto (ga-bjenxa): `git stash list` is repo-global,
// so it blocked every worktree of the repo, and WorktreeRemove never touches
// refs/stash. Neither a stash nor a failing stash probe may stop the prune.
func TestPruneAgentHomeWorktreeIfSafe_RepoStashDoesNotVeto(t *testing.T) {
	for _, probe := range []*fakeGitProbe{
		{isRepo: true, hasStashes: true, currentBranch: "builder/ga-ghi789"},
		{isRepo: true, stashesErr: errors.New("boom")},
	} {
		fx := newPruneFixture(t)
		fx.setProbe(fx.workerDir, probe)
		rigProbe := &fakeGitProbe{isRepo: true}
		fx.setProbe(fx.rigRoot, rigProbe)

		var stderr bytes.Buffer
		if !pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
			t.Fatalf("prune returned false with a repo stash; stderr=%s", stderr.String())
		}
		if !rigProbe.removeInvoked || rigProbe.removedPath != fx.workerDir {
			t.Fatalf("expected WorktreeRemove(%q); got invoked=%v path=%q", fx.workerDir, rigProbe.removeInvoked, rigProbe.removedPath)
		}
		if strings.Contains(stderr.String(), "stash") {
			t.Errorf("prune still consulted stashes; stderr=%q", stderr.String())
		}
		assertNoWorktreeStaleMarker(t, fx.workerDir)
	}
}

func TestPruneAgentHomeWorktreeIfSafe_RigPathUnresolved(t *testing.T) {
	fx := newPruneFixture(t)
	fx.cfg.Rigs = nil
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true without rig path")
	}
	if !strings.Contains(stderr.String(), "rig path unresolved") {
		t.Errorf("expected rig-unresolved log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_RigPathEmpty(t *testing.T) {
	fx := newPruneFixture(t)
	fx.cfg.Rigs = []config.Rig{{Name: "demo", Path: ""}}
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true with empty rig path")
	}
	if !strings.Contains(stderr.String(), "rig path unresolved") {
		t.Errorf("expected rig-unresolved log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_RemoveFails(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true})
	fx.setProbe(fx.rigRoot, &fakeGitProbe{
		isRepo:         true,
		worktreeRemove: func(_ string, _ bool) error { return errors.New("locked") },
	})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune returned true when WorktreeRemove failed")
	}
	if !strings.Contains(stderr.String(), "pruning worker_dir") || !strings.Contains(stderr.String(), "locked") {
		t.Errorf("expected removal-error log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_HappyPath(t *testing.T) {
	fx := newPruneFixture(t)
	wdProbe := &fakeGitProbe{isRepo: true}
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.workerDir, wdProbe)
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	if !pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatalf("prune returned false on happy path; stderr=%s", stderr.String())
	}
	if wdProbe.removeInvoked {
		t.Error("WorktreeRemove invoked on worker_dir; should be invoked on rig root only")
	}
	if !rigProbe.removeInvoked {
		t.Fatal("WorktreeRemove not invoked on rig root")
	}
	if rigProbe.removedPath != fx.workerDir {
		t.Errorf("WorktreeRemove path = %q, want %q", rigProbe.removedPath, fx.workerDir)
	}
	if !rigProbe.removedForce {
		t.Error("WorktreeRemove force flag = false, want true")
	}
	if !strings.Contains(stderr.String(), "pruned worker_dir") {
		t.Errorf("expected success log; got %q", stderr.String())
	}
	assertNoWorktreeStaleMarker(t, fx.workerDir)
}

func TestPruneAgentHomeWorktreeIfSafe_NilConfig(t *testing.T) {
	fx := newPruneFixture(t)
	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, nil, nil, &stderr) {
		t.Fatal("prune returned true with nil cfg")
	}
}

func TestLookupRigRootForSession(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "demo", Path: "/x/demo"},
			{Name: "other", Path: "/x/other"},
		},
	}
	cases := []struct {
		name     string
		template string
		want     string
	}{
		{"qualified match", "demo/polecat", "/x/demo"},
		{"other rig", "other/refinery", "/x/other"},
		{"unqualified", "polecat", ""},
		{"unknown rig", "missing/polecat", ""},
		{"empty", "", ""},
		{"leading slash", "/polecat", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bead := beads.Bead{Metadata: map[string]string{"template": c.template}}
			got := lookupRigRootForSession(bead, cfg)
			if got != c.want {
				t.Errorf("lookupRigRootForSession(%q) = %q, want %q", c.template, got, c.want)
			}
		})
	}
}

func TestPruneAgentHomeWorktreeIfSafe_NamedSessionNeverPruned(t *testing.T) {
	// ga-1xiv: a named session's home must never be auto-pruned, even when
	// it transiently carries pool metadata and classifies pool-freeable.
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	session.Metadata["configured_named_session"] = "true"
	session.Metadata["pool_managed"] = "true"

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("pruned a named session's worker_dir")
	}
	if probe, ok := fx.probesByWD[fx.rigRoot]; ok && probe.removeInvoked {
		t.Fatal("WorktreeRemove invoked for a named session home")
	}
	if !strings.Contains(stderr.String(), "named session home is never auto-pruned") {
		t.Errorf("missing named-session refusal diagnostic, got: %q", stderr.String())
	}
}

func TestPruneAgentHomeWorktreeIfSafe_LiveRuntimeNeverPruned(t *testing.T) {
	// ga-1xiv: a direct liveness probe must veto the prune even when the
	// caller's cached liveness said the session was gone.
	fx := newPruneFixture(t)
	session := fx.sessionBead()

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), session.Metadata["session_name"], runtime.Config{}); err != nil {
		t.Fatalf("fake start: %v", err)
	}

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, sp, &stderr) {
		t.Fatal("pruned worker_dir of a live runtime session")
	}
	if probe, ok := fx.probesByWD[fx.rigRoot]; ok && probe.removeInvoked {
		t.Fatal("WorktreeRemove invoked while runtime session live")
	}
	if !strings.Contains(stderr.String(), "runtime session is live") {
		t.Errorf("missing live-runtime refusal diagnostic, got: %q", stderr.String())
	}

	// Once the runtime is gone, the same bead becomes prunable again.
	if err := sp.Stop(session.Metadata["session_name"]); err != nil {
		t.Fatalf("fake stop: %v", err)
	}
	if !pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, sp, &stderr) {
		t.Fatalf("expected prune after runtime stopped; stderr: %q", stderr.String())
	}
}

// TestPruneAgentHomeWorktreeIfSafe_UnknownRuntimeLivenessNeverPruned holds the
// ga-1xiv re-probe to #5544's contract: an observation that could not reach the
// runtime is not evidence the session is gone, so the prune waits for a tick
// that can see it.
func TestPruneAgentHomeWorktreeIfSafe_UnknownRuntimeLivenessNeverPruned(t *testing.T) {
	fx := newPruneFixture(t)
	session := fx.sessionBead()
	sp := &sweepUnavailableLivenessProvider{Fake: runtime.NewFake()}

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(session, fx.cityPath, fx.cfg, sp, &stderr) {
		t.Fatal("pruned worker_dir while runtime liveness was unobservable")
	}
	if probe, ok := fx.probesByWD[fx.rigRoot]; ok && probe.removeInvoked {
		t.Fatal("WorktreeRemove invoked while runtime liveness was unobservable")
	}
	if !strings.Contains(stderr.String(), "runtime liveness unknown") {
		t.Errorf("missing unknown-liveness refusal diagnostic, got: %q", stderr.String())
	}
}

// ga-bjenxa: gc's own provisioning sediment, and the .worktree-stale marker
// this path writes when it skips a tree, are not authored work. Before, a
// worker_dir skipped once read dirty forever on its own marker.
const pruneSedimentOnly = "?? .worktree-stale\n?? .claude/\n?? .omp/\n M .beads/config.yaml\n?? AGENTS-gc.md\n"

func TestPruneAgentHomeWorktreeIfSafe_SedimentOnlyDoesNotVeto(t *testing.T) {
	fx := newPruneFixture(t)
	rigProbe := &fakeGitProbe{isRepo: true}
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, porcelain: pruneSedimentOnly})
	fx.setProbe(fx.rigRoot, rigProbe)

	var stderr bytes.Buffer
	if !pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatalf("prune refused a worker_dir whose only changes are gc sediment; stderr=%s", stderr.String())
	}
	if rigProbe.removedPath != fx.workerDir {
		t.Errorf("WorktreeRemove path = %q, want %q", rigProbe.removedPath, fx.workerDir)
	}
}

func TestPruneAgentHomeWorktreeIfSafe_AuthoredChangeAmidSedimentVetoes(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, porcelain: pruneSedimentOnly + " M internal/work.go\n", currentBranch: "builder/ga-abc123"})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune removed a worker_dir with an authored change")
	}
	assertWorktreeStaleMarker(t, fx.workerDir, "builder/ga-abc123", "uncommitted-work")
}

func TestPruneAgentHomeWorktreeIfSafe_StatusProbeErrorVetoes(t *testing.T) {
	fx := newPruneFixture(t)
	fx.setProbe(fx.workerDir, &fakeGitProbe{isRepo: true, porcelainErr: errors.New("git status failed")})

	var stderr bytes.Buffer
	if pruneAgentHomeWorktreeIfSafe(fx.sessionBead(), fx.cityPath, fx.cfg, nil, &stderr) {
		t.Fatal("prune removed a worker_dir whose status probe failed")
	}
}
