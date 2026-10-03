package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/worker"
	workertranscript "github.com/gastownhall/gascity/internal/worker/transcript"
)

// claudeTranscriptTailBudget bounds the transcript read: only the last 64 KB
// are examined, and a turn end older than that reads as not idle.
const claudeTranscriptTailBudget = 64 * 1024

// claudeTranscriptBookkeeping are entry types Claude Code appends to the main
// transcript while a background task runs after the main turn ended, without
// starting or continuing a turn: queue activity, attachments, and the
// session-metadata block (last-prompt ... bridge-session, cost-state,
// file-history-*, frame-link, artifact-*). Any type not listed here is
// decisive and reads not idle. The metadata block is sometimes written at
// prompt submit, before the user line; skipping it there is safe because the
// prompt marker is then newer than the old turn_duration.
var claudeTranscriptBookkeeping = map[string]bool{
	"queue-operation": true, "attachment": true, "pr-link": true,
	"bridge-session": true, "file-history-snapshot": true,
	"last-prompt": true, "custom-title": true, "agent-name": true, "mode": true,
	"permission-mode": true, "atis-latch": true, "frame-link": true,
	"cost-state": true, "file-history-delta": true,
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
// The seat's own UserPromptSubmit hook (gc nudge drain --inject) therefore
// records a per-session "prompt submitted" marker first thing, and this
// answers true ONLY when the last meaningful entry is system/turn_duration
// AND its timestamp is newer than that marker. No marker, a turn_duration
// without a parseable timestamp, or one not newer than the marker reads false.
// That also covers a frozen transcript the key still points at: once the seat
// has submitted a prompt since, it never reads idle again.
//
// Residual window: between submit and the marker write the hook runs
// sh -> gc hook run -> gc nudge drain, two gc process starts before any store
// work (gc version measured 0.44-0.68 s on the host at load ~20), so about
// 1 s, against the 7-17 s hole the marker closes; a dispatcher pass must also
// land inside it. Writing it in gc hook run would save one start, but that
// wrapper runs for any hook event a provider config names, and a marker from a
// non-prompt event would hold delivery back with no turn to end it. The
// transcript is read BEFORE the marker, so a prompt that submits between the
// two reads is seen as a newer marker, never missed.
//
// Entries are judged on type/subtype/timestamp fields alone (never message
// text). A bare end_turn is not enough: it is written before the Stop hooks
// run, and a Stop hook can continue the turn. Any other provider, an
// unresolvable, ambiguous or unreadable transcript, a torn last line, or no
// turn_duration in the tail reads false, so the caller keeps today's
// behaviour. The cost is one bounded tail read of a local file, a stat per
// candidate file and one marker read; no store reads.
func claudeTranscriptSaysTurnEnded(target nudgeTarget) bool {
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
	return ok && endedAt.After(submitted)
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
	for _, path := range workertranscript.DiscoverClaudeKeyedPaths(worker.MergeSearchPaths(roots), workDir, key) {
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
	if strings.TrimSpace(cityPath) == "" || sessionID == "" || sessionID == "." ||
		strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	return citylayout.RuntimePath(cityPath, "nudges", "prompt-submitted", sessionID)
}

// recordClaudePromptSubmitted writes the marker atomically. Best-effort: it
// must never fail the prompt. A failed write with no earlier marker leaves the
// gate on today's pane-quiet rule; a failed write over an earlier marker
// reopens the starting-turn hole for that one prompt only.
func recordClaudePromptSubmitted(cityPath, sessionID string, at time.Time) {
	path := claudePromptMarkerPath(cityPath, sessionID)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = fsys.WriteFileAtomic(fsys.OSFS{}, path, []byte(at.UTC().Format(time.RFC3339Nano)+"\n"), 0o644)
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
