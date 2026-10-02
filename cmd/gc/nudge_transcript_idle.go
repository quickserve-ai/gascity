package main

import (
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
	workertranscript "github.com/gastownhall/gascity/internal/worker/transcript"
)

// claudeTranscriptSaysTurnEnded reports whether a Claude seat's own transcript
// says its main turn has ended (ga-megheo). The poller's quiescence gate reads
// tmux window_activity, which moves on ANY pane output. After a Claude turn
// ends while a background subagent or shell still runs, Claude Code redraws a
// task timer every second, so window_activity never goes quiet and queued
// nudges wait for hours. The transcript does not have that problem: the main
// turn's end is recorded (end_turn / turn_duration), the background task writes
// to its own file, and the only main-file lines appended meanwhile are
// queue-operation/attachment lines that InferActivity skips. A background
// completion that starts a turn is written as a user entry, which reads in-turn.
//
// It answers true ONLY on a definite idle reading. Any other provider, a seat
// without a resolvable keyed transcript, an unreadable file, a torn last line,
// or a tail with no turn-defining entry inside the 64 KB tail window reads
// false, so the caller keeps today's behaviour. The cost is one bounded tail
// read of a local file; no store reads, no retries.
func claudeTranscriptSaysTurnEnded(target nudgeTarget) bool {
	if nudgeTargetBuiltinFamily(target) != "claude" {
		return false
	}
	path := nudgeTargetClaudeTranscriptPath(target)
	if path == "" {
		return false
	}
	meta, err := sessionlog.ExtractTailMeta(path)
	if err != nil || meta == nil || meta.MalformedTail {
		return false
	}
	return meta.Activity == "idle"
}

// nudgeTargetBuiltinFamily returns the builtin family of the seat's provider,
// preferring what the session bead recorded at creation, then the city's
// provider chain (a custom provider with base = "builtin:claude" is claude).
func nudgeTargetBuiltinFamily(target nudgeTarget) string {
	if family := strings.TrimSpace(target.providerAncestor); family != "" {
		return family
	}
	var providers map[string]config.ProviderSpec
	if target.cfg != nil {
		providers = target.cfg.Providers
	}
	if family := config.BuiltinFamily(target.providerName(), providers); family != "" {
		return family
	}
	return target.providerFamily()
}

// nudgeTargetClaudeTranscriptPath resolves the seat's transcript by its stable
// session key only (the keyed lookup SessionHandle.TranscriptPath tries
// first), never by the newest-file-in-workdir fallback, so it cannot read
// another session's transcript. It searches the default root, daemon
// observe_paths, and the seat's own CLAUDE_CONFIG_DIR/projects (fleet seats run
// under per-account config dirs that the default root does not cover).
func nudgeTargetClaudeTranscriptPath(target nudgeTarget) string {
	workDir := strings.TrimSpace(target.transcriptWorkDir)
	key := strings.TrimSpace(target.transcriptSessionKey)
	if workDir == "" || key == "" {
		return ""
	}
	if abs, err := filepath.Abs(workDir); err == nil {
		workDir = abs
	}
	var extra []string
	if target.cfg != nil {
		extra = append(extra, target.cfg.Daemon.ObservePaths...)
	}
	if dir := nudgeTargetClaudeConfigDir(target); dir != "" {
		extra = append(extra, filepath.Join(dir, "projects"))
	}
	return workertranscript.DiscoverKeyedPath(worker.MergeSearchPaths(extra), "claude", workDir, key)
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
