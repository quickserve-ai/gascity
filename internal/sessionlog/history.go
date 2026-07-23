package sessionlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SessionHistoryEntry describes one on-disk claude-family conversation
// transcript recorded for a work directory.
type SessionHistoryEntry struct {
	SessionID string    // transcript uuid (filename stem); doubles as the resume key
	Path      string    // absolute path to the .jsonl file
	Slug      string    // project slug directory the transcript was found under
	Archived  bool      // found under an archive root rather than a live projects root
	Size      int64     // file size in bytes
	ModTime   time.Time // last write = timestamp of the final transcript entry
}

// ListClaudeSessionHistory returns every claude-family transcript recorded
// for workDir across the live searchPaths (projects roots) and archiveRoots
// (transcript-reaper archives), newest first. Entries are deduplicated by
// session id with live copies preferred over archived ones, so a transcript
// that exists both live and in an archive sweep is reported once as live.
//
// Archive roots are searched one and two levels deep (the reaper writes
// <root>/reaped/<date>/<slug>/ and one-off sweeps write <root>/<label>/<slug>/),
// but only slug directories matching workDir are ever opened, so the scan
// stays cheap regardless of archive size.
func ListClaudeSessionHistory(searchPaths, archiveRoots []string, workDir string) []SessionHistoryEntry {
	slugs := claudeProjectSlugCandidates(workDir)
	if len(slugs) == 0 {
		return nil
	}
	byID := make(map[string]SessionHistoryEntry)
	record := func(e SessionHistoryEntry) {
		prev, ok := byID[e.SessionID]
		if !ok {
			byID[e.SessionID] = e
			return
		}
		// Live beats archived; among equals keep the newest copy.
		if prev.Archived == e.Archived {
			if e.ModTime.After(prev.ModTime) {
				byID[e.SessionID] = e
			}
			return
		}
		if prev.Archived && !e.Archived {
			byID[e.SessionID] = e
		}
	}
	for _, root := range searchPaths {
		for _, slug := range slugs {
			collectHistoryDir(filepath.Join(root, slug), slug, false, record)
		}
	}
	for _, root := range archiveRoots {
		for _, slug := range slugs {
			collectHistoryDir(filepath.Join(root, slug), slug, true, record)
		}
		// Sweep layout: <root>/<label>/<slug>/. Reaper layout nests one
		// deeper: <root>/reaped/<date>/<slug>/. Only slug-matching leaf
		// directories are opened, so the fan-out is ENOENT stats.
		subdirs, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, sub := range subdirs {
			if !sub.IsDir() {
				continue
			}
			subPath := filepath.Join(root, sub.Name())
			for _, slug := range slugs {
				collectHistoryDir(filepath.Join(subPath, slug), slug, true, record)
			}
			nested, err := os.ReadDir(subPath)
			if err != nil {
				continue
			}
			for _, n := range nested {
				if !n.IsDir() {
					continue
				}
				for _, slug := range slugs {
					collectHistoryDir(filepath.Join(subPath, n.Name(), slug), slug, true, record)
				}
			}
		}
	}
	out := make([]SessionHistoryEntry, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out
}

func collectHistoryDir(dir, slug string, archived bool, record func(SessionHistoryEntry)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".jsonl") {
			continue
		}
		stem := strings.TrimSuffix(ent.Name(), ".jsonl")
		if !isSessionIDStem(stem) {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		record(SessionHistoryEntry{
			SessionID: stem,
			Path:      filepath.Join(dir, ent.Name()),
			Slug:      slug,
			Archived:  archived,
			Size:      info.Size(),
			ModTime:   info.ModTime(),
		})
	}
}

// isSessionIDStem reports whether a transcript filename stem looks like a
// provider session id (uuid). Filters out non-conversation files such as
// latest-session.jsonl that share the projects directory.
func isSessionIDStem(stem string) bool {
	if len(stem) != 36 {
		return false
	}
	for i, r := range stem {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
				return false
			}
		}
	}
	return true
}

// TranscriptSummary holds cheap metadata scanned from a transcript's head
// (and, when the head misses, its tail): the display title, the agent name
// recorded by the claude-named wrapper, the first user text, and the first
// entry timestamp.
type TranscriptSummary struct {
	Title     string
	AgentName string
	FirstUser string
	FirstSeen time.Time
}

// transcriptScanWindow bounds how much of a transcript each scan reads;
// summaries must stay cheap enough to run across a full history listing.
const transcriptScanWindow = 256 * 1024

// ReadClaudeTranscriptSummary scans up to transcriptScanWindow bytes from the
// head of the transcript for title/agent/first-message records, falling back
// to the same window from the tail for titles appended later (renames,
// summary records). Returns a zero summary when the file cannot be read.
func ReadClaudeTranscriptSummary(path string) TranscriptSummary {
	var s TranscriptSummary
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close() //nolint:errcheck // read-only handle

	summaryTitle := ""
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	read := 0
	for scanner.Scan() && read < transcriptScanWindow {
		line := scanner.Bytes()
		read += len(line) + 1
		scanTranscriptLine(line, &s, &summaryTitle)
		if s.Title != "" && s.AgentName != "" && s.FirstUser != "" && !s.FirstSeen.IsZero() {
			break
		}
	}
	if s.Title == "" {
		if tail := readHistoryTailWindow(f, transcriptScanWindow); len(tail) > 0 {
			for _, line := range strings.Split(string(tail), "\n") {
				scanTranscriptLine([]byte(line), &s, &summaryTitle)
			}
		}
	}
	if s.Title == "" {
		s.Title = summaryTitle
	}
	return s
}

// scanTranscriptLine folds one JSONL record into the summary. summaryTitle
// collects {"type":"summary"} records separately so an explicit custom-title
// always wins over compaction summaries.
func scanTranscriptLine(line []byte, s *TranscriptSummary, summaryTitle *string) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return
	}
	var rec struct {
		Type        string          `json:"type"`
		CustomTitle string          `json:"customTitle"`
		AgentName   string          `json:"agentName"`
		Summary     string          `json:"summary"`
		Timestamp   time.Time       `json:"timestamp"`
		Message     json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal([]byte(trimmed), &rec); err != nil {
		return
	}
	if s.FirstSeen.IsZero() && !rec.Timestamp.IsZero() {
		s.FirstSeen = rec.Timestamp
	}
	switch rec.Type {
	case "custom-title":
		if s.Title == "" && strings.TrimSpace(rec.CustomTitle) != "" {
			s.Title = strings.TrimSpace(rec.CustomTitle)
		}
	case "agent-name":
		if s.AgentName == "" && strings.TrimSpace(rec.AgentName) != "" {
			s.AgentName = strings.TrimSpace(rec.AgentName)
		}
	case "summary":
		if *summaryTitle == "" && strings.TrimSpace(rec.Summary) != "" {
			*summaryTitle = strings.TrimSpace(rec.Summary)
		}
	case "user":
		if s.FirstUser == "" && len(rec.Message) > 0 {
			s.FirstUser = firstUserTextLine(rec.Message)
		}
	}
}

// firstUserTextLine extracts a single display line from a user message
// payload, handling both string content and content-block arrays.
func firstUserTextLine(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil || len(msg.Content) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(msg.Content, &text); err != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			return ""
		}
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				text = b.Text
				break
			}
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return line
	}
	return ""
}

// readHistoryTailWindow returns the final up-to-window bytes of f, aligned to
// the first full line, or nil when the file fits inside the head scan already.
func readHistoryTailWindow(f *os.File, window int) []byte {
	info, err := f.Stat()
	if err != nil || info.Size() <= int64(window) {
		return nil
	}
	buf := make([]byte, window)
	if _, err := f.ReadAt(buf, info.Size()-int64(window)); err != nil {
		return nil
	}
	if idx := strings.IndexByte(string(buf), '\n'); idx >= 0 {
		return buf[idx+1:]
	}
	return nil
}

// DefaultTranscriptArchiveRoots returns the standard transcript-reaper
// archive locations searched by gc session history in addition to the live
// projects roots: ~/.claude-transcript-archive itself (one-off sweep layout
// <label>/<slug>/) and its reaped/ subtree (<date>/<slug>/).
func DefaultTranscriptArchiveRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	root := filepath.Join(home, ".claude-transcript-archive")
	return []string{root}
}
