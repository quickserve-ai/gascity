package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/spf13/cobra"
)

// sessionHistoryTarget is a resolved agent whose past conversations can be
// listed and resumed: the identity gc commands address it by, the work
// directory its transcripts are partitioned under, and its transcript
// provider family.
type sessionHistoryTarget struct {
	identifier string
	workDir    string
	provider   string
	// configured is true when workDir came from config (a named session or a
	// non-pool agent), so its projects folder is this agent's own.
	configured bool
}

// newSessionHistoryCmd creates the "gc session history <agent>" command.
func newSessionHistoryCmd(stdout, stderr io.Writer) *cobra.Command {
	var limit int
	var jsonOutput bool
	var archiveRoot string
	cmd := &cobra.Command{
		Use:   "history <agent>",
		Short: "List an agent's past conversations",
		Long: `List the past provider conversations recorded for an agent, newest first.

The filesystem is the authoritative index: claude-family providers write one
transcript per conversation under <config-dir>/projects/<workdir-slug>/, so
every conversation an agent ever had in its work directory is listed — including
conversations whose session beads were closed long ago, and transcripts moved
into ~/.claude-transcript-archive/ by the transcript reaper (marked "archived").

The session id shown for each row is what "gc session resume" takes.`,
		Example: `  gc session history lana
  gc session history qcore/lana --limit 50
  gc session history mayor --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionHistory(args[0], limit, jsonOutput, archiveRoot, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum conversations to list (0 = all)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "JSON output")
	cmd.Flags().StringVar(&archiveRoot, "archive-root", "", "transcript archive root to search (default ~/.claude-transcript-archive)")
	return cmd
}

// newSessionResumeCmd creates the "gc session resume <agent>" command.
func newSessionResumeCmd(stdout, stderr io.Writer) *cobra.Command {
	var last bool
	var printOnly bool
	var archiveRoot string
	cmd := &cobra.Command{
		Use:   "resume <agent> [session-id]",
		Short: "Resume one of an agent's past conversations",
		Long: `Put an agent back into a past conversation listed by "gc session history".

By default this seeds the agent's session bead with the chosen conversation id
and requests a wake, so the reconciler's normal resume path reopens that exact
conversation — including for wake_mode=fresh agents that never auto-resume, and
for on-demand crew whose context is normally lost on idle-close. The agent must
not be running (attach to a running agent instead, or use --print).

--print skips all state changes and prints the provider command for an attended
dive in your own terminal.

Transcripts that the reaper moved into the archive are restored into the live
projects directory first, so the provider can find them again.

session-id may be any unambiguous prefix of an id from "gc session history".
Note: "gc session pin" (pin_awake) prevents the idle-close that loses context
in the first place — resume is the after-the-fact escape hatch.`,
		Example: `  gc session resume lana --last
  gc session resume lana 8dc18f31
  gc session resume mayor 9e82b97a --print`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSessionResume(args, last, printOnly, archiveRoot, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeSessionIDs,
	}
	cmd.Flags().BoolVar(&last, "last", false, "resume the most recent conversation")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the provider resume command instead of seeding the session bead")
	cmd.Flags().StringVar(&archiveRoot, "archive-root", "", "transcript archive root to search (default ~/.claude-transcript-archive)")
	return cmd
}

// resolveSessionHistoryTarget maps an agent identifier to its work directory
// and transcript provider. Configured identities (named sessions, agents)
// resolve without any bead state; arbitrary session ids/aliases fall back to
// the stored session bead.
func resolveSessionHistoryTarget(cityPath string, cfg *config.City, store beads.Store, identifier string) (sessionHistoryTarget, error) {
	if cfg != nil {
		normalized := normalizeNamedSessionTarget(identifier)
		cityName := loadedCityName(cfg, cityPath)
		if spec, ok, _ := findNamedSessionSpecForTarget(cfg, cityName, normalized); ok && spec.Agent != nil {
			workDirQualifiedName := workdirutil.SessionQualifiedName(cityPath, *spec.Agent, cfg.Rigs, spec.Identity, "")
			workDir, err := resolveWorkDirForQualifiedName(cityPath, cfg, spec.Agent, workDirQualifiedName)
			if err == nil && strings.TrimSpace(workDir) != "" {
				return sessionHistoryTarget{
					identifier: spec.Identity,
					workDir:    workDir,
					provider:   historyProviderForAgent(cfg, spec.Agent),
					configured: true,
				}, nil
			}
		}
		for i := range cfg.Agents {
			agentCfg := cfg.Agents[i]
			if agentCfg.SupportsInstanceExpansion() || strings.TrimSpace(agentCfg.QualifiedName()) != normalized {
				continue
			}
			workDir, err := resolveWorkDir(cityPath, cfg, &agentCfg)
			if err == nil && strings.TrimSpace(workDir) != "" {
				return sessionHistoryTarget{
					identifier: normalized,
					workDir:    workDir,
					provider:   historyProviderForAgent(cfg, &agentCfg),
					configured: true,
				}, nil
			}
		}
	}
	if store != nil {
		if logCtx, ok := resolveSessionLogContext(cityPath, cfg, sessionFrontDoor(store), identifier); ok {
			return sessionHistoryTarget{
				identifier: identifier,
				workDir:    logCtx.workDir,
				provider:   logCtx.provider,
			}, nil
		}
	}
	return sessionHistoryTarget{}, fmt.Errorf("agent %q not found", identifier)
}

// historyProviderForAgent resolves the transcript provider family for a
// configured agent, preferring the resolved provider's builtin ancestor and
// falling back to the raw provider name.
func historyProviderForAgent(cfg *config.City, agentCfg *config.Agent) string {
	resolved, err := config.ResolveProvider(agentCfg, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err == nil {
		if family := sessionTranscriptProvider(resolved, sessionpkg.Info{}); family != "" {
			return family
		}
	}
	return strings.TrimSpace(agentCfg.Provider)
}

// historyProviderSupported reports whether transcript history is available
// for the provider family: claude-family transcripts are keyed one file per
// conversation under the workdir slug, which is what makes the listing (and
// resume-by-id) possible at all.
func historyProviderSupported(provider string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	return p == "" || strings.Contains(p, "claude")
}

func historyArchiveRoots(archiveRoot string) []string {
	if strings.TrimSpace(archiveRoot) != "" {
		return []string{archiveRoot}
	}
	return worker.DefaultTranscriptArchiveRoots()
}

// liveSessionKeysForWorkDir maps session_key -> session bead id for every
// non-closed session bead bound to workDir, so history can mark which
// transcript is the agent's current conversation.
func liveSessionKeysForWorkDir(store beads.Store, workDir string) map[string]string {
	live := make(map[string]string)
	if store == nil || strings.TrimSpace(workDir) == "" {
		return live
	}
	found, err := sessionFrontDoor(store).ListByMetadataInfos(map[string]string{"work_dir": workDir}, 0)
	if err != nil {
		return live
	}
	for _, info := range found {
		if !sessionLogFallbackCandidateLive(info) {
			continue
		}
		if key := strings.TrimSpace(info.SessionKey); key != "" {
			live[key] = info.ID
		}
	}
	return live
}

// liveSessionKeysForScope extends liveSessionKeysForWorkDir to every recorded
// cwd in scope, plus the resolved session bead itself when it is live.
func liveSessionKeysForScope(store beads.Store, target sessionHistoryTarget, scope sessionHistoryScope) map[string]string {
	live := liveSessionKeysForWorkDir(store, target.workDir)
	for _, dir := range scope.workDirs {
		for key, id := range liveSessionKeysForWorkDir(store, dir) {
			live[key] = id
		}
	}
	if scope.hasInfo && sessionLogFallbackCandidateLive(scope.info) {
		if key := strings.TrimSpace(scope.info.SessionKey); key != "" {
			live[key] = scope.info.ID
		}
	}
	return live
}

type sessionHistoryJSON struct {
	SchemaVersion string                  `json:"schema_version"`
	OK            bool                    `json:"ok"`
	Command       string                  `json:"command"`
	Agent         string                  `json:"agent"`
	WorkDir       string                  `json:"work_dir"`
	Total         int                     `json:"total"`
	Conversations []sessionHistoryRowJSON `json:"conversations"`
}

type sessionHistoryRowJSON struct {
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	// FoundUnder is the work dir of the projects folder the transcript was
	// found under: the cwd a provider resume of it must run in.
	FoundUnder string `json:"found_under,omitempty"`
	Archived   bool   `json:"archived"`
	Live       bool   `json:"live"`
	LiveBead   string `json:"live_bead,omitempty"`
	Title      string `json:"title,omitempty"`
	AgentName  string `json:"agent_name,omitempty"`
	FirstUser  string `json:"first_user,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	LastAt     string `json:"last_at"`
	SizeBytes  int64  `json:"size_bytes"`
}

func cmdSessionHistory(identifier string, limit int, jsonOutput bool, archiveRoot string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session history: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc session history: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	store, _ := tryOpenCityStore()

	target, err := resolveSessionHistoryTarget(cityPath, cfg, store, identifier)
	if err != nil {
		fmt.Fprintf(stderr, "gc session history: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !historyProviderSupported(target.provider) {
		fmt.Fprintf(stderr, "gc session history: provider %q does not record per-conversation transcripts that history can list (claude-family only)\n", target.provider) //nolint:errcheck // best-effort stderr
		return 1
	}

	searchPaths := worker.MergeSearchPaths(cfg.Daemon.ObservePaths)
	archiveRoots := historyArchiveRoots(archiveRoot)
	scope := resolveSessionHistoryScope(cityPath, cfg, store, identifier, target)
	entries := listSessionHistory(target, scope, searchPaths, archiveRoots)
	if len(entries) == 0 {
		fmt.Fprintf(stderr, "gc session history: no conversations found for %q (work dir %s)\n", target.identifier, target.workDir) //nolint:errcheck // best-effort stderr
		return 1
	}
	total := len(entries)
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	live := liveSessionKeysForScope(store, target, scope)

	if jsonOutput {
		rows := make([]sessionHistoryRowJSON, 0, len(entries))
		for _, e := range entries {
			summary := worker.ReadClaudeTranscriptSummary(e.Path)
			row := sessionHistoryRowJSON{
				SessionID:  e.SessionID,
				Path:       e.Path,
				FoundUnder: e.foundUnder,
				Archived:   e.Archived,
				Title:      summary.Title,
				AgentName:  summary.AgentName,
				FirstUser:  summary.FirstUser,
				LastAt:     e.ModTime.UTC().Format(time.RFC3339),
				SizeBytes:  e.Size,
			}
			if !summary.FirstSeen.IsZero() {
				row.StartedAt = summary.FirstSeen.UTC().Format(time.RFC3339)
			}
			if beadID, ok := live[e.SessionID]; ok {
				row.Live = true
				row.LiveBead = beadID
			}
			rows = append(rows, row)
		}
		payload := sessionHistoryJSON{
			SchemaVersion: "1",
			OK:            true,
			Command:       "session history",
			Agent:         target.identifier,
			WorkDir:       target.workDir,
			Total:         total,
			Conversations: rows,
		}
		if err := json.NewEncoder(stdout).Encode(payload); err != nil {
			fmt.Fprintf(stderr, "gc session history: encoding JSON: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "Conversations for %s (%d", target.identifier, total) //nolint:errcheck // best-effort stdout
	if len(entries) < total {
		fmt.Fprintf(stdout, ", showing %d", len(entries)) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(stdout, "):\n") //nolint:errcheck // best-effort stdout
	for _, e := range entries {
		summary := worker.ReadClaudeTranscriptSummary(e.Path)
		marker := " "
		note := ""
		if _, ok := live[e.SessionID]; ok {
			marker = "●"
			note = " [live]"
		} else if e.Archived {
			note = " [archived]"
		}
		title := summary.Title
		if title == "" {
			title = summary.FirstUser
		}
		if title == "" {
			title = summary.AgentName
		}
		if title == "" {
			title = "(untitled)"
		}
		if len(title) > 64 {
			title = title[:61] + "..."
		}
		duration := ""
		if !summary.FirstSeen.IsZero() && e.ModTime.After(summary.FirstSeen) {
			duration = " · " + formatHistoryDuration(e.ModTime.Sub(summary.FirstSeen))
		}
		fmt.Fprintf(stdout, "%s %s  %s%s · %s%s\n    %s\n", //nolint:errcheck // best-effort stdout
			marker,
			e.SessionID,
			e.ModTime.Local().Format("2006-01-02 15:04"),
			duration,
			formatHistorySize(e.Size),
			note,
			title,
		)
	}
	fmt.Fprintf(stdout, "\nResume one with: gc session resume %s <session-id>\n", target.identifier) //nolint:errcheck // best-effort stdout
	return 0
}

func formatHistoryDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

func formatHistorySize(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%dB", size)
	}
}

func cmdSessionResume(args []string, last, printOnly bool, archiveRoot string, stdout, stderr io.Writer) int {
	identifier := args[0]
	requested := ""
	if len(args) > 1 {
		requested = strings.TrimSpace(args[1])
	}
	if requested == "" && !last {
		fmt.Fprintln(stderr, "gc session resume: pass a session id (from gc session history) or --last") //nolint:errcheck // best-effort stderr
		return 1
	}
	if requested != "" && last {
		fmt.Fprintln(stderr, "gc session resume: --last and an explicit session id are mutually exclusive") //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc session resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc session resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	store, _ := tryOpenCityStore()

	target, err := resolveSessionHistoryTarget(cityPath, cfg, store, identifier)
	if err != nil {
		fmt.Fprintf(stderr, "gc session resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !historyProviderSupported(target.provider) {
		fmt.Fprintf(stderr, "gc session resume: provider %q transcripts cannot be resumed by id (claude-family only)\n", target.provider) //nolint:errcheck // best-effort stderr
		return 1
	}

	searchPaths := worker.MergeSearchPaths(cfg.Daemon.ObservePaths)
	entries := worker.ListClaudeSessionHistory(searchPaths, historyArchiveRoots(archiveRoot), target.workDir)
	if len(entries) == 0 {
		fmt.Fprintf(stderr, "gc session resume: no conversations found for %q (work dir %s)\n", target.identifier, target.workDir) //nolint:errcheck // best-effort stderr
		return 1
	}
	live := liveSessionKeysForWorkDir(store, target.workDir)

	var chosen *worker.SessionHistoryEntry
	if last {
		for i := range entries {
			if _, isLive := live[entries[i].SessionID]; isLive {
				continue
			}
			chosen = &entries[i]
			break
		}
		if chosen == nil {
			fmt.Fprintf(stderr, "gc session resume: every recorded conversation for %q is currently live\n", target.identifier) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else {
		var matches []*worker.SessionHistoryEntry
		for i := range entries {
			if strings.HasPrefix(entries[i].SessionID, requested) {
				matches = append(matches, &entries[i])
			}
		}
		switch len(matches) {
		case 0:
			fmt.Fprintf(stderr, "gc session resume: no conversation matching %q — list them with: gc session history %s\n", requested, target.identifier) //nolint:errcheck // best-effort stderr
			return 1
		case 1:
			chosen = matches[0]
		default:
			fmt.Fprintf(stderr, "gc session resume: %q is ambiguous (%d matches) — use more of the id\n", requested, len(matches)) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	if beadID, isLive := live[chosen.SessionID]; isLive && !printOnly {
		fmt.Fprintf(stderr, "gc session resume: conversation %s is already live on %s — attach with: gc session attach %s\n", chosen.SessionID, beadID, target.identifier) //nolint:errcheck // best-effort stderr
		return 1
	}

	// A transcript the reaper archived must be restored into the live
	// projects tree first: both the provider's --resume and the
	// reconciler's stale-resume probe only look at live roots.
	transcriptPath := chosen.Path
	if chosen.Archived {
		restored, err := restoreArchivedTranscript(*chosen)
		if err != nil {
			fmt.Fprintf(stderr, "gc session resume: restoring archived transcript: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		transcriptPath = restored
		fmt.Fprintf(stdout, "Restored archived transcript to %s\n", restored) //nolint:errcheck // best-effort stdout
	}

	if printOnly {
		fmt.Fprintf(stdout, "cd %s && claude --resume %s\n", target.workDir, chosen.SessionID) //nolint:errcheck // best-effort stdout
		return 0
	}

	if store == nil {
		fmt.Fprintln(stderr, "gc session resume: bead store unavailable — use --print for an attended resume") //nolint:errcheck // best-effort stderr
		return 1
	}
	sessionID, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, identifier)
	if err != nil {
		fmt.Fprintf(stderr, "gc session resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sessFront := sessionFrontDoor(store)
	info, err := sessFront.Get(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc session resume: %v\n", err) //nolint:errcheck
		return 1
	}
	if sessionLogFallbackCandidateLive(info) {
		fmt.Fprintf(stderr, "gc session resume: %s is currently running — attach with: gc session attach %s, or use --print for a side-channel dive\n", target.identifier, target.identifier) //nolint:errcheck
		return 1
	}

	patch := sessionpkg.MetadataPatch{
		"session_key":                chosen.SessionID,
		sessionpkg.ResumeSeededKey(): "true",
		"continuation_reset_pending": "",
	}
	sessionpkg.StampPriorSessionKeyInfo(patch, info)
	if setMetaBatch(sessFront, sessionID, patch, stderr) != nil {
		return 1
	}
	if _, err := sessFront.WakeSession(sessionID, time.Now().UTC(), sessionpkg.WakeOpts{}); err != nil {
		if state, conflict := sessionpkg.WakeConflictState(err); conflict {
			fmt.Fprintf(stderr, "gc session resume: session %s is %s\n", sessionID, state) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stderr, "gc session resume: requesting wake: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	_ = pokeController(cityPath)

	fmt.Fprintf(stdout, "Seeded %s (bead %s) with conversation %s — the reconciler will resume it.\n", target.identifier, sessionID, chosen.SessionID) //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Transcript: %s\nAttach with: gc session attach %s\n", transcriptPath, target.identifier)                                      //nolint:errcheck // best-effort stdout
	return 0
}

// restoreArchivedTranscript copies an archived transcript back into the
// primary live projects root under its original slug. Refuses to overwrite:
// an existing live copy wins and is returned as-is.
func restoreArchivedTranscript(entry worker.SessionHistoryEntry) (string, error) {
	roots := worker.DefaultSearchPaths()
	if len(roots) == 0 {
		return "", fmt.Errorf("no live projects root available")
	}
	destDir := filepath.Join(roots[0], entry.Slug)
	dest := filepath.Join(destDir, entry.SessionID+".jsonl")
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(entry.Path)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", err
	}
	return dest, nil
}

// sessionHistoryItem is one listed conversation plus the work directory whose
// projects folder it was found under: the cwd a provider resume must run in.
type sessionHistoryItem struct {
	worker.SessionHistoryEntry
	foundUnder string
}

// sessionHistoryScope is the recorded state that widens a history search past
// the configured work dir. Claude keys each transcript by its process's cwd,
// and a seat's process does not always run in its configured work dir: the
// launch path (resolvePreparedTaskWorkDir) moves it into the work_dir of an
// in-progress task it is assigned, typically a shared worktree. The
// conversation ids on the bead are looked up by id wherever they are; the
// folders of the bead's work dirs and of its tasks' work_dirs are listed, but
// those can be shared with other seats, so only attributable transcripts are
// taken from them.
type sessionHistoryScope struct {
	info    sessionpkg.Info
	hasInfo bool
	// ownWorkDir is true when the target's work dir is its own folder, listed
	// unfiltered as before: configured, or read off a session bead that is not
	// a pool instance and does not lie under .gc/worktrees/. Those two can be
	// worktrees other seats share.
	ownWorkDir bool
	workDirs   []string
	keys       []string
	keySet     map[string]bool
	// names is the agent-name a transcript must record to be taken from a
	// shared folder without its id on the bead: a named session's identity or
	// an alias, never a pool instance's (its agent_name is its template, which
	// every instance records, and it attributes by id only).
	names map[string]bool
}

func (s sessionHistoryScope) attributable(e worker.SessionHistoryEntry) bool {
	if s.keySet[e.SessionID] {
		return true
	}
	if len(s.names) == 0 {
		return false
	}
	return s.names[worker.ReadClaudeTranscriptAgentName(e.Path)]
}

// resolveSessionHistoryScope collects what the session has recorded: the
// conversation ids on its bead (session_key, prior_session_key), its bead's
// work-dir mirrors (work_dir, gc.work_dir, worker_dir), and the work_dir of
// each in-progress task assigned to it (the same assignee set and task lookup
// the launch path uses to pick the process cwd). A pool instance's own work
// dir, and one that lies under .gc/worktrees/, is filtered like the others:
// either can be a worktree other seats share.
func resolveSessionHistoryScope(cityPath string, cfg *config.City, store beads.Store, identifier string, target sessionHistoryTarget) sessionHistoryScope {
	scope := sessionHistoryScope{ownWorkDir: target.configured, keySet: make(map[string]bool), names: make(map[string]bool)}
	seenDirs := make(map[string]bool)
	addDir := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		dir = resolveWorkDirAgainstCity(cityPath, dir)
		key := normalizePathForCompare(dir)
		if seenDirs[key] {
			return
		}
		seenDirs[key] = true
		scope.workDirs = append(scope.workDirs, dir)
	}
	addKey := func(key string) {
		if key = strings.TrimSpace(key); key != "" && !scope.keySet[key] {
			scope.keySet[key] = true
			scope.keys = append(scope.keys, key)
		}
	}

	addDir(target.workDir)
	if store == nil {
		return scope
	}
	assignees := []string{target.identifier}
	if sessionID, err := resolveSessionIDAllowClosedWithConfig(cityPath, cfg, store, identifier); err == nil {
		if info, err := sessionFrontDoor(store).Get(sessionID); err == nil {
			scope.info, scope.hasInfo = info, true
			pool := isPoolManagedSessionInfo(info)
			scope.ownWorkDir = scope.ownWorkDir || (!pool && !underSharedWorktreeRoot(target.workDir))
			for _, name := range []string{info.ConfiguredNamedIdentity, info.Alias} {
				if name = strings.TrimSpace(name); name != "" && !pool {
					scope.names[name] = true
				}
			}
			addDir(info.WorkDir)
			addDir(info.WorkDirCanonical)
			addDir(info.WorkerDir)
			addKey(info.SessionKey)
			if b, err := store.Get(sessionID); err == nil {
				addKey(b.Metadata[sessionpkg.PriorSessionKeyMetadata])
			}
			assignees = append(taskWorkDirAssignees(startCandidate{info: info}, cfg), assignees...)
		}
	}
	for _, dir := range inProgressTaskWorkDirs(cityPath, store, assignees...) {
		addDir(dir)
	}
	return scope
}

// underSharedWorktreeRoot reports whether dir lies under a .gc/worktrees/
// tree, where gc creates the git worktrees tasks run in and several seats can
// share one.
func underSharedWorktreeRoot(dir string) bool {
	return strings.Contains(filepath.ToSlash(filepath.Clean(dir))+"/", "/.gc/worktrees/")
}

// inProgressTaskWorkDirs returns the work_dir of every in-progress task bead
// in the city store assigned to any of assignees, using the per-bead
// resolution the launch path uses but collecting every distinct dir instead of
// stopping at the newest.
func inProgressTaskWorkDirs(cityPath string, store beads.Store, assignees ...string) []string {
	if store == nil {
		return nil
	}
	var dirs []string
	seenAssignee := make(map[string]bool, len(assignees))
	seenDir := make(map[string]bool)
	for _, assignee := range assignees {
		assignee = strings.TrimSpace(assignee)
		if assignee == "" || seenAssignee[assignee] {
			continue
		}
		seenAssignee[assignee] = true
		assigned, err := store.List(beads.ListQuery{
			Assignee: assignee,
			Status:   "in_progress",
			Live:     true,
			TierMode: beads.TierBoth,
			Sort:     beads.SortCreatedDesc,
		})
		if err != nil {
			continue
		}
		for _, b := range assigned {
			if dir := resolveTaskBeadWorkDir(cityPath, store, b); dir != "" && !seenDir[dir] {
				seenDir[dir] = true
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs
}

// listSessionHistory lists the conversations in the target's own work dir's
// projects folder (unfiltered, as before), the attributable ones from every
// other folder in scope, and every copy of each conversation id on the bead
// wherever it was found, deduplicated by session id with live copies preferred
// over archived ones and the newest copy kept among equals.
func listSessionHistory(target sessionHistoryTarget, scope sessionHistoryScope, searchPaths, archiveRoots []string) []sessionHistoryItem {
	byID := make(map[string]sessionHistoryItem)
	record := func(item sessionHistoryItem) {
		prev, ok := byID[item.SessionID]
		switch {
		case !ok:
		case prev.Archived == item.Archived && item.ModTime.After(prev.ModTime):
		case prev.Archived && !item.Archived:
		default:
			return
		}
		byID[item.SessionID] = item
	}
	if scope.ownWorkDir {
		for _, e := range worker.ListClaudeSessionHistory(searchPaths, archiveRoots, target.workDir) {
			record(sessionHistoryItem{SessionHistoryEntry: e, foundUnder: target.workDir})
		}
	}
	for _, dir := range scope.workDirs {
		if scope.ownWorkDir && normalizePathForCompare(dir) == normalizePathForCompare(target.workDir) {
			continue
		}
		for _, e := range worker.ListClaudeSessionHistory(searchPaths, archiveRoots, dir) {
			if scope.attributable(e) {
				record(sessionHistoryItem{SessionHistoryEntry: e, foundUnder: dir})
			}
		}
	}
	knownDirs := append([]string{target.workDir}, scope.workDirs...)
	for _, key := range scope.keys {
		for _, e := range worker.FindClaudeTranscriptsByID(searchPaths, archiveRoots, key) {
			record(sessionHistoryItem{SessionHistoryEntry: e, foundUnder: transcriptFolderWorkDir(e, knownDirs)})
		}
	}
	items := make([]sessionHistoryItem, 0, len(byID))
	for _, item := range byID {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ModTime.After(items[j].ModTime) })
	return items
}

// transcriptFolderWorkDir names the work dir whose projects folder holds e:
// a known dir whose slug is e's folder, else a cwd the transcript records that
// maps to that folder, else "" (the folder's work dir cannot be told).
func transcriptFolderWorkDir(e worker.SessionHistoryEntry, knownDirs []string) string {
	for _, dir := range knownDirs {
		for _, slug := range worker.ClaudeProjectSlugCandidates(dir) {
			if slug == e.Slug {
				return dir
			}
		}
	}
	return worker.ClaudeTranscriptCwdForSlug(e.Path, e.Slug)
}
