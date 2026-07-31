package main

import (
	"encoding/json"
	"fmt"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sling"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type beadWorktreeCandidate struct {
	Rig    string
	BeadID string
	Path   string
}

// discoverBeadWorktreeCandidates finds only the two layouts Gas City creates:
// direct per-bead worktrees and polecat-home/worktrees/<bead-id>. It does not
// perform an unrestricted recursive scan, so main, refinery, staging, and
// arbitrary nested directories never become removal candidates.
func discoverBeadWorktreeCandidates(cityPath string, cfg *config.City, rigName string) []beadWorktreeCandidate {
	if cfg == nil || rigName == "" {
		return nil
	}
	rigRoot := filepath.Join(cityPath, ".gc", "worktrees", rigName)
	protectedHomes := make(map[string]bool, len(cfg.Agents)*2)
	for i := range cfg.Agents {
		protectedHomes[cfg.Agents[i].Name] = true
		protectedHomes[cfg.Agents[i].BindingQualifiedName()] = true
	}
	var candidates []beadWorktreeCandidate
	addChildren := func(parent string) {
		entries, err := os.ReadDir(parent)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			if protectedHomes[entry.Name()] {
				continue
			}
			beadID := extractBeadIDFromWorktreeName(cfg, entry.Name())
			if beadID == "" {
				continue
			}
			if entry.Name() != beadID {
				continue
			}
			path := filepath.Join(parent, entry.Name())
			if !isStrictlyUnderDir(rigRoot, path) {
				continue
			}
			candidates = append(candidates, beadWorktreeCandidate{Rig: rigName, BeadID: beadID, Path: path})
		}
	}

	addChildren(rigRoot)
	polecatRoot := filepath.Join(rigRoot, "polecats")
	homes, err := os.ReadDir(polecatRoot)
	if err != nil {
		return candidates
	}
	for _, home := range homes {
		if !home.IsDir() || home.Type()&os.ModeSymlink != 0 || strings.HasPrefix(home.Name(), ".gascity-worktree-stage.") {
			continue
		}
		addChildren(filepath.Join(polecatRoot, home.Name(), "worktrees"))
	}
	return candidates
}

type beadWorktreeAction string

const (
	beadWorktreeSkip   beadWorktreeAction = "skip"
	beadWorktreeRemove beadWorktreeAction = "remove"
)

type beadWorktreeDecision struct {
	Candidate beadWorktreeCandidate
	Bead      beads.Bead
	Action    beadWorktreeAction
	Reason    string
	Branch    string
}

type beadWorktreeGitProbe interface {
	IsRepo() bool
	HasUncommittedWork() bool
	HasUnpushedCommitsResult() (bool, error)
	HasStashesResult() (bool, error)
	CurrentBranch() (string, error)
	WorktreeRemove(path string, force bool) error
	WorktreeMove(oldPath, newPath string) error
	WorktreeList() ([]git.Worktree, error)
}

var newBeadWorktreeGitProbe = func(path string) beadWorktreeGitProbe { return git.New(path) }

func evaluateBeadWorktreeCandidate(candidate beadWorktreeCandidate, cityPath string, store beads.Store, sp runtime.Provider, wg beadWorktreeGitProbe) beadWorktreeDecision {
	decision := beadWorktreeDecision{Candidate: candidate, Action: beadWorktreeSkip}
	if store == nil {
		decision.Reason = "bead store unavailable"
		return decision
	}
	bead, err := store.Get(candidate.BeadID)
	if err != nil {
		decision.Reason = "bead lookup failed: " + err.Error()
		return decision
	}
	decision.Bead = bead
	if bead.Status != "closed" {
		decision.Reason = "bead status is " + bead.Status
		return decision
	}
	if !safeBeadWorktreePath(cityPath, candidate) {
		decision.Reason = "path is not a real directory strictly contained by the rig worktree root"
		return decision
	}
	sessionName := strings.TrimSpace(bead.Metadata["gc.session_name"])
	if sessionName == "" {
		sessionName = strings.TrimSpace(bead.Metadata["session_name"])
	}
	if sessionName != "" {
		if sp == nil {
			decision.Reason = "owning runtime liveness unavailable: " + sessionName
			return decision
		}
		if sp.IsRunning(sessionName) {
			decision.Reason = "owning runtime is live: " + sessionName
			return decision
		}
	}
	if wg == nil || !wg.IsRepo() {
		decision.Reason = "candidate is not a registered git worktree"
		return decision
	}
	branch, branchErr := wg.CurrentBranch()
	if branchErr != nil {
		decision.Reason = "branch probe failed: " + branchErr.Error()
		return decision
	}
	decision.Branch = branch
	unpushed, err := wg.HasUnpushedCommitsResult()
	if err != nil {
		decision.Reason = "unpushed probe failed: " + err.Error()
		return decision
	}
	stashes, err := wg.HasStashesResult()
	if err != nil {
		decision.Reason = "stash probe failed: " + err.Error()
		return decision
	}
	dirty := wg.HasUncommittedWork()
	rejected := strings.TrimSpace(bead.Metadata["rejection_reason"]) != "" || strings.TrimSpace(bead.Metadata["gc.rejection_reason"]) != ""
	resumePending := strings.TrimSpace(bead.Metadata["resume_pending"]) != "" || strings.TrimSpace(bead.Metadata["gc.resume_pending"]) != ""
	if dirty || unpushed || stashes || rejected || resumePending {
		decision.Reason = fmt.Sprintf("preserve checkout: dirty=%v unpushed=%v stashes=%v rejected=%v resume_pending=%v", dirty, unpushed, stashes, rejected, resumePending)
		return decision
	}
	decision.Action = beadWorktreeRemove
	decision.Reason = "closed and safe to remove"
	return decision
}

func safeBeadWorktreePath(cityPath string, candidate beadWorktreeCandidate) bool {
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

func registeredBeadWorktrees(wg beadWorktreeGitProbe) (map[string]string, error) {
	worktrees, err := wg.WorktreeList()
	if err != nil {
		return nil, err
	}
	registered := make(map[string]string, len(worktrees))
	for _, worktree := range worktrees {
		canonical, err := filepath.EvalSymlinks(worktree.Path)
		if err != nil {
			continue
		}
		registered[canonical] = strings.TrimPrefix(worktree.Branch, "refs/heads/")
	}
	return registered, nil
}

func branchMatchesBead(cfg *config.City, branch, beadID string) bool {
	if branch == "" || branch == "HEAD" {
		return false
	}
	return beadIDFromBranch(cfg, branch) == beadID
}

// reapClosedBeadWorktrees scans per-bead git worktrees under
// cityPath/.gc/worktrees/<rig>/ and removes any that are associated with a
// closed bead and pass all safety gates (no uncommitted work, no unpushed
// commits, no stashes). Named session home directories are never removed.
// Returns the number of worktrees successfully removed.
func reapClosedBeadWorktrees(
	cityPath string,
	cfg *config.City,
	rigBeadStores map[string]beads.Store,
	sp runtime.Provider,
	rec events.Recorder,
	stderr io.Writer,
	dryRun bool,
	sessionSnapshotAvailable bool,
	activeSessions ...sessionpkg.Info,
) int {
	if stderr == nil {
		stderr = io.Discard
	}
	if rec == nil {
		rec = events.Discard
	}
	if cfg == nil || len(rigBeadStores) == 0 {
		return 0
	}

	reaped := 0
	for rigName, store := range rigBeadStores {
		if store == nil {
			continue
		}
		rigRoot := configuredRigRoot(cityPath, cfg, rigName)
		if rigRoot == "" {
			fmt.Fprintf(stderr, "reapClosedBeadWorktrees: skipping rig %s: owning rig root unresolved\n", rigName) //nolint:errcheck
			continue
		}
		rigGit := newBeadWorktreeGitProbe(rigRoot)
		registered, registrationErr := registeredBeadWorktrees(rigGit)
		for _, candidate := range discoverBeadWorktreeCandidates(cityPath, cfg, rigName) {
			if !sessionSnapshotAvailable {
				fmt.Fprintf(stderr, "reapClosedBeadWorktrees: dry-run action=would-skip path=%s bead=%s reason=runtime/session snapshot unavailable\n", candidate.Path, candidate.BeadID) //nolint:errcheck
				continue
			}
			if live, reason := candidateOwnedByActiveSession(candidate, activeSessions, sp); live {
				recordBeadWorktreeSkip(rec, stderr, candidate, reason)
				continue
			}
			if registrationErr != nil {
				recordBeadWorktreeSkip(rec, stderr, candidate, "worktree registration probe failed: "+registrationErr.Error())
				continue
			}
			canonicalCandidate, err := filepath.EvalSymlinks(candidate.Path)
			if err != nil {
				recordBeadWorktreeSkip(rec, stderr, candidate, "candidate canonicalization failed: "+err.Error())
				continue
			}
			registeredBranch, registeredOK := registered[canonicalCandidate]
			if !registeredOK || !branchMatchesBead(cfg, registeredBranch, candidate.BeadID) {
				recordBeadWorktreeSkip(rec, stderr, candidate, "candidate is not authoritatively registered to its bead branch")
				continue
			}
			initialInfo, err := os.Stat(candidate.Path)
			if err != nil {
				recordBeadWorktreeSkip(rec, stderr, candidate, "candidate identity probe failed: "+err.Error())
				continue
			}
			decision := evaluateBeadWorktreeCandidate(candidate, cityPath, store, sp, newBeadWorktreeGitProbe(candidate.Path))
			freshDecision := evaluateBeadWorktreeCandidate(candidate, cityPath, store, sp, newBeadWorktreeGitProbe(candidate.Path))
			if freshDecision.Action != decision.Action || freshDecision.Action != beadWorktreeRemove {
				recordBeadWorktreeSkip(rec, stderr, candidate, "candidate changed before mutation: "+freshDecision.Reason)
				continue
			}
			freshRegistered, freshRegistrationErr := registeredBeadWorktrees(rigGit)
			currentInfo, currentStatErr := os.Stat(candidate.Path)
			currentLstat, currentLstatErr := os.Lstat(candidate.Path)
			if freshRegistrationErr != nil || currentStatErr != nil || currentLstatErr != nil || currentLstat.Mode()&os.ModeSymlink != 0 || !os.SameFile(initialInfo, currentInfo) || freshRegistered[canonicalCandidate] != registeredBranch {
				recordBeadWorktreeSkip(rec, stderr, candidate, "candidate registration or filesystem identity changed before mutation")
				continue
			}
			if dryRun {
				fmt.Fprintf(stderr, "reapClosedBeadWorktrees: dry-run action=would-remove path=%s bead=%s reason=%s\n", candidate.Path, candidate.BeadID, freshDecision.Reason) //nolint:errcheck
				continue
			}
			quarantinePath := candidate.Path + ".gc-reap-" + fmt.Sprintf("%d", time.Now().UnixNano())
			if err := rigGit.WorktreeMove(candidate.Path, quarantinePath); err != nil {
				recordBeadWorktreeSkip(rec, stderr, candidate, "worktree quarantine move failed: "+err.Error())
				continue
			}
			quarantined, quarantineErr := registeredBeadWorktrees(rigGit)
			canonicalQuarantine, canonicalQuarantineErr := filepath.EvalSymlinks(quarantinePath)
			if quarantineErr != nil || canonicalQuarantineErr != nil || quarantined[canonicalQuarantine] != registeredBranch {
				restoreErr := rigGit.WorktreeMove(quarantinePath, candidate.Path)
				fmt.Fprintf(stderr, "reapClosedBeadWorktrees: quarantine verification failed for %s; restore=%v\n", candidate.Path, restoreErr) //nolint:errcheck
				continue
			}
			quarantinedCandidate := candidate
			quarantinedCandidate.Path = quarantinePath
			if live, reason := candidateOwnedByActiveSession(quarantinedCandidate, activeSessions, sp); live {
				restoreErr := rigGit.WorktreeMove(quarantinePath, candidate.Path)
				fmt.Fprintf(stderr, "reapClosedBeadWorktrees: quarantined candidate became unsafe (%s); restore=%v\n", reason, restoreErr) //nolint:errcheck
				continue
			}
			quarantineDecision := evaluateBeadWorktreeCandidate(quarantinedCandidate, cityPath, store, sp, newBeadWorktreeGitProbe(quarantinePath))
			if quarantineDecision.Action != beadWorktreeRemove {
				restoreErr := rigGit.WorktreeMove(quarantinePath, candidate.Path)
				fmt.Fprintf(stderr, "reapClosedBeadWorktrees: quarantined candidate failed final safety check (%s); restore=%v\n", quarantineDecision.Reason, restoreErr) //nolint:errcheck
				continue
			}
			if err := rigGit.WorktreeRemove(quarantinePath, false); err != nil {
				restoreErr := rigGit.WorktreeMove(quarantinePath, candidate.Path)
				fmt.Fprintf(stderr, "reapClosedBeadWorktrees: quarantined %s but removal failed: %v; restore=%v\n", candidate.Path, err, restoreErr) //nolint:errcheck
				continue
			}
			fmt.Fprintf(stderr, "reapClosedBeadWorktrees: removed worktree %s for closed bead %s\n", candidate.Path, candidate.BeadID) //nolint:errcheck
			if raw, err := json.Marshal(events.BeadWorktreeReapedPayload{BeadID: candidate.BeadID, Path: candidate.Path, Rig: rigName, Branch: freshDecision.Branch}); err == nil {
				rec.Record(events.Event{Type: events.BeadWorktreeReaped, Actor: "gc", Subject: candidate.BeadID, Payload: raw})
			}
			reaped++
		}
	}
	return reaped
}

func configuredRigRoot(cityPath string, cfg *config.City, rigName string) string {
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name != rigName {
			continue
		}
		path := strings.TrimSpace(cfg.Rigs[i].Path)
		if path == "" {
			return ""
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(cityPath, path)
		}
		return filepath.Clean(path)
	}
	return ""
}

// candidateOwnedByActiveSession reports whether a reap candidate lives inside
// the work dir of a session that is still live. Takes session.Info rather than
// raw beads: upstream's WI-7 refactor deleted the snapshot's raw-bead surface
// (sessionBeadSnapshot.Open), leaving OpenInfos as the sole domain projection,
// and Info exposes WorkDir/SessionName as typed fields instead of metadata-map
// lookups.
func candidateOwnedByActiveSession(candidate beadWorktreeCandidate, sessions []sessionpkg.Info, sp runtime.Provider) (bool, string) {
	canonicalCandidate, err := filepath.EvalSymlinks(candidate.Path)
	if err != nil {
		return true, "candidate path liveness check failed: " + err.Error()
	}
	for _, session := range sessions {
		workDir := strings.TrimSpace(session.WorkDir)
		if workDir == "" {
			continue
		}
		canonicalWorkDir, err := filepath.EvalSymlinks(workDir)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(canonicalWorkDir, canonicalCandidate)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		sessionName := strings.TrimSpace(session.SessionName)
		if sp == nil || sessionName == "" || sp.IsRunning(sessionName) {
			return true, "candidate belongs to active or unverifiable session path: " + sessionName
		}
	}
	return false, ""
}

func beadWorktreeOwnerIsLive(bead beads.Bead, sp runtime.Provider) bool {
	if sp == nil {
		return false
	}
	sessionName := strings.TrimSpace(bead.Metadata["gc.session_name"])
	if sessionName == "" {
		sessionName = strings.TrimSpace(bead.Metadata["session_name"])
	}
	return sessionName != "" && sp.IsRunning(sessionName)
}

func recordBeadWorktreeSkip(rec events.Recorder, stderr io.Writer, candidate beadWorktreeCandidate, reason string) {
	fmt.Fprintf(stderr, "reapClosedBeadWorktrees: skipping %s (bead %s: %s)\n", candidate.Path, candidate.BeadID, reason) //nolint:errcheck
	if raw, err := json.Marshal(events.BeadWorktreeReapSkippedPayload{BeadID: candidate.BeadID, Path: candidate.Path, Rig: candidate.Rig, Reason: reason}); err == nil {
		rec.Record(events.Event{Type: events.BeadWorktreeReapSkipped, Actor: "gc", Subject: candidate.BeadID, Payload: raw})
	}
}

// extractBeadIDFromWorktreeName scans consecutive dash-separated segment pairs
// in name for one that LooksLikeConfiguredBeadID. Returns the first match, or
// "" if none. Handles names like "builder-ga-34q3ss-pr2738" → "ga-34q3ss" and
// bare "ga-06kfi6" → "ga-06kfi6".
func extractBeadIDFromWorktreeName(cfg *config.City, name string) string {
	if name == "" || cfg == nil {
		return ""
	}
	parts := strings.Split(name, "-")
	for i := 0; i+1 < len(parts); i++ {
		candidate := parts[i] + "-" + parts[i+1]
		if sling.LooksLikeConfiguredBeadID(cfg, candidate) {
			return candidate
		}
	}
	return ""
}

// isStrictlyUnderDir reports whether path is strictly contained within dir
// (i.e., it is not dir itself and has dir as a prefix component).
func isStrictlyUnderDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..")
}
