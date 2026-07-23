package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	historyTestUUIDA = "11111111-2222-3333-4444-555555555555"
	historyTestUUIDB = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	historyTestUUIDC = "99999999-8888-7777-6666-555555555555"
)

func writeHistoryTranscript(t *testing.T, dir, stem string, lines []string, mod time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, stem+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
	return path
}

func TestListClaudeSessionHistory(t *testing.T) {
	workDir := "/work/agents/lana"
	slug := ProjectSlug(workDir)
	liveRoot := t.TempDir()
	archiveRoot := t.TempDir()

	now := time.Now()
	writeHistoryTranscript(t, filepath.Join(liveRoot, slug), historyTestUUIDA,
		[]string{`{"type":"user","timestamp":"2026-07-14T10:00:00Z","message":{"content":"hi"}}`}, now.Add(-1*time.Hour))
	// Reaper layout: <root>/reaped/<date>/<slug>/.
	writeHistoryTranscript(t, filepath.Join(archiveRoot, "reaped", "2026-07-10", slug), historyTestUUIDB,
		[]string{`{"type":"user","timestamp":"2026-07-10T10:00:00Z","message":{"content":"old"}}`}, now.Add(-96*time.Hour))
	// Sweep layout: <root>/<label>/<slug>/ — same uuid as the live copy; live must win.
	writeHistoryTranscript(t, filepath.Join(archiveRoot, "sweep-1", slug), historyTestUUIDA,
		[]string{`{"type":"user","timestamp":"2026-07-14T10:00:00Z","message":{"content":"hi"}}`}, now.Add(-2*time.Hour))
	// Non-uuid files are skipped.
	writeHistoryTranscript(t, filepath.Join(liveRoot, slug), "latest-session",
		[]string{`{"type":"user","message":{"content":"x"}}`}, now)

	entries := ListClaudeSessionHistory([]string{liveRoot}, []string{archiveRoot}, workDir)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (%+v)", len(entries), entries)
	}
	if entries[0].SessionID != historyTestUUIDA || entries[0].Archived {
		t.Fatalf("newest entry = %+v, want live %s", entries[0], historyTestUUIDA)
	}
	if entries[1].SessionID != historyTestUUIDB || !entries[1].Archived {
		t.Fatalf("older entry = %+v, want archived %s", entries[1], historyTestUUIDB)
	}
	if entries[1].Slug != slug {
		t.Fatalf("archived entry slug = %q, want %q", entries[1].Slug, slug)
	}
}

func TestListClaudeSessionHistoryEmptyWorkDir(t *testing.T) {
	if got := ListClaudeSessionHistory([]string{t.TempDir()}, nil, ""); got != nil {
		t.Fatalf("expected nil for empty workDir, got %+v", got)
	}
}

func TestReadClaudeTranscriptSummary(t *testing.T) {
	dir := t.TempDir()
	path := writeHistoryTranscript(t, dir, historyTestUUIDC, []string{
		`{"type":"custom-title","customTitle":"debugging vapi","sessionId":"x"}`,
		`{"type":"agent-name","agentName":"qcore/lana","sessionId":"x"}`,
		`{"type":"user","timestamp":"2026-07-14T10:00:00Z","message":{"content":"[gastown] header\n\nfix the vapi recording bug"}}`,
	}, time.Time{})

	s := ReadClaudeTranscriptSummary(path)
	if s.Title != "debugging vapi" {
		t.Errorf("Title = %q, want debugging vapi", s.Title)
	}
	if s.AgentName != "qcore/lana" {
		t.Errorf("AgentName = %q, want qcore/lana", s.AgentName)
	}
	if s.FirstUser != "[gastown] header" {
		t.Errorf("FirstUser = %q, want first non-empty line", s.FirstUser)
	}
	if s.FirstSeen.IsZero() {
		t.Error("FirstSeen not captured")
	}
}

func TestReadClaudeTranscriptSummaryContentBlocks(t *testing.T) {
	dir := t.TempDir()
	path := writeHistoryTranscript(t, dir, historyTestUUIDC, []string{
		`{"type":"user","timestamp":"2026-07-14T10:00:00Z","message":{"content":[{"type":"text","text":"block message"}]}}`,
	}, time.Time{})

	s := ReadClaudeTranscriptSummary(path)
	if s.FirstUser != "block message" {
		t.Errorf("FirstUser = %q, want block message", s.FirstUser)
	}
	if s.Title != "" {
		t.Errorf("Title = %q, want empty", s.Title)
	}
}

func TestIsSessionIDStem(t *testing.T) {
	if !isSessionIDStem(historyTestUUIDA) {
		t.Error("uuid rejected")
	}
	for _, bad := range []string{"latest-session", "", "9e82b97a", strings.Repeat("x", 36)} {
		if isSessionIDStem(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
