package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

// The reported case: lana's task lives in a RIG store (qcore), not the city
// store. history must find its work dir there, as the reconciler does.
// bindHistoryTestRig declares rig name in the fixture city (city.toml plus its
// .gc/site.toml path binding) and returns the rig's directory.
func bindHistoryTestRig(t *testing.T, fx historyWorktreeFixture, name string) string {
	t.Helper()
	rigDir := filepath.Join(fx.cityDir, "rigs", name)
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for file, entry := range map[string]string{
		filepath.Join(fx.cityDir, "city.toml"):        fmt.Sprintf("\n[[rigs]]\nname = %q\nprefix = %q\n", name, name[:2]),
		filepath.Join(fx.cityDir, ".gc", "site.toml"): fmt.Sprintf("\n[[rig]]\nname = %q\npath = %q\n", name, rigDir),
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, append(data, []byte(entry)...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return rigDir
}

func TestSessionHistoryListsTranscriptUnderRigStoreTaskWorktree(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, nil, false)
	rigDir := bindHistoryTestRig(t, fx, "qcore")
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("rig file store: %v", err)
	}
	rigStore, err := openStoreAtForCity(rigDir, fx.cityDir)
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	task, err := rigStore.Create(beads.Bead{Title: "rig task", Type: "task", Assignee: "lana", Metadata: map[string]string{"work_dir": fx.worktree}})
	if err != nil {
		t.Fatalf("create rig task: %v", err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(task.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark rig task in progress: %v", err)
	}

	rows := sessionHistoryJSONRows(t, "lana")
	if row, ok := rows[worktreeConvID]; !ok || row.FoundUnder != fx.worktree {
		t.Fatalf("history row for the rig-task worktree conversation = %+v (listed=%v), want found_under %s", row, ok, fx.worktree)
	}
	if _, ok := rows[foreignConvID]; ok {
		t.Fatalf("history lists another seat's conversation %s", foreignConvID)
	}
}

// A session addressed by bead id whose stored work dir is a shared worktree
// (under .gc/worktrees/) gets that folder filtered like any other shared one.
func TestSessionHistoryByBeadIDFiltersASharedWorktreeWorkDir(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, nil, false)
	shared := filepath.Join(fx.cityDir, ".gc", "worktrees", "qcore", "polecats", "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	nina, err := fx.store.Create(beads.Bead{
		Title:  "nina",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "nina-runtime",
			"alias":        "nina",
			"agent_name":   "nina",
			"template":     "nina",
			"provider":     "claude",
			"state":        "asleep",
			"work_dir":     shared,
			"session_key":  currentConvID,
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	writeNamedTestSession(t, fx.liveRoot, shared, currentConvID+".jsonl", historyTranscriptLines(currentConvID, "nina", shared, "nina's own")...)
	writeNamedTestSession(t, fx.liveRoot, shared, foreignConvID+".jsonl", historyTranscriptLines(foreignConvID, "ray", shared, "ray in the shared worktree")...)

	rows := sessionHistoryJSONRows(t, nina.ID)
	if _, ok := rows[currentConvID]; !ok {
		t.Fatalf("history does not list nina's own conversation %s: %v", currentConvID, rows)
	}
	if _, ok := rows[foreignConvID]; ok {
		t.Fatalf("history lists ray's conversation %s from nina's shared worktree work dir", foreignConvID)
	}
}

// Two pool instances of one template share a worktree as their work dir and
// record the template as agent-name. An instance has no named identity or
// alias, so only conversations whose id is on its bead are its own.
func TestSessionHistoryPoolInstanceInSharedWorktreeListsOnlyItsOwnConversation(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, nil, false)
	shared := filepath.Join(t.TempDir(), "worktrees", "polecats", "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	instance := func(n int, key string) string {
		b, err := fx.store.Create(beads.Bead{
			Title:  "polecat",
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name": fmt.Sprintf("polecat-%d", n),
				"agent_name":   "polecat",
				"template":     "polecat",
				"provider":     "claude",
				"state":        "asleep",
				"work_dir":     shared,
				"session_key":  key,
				"pool_slot":    fmt.Sprint(n),
			},
		})
		if err != nil {
			t.Fatalf("create instance %d: %v", n, err)
		}
		writeNamedTestSession(t, fx.liveRoot, shared, key+".jsonl", historyTranscriptLines(key, "polecat", shared, fmt.Sprintf("instance %d", n))...)
		return b.ID
	}
	instance(1, foreignConvID)
	second := instance(2, currentConvID)
	if err := fx.store.SetMetadata(second, "alias", "capable"); err != nil {
		t.Fatalf("alias instance 2: %v", err)
	}
	writeNamedTestSession(t, fx.liveRoot, shared, priorConvID+".jsonl", historyTranscriptLines(priorConvID, "capable", shared, "recorded under the alias, id not on the bead")...)

	rows := sessionHistoryJSONRows(t, second)
	if _, ok := rows[currentConvID]; !ok {
		t.Fatalf("history for instance 2 does not list its own conversation %s: %v", currentConvID, rows)
	}
	if _, ok := rows[foreignConvID]; ok {
		t.Fatalf("history for instance 2 lists instance 1's conversation %s from the shared worktree", foreignConvID)
	}
	if _, ok := rows[priorConvID]; ok {
		t.Fatalf("history for aliased pool instance 2 lists %s by agent-name; a pool instance attributes by id only", priorConvID)
	}
}

// One conversation id present in two projects folders is one row: the newest
// copy, with found_under naming the folder it came from.
func TestSessionHistoryListsOneRowForAConversationInTwoFolders(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, nil, true)
	older := writeNamedTestSession(t, fx.liveRoot, fx.agentDir, worktreeConvID+".jsonl",
		historyTranscriptLines(worktreeConvID, "lana", fx.agentDir, "stale copy")...)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionHistory("lana", 0, false, t.TempDir(), &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionHistory = %d; stderr=%s", code, stderr.String())
	}
	if n := strings.Count(stdout.String(), worktreeConvID); n != 1 {
		t.Fatalf("history lists %s %d times, want once:\n%s", worktreeConvID, n, stdout.String())
	}
	if row := sessionHistoryJSONRows(t, "lana")[worktreeConvID]; row.FoundUnder != fx.worktree {
		t.Fatalf("found_under = %q, want the newer copy's work dir %s", row.FoundUnder, fx.worktree)
	}
}

type historyRowFoundUnder struct {
	SessionID  string `json:"session_id"`
	FoundUnder string `json:"found_under"`
}

func sessionHistoryJSONRows(t *testing.T, identifier string) map[string]historyRowFoundUnder {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cmdSessionHistory(identifier, 0, true, t.TempDir(), &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionHistory(--json %s) = %d; stderr=%s", identifier, code, stderr.String())
	}
	var payload struct {
		Conversations []historyRowFoundUnder `json:"conversations"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode history JSON: %v\n%s", err, stdout.String())
	}
	rows := make(map[string]historyRowFoundUnder, len(payload.Conversations))
	for _, row := range payload.Conversations {
		rows[row.SessionID] = row
	}
	return rows
}

// rigStoreStub stands in for a rig store whose List stalls until release is
// closed, or fails with err.
type rigStoreStub struct {
	beads.Store
	release chan struct{}
	err     error
}

func (s rigStoreStub) List(beads.ListQuery) ([]beads.Bead, error) {
	if s.release != nil {
		<-s.release
	}
	return nil, s.err
}

// A rig store can open and then stall in List (bd allows minutes). history
// bounds the whole task lookup, skips what has not answered, and names each
// skipped or failed rig on stderr.
func TestSessionHistoryBoundsAndNamesStalledOrFailedRigTaskLookups(t *testing.T) {
	fx := setupHistoryWorktreeFixture(t, nil, false)
	bindHistoryTestRig(t, fx, "slow")
	bindHistoryTestRig(t, fx, "broken")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	prevOpener, prevTimeout := historyRigStoreOpener, historyTaskLookupTimeout
	t.Cleanup(func() { historyRigStoreOpener, historyTaskLookupTimeout = prevOpener, prevTimeout })
	historyTaskLookupTimeout = 300 * time.Millisecond
	historyRigStoreOpener = func(*config.City) rigStoreOpener {
		return func(rigPath, _ string) (beads.Store, error) {
			if filepath.Base(rigPath) == "slow" {
				return rigStoreStub{Store: beads.NewMemStore(), release: release}, nil
			}
			return rigStoreStub{Store: beads.NewMemStore(), err: fmt.Errorf("dolt unreachable")}, nil
		}
	}

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- cmdSessionHistory("lana", 0, false, t.TempDir(), &stdout, &stderr) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("cmdSessionHistory = %d; stderr=%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("history did not return while a rig store's List stalled")
	}
	for _, rig := range []string{"slow", "broken"} {
		if want := "rig " + rig + " task lookup skipped"; !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
	if !strings.Contains(stderr.String(), "dolt unreachable") {
		t.Fatalf("stderr = %q, want the failed rig's error", stderr.String())
	}
	if !strings.Contains(stdout.String(), agentDirConvID) {
		t.Fatalf("history dropped the agent-dir conversation:\n%s", stdout.String())
	}
}
