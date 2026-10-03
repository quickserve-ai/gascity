package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// The seat's configured work dir is A, but its process ran with cwd inside a
// worktree B (the launch path's task work_dir override), so claude keyed the
// conversation under B's projects folder. history must list it and resume must
// accept it (ga-fe2rmu: qcore/lana, conversation f8de97bc, 2026-10-02).

const (
	worktreeConvID = "f8de97bc-0b70-4f60-92a9-d8e41912af84"
	foreignConvID  = "aaaaaaaa-0000-4000-8000-000000000001"
	agentDirConvID = "bbbbbbbb-0000-4000-8000-000000000002"
	currentConvID  = "cccccccc-0000-4000-8000-000000000003"
	priorConvID    = "dddddddd-0000-4000-8000-000000000004"
)

type historyWorktreeFixture struct {
	cityDir    string
	agentDir   string // A: configured work dir
	worktree   string // B: the cwd the seat actually ran in
	liveRoot   string // $HOME/.claude/projects
	store      beads.Store
	sessionID  string
	taskBeadID string
}

func historyTranscriptLines(sessionID, agentName, cwd, text string) []string {
	return []string{
		fmt.Sprintf(`{"type":"custom-title","customTitle":%q,"sessionId":%q}`, agentName, sessionID),
		fmt.Sprintf(`{"type":"agent-name","agentName":%q,"sessionId":%q}`, agentName, sessionID),
		fmt.Sprintf(`{"uuid":"1","parentUuid":"","type":"user","cwd":%q,"sessionId":%q,"message":{"role":"user","content":%q},"timestamp":"2026-10-02T01:54:29Z"}`, cwd, sessionID, text),
	}
}

func setupHistoryWorktreeFixture(t *testing.T, sessionMeta map[string]string, withInProgressTask bool) historyWorktreeFixture {
	t.Helper()
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	home := t.TempDir()
	t.Setenv("HOME", home)
	liveRoot := filepath.Join(home, ".claude", "projects")

	cityDir := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agents", "lana")
	worktree := filepath.Join(t.TempDir(), "worktrees", "polecats", "gastown.capable")
	for _, dir := range []string{agentDir, worktree, filepath.Join(cityDir, ".gc"), liveRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	t.Setenv("GC_CITY", cityDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\n\n[providers.claude]\nbase = \"builtin:claude\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	agentConfigDir := filepath.Join(cityDir, "agents", "lana")
	if err := os.MkdirAll(agentConfigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", agentConfigDir, err)
	}
	if err := os.WriteFile(filepath.Join(agentConfigDir, "agent.toml"), []byte(fmt.Sprintf("provider = \"claude\"\nwork_dir = %q\n", agentDir)), 0o644); err != nil {
		t.Fatalf("write agent.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "site.toml"), []byte("workspace_name = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write site.toml: %v", err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	meta := map[string]string{
		"session_name": "lana-runtime",
		"alias":        "lana",
		"agent_name":   "lana",
		"template":     "lana",
		"provider":     "claude",
		"state":        "asleep",
		"work_dir":     agentDir,
	}
	for k, v := range sessionMeta {
		meta[k] = v
	}
	sess, err := store.Create(beads.Bead{
		Title:    "lana",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	fx := historyWorktreeFixture{
		cityDir:   cityDir,
		agentDir:  agentDir,
		worktree:  worktree,
		liveRoot:  liveRoot,
		store:     store,
		sessionID: sess.ID,
	}
	if withInProgressTask {
		task, err := store.Create(beads.Bead{
			Title:    "voice-booking E2E",
			Type:     "task",
			Assignee: "lana",
			Metadata: map[string]string{"work_dir": worktree},
		})
		if err != nil {
			t.Fatalf("create task bead: %v", err)
		}
		inProgress := "in_progress"
		if err := store.Update(task.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("mark task in progress: %v", err)
		}
		fx.taskBeadID = task.ID
	}

	// The seat's conversation lives ONLY under B's projects folder; B is a
	// shared polecat worktree, so another seat's conversation sits beside it.
	writeNamedTestSession(t, liveRoot, worktree, worktreeConvID+".jsonl",
		historyTranscriptLines(worktreeConvID, "lana", worktree, "lana worktree conversation")...)
	writeNamedTestSession(t, liveRoot, worktree, foreignConvID+".jsonl",
		historyTranscriptLines(foreignConvID, "ray", worktree, "ray conversation in the shared worktree")...)
	writeNamedTestSession(t, liveRoot, agentDir, agentDirConvID+".jsonl",
		historyTranscriptLines(agentDirConvID, "lana", agentDir, "lana agent-dir conversation")...)
	return fx
}

func TestSessionHistoryListsTranscriptUnderTaskWorktreeCwd(t *testing.T) {
	setupHistoryWorktreeFixture(t, nil, true)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionHistory("lana", 0, false, t.TempDir(), &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionHistory = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, worktreeConvID) {
		t.Fatalf("history does not list the worktree conversation %s:\n%s", worktreeConvID, out)
	}
	if !strings.Contains(out, agentDirConvID) {
		t.Fatalf("history dropped the agent-dir conversation %s:\n%s", agentDirConvID, out)
	}
	if strings.Contains(out, foreignConvID) {
		t.Fatalf("history lists another seat's conversation %s from the shared worktree:\n%s", foreignConvID, out)
	}
}
