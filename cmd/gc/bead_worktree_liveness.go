package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A per-bead worktree is protected from reaping when a process is actively
// working inside it, even though its bead is closed and its tree is momentarily
// git-clean. This is the "closed-bead != end-of-use" guard: bead status and
// git-cleanliness say nothing about whether an agent is mid-stage in the tree
// right now (a source anchor closes at plan-delivery while later review stages
// keep committing in the same tree; a tree is transiently clean between a push
// and the next stage). Deleting such a tree destroys live work — the founding
// incident behind gastownhall/gascity#4492.
//
// The design is modeled on the dolt_cleanup fail-closed precedent
// (dolt_cleanup_discovery.go): the /proc/<pid>/cwd signal is authoritative, and
// when it cannot be gathered the caller must protect every worktree rather than
// risk deleting live work.

// liveWorktreeState captures the working directories of every live process the
// reaper could observe on this host, plus whether the enumeration itself
// succeeded.
type liveWorktreeState struct {
	// cwds is the set of canonicalized (symlink-resolved, absolute) working
	// directories of live processes. Deduplicated.
	cwds []string
	// scanned reports whether the process table was enumerated at all. False
	// means liveness is indeterminate — the host has no /proc, or the
	// top-level walk failed — and the reaper must fail closed by protecting
	// every candidate worktree.
	scanned bool
}

// collectLiveWorktreeStateFn is the seam the reaper calls to gather live
// process cwds. Indirected through a package-level var so tests can inject a
// deterministic set (including the fail-closed scanned=false case) without
// standing up real processes.
var collectLiveWorktreeStateFn = collectLiveWorktreeState

// liveScanTimeout bounds the external process-table probe. It is generous
// relative to the measured cost (~0.4s for 746 processes on the fleet host) so
// a loaded box does not turn a slow scan into a fail-closed tick, but bounded
// so a wedged probe — lsof stalling on an unresponsive mount is the classic
// one — cannot hang the controller.
const liveScanTimeout = 30 * time.Second

// collectLiveWorktreeState records the canonical working directory of every
// process the reaper can observe, using whichever mechanism this host actually
// has. It returns scanned=false when liveness could not be established at all,
// so the caller fails closed and reaps nothing.
//
// TWO MECHANISMS, BECAUSE /proc IS NOT PORTABLE (ga-bq84cj). This function
// walked /proc unconditionally. Darwin has no /proc, so on the macOS fleet host
// the walk failed on EVERY tick, the reaper protected every candidate, and
// auto_reap_closed_bead_worktrees=true was a no-op for the life of the host:
// 157,136 reap_skipped events carrying "liveness scan unavailable", and zero
// worktrees ever reaped, while .gc/worktrees grew to 87G. A guard that cannot
// succeed is not a conservative guard, it is a disabled feature that reads like
// one.
//
// The fail-closed posture is unchanged and deliberate: an unreadable process
// table still protects everything. What changes is that on this platform the
// table is now readable.
func collectLiveWorktreeState() liveWorktreeState {
	if state, ok := collectLiveWorktreeStateProc(); ok {
		return state
	}
	return collectLiveWorktreeStateLsof()
}

// collectLiveWorktreeStateProc walks /proc/<pid>/cwd for every process on the
// host and records their canonical working directories. The bool reports
// whether this host has a /proc to walk at all; false means "not this
// mechanism", NOT "no live processes", and the caller must try another probe
// rather than treat the empty result as an answer.
//
// Per-process readlink failures are skipped, not fatal: a process may exit
// mid-walk, and a process owned by another user may have a cwd this process
// cannot resolve. The fleet runs every agent as the same user, so agent
// worktree cwds are always visible here; the active-session-directory
// cross-check plus the git-clean and closed-bead gates back-stop any process
// this scan cannot see. This matches the dolt reaper's posture: the cwd signal
// protects, it never authorizes a deletion the other gates would refuse.
func collectLiveWorktreeStateProc() (liveWorktreeState, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return liveWorktreeState{scanned: false}, false
	}
	seen := make(map[string]struct{})
	var cwds []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue // not a PID directory
		}
		link, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if err != nil || link == "" {
			continue
		}
		// A cwd whose inode has been unlinked carries a trailing " (deleted)"
		// marker. The directory is gone, so it can never match a live worktree
		// path on disk — drop it rather than canonicalize a bogus path. (The
		// rare live directory literally named "... (deleted)" would be dropped
		// too; that only ever loses protection for a pathological path the
		// fleet never creates, and the git-clean gate still applies.)
		if strings.HasSuffix(link, " (deleted)") {
			continue
		}
		canon := pathutil.NormalizePathForCompare(link)
		if canon == "" {
			continue
		}
		if _, ok := seen[canon]; ok {
			continue
		}
		seen[canon] = struct{}{}
		cwds = append(cwds, canon)
	}
	return liveWorktreeState{cwds: cwds, scanned: true}, true
}

// lsofCwdOutputFn is the seam for the external probe, indirected so tests can
// feed recorded lsof output (and its failure modes) without a real process
// table.
var lsofCwdOutputFn = runLsofCwd

// runLsofCwd asks lsof for the cwd descriptor of every process, in its
// machine-readable field format: one "p<pid>" line per process followed by the
// "n<path>" line for that process's cwd.
//
// -w suppresses warning lines about descriptors it cannot stat. Those warnings
// are routine (other users' processes, vanished pids) and are NOT a reason to
// distrust the rest of the output.
func runLsofCwd(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "lsof", "-a", "-d", "cwd", "-F", "pn", "-w").Output()
}

// collectLiveWorktreeStateLsof builds the live set from lsof, for hosts without
// /proc (Darwin).
//
// EXIT STATUS IS NOT THE SIGNAL HERE. lsof exits non-zero when ANY part of its
// search failed — routinely true on a shared box, where some processes belong
// to other users — while still printing every entry it did resolve. Keying on
// the exit code would discard a complete, usable process table. What we key on
// instead is whether we parsed any cwd at all: this process itself always has
// one, so an empty parse means the probe did not work, and that fails closed.
func collectLiveWorktreeStateLsof() liveWorktreeState {
	ctx, cancel := context.WithTimeout(context.Background(), liveScanTimeout)
	defer cancel()

	out, err := lsofCwdOutputFn(ctx)
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// Timed out or cancelled: whatever we have is a partial view of the
		// process table, and a partial view can omit exactly the process that
		// would have protected a tree. Indeterminate, so fail closed.
		return liveWorktreeState{scanned: false}
	}
	cwds := parseLsofCwds(out)
	if len(cwds) == 0 {
		// No lsof on PATH, an unparseable format, or a genuinely empty read.
		// All three are indeterminate rather than "nothing is live".
		return liveWorktreeState{scanned: false}
	}
	_ = err // see the note above: a non-zero exit with usable output is normal.
	return liveWorktreeState{cwds: cwds, scanned: true}
}

// parseLsofCwds extracts canonical cwd paths from lsof -F pn output,
// deduplicated. Non-absolute and unresolvable entries are dropped.
func parseLsofCwds(out []byte) []string {
	seen := make(map[string]struct{})
	var cwds []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	// lsof paths are bounded by the OS path limit, but a pathological line
	// should not abort the scan; give the scanner room well past PATH_MAX.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 2 || line[0] != 'n' {
			continue // "p<pid>" process lines and any other field
		}
		path := line[1:]
		if !filepath.IsAbs(path) {
			continue
		}
		canon := pathutil.NormalizePathForCompare(path)
		if canon == "" {
			continue
		}
		if _, ok := seen[canon]; ok {
			continue
		}
		seen[canon] = struct{}{}
		cwds = append(cwds, canon)
	}
	return cwds
}

// worktreeIsLive reports whether any live signal sits at or beneath
// worktreePath: a live process cwd, or a recorded active-session working
// directory. "At or beneath" means the worktree is protected when a process is
// running in it OR in any subdirectory of it (an agent whose cwd is a nested
// test/build subdir of its assigned tree still counts). It returns the matching
// path as a human-readable reason for dry-run/operator output.
//
// The caller is responsible for the fail-closed case: worktreeIsLive assumes
// the live set was successfully gathered. When liveWorktreeState.scanned is
// false the caller must protect unconditionally and never reach this function
// for a reap decision.
func worktreeIsLive(worktreePath string, live liveWorktreeState, sessionDirs []string) (bool, string) {
	wt := pathutil.NormalizePathForCompare(worktreePath)
	if wt == "" {
		return false, ""
	}
	for _, cwd := range live.cwds {
		if pathAtOrUnder(wt, cwd) {
			return true, "live process cwd " + cwd
		}
	}
	for _, dir := range sessionDirs {
		d := pathutil.NormalizePathForCompare(dir)
		if d == "" {
			continue
		}
		if pathAtOrUnder(wt, d) {
			return true, "active session dir " + d
		}
	}
	return false, ""
}

// pathAtOrUnder reports whether candidate equals root or is lexically contained
// beneath it. Both arguments must already be normalized (symlink-resolved,
// absolute, cleaned) — collectLiveWorktreeState normalizes cwds once at
// gather-time and worktreeIsLive normalizes the worktree once, so this avoids
// re-resolving symlinks on every pair in what can be a large process × worktree
// cross-product each tick.
func pathAtOrUnder(root, candidate string) bool {
	if root == "" || candidate == "" {
		return false
	}
	if root == candidate {
		return true
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// liveSessionWorktreeDirs collects the recorded working directories of every
// open (non-closed) session in the snapshot: the canonical worker_dir first
// (via WorkerDirFromInfo), plus the raw work_dir and gc.work_dir mirrors so a
// session whose canonical dir is momentarily unstamped still contributes a
// protecting path. The result is the "active session set" the reaper
// cross-checks against — a belt-and-suspenders signal alongside the
// authoritative /proc cwd scan, since session metadata is stamped at
// create/dispatch and is not continuously refreshed. Deduplicated; empty
// entries dropped.
func liveSessionWorktreeDirs(snapshot *sessionBeadSnapshot) []string {
	if snapshot == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var dirs []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		dirs = append(dirs, p)
	}
	for _, info := range snapshot.OpenInfos() {
		add(sessionpkg.WorkerDirFromInfo(info))
		add(info.WorkDir)
		add(info.WorkDirCanonical)
		add(info.WorkerDir)
	}
	return dirs
}
