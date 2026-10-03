package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
	workertranscript "github.com/gastownhall/gascity/internal/worker/transcript"
)

// claudeTranscriptTailBudget bounds the transcript read: only the last 64 KB
// are examined, and a turn end older than that reads as not idle.
const claudeTranscriptTailBudget = 64 * 1024

// claudeTranscriptBookkeeping are entry types Claude Code appends to the main
// transcript while a background task runs after the main turn ended, without
// starting or continuing a turn. Any type not listed here is decisive and
// reads not idle.
var claudeTranscriptBookkeeping = map[string]bool{
	"queue-operation": true, "attachment": true, "pr-link": true,
	"bridge-session": true, "file-history-snapshot": true,
}

func claudeTranscriptIsBookkeeping(entryType string) bool {
	return claudeTranscriptBookkeeping[entryType] || strings.HasPrefix(entryType, "artifact-")
}

// claudeTranscriptSaysTurnEnded reports whether a Claude seat's own transcript
// says its main turn has ended (ga-megheo). The poller's quiescence gate reads
// tmux window_activity, which moves on ANY pane output. After a Claude turn
// ends while a background subagent or shell still runs, Claude Code redraws a
// task timer every second, so window_activity never goes quiet and queued
// nudges wait for hours. The transcript does not have that problem: the
// background task writes to its own file, and the main file only gains
// bookkeeping lines until a new turn starts.
//
// The transcript is only a NECESSARY condition. Claude appends a prompt's user
// line only after the UserPromptSubmit hooks finish (p50 6.9 s, p90 16.7 s on
// real transcripts), so a starting turn's file still ends in turn_duration.
// It answers true ONLY when all three independent readings agree:
//
//  1. the last meaningful transcript entry is system/turn_duration;
//  2. its timestamp is newer than the seat's "prompt submitted" marker, which
//     the seat's UserPromptSubmit hook (gc nudge drain --inject) writes, and
//     the dispatcher also writes on each confirmed submit of its own;
//  3. the pane, read now, shows no live Claude working indicator
//     (runtime.IdleSnapshotProvider; the "running UserPromptSubmit hooks"
//     spinner counts as working).
//
// The marker alone is not enough: the hook drains its stdin before it starts
// gc, then pays a gc process start (0.6-0.7 s at load ~25), and it is not
// written at all for some prompts, a fresh session's first prompt, or a seat
// resumed into a file ending in its previous incarnation's turn_duration.
// The pane reading covers those; the marker covers a turn the pane has not
// redrawn yet. No marker, no parseable timestamp, an older turn_duration, or
// no idle pane reading (no runtime, a capture error) reads false. A frozen
// transcript the key still points at never reads idle once the seat has
// prompted since. The transcript is read BEFORE the marker, so a prompt that
// submits between the two reads is seen as a newer marker, never missed.
//
// Entries are judged on type/subtype/timestamp fields alone (never message
// text). A bare end_turn is not enough: it is written before the Stop hooks
// run, and a Stop hook can continue the turn. Any other provider, an
// unresolvable, ambiguous or unreadable transcript, a torn last line, or no
// turn_duration in the tail reads false, so the caller keeps today's
// behaviour. The cost is one bounded tail read of a local file, a stat per
// candidate file, one marker read and one pane capture; no store reads.
func claudeTranscriptSaysTurnEnded(target nudgeTarget, sp runtime.Provider) bool {
	if nudgeTargetBuiltinFamily(target) != "claude" {
		return false
	}
	path := nudgeTargetClaudeTranscriptPath(target)
	if path == "" {
		return false
	}
	tail, err := readClaudeTranscriptTail(path)
	if err != nil {
		return false
	}
	ended, endedAt := claudeTranscriptTailTurnEnd(tail)
	if !ended {
		return false
	}
	submitted, ok := readClaudePromptSubmitted(target.cityPath, target.sessionID)
	if !ok || !endedAt.After(submitted) {
		return false
	}
	pane, ok := sp.(runtime.IdleSnapshotProvider)
	if !ok || target.sessionName == "" {
		return false
	}
	idle, err := pane.SnapshotIdle(target.sessionName)
	return err == nil && idle
}

func readClaudeTranscriptTail(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	offset := info.Size() - claudeTranscriptTailBudget
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	buf = buf[:n]
	if offset > 0 { // drop the partial first line
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			buf = nil
		}
	}
	return buf, nil
}

// claudeTranscriptTailTurnEnd walks the tail backwards: bookkeeping lines
// and stop_hook_summary are skipped, turn_duration means the turn ended at its
// timestamp, and anything else (user, assistant, tool, other system subtypes,
// unknown types, an unparseable line or timestamp) means not ended.
func claudeTranscriptTailTurnEnd(tail []byte) (bool, time.Time) {
	lines := bytes.Split(bytes.TrimRight(tail, "\n"), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(line, &entry) != nil {
			return false, time.Time{}
		}
		switch {
		case entry.Type == "system" && entry.Subtype == "turn_duration":
			at, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
			if err != nil {
				return false, time.Time{}
			}
			return true, at
		case entry.Type == "system" && entry.Subtype == "stop_hook_summary":
			continue
		case claudeTranscriptIsBookkeeping(entry.Type):
			continue
		default:
			return false, time.Time{}
		}
	}
	return false, time.Time{}
}

// nudgeTargetBuiltinFamily returns the builtin family of the seat's provider:
// the bead's recorded builtin_ancestor when set, else the city's provider
// chain (a custom provider with base = "builtin:claude" is claude; one that
// declares base = "" is not), else the builtin of that name. The legacy
// provider_kind is deliberately not consulted.
func nudgeTargetBuiltinFamily(target nudgeTarget) string {
	if family := strings.TrimSpace(target.providerAncestor); family != "" {
		return family
	}
	var providers map[string]config.ProviderSpec
	if target.cfg != nil {
		providers = target.cfg.Providers
	}
	return config.BuiltinFamily(target.providerName(), providers)
}

// nudgeTargetClaudeTranscriptPath resolves the seat's transcript by its stable
// session key only (the keyed lookup SessionHandle.TranscriptPath tries
// first), never by the newest-file-in-workdir fallback. Every root is checked
// (the seat's CLAUDE_CONFIG_DIR/projects, daemon observe_paths, and the default
// root), and within each root every path-alias spelling of the work dir
// (/tmp/x and /private/tmp/x on macOS give two slugs). If the key resolves to
// two different files, the answer is ambiguous and "" is returned, so neither
// a root nor a newer alias copy can override another and the search order
// does not matter; one file reached through a symlinked root is one file.
func nudgeTargetClaudeTranscriptPath(target nudgeTarget) string {
	workDir := strings.TrimSpace(target.transcriptWorkDir)
	key := strings.TrimSpace(target.transcriptSessionKey)
	if workDir == "" || key == "" {
		return ""
	}
	if abs, err := filepath.Abs(workDir); err == nil {
		workDir = abs
	}
	var roots []string
	if dir := nudgeTargetClaudeConfigDir(target); dir != "" {
		roots = append(roots, filepath.Join(dir, "projects"))
	}
	if target.cfg != nil {
		roots = append(roots, target.cfg.Daemon.ObservePaths...)
	}
	var found string
	var foundInfo os.FileInfo
	paths, err := workertranscript.DiscoverClaudeKeyedPaths(worker.MergeSearchPaths(roots), workDir, key)
	if err != nil {
		return ""
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return ""
		}
		if found == "" {
			found, foundInfo = path, info
		} else if !os.SameFile(foundInfo, info) {
			return ""
		}
	}
	return found
}

// nudgeTargetClaudeConfigDir returns the CLAUDE_CONFIG_DIR the seat's
// provider/agent env (or the workspace env) declares, or "".
func nudgeTargetClaudeConfigDir(target nudgeTarget) string {
	if target.cfg == nil {
		return ""
	}
	agent := target.agent
	if name := target.providerName(); name != "" {
		agent.Provider = name
	}
	resolved, err := config.ResolveProvider(&agent, &target.cfg.Workspace, target.cfg.Providers,
		func(name string) (string, error) { return name, nil })
	if err == nil && resolved != nil {
		if dir := strings.TrimSpace(resolved.Env["CLAUDE_CONFIG_DIR"]); dir != "" {
			return dir
		}
	}
	return strings.TrimSpace(target.cfg.Workspace.Env["CLAUDE_CONFIG_DIR"])
}

// claudePromptMarkerPath is the per-session "prompt submitted" marker, keyed by
// the session bead id (GC_SESSION_ID in the seat, target.sessionID in the
// dispatcher). "" for an id that is not a safe file name.
func claudePromptMarkerPath(cityPath, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if strings.TrimSpace(cityPath) == "" || sessionID == "" || strings.HasPrefix(sessionID, ".") ||
		strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	return citylayout.RuntimePath(cityPath, "nudges", "prompt-submitted", sessionID)
}

// recordClaudePromptSubmitted writes the marker atomically. It must never fail
// the prompt, so callers only log its error. When the write fails, the old
// marker is removed: left in place it would keep vouching for the previous
// prompt, while a missing marker shuts the transcript path until the next
// write. The error is returned only if that removal fails too.
func recordClaudePromptSubmitted(cityPath, sessionID string, at time.Time) error {
	path := claudePromptMarkerPath(cityPath, sessionID)
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		err = claudePromptMarkerWrite(path, []byte(at.UTC().Format(time.RFC3339Nano)+"\n"))
	}
	if err == nil {
		pruneClaudePromptMarkers(dir, time.Now())
		return nil
	}
	if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return fmt.Errorf("prompt marker %s: write: %v; removing the stale marker: %w", path, err, rmErr)
	}
	return nil
}

// claudePromptMarkerWrite writes one marker by temp file + rename (no
// directory sweep per write); a var so tests can fail it.
var claudePromptMarkerWrite = func(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// claudePromptMarkerTTL: a marker this old belongs to a seat that has not
// prompted for a week or is gone. Pruning it only shuts the transcript path
// for that seat until its next prompt.
const claudePromptMarkerTTL = 7 * 24 * time.Hour

// pruneClaudePromptMarkers removes markers and stray temp files older than
// claudePromptMarkerTTL, at most once a day: each call costs one stat of the
// ".pruned" stamp (a name no session id can take), and only a stale stamp
// pays for the directory read.
func pruneClaudePromptMarkers(dir string, now time.Time) {
	stamp := filepath.Join(dir, ".pruned")
	if info, err := os.Stat(stamp); err == nil && now.Sub(info.ModTime()) < 24*time.Hour {
		return
	}
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		return
	}
	_ = os.Chtimes(stamp, now, now)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Name() == ".pruned" || entry.IsDir() {
			continue
		}
		if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > claudePromptMarkerTTL {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// readClaudePromptSubmitted returns the marker time, or false when there is no
// readable marker.
func readClaudePromptSubmitted(cityPath, sessionID string) (time.Time, bool) {
	path := claudePromptMarkerPath(cityPath, sessionID)
	if path == "" {
		return time.Time{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}
