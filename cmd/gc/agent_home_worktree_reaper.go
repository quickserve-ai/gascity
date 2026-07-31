package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type stoppedAgentHomeCandidate struct {
	Rig          string
	Path         string
	OriginalPath string
	SizeBytes    int64
	Session      beads.Bead
	Owners       []beads.Bead
}

type stoppedAgentHomeAction string

const (
	stoppedAgentHomeSkip   stoppedAgentHomeAction = "skip"
	stoppedAgentHomeRemove stoppedAgentHomeAction = "remove"
)

type stoppedAgentHomeDecision struct {
	Candidate stoppedAgentHomeCandidate
	Action    stoppedAgentHomeAction
	Reason    string
	Branch    string
	Gates     []string
}

func (d *stoppedAgentHomeDecision) gate(name string, passed bool) {
	result := "fail"
	if passed {
		result = "pass"
	}
	d.Gates = append(d.Gates, name+"="+result)
}

func (d stoppedAgentHomeDecision) gateSummary() string {
	return strings.Join(d.Gates, ",")
}

var newStoppedAgentHomeGitProbe = func(path string) beadWorktreeGitProbe { return git.New(path) }

const stoppedAgentHomeHistoryLimit = 4096

func loadConfiguredStoppedAgentHomeHistory(cfg *config.City, store beads.Store) ([]beads.Bead, error) {
	if cfg == nil || store == nil {
		return nil, fmt.Errorf("configured agent-home history store unavailable")
	}
	filters := make([]map[string]string, 0, len(cfg.NamedSessions)+len(cfg.Agents))
	for i := range cfg.NamedSessions {
		if identity := strings.TrimSpace(cfg.NamedSessions[i].QualifiedName()); identity != "" {
			filters = append(filters, map[string]string{namedSessionIdentityMetadata: identity})
		}
	}
	for i := range cfg.Agents {
		for _, name := range cfg.Agents[i].NamepoolNames {
			if alias := strings.TrimSpace(cfg.Agents[i].QualifiedInstanceName(name)); alias != "" {
				filters = append(filters, map[string]string{"alias": alias})
			}
		}
	}
	return listSessionHistoryByMetadata(store, filters)
}

func loadStoppedAgentHomeCandidateHistory(store beads.Store, candidate stoppedAgentHomeCandidate) ([]beads.Bead, error) {
	if store == nil {
		return nil, fmt.Errorf("candidate session-history store unavailable")
	}
	if len(candidate.Owners) == 0 {
		return nil, fmt.Errorf("candidate owner history unavailable")
	}
	filters := make([]map[string]string, 0, len(candidate.Owners)*2)
	owners := candidate.Owners
	for _, owner := range owners {
		if identity := strings.TrimSpace(owner.Metadata[namedSessionIdentityMetadata]); identity != "" {
			filters = append(filters, map[string]string{namedSessionIdentityMetadata: identity})
		}
		if alias := strings.TrimSpace(owner.Metadata["alias"]); alias != "" {
			filters = append(filters, map[string]string{"alias": alias})
		}
	}
	if len(filters) == 0 {
		return nil, fmt.Errorf("candidate owner metadata filters unavailable")
	}
	history, err := listSessionHistoryByMetadata(store, filters)
	if err != nil {
		return nil, err
	}
	if len(history) == 0 {
		return nil, fmt.Errorf("candidate owner history disappeared")
	}
	return history, nil
}

func listSessionHistoryByMetadata(store beads.Store, filters []map[string]string) ([]beads.Bead, error) {
	seen := make(map[string]bool)
	var sessions []beads.Bead
	for _, filter := range filters {
		rows, err := store.ListByMetadata(filter, stoppedAgentHomeHistoryLimit+1, beads.IncludeClosed)
		if err != nil {
			return nil, err
		}
		if len(rows) > stoppedAgentHomeHistoryLimit {
			return nil, fmt.Errorf("agent-home history limit exceeded for %v", filter)
		}
		for _, row := range rows {
			if seen[row.ID] || !sessionpkg.IsSessionBeadOrRepairable(row) {
				continue
			}
			seen[row.ID] = true
			sessions = append(sessions, row)
		}
	}
	return sessions, nil
}

func discoverStoppedAgentHomeCandidates(cityPath string, cfg *config.City, sessions []beads.Bead) []stoppedAgentHomeCandidate {
	if cfg == nil {
		return nil
	}
	root := filepath.Join(cityPath, ".gc", "worktrees")
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}
	var candidates []stoppedAgentHomeCandidate
	indexByPath := make(map[string]int)
	for _, session := range sessions {
		if session.Status != "closed" {
			continue
		}
		workDir := strings.TrimSpace(session.Metadata["work_dir"])
		if workDir == "" || !filepath.IsAbs(workDir) || !pathutil.PathWithin(root, workDir) || pathutil.SamePath(root, workDir) {
			continue
		}
		canonical, err := filepath.EvalSymlinks(workDir)
		if err != nil || !isStrictlyUnderDir(canonicalRoot, canonical) {
			continue
		}
		if index, ok := indexByPath[canonical]; ok {
			candidates[index].Owners = append(candidates[index].Owners, session)
			if sessionUsesDisposableConfiguredHome(cityPath, cfg, session) && !sessionUsesDisposableConfiguredHome(cityPath, cfg, candidates[index].Session) {
				candidates[index].Session = session
			}
			continue
		}
		rel, err := filepath.Rel(canonicalRoot, canonical)
		if err != nil {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		info, err := os.Lstat(canonical)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		indexByPath[canonical] = len(candidates)
		candidates = append(candidates, stoppedAgentHomeCandidate{
			Rig: parts[0], Path: canonical, Session: session, Owners: []beads.Bead{session},
		})
	}
	eligible := candidates[:0]
	for _, candidate := range candidates {
		if sessionUsesDisposableConfiguredHome(cityPath, cfg, candidate.Session) {
			eligible = append(eligible, candidate)
		}
	}
	return eligible
}

func sessionUsesDisposableConfiguredHome(cityPath string, cfg *config.City, session beads.Bead) bool {
	if cfg == nil {
		return false
	}
	workDir := strings.TrimSpace(session.Metadata["work_dir"])
	if workDir == "" {
		return false
	}
	template := normalizedSessionTemplate(session, cfg)
	if template == "" {
		template = strings.TrimSpace(session.Metadata["template"])
	}
	agent := findAgentByTemplate(cfg, template)
	if agent == nil {
		return false
	}
	qualifiedIdentity := ""
	if isNamedSessionBead(session) {
		identity := namedSessionIdentity(session)
		if identity == "" || !configuredNamedSessionBeadHasSpec(session, cfg, cfg.Workspace.Name) {
			return false
		}
		qualifiedIdentity = identity
	} else {
		if strings.TrimSpace(session.Metadata["pool_slot"]) == "" || len(agent.NamepoolNames) == 0 {
			return false
		}
		aliasBase := targetBasename(session.Metadata["alias"])
		name := aliasBase
		if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
			name = name[dot+1:]
		}
		configured := false
		for _, allowed := range agent.NamepoolNames {
			if name == allowed {
				configured = true
				break
			}
		}
		if !configured {
			return false
		}
		qualifiedIdentity = session.Metadata["alias"]
	}
	expected, err := resolveWorkDirForQualifiedName(cityPath, cfg, agent, qualifiedIdentity)
	return err == nil && sameCanonicalPath(expected, workDir)
}

func directorySizeBytes(root string) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, statErr := entry.Info()
			if statErr != nil {
				return statErr
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func evaluateStoppedAgentHomeCandidate(
	cityPath string,
	cfg *config.City,
	candidate stoppedAgentHomeCandidate,
	runningSessions map[string]bool,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	activeSessions []beads.Bead,
	probe beadWorktreeGitProbe,
	registered []git.Worktree,
) stoppedAgentHomeDecision {
	decision := stoppedAgentHomeDecision{Candidate: candidate, Action: stoppedAgentHomeSkip}
	owners := candidate.Owners
	if len(owners) == 0 {
		decision.gate("runtime", false)
		decision.Reason = "owner history unavailable"
		return decision
	}
	assignmentIDs := make([]string, 0, len(owners)*6)
	for _, owner := range owners {
		if owner.Status != "closed" {
			decision.gate("runtime", false)
			decision.Reason = "historical owner session bead is not closed: " + owner.ID
			return decision
		}
		sessionName := strings.TrimSpace(owner.Metadata["session_name"])
		if sessionName == "" {
			decision.gate("runtime", false)
			decision.Reason = "runtime session identity unavailable for owner: " + owner.ID
			return decision
		}
		if runningSessions[sessionName] {
			decision.gate("runtime", false)
			decision.Reason = "runtime session is live: " + sessionName
			return decision
		}
		assignmentIDs = append(assignmentIDs, owner.ID, owner.Metadata["session_name"], owner.Metadata[namedSessionIdentityMetadata], owner.Metadata["configured_named_identity"], owner.Metadata["alias"], owner.Assignee)
	}
	decision.gate("runtime", true)
	for _, active := range activeSessions {
		activePath := strings.TrimSpace(active.Metadata["work_dir"])
		if active.Status == "closed" || activePath == "" {
			continue
		}
		overlaps, known := canonicalPathsOverlap(activePath, stoppedAgentHomeOwnershipPath(candidate))
		if !known {
			decision.gate("ownership", false)
			decision.Reason = "active session path ownership is unverifiable: " + active.ID
			return decision
		}
		if overlaps {
			decision.gate("ownership", false)
			decision.Reason = "candidate overlaps active session path: " + active.ID
			return decision
		}
	}
	decision.gate("ownership", true)
	if cityStore == nil {
		decision.gate("assignments", false)
		decision.Reason = "assignment snapshot unavailable: city bead store unavailable"
		return decision
	}
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if store, ok := rigStores[rig.Name]; !ok || store == nil {
			decision.gate("assignments", false)
			decision.Reason = "rig store snapshot incomplete: " + rig.Name
			return decision
		}
	}
	hasAssigned, err := sessionHasOpenAssignedWorkInStores(cityStore, rigStores, compactSessionAssignmentIdentifiers(assignmentIDs))
	if err != nil {
		decision.gate("assignments", false)
		decision.Reason = "assignment probe failed: " + err.Error()
		return decision
	}
	if hasAssigned {
		decision.gate("assignments", false)
		decision.Reason = "session still has assigned work"
		return decision
	}
	decision.gate("assignments", true)
	if !safeStoppedAgentHomePath(cityPath, candidate) {
		decision.gate("containment", false)
		decision.Reason = "path is not a real directory strictly contained by the rig worktree root"
		return decision
	}
	decision.gate("containment", true)
	if probe == nil || !probe.IsRepo() {
		decision.gate("registration", false)
		decision.Reason = "candidate is not a registered git worktree"
		return decision
	}
	branch, err := probe.CurrentBranch()
	if err != nil {
		decision.gate("registration", false)
		decision.Reason = "branch probe failed: " + err.Error()
		return decision
	}
	decision.Branch = branch
	registeredSelf := false
	for _, worktree := range registered {
		canonical, evalErr := filepath.EvalSymlinks(worktree.Path)
		if evalErr != nil {
			decision.gate("registration", false)
			decision.Reason = "registered worktree path is unverifiable: " + worktree.Path
			return decision
		}
		if sameCanonicalPath(canonical, candidate.Path) {
			registeredSelf = true
			continue
		}
		if canonicalPathWithin(candidate.Path, canonical) {
			decision.gate("registration", true)
			decision.gate("nested", false)
			decision.Reason = "candidate contains nested registered worktree: " + canonical
			return decision
		}
	}
	if !registeredSelf {
		decision.gate("registration", false)
		decision.Reason = "candidate is not authoritatively registered"
		return decision
	}
	decision.gate("registration", true)
	decision.gate("nested", true)
	if probe.HasUncommittedWork() {
		decision.gate("dirty", false)
		decision.Reason = "candidate has uncommitted changes"
		return decision
	}
	decision.gate("dirty", true)
	unpushed, err := probe.HasUnpushedCommitsResult()
	if err != nil {
		decision.gate("unpushed", false)
		decision.Reason = "unpushed probe failed: " + err.Error()
		return decision
	}
	if unpushed {
		decision.gate("unpushed", false)
		decision.Reason = "candidate has unpushed commits"
		return decision
	}
	decision.gate("unpushed", true)
	stashed, err := probe.HasStashesResult()
	if err != nil {
		decision.gate("stash", false)
		decision.Reason = "stash probe failed: " + err.Error()
		return decision
	}
	if stashed {
		decision.gate("stash", false)
		decision.Reason = "candidate has stashed work"
		return decision
	}
	decision.gate("stash", true)
	decision.Action = stoppedAgentHomeRemove
	decision.Reason = "closed, stopped, unassigned, and git-safe"
	return decision
}

func stoppedAgentHomeOwnershipPath(candidate stoppedAgentHomeCandidate) string {
	if candidate.OriginalPath != "" {
		return candidate.OriginalPath
	}
	return candidate.Path
}

func sameCanonicalPath(a, b string) bool {
	ca, errA := filepath.EvalSymlinks(a)
	cb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && pathutil.SamePath(ca, cb)
}

func canonicalPathsOverlap(a, b string) (bool, bool) {
	ca, errA := filepath.EvalSymlinks(a)
	cb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false, false
	}
	return pathutil.PathWithin(ca, cb) || pathutil.PathWithin(cb, ca), true
}

func canonicalPathWithin(parent, child string) bool {
	cp, errParent := filepath.EvalSymlinks(parent)
	cc, errChild := filepath.EvalSymlinks(child)
	return errParent == nil && errChild == nil && pathutil.PathWithin(cp, cc) && !pathutil.SamePath(cp, cc)
}

func safeStoppedAgentHomePath(cityPath string, candidate stoppedAgentHomeCandidate) bool {
	rigRoot := filepath.Join(cityPath, ".gc", "worktrees", candidate.Rig)
	info, err := os.Lstat(candidate.Path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	canonicalRoot, err := filepath.EvalSymlinks(rigRoot)
	if err != nil {
		return false
	}
	canonicalPath, err := filepath.EvalSymlinks(candidate.Path)
	if err != nil {
		return false
	}
	return isStrictlyUnderDir(canonicalRoot, canonicalPath)
}

func reapStoppedAgentHomeWorktrees(
	cityPath string,
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	sp runtime.Provider,
	rec events.Recorder,
	stderr io.Writer,
	dryRun bool,
	sessionSnapshotAvailable bool,
	sessions []beads.Bead,
	activeSnapshots ...[]beads.Bead,
) int {
	if stderr == nil {
		stderr = io.Discard
	}
	if rec == nil {
		rec = events.Discard
	}
	if cfg == nil {
		return 0
	}
	candidates := discoverStoppedAgentHomeCandidates(cityPath, cfg, sessions)
	activeSessions := sessions
	if len(activeSnapshots) > 0 {
		activeSessions = activeSnapshots[0]
	}
	if dryRun {
		for i := range candidates {
			size, err := directorySizeBytes(candidates[i].Path)
			if err != nil {
				continue
			}
			candidates[i].SizeBytes = size
		}
	}
	if !sessionSnapshotAvailable {
		for _, candidate := range candidates {
			fmt.Fprintf(stderr, "reapStoppedAgentHomes: action=would-skip path=%s session=%s size_bytes=%d reason=runtime/session snapshot unavailable\n", candidate.Path, candidate.Session.ID, candidate.SizeBytes) //nolint:errcheck
		}
		return 0
	}
	if sp == nil {
		for _, candidate := range candidates {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "runtime snapshot unavailable: provider unavailable")
		}
		return 0
	}
	runningNames, runtimeErr := sp.ListRunning("")
	if runtimeErr != nil {
		for _, candidate := range candidates {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "runtime snapshot unavailable: "+runtimeErr.Error())
		}
		return 0
	}
	runningSessions := make(map[string]bool, len(runningNames))
	for _, name := range runningNames {
		runningSessions[name] = true
	}
	removed := 0
	for _, candidate := range candidates {
		rigRoot := configuredRigRoot(cityPath, cfg, candidate.Rig)
		if rigRoot == "" {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "owning rig root unresolved")
			continue
		}
		rigProbe := newStoppedAgentHomeGitProbe(rigRoot)
		registered, err := rigProbe.WorktreeList()
		if err != nil {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "worktree registration probe failed: "+err.Error())
			continue
		}
		initialInfo, err := os.Stat(candidate.Path)
		if err != nil {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "candidate identity probe failed: "+err.Error())
			continue
		}
		decision := evaluateStoppedAgentHomeCandidate(cityPath, cfg, candidate, runningSessions, cityStore, rigStores, activeSessions, newStoppedAgentHomeGitProbe(candidate.Path), registered)
		if decision.Action != stoppedAgentHomeRemove {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, decision.Reason+" gates="+decision.gateSummary())
			continue
		}
		freshSessions, sessionErr := loadStoppedAgentHomeCandidateHistory(cityStore, candidate)
		if sessionErr != nil {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "fresh session history unavailable: "+sessionErr.Error())
			continue
		}
		freshActiveSnapshot, activeErr := loadSessionBeadSnapshot(cityStore)
		if activeErr != nil || freshActiveSnapshot.LoadError() != nil {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "fresh active-session snapshot unavailable")
			continue
		}
		freshCandidate, found := stoppedAgentHomeCandidateByPath(discoverStoppedAgentHomeCandidates(cityPath, cfg, freshSessions), candidate.Path)
		if !found {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "candidate no longer eligible after session-history refresh")
			continue
		}
		freshRegistered, err := rigProbe.WorktreeList()
		if err != nil {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "fresh registration probe failed: "+err.Error())
			continue
		}
		freshRunningNames, runtimeErr := sp.ListRunning("")
		if runtimeErr != nil {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "fresh runtime snapshot unavailable: "+runtimeErr.Error())
			continue
		}
		freshRunningSessions := make(map[string]bool, len(freshRunningNames))
		for _, name := range freshRunningNames {
			freshRunningSessions[name] = true
		}
		fresh := evaluateStoppedAgentHomeCandidate(cityPath, cfg, freshCandidate, freshRunningSessions, cityStore, rigStores, freshActiveSnapshot.Open(), newStoppedAgentHomeGitProbe(candidate.Path), freshRegistered)
		currentInfo, statErr := os.Stat(candidate.Path)
		if fresh.Action != stoppedAgentHomeRemove || statErr != nil || !os.SameFile(initialInfo, currentInfo) {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "candidate changed before mutation: "+fresh.Reason)
			continue
		}
		if dryRun {
			fmt.Fprintf(stderr, "reapStoppedAgentHomes: action=would-remove path=%s session=%s size_bytes=%d gates=%s reason=%s\n", candidate.Path, candidate.Session.ID, candidate.SizeBytes, fresh.gateSummary(), fresh.Reason) //nolint:errcheck
			continue
		}
		sizeBytes, sizeErr := directorySizeBytes(candidate.Path)
		if sizeErr != nil {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "size probe failed: "+sizeErr.Error())
			continue
		}
		candidate.SizeBytes = sizeBytes
		quarantinePath := candidate.Path + ".gc-reap-" + fmt.Sprintf("%d", time.Now().UnixNano())
		if err := rigProbe.WorktreeMove(candidate.Path, quarantinePath); err != nil {
			recordStoppedAgentHomeSkip(rec, stderr, candidate, "worktree quarantine move failed: "+err.Error())
			continue
		}
		quarantinedRegistered, registrationErr := rigProbe.WorktreeList()
		quarantinedCandidate := freshCandidate
		quarantinedCandidate.Path = quarantinePath
		quarantinedCandidate.OriginalPath = candidate.Path
		quarantinedCandidate.SizeBytes = candidate.SizeBytes
		latestSessions, latestSessionErr := loadStoppedAgentHomeCandidateHistory(cityStore, candidate)
		latestActiveSnapshot, latestActiveErr := loadSessionBeadSnapshot(cityStore)
		latestRunningNames, latestRuntimeErr := sp.ListRunning("")
		latestRunning := make(map[string]bool, len(latestRunningNames))
		for _, name := range latestRunningNames {
			latestRunning[name] = true
		}
		quarantinedCandidate.Owners = latestSessions
		quarantineDecision := stoppedAgentHomeDecision{Candidate: quarantinedCandidate, Action: stoppedAgentHomeSkip, Reason: "quarantine verification unavailable"}
		if registrationErr == nil && latestSessionErr == nil && latestRuntimeErr == nil && latestActiveErr == nil && latestActiveSnapshot.LoadError() == nil {
			quarantineDecision = evaluateStoppedAgentHomeCandidate(cityPath, cfg, quarantinedCandidate, latestRunning, cityStore, rigStores, latestActiveSnapshot.Open(), newStoppedAgentHomeGitProbe(quarantinePath), quarantinedRegistered)
		}
		if quarantineDecision.Action != stoppedAgentHomeRemove {
			restoreErr := rigProbe.WorktreeMove(quarantinePath, candidate.Path)
			fmt.Fprintf(stderr, "reapStoppedAgentHomes: quarantined candidate became unsafe reason=%s restore=%v\n", quarantineDecision.Reason, restoreErr) //nolint:errcheck
			continue
		}
		if err := rigProbe.WorktreeRemove(quarantinePath, false); err != nil {
			restoreErr := rigProbe.WorktreeMove(quarantinePath, candidate.Path)
			fmt.Fprintf(stderr, "reapStoppedAgentHomes: non-force removal failed path=%s error=%v restore=%v\n", quarantinePath, err, restoreErr) //nolint:errcheck
			continue
		}
		fmt.Fprintf(stderr, "reapStoppedAgentHomes: removed path=%s session=%s size_bytes=%d\n", candidate.Path, candidate.Session.ID, candidate.SizeBytes) //nolint:errcheck
		if raw, err := json.Marshal(events.BeadWorktreeReapedPayload{BeadID: candidate.Session.ID, Path: candidate.Path, Rig: candidate.Rig, Branch: fresh.Branch}); err == nil {
			rec.Record(events.Event{Type: events.BeadWorktreeReaped, Actor: "gc", Subject: candidate.Session.ID, Payload: raw})
		}
		removed++
	}
	return removed
}

func stoppedAgentHomeCandidateByPath(candidates []stoppedAgentHomeCandidate, path string) (stoppedAgentHomeCandidate, bool) {
	for _, candidate := range candidates {
		if sameCanonicalPath(candidate.Path, path) {
			return candidate, true
		}
	}
	return stoppedAgentHomeCandidate{}, false
}

func recordStoppedAgentHomeSkip(rec events.Recorder, stderr io.Writer, candidate stoppedAgentHomeCandidate, reason string) {
	fmt.Fprintf(stderr, "reapStoppedAgentHomes: action=would-skip path=%s session=%s size_bytes=%d reason=%s\n", candidate.Path, candidate.Session.ID, candidate.SizeBytes, reason) //nolint:errcheck
	if raw, err := json.Marshal(events.BeadWorktreeReapSkippedPayload{BeadID: candidate.Session.ID, Path: candidate.Path, Rig: candidate.Rig, Reason: reason}); err == nil {
		rec.Record(events.Event{Type: events.BeadWorktreeReapSkipped, Actor: "gc", Subject: candidate.Session.ID, Payload: raw})
	}
}
