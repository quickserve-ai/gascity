package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type fakeStoppedAgentHomeGit struct {
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
	onMove       func(oldPath, newPath string)
}

func (f *fakeStoppedAgentHomeGit) IsRepo() bool             { return f.isRepo }
func (f *fakeStoppedAgentHomeGit) HasUncommittedWork() bool { return f.dirty }
func (f *fakeStoppedAgentHomeGit) HasUnpushedCommitsResult() (bool, error) {
	return f.unpushed, f.unpushedErr
}
func (f *fakeStoppedAgentHomeGit) HasStashesResult() (bool, error) { return f.stashes, f.stashesErr }
func (f *fakeStoppedAgentHomeGit) CurrentBranch() (string, error)  { return f.branch, nil }
func (f *fakeStoppedAgentHomeGit) WorktreeRemove(path string, force bool) error {
	f.removedPath, f.removedForce = path, force
	return f.removeErr
}

func (f *fakeStoppedAgentHomeGit) WorktreeList() ([]git.Worktree, error) {
	return f.worktrees, f.worktreeErr
}

func (f *fakeStoppedAgentHomeGit) WorktreeMove(oldPath, newPath string) error {
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
	if f.onMove != nil {
		f.onMove(oldPath, newPath)
	}
	return nil
}

func namedHomeReaperFixture(t *testing.T) (string, *config.City, string, beads.Bead) {
	t.Helper()
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "rig")
	home := filepath.Join(cityPath, ".gc", "worktrees", "qcore", "refinery")
	if err := os.MkdirAll(filepath.Join(home, "payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	home = canonicalHome
	if err := os.WriteFile(filepath.Join(home, ".git"), []byte("gitdir: fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
		Rigs:      []config.Rig{{Name: "qcore", Path: rigRoot}},
		Agents:    []config.Agent{{Name: "worker", Dir: "qcore", WorkDir: home}},
		NamedSessions: []config.NamedSession{{
			Name: "refinery", Template: "worker", Dir: "qcore", Mode: "on_demand",
		}},
	}
	session := beads.Bead{ID: "ga-session", Type: sessionBeadType, Status: "closed", Metadata: map[string]string{
		"session_name": "qcore--refinery", "alias": "qcore/refinery", "template": "qcore/worker",
		"work_dir": home, namedSessionMetadataKey: "true", namedSessionIdentityMetadata: "qcore/refinery",
	}}
	return cityPath, cfg, home, session
}

func namedHomeRigStores() map[string]beads.Store {
	return map[string]beads.Store{"qcore": beads.NewMemStore()}
}

func TestDiscoverStoppedAgentHomeCandidatesUsesClosedSessionWorkDir(t *testing.T) {
	cityPath, cfg, home, closed := namedHomeReaperFixture(t)
	open := closed
	open.ID = "ga-open"
	open.Status = "open"
	outside := closed
	outside.ID = "ga-outside"
	outside.Metadata = map[string]string{"work_dir": filepath.Join(cityPath, ".gc", "agents", "qcore", "lana"), "configured_named_session": "true"}

	got := discoverStoppedAgentHomeCandidates(cityPath, cfg, []beads.Bead{open, outside, closed})
	if len(got) != 1 {
		t.Fatalf("candidates = %#v, want one closed contained home", got)
	}
	if !sameCanonicalPath(got[0].Path, home) || got[0].Session.ID != closed.ID {
		t.Fatalf("candidate path=%q session=%q, want path=%q session=%q", got[0].Path, got[0].Session.ID, home, closed.ID)
	}
}

func TestDiscoverStoppedAgentHomeCandidatesPreservesSharedHistory(t *testing.T) {
	cityPath, cfg, home, first := namedHomeReaperFixture(t)
	second := first
	second.ID = "ga-session-2"
	got := discoverStoppedAgentHomeCandidates(cityPath, cfg, []beads.Bead{first, second})
	if len(got) != 1 || len(got[0].Owners) != 2 {
		t.Fatalf("candidates = %#v, want one candidate with both historical owners", got)
	}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-work", Status: "in_progress", Assignee: second.ID}}, nil)
	probe := &fakeStoppedAgentHomeGit{isRepo: true, worktrees: []git.Worktree{{Path: home}}}
	decision := evaluateStoppedAgentHomeCandidate(cityPath, cfg, got[0], nil, store, namedHomeRigStores(), nil, probe, probe.worktrees)
	if decision.Action != stoppedAgentHomeSkip || !strings.Contains(decision.Reason, "assigned work") {
		t.Fatalf("shared-history assignment decision = %#v, want assigned-work skip", decision)
	}
}

func TestReapStoppedAgentHomesFailsClosedWhenRuntimeListFails(t *testing.T) {
	cityPath, cfg, _, session := namedHomeReaperFixture(t)
	var out bytes.Buffer
	removed := reapStoppedAgentHomeWorktrees(cityPath, cfg, beads.NewMemStore(), nil, runtime.NewFailFake(), nil, &out, false, []beads.Bead{session})
	if removed != 0 || !strings.Contains(out.String(), "runtime snapshot unavailable") {
		t.Fatalf("removed=%d output=%q, want fail-closed runtime snapshot skip", removed, out.String())
	}
}

func TestEvaluateStoppedAgentHomeCandidateFailsClosed(t *testing.T) {
	cityPath, cfg, home, session := namedHomeReaperFixture(t)
	candidate := stoppedAgentHomeCandidate{Rig: "qcore", Path: home, Session: session, Owners: []beads.Bead{session}}
	healthy := func() *fakeStoppedAgentHomeGit {
		return &fakeStoppedAgentHomeGit{isRepo: true, branch: "refinery/old", worktrees: []git.Worktree{{Path: home, Branch: "refs/heads/refinery/old"}}}
	}
	cityStore := beads.NewMemStore()

	tests := []struct {
		name      string
		candidate stoppedAgentHomeCandidate
		git       *fakeStoppedAgentHomeGit
		running   map[string]bool
		active    []beads.Bead
		store     beads.Store
		want      stoppedAgentHomeAction
		contains  string
	}{
		{name: "safe", candidate: candidate, git: healthy(), store: cityStore, want: stoppedAgentHomeRemove},
		{name: "dirty", candidate: candidate, git: &fakeStoppedAgentHomeGit{isRepo: true, dirty: true, worktrees: []git.Worktree{{Path: home}}}, store: cityStore, want: stoppedAgentHomeSkip, contains: "uncommitted"},
		{name: "unpushed", candidate: candidate, git: &fakeStoppedAgentHomeGit{isRepo: true, unpushed: true, worktrees: []git.Worktree{{Path: home}}}, store: cityStore, want: stoppedAgentHomeSkip, contains: "unpushed"},
		{name: "stash", candidate: candidate, git: &fakeStoppedAgentHomeGit{isRepo: true, stashes: true, worktrees: []git.Worktree{{Path: home}}}, store: cityStore, want: stoppedAgentHomeSkip, contains: "stashed"},
		{name: "probe error", candidate: candidate, git: &fakeStoppedAgentHomeGit{isRepo: true, unpushedErr: errors.New("boom"), worktrees: []git.Worktree{{Path: home}}}, store: cityStore, want: stoppedAgentHomeSkip, contains: "probe failed"},
		{name: "runtime live", candidate: candidate, git: healthy(), running: map[string]bool{"qcore--refinery": true}, store: cityStore, want: stoppedAgentHomeSkip, contains: "runtime session is live"},
		{name: "active path", candidate: candidate, git: healthy(), store: cityStore, active: []beads.Bead{{ID: "ga-live", Status: "open", Metadata: map[string]string{"work_dir": home}}}, want: stoppedAgentHomeSkip, contains: "active session"},
		{name: "assignment store unavailable", candidate: candidate, git: healthy(), store: nil, want: stoppedAgentHomeSkip, contains: "assignment"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateStoppedAgentHomeCandidate(cityPath, cfg, tt.candidate, tt.running, tt.store, namedHomeRigStores(), tt.active, tt.git, tt.git.worktrees)
			if got.Action != tt.want {
				t.Fatalf("action = %q reason=%q, want %q", got.Action, got.Reason, tt.want)
			}
			if tt.contains != "" && !strings.Contains(got.Reason, tt.contains) {
				t.Fatalf("reason = %q, want substring %q", got.Reason, tt.contains)
			}
		})
	}
}

func TestEvaluateStoppedAgentHomeCandidateSkipsLiveAssignedAndNested(t *testing.T) {
	cityPath, cfg, home, session := namedHomeReaperFixture(t)
	candidate := stoppedAgentHomeCandidate{Rig: "qcore", Path: home, Session: session, Owners: []beads.Bead{session}}
	nested := filepath.Join(home, "worktrees", "ga-child")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	cityStore := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-work", Status: "in_progress", Assignee: "qcore/refinery"}}, nil)
	probe := &fakeStoppedAgentHomeGit{isRepo: true, worktrees: []git.Worktree{{Path: home}, {Path: nested}}}

	got := evaluateStoppedAgentHomeCandidate(cityPath, cfg, candidate, nil, cityStore, namedHomeRigStores(), nil, probe, probe.worktrees)
	if got.Action != stoppedAgentHomeSkip || !strings.Contains(got.Reason, "assigned work") {
		t.Fatalf("assigned-work action=%q reason=%q", got.Action, got.Reason)
	}

	cityStore = beads.NewMemStore()
	got = evaluateStoppedAgentHomeCandidate(cityPath, cfg, candidate, nil, cityStore, namedHomeRigStores(), nil, probe, probe.worktrees)
	if got.Action != stoppedAgentHomeSkip || !strings.Contains(got.Reason, "nested registered worktree") {
		t.Fatalf("nested-worktree decision = %#v", got)
	}
}

func TestReapStoppedAgentHomesDryRunReportsSizeAndNeverMutates(t *testing.T) {
	cityPath, cfg, home, session := namedHomeReaperFixture(t)
	cityStore := beads.NewMemStoreFrom(1, []beads.Bead{session}, nil)
	rigProbe := &fakeStoppedAgentHomeGit{isRepo: true, worktrees: []git.Worktree{{Path: home, Branch: "refs/heads/refinery/old"}}}
	homeProbe := &fakeStoppedAgentHomeGit{isRepo: true, branch: "refinery/old"}
	oldFactory := newStoppedAgentHomeGitProbe
	newStoppedAgentHomeGitProbe = func(path string) stoppedAgentHomeGitProbe {
		if path == filepath.Join(cityPath, "rig") {
			return rigProbe
		}
		return homeProbe
	}
	t.Cleanup(func() { newStoppedAgentHomeGitProbe = oldFactory })

	var out bytes.Buffer
	removed := reapStoppedAgentHomeWorktrees(cityPath, cfg, cityStore, namedHomeRigStores(), runtime.NewFake(), nil, &out, true, []beads.Bead{session})
	if removed != 0 || rigProbe.removedPath != "" {
		t.Fatalf("dry run mutated: removed=%d path=%q", removed, rigProbe.removedPath)
	}
	if text := out.String(); !strings.Contains(text, "action=would-remove") || !strings.Contains(text, "size_bytes=") || !strings.Contains(text, home) {
		t.Fatalf("dry-run output = %q", text)
	}
}

func TestReapStoppedAgentHomesRemovesOnlyThroughNonForceGit(t *testing.T) {
	cityPath, cfg, home, session := namedHomeReaperFixture(t)
	rigProbe := &fakeStoppedAgentHomeGit{isRepo: true, worktrees: []git.Worktree{{Path: home, Branch: "refs/heads/refinery/old"}}}
	homeProbe := &fakeStoppedAgentHomeGit{isRepo: true, branch: "refinery/old"}
	oldFactory := newStoppedAgentHomeGitProbe
	newStoppedAgentHomeGitProbe = func(path string) stoppedAgentHomeGitProbe {
		if path == filepath.Join(cityPath, "rig") {
			return rigProbe
		}
		return homeProbe
	}
	t.Cleanup(func() { newStoppedAgentHomeGitProbe = oldFactory })

	cityStore := beads.NewMemStoreFrom(1, []beads.Bead{session}, nil)
	var out bytes.Buffer
	removed := reapStoppedAgentHomeWorktrees(cityPath, cfg, cityStore, namedHomeRigStores(), runtime.NewFake(), nil, &out, false, []beads.Bead{session})
	if removed != 1 || !strings.HasPrefix(rigProbe.removedPath, home+".gc-reap-") || rigProbe.removedForce {
		t.Fatalf("removed=%d path=%q force=%v output=%q, want one quarantined non-force removal of %q", removed, rigProbe.removedPath, rigProbe.removedForce, out.String(), home)
	}
}

func TestReapStoppedAgentHomesRefreshesSessionOwnershipBeforeMutation(t *testing.T) {
	cityPath, cfg, home, closed := namedHomeReaperFixture(t)
	opened := closed
	opened.ID = "ga-new-owner"
	opened.Status = "open"
	opened.Metadata = map[string]string{"session_name": "qcore--new-refinery", "alias": "qcore/refinery", "work_dir": home, "template": "qcore/worker", namedSessionMetadataKey: "true", namedSessionIdentityMetadata: "qcore/refinery"}
	cityStore := beads.NewMemStoreFrom(1, []beads.Bead{closed, opened}, nil)
	rigProbe := &fakeStoppedAgentHomeGit{isRepo: true, worktrees: []git.Worktree{{Path: home, Branch: "refs/heads/refinery/old"}}}
	homeProbe := &fakeStoppedAgentHomeGit{isRepo: true, branch: "refinery/old"}
	oldFactory := newStoppedAgentHomeGitProbe
	newStoppedAgentHomeGitProbe = func(path string) stoppedAgentHomeGitProbe {
		if path == filepath.Join(cityPath, "rig") {
			return rigProbe
		}
		return homeProbe
	}
	t.Cleanup(func() { newStoppedAgentHomeGitProbe = oldFactory })

	var out bytes.Buffer
	removed := reapStoppedAgentHomeWorktrees(cityPath, cfg, cityStore, namedHomeRigStores(), runtime.NewFake(), nil, &out, false, []beads.Bead{closed})
	if removed != 0 || rigProbe.removedPath != "" || !strings.Contains(out.String(), "overlaps active session") {
		t.Fatalf("removed=%d path=%q output=%q, want fresh active-session skip", removed, rigProbe.removedPath, out.String())
	}
}

func TestDiscoverStoppedAgentHomeCandidatesRequiresCurrentNamedSpec(t *testing.T) {
	cityPath, cfg, _, closed := namedHomeReaperFixture(t)
	cfg.NamedSessions = nil
	if got := discoverStoppedAgentHomeCandidates(cityPath, cfg, []beads.Bead{closed}); len(got) != 0 {
		t.Fatalf("stale named-session metadata produced candidates: %#v", got)
	}
}

func TestDiscoverStoppedAgentHomeCandidatesRejectsConfiguredWorkDirDrift(t *testing.T) {
	cityPath, cfg, _, closed := namedHomeReaperFixture(t)
	cfg.Agents[0].WorkDir = filepath.Join(cityPath, ".gc", "worktrees", "qcore", "replacement", "refinery")
	if got := discoverStoppedAgentHomeCandidates(cityPath, cfg, []beads.Bead{closed}); len(got) != 0 {
		t.Fatalf("stale configured work_dir produced candidates: %#v", got)
	}
}

func TestSessionUsesDisposableConfiguredHomeAcceptsBindingQualifiedNamepoolAlias(t *testing.T) {
	cityPath := t.TempDir()
	home := filepath.Join(cityPath, ".gc", "worktrees", "frontend", "polecats", "ops.furiosa")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Agents: []config.Agent{{
		Name: "worker", Dir: "frontend", BindingName: "ops", NamepoolNames: []string{"furiosa", "nux"}, WorkDir: home,
	}}}
	session := beads.Bead{Metadata: map[string]string{
		"template": "frontend/ops.worker", "alias": "frontend/ops.furiosa", "pool_slot": "1", "work_dir": home,
	}}
	if !sessionUsesDisposableConfiguredHome(cityPath, cfg, session) {
		t.Fatal("binding-qualified namepool alias was not recognized as configured")
	}
}

func TestDiscoverStoppedAgentHomeCandidatesFindsConfiguredNamepoolLayout(t *testing.T) {
	cityPath := t.TempDir()
	home := filepath.Join(cityPath, ".gc", "worktrees", "frontend", "polecats", "ops.furiosa")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Prefix: "ga"},
		Rigs:      []config.Rig{{Name: "frontend", Path: filepath.Join(cityPath, "frontend-rig")}},
		Agents:    []config.Agent{{Name: "worker", Dir: "frontend", BindingName: "ops", NamepoolNames: []string{"furiosa"}, WorkDir: home}},
	}
	session := beads.Bead{ID: "ga-namepool", Type: sessionBeadType, Status: "closed", Metadata: map[string]string{
		"template": "frontend/ops.worker", "session_name": "ops-furiosa-session", "alias": "frontend/ops.furiosa", "pool_slot": "1", "work_dir": home,
	}}
	got := discoverStoppedAgentHomeCandidates(cityPath, cfg, []beads.Bead{session})
	if len(got) != 1 || filepath.Base(got[0].Path) != "ops.furiosa" {
		t.Fatalf("candidates = %#v, want configured namepool home", got)
	}

	session.Metadata["work_dir"] = filepath.Join(cityPath, ".gc", "worktrees", "frontend", "unrelated")
	if err := os.MkdirAll(session.Metadata["work_dir"], 0o755); err != nil {
		t.Fatal(err)
	}
	if got := discoverStoppedAgentHomeCandidates(cityPath, cfg, []beads.Bead{session}); len(got) != 0 {
		t.Fatalf("mismatched namepool path produced candidates: %#v", got)
	}
}

func TestEvaluateStoppedAgentHomeCandidateRejectsOverlappingActiveSessionPaths(t *testing.T) {
	cityPath, cfg, home, session := namedHomeReaperFixture(t)
	candidate := stoppedAgentHomeCandidate{Rig: "qcore", Path: home, Session: session, Owners: []beads.Bead{session}}
	probe := &fakeStoppedAgentHomeGit{isRepo: true, worktrees: []git.Worktree{{Path: home}}}
	for _, activePath := range []string{filepath.Join(home, "nested"), filepath.Dir(home)} {
		if err := os.MkdirAll(activePath, 0o755); err != nil {
			t.Fatal(err)
		}
		active := []beads.Bead{{ID: "ga-live", Status: "open", Metadata: map[string]string{"work_dir": activePath}}}
		got := evaluateStoppedAgentHomeCandidate(cityPath, cfg, candidate, nil, beads.NewMemStore(), map[string]beads.Store{"qcore": beads.NewMemStore()}, active, probe, probe.worktrees)
		if got.Action != stoppedAgentHomeSkip || !strings.Contains(got.Reason, "overlaps active session") {
			t.Fatalf("active path %q decision = %#v, want overlap skip", activePath, got)
		}
	}
}

func TestEvaluateStoppedAgentHomeCandidateRequiresCompleteRigStoreSnapshot(t *testing.T) {
	cityPath, cfg, home, session := namedHomeReaperFixture(t)
	cfg.Rigs = append(cfg.Rigs, config.Rig{Name: "other", Path: filepath.Join(cityPath, "other")})
	candidate := stoppedAgentHomeCandidate{Rig: "qcore", Path: home, Session: session, Owners: []beads.Bead{session}}
	probe := &fakeStoppedAgentHomeGit{isRepo: true, worktrees: []git.Worktree{{Path: home}}}
	got := evaluateStoppedAgentHomeCandidate(cityPath, cfg, candidate, nil, beads.NewMemStore(), map[string]beads.Store{"qcore": beads.NewMemStore()}, nil, probe, probe.worktrees)
	if got.Action != stoppedAgentHomeSkip || !strings.Contains(got.Reason, "rig store snapshot incomplete") {
		t.Fatalf("decision = %#v, want incomplete-store skip", got)
	}
}

func TestEvaluateStoppedAgentHomeCandidateRejectsMissingOwnerHistory(t *testing.T) {
	cityPath, cfg, home, session := namedHomeReaperFixture(t)
	candidate := stoppedAgentHomeCandidate{Rig: "qcore", Path: home, Session: session}
	probe := &fakeStoppedAgentHomeGit{isRepo: true, worktrees: []git.Worktree{{Path: home}}}
	got := evaluateStoppedAgentHomeCandidate(cityPath, cfg, candidate, nil, beads.NewMemStore(), namedHomeRigStores(), nil, probe, probe.worktrees)
	if got.Action != stoppedAgentHomeSkip || !strings.Contains(got.Reason, "owner history unavailable") {
		t.Fatalf("decision = %#v, want missing-owner skip", got)
	}
}

func TestReapStoppedAgentHomesQuarantinesBeforeNonForceRemoval(t *testing.T) {
	cityPath, cfg, home, session := namedHomeReaperFixture(t)
	rigProbe := &fakeStoppedAgentHomeGit{isRepo: true, worktrees: []git.Worktree{{Path: home, Branch: "refs/heads/refinery/old"}}}
	homeProbe := &fakeStoppedAgentHomeGit{isRepo: true, branch: "refinery/old"}
	oldFactory := newStoppedAgentHomeGitProbe
	newStoppedAgentHomeGitProbe = func(path string) stoppedAgentHomeGitProbe {
		if path == filepath.Join(cityPath, "rig") {
			return rigProbe
		}
		return homeProbe
	}
	t.Cleanup(func() { newStoppedAgentHomeGitProbe = oldFactory })

	stores := map[string]beads.Store{"qcore": beads.NewMemStore()}
	var out bytes.Buffer
	removed := reapStoppedAgentHomeWorktrees(cityPath, cfg, beads.NewMemStoreFrom(1, []beads.Bead{session}, nil), stores, runtime.NewFake(), nil, &out, false, []beads.Bead{session})
	if removed != 1 || rigProbe.movedFrom != home || !strings.HasPrefix(rigProbe.removedPath, home+".gc-reap-") || rigProbe.removedForce {
		t.Fatalf("removed=%d move=%q remove=%q force=%v output=%q, want quarantine then non-force removal", removed, rigProbe.movedFrom, rigProbe.removedPath, rigProbe.removedForce, out.String())
	}
}

func TestReapStoppedAgentHomesDryRunReportsAllGateResults(t *testing.T) {
	cityPath, cfg, home, session := namedHomeReaperFixture(t)
	rigProbe := &fakeStoppedAgentHomeGit{isRepo: true, worktrees: []git.Worktree{{Path: home, Branch: "refs/heads/refinery/old"}}}
	homeProbe := &fakeStoppedAgentHomeGit{isRepo: true, branch: "refinery/old"}
	oldFactory := newStoppedAgentHomeGitProbe
	newStoppedAgentHomeGitProbe = func(path string) stoppedAgentHomeGitProbe {
		if path == filepath.Join(cityPath, "rig") {
			return rigProbe
		}
		return homeProbe
	}
	t.Cleanup(func() { newStoppedAgentHomeGitProbe = oldFactory })
	var out bytes.Buffer
	stores := map[string]beads.Store{"qcore": beads.NewMemStore()}
	reapStoppedAgentHomeWorktrees(cityPath, cfg, beads.NewMemStoreFrom(1, []beads.Bead{session}, nil), stores, runtime.NewFake(), nil, &out, true, []beads.Bead{session})
	for _, gate := range []string{"runtime=pass", "ownership=pass", "assignments=pass", "containment=pass", "registration=pass", "nested=pass", "dirty=pass", "unpushed=pass", "stash=pass"} {
		if !strings.Contains(out.String(), gate) {
			t.Fatalf("dry-run output %q missing gate %q", out.String(), gate)
		}
	}
}

func TestReapStoppedAgentHomesRestoresWhenOwnerAppearsAfterQuarantine(t *testing.T) {
	cityPath, cfg, home, session := namedHomeReaperFixture(t)
	cityStore := beads.NewMemStoreFrom(1, []beads.Bead{session}, nil)
	rigProbe := &fakeStoppedAgentHomeGit{isRepo: true, worktrees: []git.Worktree{{Path: home, Branch: "refs/heads/refinery/old"}}}
	homeProbe := &fakeStoppedAgentHomeGit{isRepo: true, branch: "refinery/old"}
	created := false
	rigProbe.onMove = func(oldPath, _ string) {
		if created || oldPath != home {
			return
		}
		created = true
		_, err := cityStore.Create(beads.Bead{ID: "ga-new-owner", Type: sessionBeadType, Status: "open", Metadata: map[string]string{
			"session_name": "qcore--new-refinery", "alias": "qcore/refinery", "work_dir": home, "template": "qcore/worker", namedSessionMetadataKey: "true", namedSessionIdentityMetadata: "qcore/refinery",
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	oldFactory := newStoppedAgentHomeGitProbe
	newStoppedAgentHomeGitProbe = func(path string) stoppedAgentHomeGitProbe {
		if path == filepath.Join(cityPath, "rig") {
			return rigProbe
		}
		return homeProbe
	}
	t.Cleanup(func() { newStoppedAgentHomeGitProbe = oldFactory })

	var out bytes.Buffer
	removed := reapStoppedAgentHomeWorktrees(cityPath, cfg, cityStore, namedHomeRigStores(), runtime.NewFake(), nil, &out, false, []beads.Bead{session})
	if removed != 0 || rigProbe.removedPath != "" || !strings.Contains(out.String(), "owner session bead is not closed") {
		t.Fatalf("removed=%d path=%q output=%q, want post-quarantine ownership skip", removed, rigProbe.removedPath, out.String())
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("original home was not restored: %v", err)
	}
}

func TestListSessionHistoryByMetadataFailsClosedAtCap(t *testing.T) {
	store := beads.NewMemStore()
	for i := 0; i <= stoppedAgentHomeHistoryLimit; i++ {
		_, err := store.Create(beads.Bead{ID: fmt.Sprintf("ga-history-%d", i), Type: sessionBeadType, Status: "closed", Metadata: map[string]string{"alias": "qcore/refinery"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := listSessionHistoryByMetadata(store, []map[string]string{{"alias": "qcore/refinery"}})
	if err == nil || !strings.Contains(err.Error(), "history limit") {
		t.Fatalf("error = %v, want fail-closed history limit", err)
	}
}

func TestActiveSessionBeadsUsesCanonicalWorkerDir(t *testing.T) {
	workerDir := filepath.Join(t.TempDir(), "worker")
	infos := []sessionpkg.Info{{ID: "ga-session", WorkerDir: workerDir, WorkDir: "/legacy/home", SessionName: "worker-live"}}
	got := activeSessionBeads(infos)
	if len(got) != 1 {
		t.Fatalf("activeSessionBeads() len = %d, want 1", len(got))
	}
	if got[0].Metadata["work_dir"] != workerDir {
		t.Fatalf("active work_dir = %q, want canonical worker_dir %q", got[0].Metadata["work_dir"], workerDir)
	}
}
