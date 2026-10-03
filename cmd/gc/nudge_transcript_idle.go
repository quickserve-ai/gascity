package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
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

// claudeTranscriptSaysTurnEnded reports whether a Claude seat's own transcript
// says its main turn has ended (ga-megheo). The poller's quiescence gate reads
// tmux window_activity, which moves on ANY pane output. After a Claude turn
// ends while a background subagent or shell still runs, Claude Code redraws a
// task timer every second, so window_activity never goes quiet and queued
// nudges wait for hours. The transcript does not have that problem: the
// background task writes to its own file, and the main file only gains
// bookkeeping lines until a new turn starts.
//
// It answers true ONLY when the last meaningful entry is system/turn_duration,
// judged on entry type/subtype fields alone (never message text). A bare
// end_turn is not enough: it is written before the Stop hooks run, and a Stop
// hook can continue the turn. Any other provider, an unresolvable, ambiguous or
// unreadable transcript, a torn last line, or no turn_duration in the tail
// reads false, so the caller keeps today's behaviour. The cost is one bounded
// tail read of a local file plus a stat per search root; no store reads.
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
	return claudeTranscriptTailTurnEnded(tail)
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

// claudeTranscriptTailTurnEnded walks the tail backwards: bookkeeping lines
// and stop_hook_summary are skipped, turn_duration means idle, and anything
// else (user, assistant, tool, other system subtypes, unknown types, an
// unparseable line) means not idle.
func claudeTranscriptTailTurnEnded(tail []byte) bool {
	lines := bytes.Split(bytes.TrimRight(tail, "\n"), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(line, &entry) != nil {
			return false
		}
		switch {
		case entry.Type == "system" && entry.Subtype == "turn_duration":
			return true
		case entry.Type == "system" && entry.Subtype == "stop_hook_summary":
			continue
		case claudeTranscriptBookkeeping[entry.Type]:
			continue
		default:
			return false
		}
	}
	return false
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
// first), never by the newest-file-in-workdir fallback. Every root is checked:
// the seat's CLAUDE_CONFIG_DIR/projects, daemon observe_paths, and the default
// root. If the key resolves to two different files, the answer is ambiguous
// and "" is returned, so no root can override another and the search order
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
	for _, root := range worker.MergeSearchPaths(roots) {
		path := workertranscript.DiscoverKeyedPath([]string{root}, "claude", workDir, key)
		if path == "" {
			continue
		}
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
