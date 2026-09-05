//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ga-bq84cj. macOS has no /proc, so the /proc-based scanner returned
// scanned=false on every run, the reaper failed closed every time, and worktree
// reclamation NEVER worked on this platform: 157k skip events, zero reaps ever,
// while a single closed polecat worktree held 19 GB on a volume at 90%.
//
// The fail-closed posture was never the bug and is preserved exactly here. The
// bug was that the only liveness signal was /proc-only, which on Darwin made the
// gate permanently closed rather than closed-when-uncertain. A safety check that
// can never pass is indistinguishable from a disabled reaper, except that it
// looks safe.

// lsofPath is the enumerator. Held in a var so tests can point it at a stub and
// exercise the parse and the fail-closed paths without a real process table.
var lsofPath = "/usr/sbin/lsof"

// lsofCWDTimeout bounds the enumeration. Measured on the live fleet host:
// 659 processes in ~0.66s (700+ processes in ~0.7s a week later). The ceiling
// leaves ~15x headroom over that because a slow answer is still useful, while
// NO answer means fail-closed and zero reclamation for this pass.
//
// ga-singc6: this was 30s, LARGER than the reconciler's healthy tick period
// (~23s median). An inner deadline above its outer budget cannot protect the
// outer one — a single pathological enumeration could stretch the city's clock
// tick by more than a whole tick. 10s keeps the worst case inside the tick.
// The cost of tripping it is bounded and safe: the scan reports indeterminate,
// every candidate is protected, and the next pass tries again.
var lsofCWDTimeout = 10 * time.Second

// collectLiveWorktreeState enumerates every live process's current working
// directory via lsof and records the canonical paths.
//
// Contract, identical to the /proc scanner: scanned=false means liveness is
// indeterminate and the caller MUST protect every candidate worktree.
//
// Two deliberate decisions about lsof's behavior:
//
//  1. A NON-ZERO EXIT IS NOT A FAILURE. lsof routinely exits 1 while emitting
//     perfectly good output — it reports partial success when some processes
//     cannot be examined (another user's, or one that exited mid-scan). Judging
//     on the exit code alone would throw away a complete answer and silently
//     restore the very never-reaps behavior this fixes. So the verdict is based
//     on what was PARSED, not on the status.
//
//  2. ZERO PARSED CWDS IS A FAILURE, even on a clean exit. This process is
//     itself live and has a cwd, so an empty result cannot be true — it means
//     lsof is missing, was blocked, or changed its output format. Failing closed
//     there is what keeps a future format change from turning into a reaping
//     spree. This is strictly safer than the /proc scanner, which returns
//     scanned=true with an empty set.
func collectLiveWorktreeState() liveWorktreeState {
	// -a AND the selectors; -d cwd restricts to the cwd descriptor; -Fpn emits
	// machine-readable fields (p<pid>, n<path>) instead of columns; -n skips
	// DNS; -P skips port-name lookups. The last two only matter for sockets but
	// cost nothing here and keep lsof from stalling on a slow resolver.
	ctx, cancel := context.WithTimeout(context.Background(), lsofCWDTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, lsofPath, "-a", "-d", "cwd", "-Fpn", "-n", "-P")

	// Setpgid + WaitDelay together are what actually bound this call, and both
	// are required.
	//
	// Killing only the direct child is not enough: if the enumerator spawns a
	// child that outlives it, that grandchild inherits the stdout pipe, and
	// Wait() blocks until EVERY writer closes it — so the process dies while the
	// call hangs anyway. A regression test (FailsClosedOnTimeout) caught exactly
	// that: a 300ms timeout took 30s. Setpgid puts the enumerator in its own
	// process group so the whole group is signaled, and WaitDelay caps how long
	// Wait() will linger on inherited pipes afterwards.
	//
	// HONEST COVERAGE NOTE: WaitDelay is defense-in-depth and is NOT covered by a
	// killing test. Mutation-setting it to 0 leaves the suite green, because once
	// the whole process group is signaled nothing survives to hold the pipe, and
	// macOS ships no setsid binary to build a group-escaping stub with. Keep it
	// anyway — it is the only thing that bounds Wait() if a future enumerator
	// double-forks out of the group — but do not read the green suite as proof
	// that it works.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid signals the whole process group.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil

	err := cmd.Run()
	if ctx.Err() != nil {
		// A timeout leaves a truncated table. Partial liveness data is the one
		// thing we must not act on: a worktree whose protecting process was in
		// the unread remainder would look reapable.
		return liveWorktreeState{scanned: false}
	}
	if err != nil && stdout.Len() == 0 {
		// Could not start, or died before emitting anything: indeterminate.
		// A non-zero exit WITH output is handled below and is not a failure.
		return liveWorktreeState{scanned: false}
	}

	raw := parseLsofCWDs(stdout.Bytes())
	if len(raw) == 0 {
		return liveWorktreeState{scanned: false}
	}
	return liveWorktreeState{cwds: normalizeLiveCWDs(raw), scanned: true}
}

// parseLsofCWDs extracts cwd paths from lsof -F field output.
//
// The format is one field per line, the leading byte naming the field: "p" a
// pid (which begins a new process record), "f" a descriptor, "n" a name. We
// only want the n-path belonging to an f=cwd descriptor. Tracking the current
// descriptor matters: -d cwd should make every record a cwd, but relying on
// that would mean a future flag change silently starts feeding open FILE paths
// into the live set, which would protect worktrees for the wrong reason.
func parseLsofCWDs(out []byte) []string {
	var paths []string
	inCWD := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			// New process record; descriptor state resets.
			inCWD = false
		case 'f':
			inCWD = strings.TrimSpace(line[1:]) == "cwd"
		case 'n':
			if inCWD {
				if p := line[1:]; p != "" {
					paths = append(paths, p)
				}
			}
		}
	}
	return paths
}
