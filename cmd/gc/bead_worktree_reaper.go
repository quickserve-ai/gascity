package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/sling"
)

// reapDecision records one worktree the reaper acted on or declined to act on,
// for the dry-run report and event stream.
type reapDecision struct {
	BeadID string
	Path   string
	Rig    string
	Branch string
	// Reason explains a protected decision (why the worktree was left in
	// place). Empty for a reap/would-reap decision.
	Reason string
}

// reapReport is the outcome of one reapClosedBeadWorktrees pass. Reaped holds
// the worktrees removed (or, in dry-run, the ones that would be removed);
// Protected holds worktrees left in place with the reason (too young/quarantined,
// referenced by a non-terminal bead in another molecule, live process, active
// session, unsafe git state, or an indeterminate age/liveness/borrow-veto scan).
type reapReport struct {
	Reaped    []reapDecision
	Protected []reapDecision
	DryRun    bool
}

// reapClosedBeadWorktrees discovers per-bead git worktrees under
// cityPath/.gc/worktrees/<rig>/ and removes any whose associated bead is closed
// and that pass every safety gate. It returns a reapReport describing what was
// reaped and what was protected.
//
// Discovery is authoritative at any nesting depth: for each rig it runs
// `git worktree list --porcelain` from the rig's own repository (the repo that
// owns these worktrees, per worktree-setup.sh's `git -C <rig> worktree add`),
// rather than a single-level directory scan. Per-bead worktrees are nested
// under agent-home directories (depth-2, sometimes deeper); the old
// os.ReadDir(.gc/worktrees/<rig>/) scan saw only the agent homes and reaped
// nothing (gastownhall/gascity#4492 root cause A).
//
// Safety gates, in order, all fail closed toward keeping the worktree:
//  1. Named agent-home directories are never removed.
//  2. The bead named by the worktree must exist and be closed.
//  3. Freshness quarantine: a worktree younger than
//     cfg.Daemon.AutoReapClosedBeadWorktreesMinAge is exempt, protecting
//     against the race between worktree creation and its owning bead's
//     work-dir metadata being stamped by the next reconcile pass. An
//     indeterminate age (the ".git" pointer file cannot be stat'd) protects.
//  4. Git state: no uncommitted changes, no unpushed commits, no stashes.
//     This is the CHEAP, LOCAL gate (three git subprocesses against the
//     worktree itself), so it runs before the two expensive signals below;
//     a candidate it protects never triggers them (ga-singc6).
//  5. Borrow-veto scan: batched once per rig per tick over the candidates
//     that survived gate 4, this finds any non-terminal bead — in any
//     molecule — whose gc.work_dir/work_dir metadata still points at the
//     worktree's path and protects it if so. It is a full List of the rig's
//     store (for a hub-backed rig, a remote scan of every open issue and
//     wisp). A query error protects every remaining candidate in that rig's
//     tick.
//  6. Liveness: no live process cwd and no active-session working directory may
//     sit at or beneath the worktree. If the liveness scan is indeterminate
//     (no /proc), NOTHING is reaped this pass — the reaper cannot prove any
//     tree is idle (root cause B: closed-bead != end-of-use). The scan is a
//     host-wide process-table enumeration (lsof on darwin) and is gathered
//     LAZILY, at most once per pass, and only when a candidate actually
//     reaches this gate — see the ga-singc6 note at the gather site.
//
// Gate order never changes WHICH worktrees are reaped — a worktree is reaped
// only when every gate passes and protected when any gate protects — it only
// decides which reason is recorded and how much the pass costs. Cheap and
// local runs before expensive and remote.
//
// When dryRun is true the reaper performs all discovery and classification and
// emits bead.worktree.reap_skipped events describing what it would reap and
// what it protected, but removes nothing. liveSessionDirs is the active-session
// working-directory set the liveness gate cross-checks against, alongside the
// authoritative /proc cwd scan.
func reapClosedBeadWorktrees(
	cityPath string,
	cfg *config.City,
	rigBeadStores map[string]beads.Store,
	liveSessionDirs []string,
	dryRun bool,
	rec events.Recorder,
	stderr io.Writer,
) reapReport {
	report := reapReport{DryRun: dryRun}
	if stderr == nil {
		stderr = io.Discard
	}
	if rec == nil {
		rec = events.Discard
	}
	if cfg == nil || len(rigBeadStores) == 0 {
		return report
	}

	// Build a guard set of session home names so agent template directories
	// are never touched.
	sessionHomes := make(map[string]bool, len(cfg.Agents))
	for i := range cfg.Agents {
		if name := cfg.Agents[i].BindingQualifiedName(); name != "" {
			sessionHomes[name] = true
		}
	}

	// Authoritative liveness signal, gathered at most ONCE for the whole pass
	// and only on demand. When the scan is indeterminate the reaper protects
	// every candidate (fail closed).
	//
	// ga-singc6: this used to run unconditionally, before discovering whether
	// any reap candidate existed. On darwin the scan is a host-wide
	// `lsof -a -d cwd` over the entire process table, executed inline in the
	// controller's reconciler tick — and that tick is the city's clock (orders
	// dispatch once per tick). Measured over 70h of reconciler-trace records:
	// this phase ran on 99.9% of ticks while the fleet usually had ZERO eligible
	// candidates (six worktrees, none reapable), and on a slow host it grew from
	// a 5s median to 48s, accounting for ~73% of tick inflation. Gathering it
	// lazily removes the cost entirely in the common zero-candidate case and
	// changes nothing about the verdict: every candidate that reaches the gate
	// still sees the same scan, and an indeterminate scan still protects all.
	var (
		live         liveWorktreeState
		liveGathered bool
	)
	liveness := func() liveWorktreeState {
		if !liveGathered {
			live = collectLiveWorktreeStateFn()
			liveGathered = true
		}
		return live
	}

	wtRoot := filepath.Join(cityPath, ".gc", "worktrees")

	for rigName, store := range rigBeadStores {
		if store == nil {
			continue
		}
		rigRoot := rigRootByName(cfg, rigName)
		if rigRoot == "" {
			// No configured filesystem path for this rig — cannot resolve the
			// owning repository, so we cannot safely enumerate or remove.
			continue
		}
		rigWorktreeDir := filepath.Join(wtRoot, rigName)

		worktrees, err := git.New(rigRoot).WorktreeList()
		if err != nil {
			fmt.Fprintf(stderr, "reapClosedBeadWorktrees: listing worktrees for rig %s (%s): %v\n", rigName, rigRoot, err) //nolint:errcheck
			continue
		}

		// Pass 1: discover reap-eligible candidates — closed bead, and old
		// enough to be past the freshness quarantine (FR-5). Every other gate
		// (borrow-veto, liveness, git safety) is deferred to pass 2 so the
		// borrow-veto scan below can run as a single batched query per rig
		// (FR-3) instead of once per worktree.
		var candidates []reapCandidate
		for _, wt := range worktrees {
			worktreePath := wt.Path

			// Only per-bead worktrees under this rig's .gc/worktrees/<rig>/
			// subtree are in scope. This excludes the rig's main working tree
			// and any worktree checked out elsewhere.
			if !pathutil.PathWithin(rigWorktreeDir, worktreePath) || pathutil.SamePath(rigWorktreeDir, worktreePath) {
				continue
			}
			// Defense in depth: never act on a path that is not strictly under
			// the city worktree root.
			if !isStrictlyUnderDir(wtRoot, worktreePath) {
				continue
			}

			base := filepath.Base(worktreePath)

			// Session home guard: never touch agent template directories.
			if sessionHomes[base] {
				continue
			}

			// Extract a bead ID candidate from the worktree's leaf name.
			beadID := extractBeadIDFromWorktreeName(cfg, base)
			if beadID == "" {
				continue
			}

			// Confirm the bead exists and is closed in this rig's store.
			bead, err := store.Get(beadID)
			if err != nil || bead.Status != "closed" {
				// ErrNotFound, transient error, or bead not yet closed — skip.
				continue
			}

			// Freshness quarantine (FR-5): a worktree younger than the
			// configured minimum age is exempt from reaping, protecting
			// against the race between worktree creation and its owning
			// bead's work-dir metadata being stamped by the next reconcile
			// pass. Age is fail-closed — an indeterminate age protects.
			minAge := cfg.Daemon.AutoReapClosedBeadWorktreesMinAge()
			age, ok := computeWorktreeAge(worktreePath)
			reason := ""
			switch {
			case !ok:
				reason = "worktree age indeterminate (failing closed)"
			case minAge > 0 && age < minAge:
				reason = fmt.Sprintf("worktree too young to reap (quarantine): age=%s min_age=%s", age.Round(time.Second), minAge)
			}
			if reason != "" {
				branch, _ := git.New(worktreePath).CurrentBranch()
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: protecting %s (bead %s closed but %s)\n",
					worktreePath, beadID, reason,
				)
				recordReapSkipped(rec, beadID, worktreePath, rigName, reason)
				report.Protected = append(report.Protected, reapDecision{
					BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch, Reason: reason,
				})
				continue
			}

			candidates = append(candidates, reapCandidate{beadID: beadID, worktreePath: worktreePath})
		}

		if len(candidates) == 0 {
			continue
		}

		// Git safety gate FIRST (ga-singc6). It is local and cheap — three git
		// subprocesses against the worktree — while the two gates after it are
		// a full remote store scan and a host-wide process-table scan. On the
		// live fleet a single worktree sat protected by this gate for 11 hours
		// (a repo-global stash, ga-gsfxag) and every ~20s tick paid the store
		// scan and the process scan anyway, for a verdict this gate had already
		// reached. Protecting here costs nothing downstream: a candidate that
		// fails git safety is dropped before either expensive signal is asked
		// for. Verdict-neutral — see the gate-order note in the doc comment.
		survivors := make([]reapCandidate, 0, len(candidates))
		for _, c := range candidates {
			reason := gitSafetyReason(c.worktreePath)
			if reason == "" {
				survivors = append(survivors, c)
				continue
			}
			branch, _ := git.New(c.worktreePath).CurrentBranch()
			fmt.Fprintf(stderr, //nolint:errcheck
				"reapClosedBeadWorktrees: protecting %s (bead %s closed but %s)\n",
				c.worktreePath, c.beadID, reason,
			)
			recordReapSkipped(rec, c.beadID, c.worktreePath, rigName, reason)
			report.Protected = append(report.Protected, reapDecision{
				BeadID: c.beadID, Path: c.worktreePath, Rig: rigName, Branch: branch, Reason: reason,
			})
		}
		candidates = survivors
		if len(candidates) == 0 {
			continue
		}

		// Borrow-veto scan (FR-1/FR-2/FR-3): one batched query for every
		// surviving candidate in this rig instead of one query per candidate.
		// A query error fails closed — every remaining candidate in this
		// rig's tick is protected (NFR-1).
		referencingBeads, listErr := scanBorrowVetoReferences(store, candidates)
		if listErr != nil {
			reason := fmt.Sprintf("borrow-veto scan failed (failing closed): %v", listErr)
			for _, c := range candidates {
				branch, _ := git.New(c.worktreePath).CurrentBranch()
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: protecting %s (bead %s closed but %s)\n",
					c.worktreePath, c.beadID, reason,
				)
				recordReapSkipped(rec, c.beadID, c.worktreePath, rigName, reason)
				report.Protected = append(report.Protected, reapDecision{
					BeadID: c.beadID, Path: c.worktreePath, Rig: rigName, Branch: branch, Reason: reason,
				})
			}
			continue
		}

		// Pass 2: apply the borrow-veto verdict, then the liveness gate, to
		// each surviving candidate. Git safety already ran above.
		for _, c := range candidates {
			worktreePath := c.worktreePath
			beadID := c.beadID

			// Borrow-veto (FR-1/FR-2/FR-7): protect when any non-terminal
			// bead — regardless of molecule — still references this path via
			// work-dir metadata.
			reason := ""
			if refs := referencingBeads[worktreePath]; len(refs) > 0 {
				reason = fmt.Sprintf("borrow-veto: referenced by non-terminal bead(s) %s", strings.Join(refs, ", "))
			}

			// Liveness gate (fail closed). Protect the tree when a live process
			// or active session is working in it, or when liveness could not be
			// determined at all.
			if reason == "" {
				live := liveness()
				switch {
				case !live.scanned:
					reason = "liveness scan unavailable (failing closed, protecting all)"
				default:
					if isLive, why := worktreeIsLive(worktreePath, live, liveSessionDirs); isLive {
						reason = "live: " + why
					}
				}
			}

			branch, _ := git.New(worktreePath).CurrentBranch()

			if reason != "" {
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: protecting %s (bead %s closed but %s)\n",
					worktreePath, beadID, reason,
				)
				recordReapSkipped(rec, beadID, worktreePath, rigName, reason)
				report.Protected = append(report.Protected, reapDecision{
					BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch, Reason: reason,
				})
				continue
			}

			if dryRun {
				const whatIf = "dry-run: would reap (closed bead, clean tree, no live process)"
				fmt.Fprintf(stderr, //nolint:errcheck
					"reapClosedBeadWorktrees: %s: %s for closed bead %s\n",
					whatIf, worktreePath, beadID,
				)
				recordReapSkipped(rec, beadID, worktreePath, rigName, whatIf)
				report.Reaped = append(report.Reaped, reapDecision{
					BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch,
				})
				continue
			}

			// Remove the worktree from the OWNING rig repository. git worktree
			// remove must be run from the main repo root, not from within the
			// worktree being removed.
			if err := git.New(rigRoot).WorktreeRemove(worktreePath, false); err != nil {
				fmt.Fprintf(stderr, "reapClosedBeadWorktrees: removing %s: %v\n", worktreePath, err) //nolint:errcheck
				continue
			}
			fmt.Fprintf(stderr, //nolint:errcheck
				"reapClosedBeadWorktrees: removed worktree %s for closed bead %s\n",
				worktreePath, beadID,
			)
			if raw, err := json.Marshal(events.BeadWorktreeReapedPayload{
				BeadID: beadID,
				Path:   worktreePath,
				Rig:    rigName,
				Branch: branch,
			}); err == nil {
				rec.Record(events.Event{
					Type:    events.BeadWorktreeReaped,
					Actor:   "gc",
					Subject: beadID,
					Payload: raw,
				})
			}
			report.Reaped = append(report.Reaped, reapDecision{
				BeadID: beadID, Path: worktreePath, Rig: rigName, Branch: branch,
			})
		}
	}
	return report
}

// reapCandidate is a worktree that survived the closed-bead check and the
// freshness quarantine in pass 1, awaiting the batched borrow-veto scan and
// the remaining safety gates in pass 2.
type reapCandidate struct {
	beadID       string
	worktreePath string
}

// gitSafetyReason applies the git-state gate to one worktree and returns the
// protecting reason, or "" when the tree is clean, fully pushed, and the repo
// carries no stashes. Probe failures fail closed with their own reason. The
// stash probe is repo-global by construction (`git stash list` reads
// refs/stash of the common repository), so one stash anywhere in the repo
// protects every worktree of that repo — see ga-gsfxag.
func gitSafetyReason(worktreePath string) string {
	wg := git.New(worktreePath)
	hasUncommitted := wg.HasUncommittedWork()
	hasUnpushed, unpushedErr := wg.HasUnpushedCommitsResult()
	hasStashes, stashesErr := wg.HasStashesResult()
	switch {
	case unpushedErr != nil:
		return "unpushed commit probe failed (failing closed): " + unpushedErr.Error()
	case stashesErr != nil:
		return "stash probe failed (failing closed): " + stashesErr.Error()
	case hasUncommitted || hasUnpushed || hasStashes:
		return fmt.Sprintf("unsafe git state: uncommitted=%v unpushed=%v stashes=%v", hasUncommitted, hasUnpushed, hasStashes)
	}
	return ""
}

// computeWorktreeAge returns how long ago worktreePath was created, using the
// mtime of its ".git" pointer file (written once by `git worktree add` and not
// rewritten during normal use) as a creation-time proxy. Worktree structs carry
// no timestamp of their own. ok is false when the file cannot be stat'd, so the
// caller can fail closed instead of treating an indeterminate age as zero.
func computeWorktreeAge(worktreePath string) (age time.Duration, ok bool) {
	info, err := os.Stat(filepath.Join(worktreePath, ".git"))
	if err != nil {
		return 0, false
	}
	return time.Since(info.ModTime()), true
}

// scanBorrowVetoReferences issues one batched beads.Store.List query and
// returns, for each candidate's worktree path, the IDs of any non-terminal
// beads — in any molecule — whose gc.work_dir or legacy work_dir metadata
// still points at that path (FR-1/FR-2/FR-3). Terminal status is decided by
// convoycore.IsTerminalStatus, not a bare "!= closed" check, so a tombstoned
// reference does not veto. Path matching is symlink/alias-normalized on both
// sides via pathutil.NormalizePathForCompare, matching the liveness gate, so a
// metadata path recorded in a different-but-equivalent form still vetoes.
// A query error is returned as-is; the caller must fail closed and protect
// every candidate in the rig (NFR-1).
func scanBorrowVetoReferences(store beads.Store, candidates []reapCandidate) (map[string][]string, error) {
	// The query excludes closed beads at the store level (IsTerminalStatus
	// would discard them anyway) and skips label hydration this scan never
	// reads. TierBoth is explicit so the reaper's safety contract does not
	// depend on a wrapping store expanding the default tier for it. Live is
	// explicit because this is a destructive-path safety gate: it must
	// observe a reopen or work_dir stamp written by ANOTHER process
	// immediately, not after a CachingStore reconcile converges — and the
	// ga-singc6 status memo's staleness argument leans on this scan being
	// authoritative at reap time.
	all, err := store.List(beads.ListQuery{AllowScan: true, SkipLabels: true, TierMode: beads.TierBoth, Live: true})
	if err != nil {
		return nil, err
	}
	byNorm := make(map[string]string, len(candidates)) // normalized -> raw candidate path
	for _, c := range candidates {
		byNorm[pathutil.NormalizePathForCompare(c.worktreePath)] = c.worktreePath
	}
	refs := make(map[string][]string)
	for _, b := range all {
		if convoycore.IsTerminalStatus(b.Status) {
			continue
		}
		for _, key := range [...]string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
			p := strings.TrimSpace(b.Metadata[key])
			if p == "" {
				continue
			}
			if raw, hit := byNorm[pathutil.NormalizePathForCompare(p)]; hit {
				refs[raw] = append(refs[raw], b.ID)
				break
			}
		}
	}
	return refs, nil
}

// recordReapSkipped emits a bead.worktree.reap_skipped event carrying the
// reason a worktree was protected or (in dry-run) flagged as would-reap.
func recordReapSkipped(rec events.Recorder, beadID, path, rig, reason string) {
	raw, err := json.Marshal(events.BeadWorktreeReapSkippedPayload{
		BeadID: beadID,
		Path:   path,
		Rig:    rig,
		Reason: reason,
	})
	if err != nil {
		return
	}
	rec.Record(events.Event{
		Type:    events.BeadWorktreeReapSkipped,
		Actor:   "gc",
		Subject: beadID,
		Payload: raw,
	})
}

// rigRootByName returns the configured filesystem path of the rig with the
// given name, or "" when the rig is unknown or has no path. This is the
// repository that owns the rig's per-bead worktrees.
func rigRootByName(cfg *config.City, rigName string) string {
	if cfg == nil {
		return ""
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == rigName {
			return strings.TrimSpace(cfg.Rigs[i].Path)
		}
	}
	return ""
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
	// Normalize both sides. git worktree list reports canonical paths, while
	// dir is derived from the configured city path, which may still contain a
	// symlinked ancestor (on macOS every $TMPDIR path does, via /var ->
	// private/var). Comparing the two raw forms makes filepath.Rel return a
	// "../.." escape for a worktree that is plainly inside the city, so this
	// defense-in-depth check silently drops every reap candidate. The
	// PathWithin gate directly above already compares normalized.
	dir = pathutil.NormalizePathForCompare(dir)
	path = pathutil.NormalizePathForCompare(path)
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..")
}
