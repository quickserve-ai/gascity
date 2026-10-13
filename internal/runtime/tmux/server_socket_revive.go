package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// socketReviver recovers a tmux server whose socket file was unlinked while
// the server kept running (ga-46jb3c, 2026-10-13: /tmp/tmux-501/gastown and
// its directory vanished under a 10-day-old server holding every seat). tmux
// then reports "no server running", the socket observation reads the path as
// safely absent, and new-session founds a SECOND server at the same path with
// duplicate seats. SIGUSR1 is tmux's documented signal to recreate the socket.
//
// Seams are injectable so tests run without tmux, lsof or real signals.
type socketReviver struct {
	// socketNode reports whether path exists and, if so, whether it is a
	// unix socket.
	socketNode  func(path string) (exists, isSocket bool, err error)
	findServers func(ctx context.Context, path string) ([]int, error)
	signal      func(pid int) error
	// serverPID asks the server listening on path for its pid without
	// starting one (tmux -N, 3.2+).
	serverPID func(ctx context.Context, path string) (int, error)
	// budget bounds the whole revival end to end: the per-path lock wait
	// and every probe. timeout is the restoration window after SIGUSR1,
	// kept inside budget.
	budget   time.Duration
	timeout  time.Duration
	interval time.Duration
}

// errOwnerProbeUnavailable means the owner probe cannot run on this host
// (lsof not installed). Revival is skipped rather than refusing every cold
// start.
var errOwnerProbeUnavailable = errors.New("socket owner probe unavailable")

// productionSocketReviver uses lstat, lsof and SIGUSR1. Detection is lsof
// because macOS keeps a unix socket's bound name after the file is unlinked:
// `lsof -a -U -c tmux -F pcn` still lists the server's listener fd under the
// original path (measured on macOS 27, tmux 3.7c; see ga-46jb3c).
var productionSocketReviver = &socketReviver{
	socketNode: func(path string) (bool, bool, error) {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		if err != nil {
			return false, false, err
		}
		return true, info.Mode()&os.ModeSocket != 0, nil
	},
	findServers: findTmuxSocketOwners,
	signal: func(pid int) error {
		return signalPID(pid, processSignalUsr1)
	},
	serverPID: func(ctx context.Context, path string) (int, error) {
		out, err := realExecutor{}.executeCtx(ctx, []string{"-u", "-N", "-S", path, "display-message", "-p", "#{pid}"})
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(strings.TrimSpace(out))
	},
	budget:   6 * time.Second,
	timeout:  2 * time.Second,
	interval: 50 * time.Millisecond,
}

// reviveLocks serializes revival per socket path: a second SIGUSR1 makes
// tmux unlink and rebind again, racing the first caller's new-session.
// Each lock is a 1-slot channel so a waiter can abandon it at its deadline.
var reviveLocks sync.Map // path -> chan struct{}

func acquireReviveLock(ctx context.Context, path string) (release func(), err error) {
	v, _ := reviveLocks.LoadOrStore(path, make(chan struct{}, 1))
	slot := v.(chan struct{})
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// reviver returns the seams in force. A test that injects only
// serverSocketObserver owns the whole socket verdict, so no revival runs.
func (t *Tmux) reviver() *socketReviver {
	if t.socketReviver != nil {
		return t.socketReviver
	}
	if t.serverSocketObserver != nil {
		return nil
	}
	return productionSocketReviver
}

func orphanErr(path string, pid int, reason string, err error) error {
	if err != nil {
		return fmt.Errorf("%w: protocol=orphaned-server path=%s pid=%d reason=%s: %w", ErrServerDegraded, path, pid, reason, err)
	}
	return fmt.Errorf("%w: protocol=orphaned-server path=%s pid=%d reason=%s", ErrServerDegraded, path, pid, reason)
}

// reviveOrphanedServer runs in the new-session preflight after tmux reported
// no server and the socket observation called path safe to create. nil means
// creating a server is safe: a node at path that no live tmux server holds (a
// stale socket, handled exactly as before), a node a concurrent caller
// revived that passes verification, no live tmux server holding path
// (including a server that was deliberately stopped, which has no process
// left), the owner probe is not installed, or one server did hold it and
// SIGUSR1 restored a socket that it provably owns. Any other uncertainty
// refuses with ErrServerDegraded so the caller retries next tick instead of
// founding a second server.
func (t *Tmux) reviveOrphanedServer(path string) error {
	r := t.reviver()
	if r == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.budget)
	defer cancel()
	release, err := acquireReviveLock(ctx, path)
	if err != nil {
		return orphanErr(path, 0, "revive-lock-timeout", err)
	}
	defer release()

	// Read under the lock so a concurrent reviver's result is visible. The
	// observation does not say whether it saw path absent or stale, so a node
	// here is verified, never trusted: it may be one a predecessor restored
	// and then rejected.
	exists, _, err := r.socketNode(path)
	if err != nil {
		return orphanErr(path, 0, "socket-lstat", err)
	}
	pids, err := r.findServers(ctx, path)
	if errors.Is(err, errOwnerProbeUnavailable) {
		log.Printf("tmux socket %s: the revive check is unavailable (%v); creating a server without checking for a live orphan", path, err)
		return nil
	}
	if err != nil {
		return orphanErr(path, 0, "owner-detection-failed", err)
	}
	if exists {
		if len(pids) == 0 {
			return nil // stale: no server holds it
		}
		if reason := revivedSocketReason(ctx, r, path, 0); reason != "" {
			return orphanErr(path, 0, "existing-"+reason, nil)
		}
		return nil
	}
	switch len(pids) {
	case 0:
		return nil
	case 1:
	default:
		return fmt.Errorf("%w: protocol=orphaned-server path=%s reason=multiple-owners pids=%v", ErrServerDegraded, path, pids)
	}
	pid := pids[0]
	// tmux re-binds in place but does not recreate a deleted directory.
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	// Re-probe right before the signal. This narrows, but cannot close, the
	// pid-reuse race: without a pidfd on macOS, the pid can still exit and
	// be reused by any process between this probe and the kill below.
	if again, err := r.findServers(ctx, path); err != nil || !slices.Equal(again, pids) {
		return orphanErr(path, pid, fmt.Sprintf("owner-changed-before-signal owners=%v", again), err)
	}
	if err := r.signal(pid); err != nil {
		return orphanErr(path, pid, "sigusr1-failed", err)
	}
	// The restoration window bounds every probe after the signal too.
	rctx, rcancel := context.WithTimeout(ctx, r.timeout)
	defer rcancel()
	var reason string
	for {
		if reason = revivedSocketReason(rctx, r, path, pid); reason == "" {
			log.Printf("tmux socket %s was missing while server pid %d lived; sent SIGUSR1, socket restored", path, pid)
			return nil
		}
		if !strings.HasSuffix(reason, "-pending") || !sleepCtx(rctx, r.interval) {
			break
		}
	}
	if reason == "socket-absent-pending" {
		reason = "socket-not-restored"
	}
	log.Printf("tmux socket %s was missing while server pid %d lived; sent SIGUSR1, socket NOT restored (%s): refusing new-session", path, pid, reason)
	return orphanErr(path, pid, reason, nil)
}

// sleepCtx waits d and reports true, or reports false as soon as ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// revivedSocketReason is "" when path is a socket owned by pid alone and the
// server on it answers with pid; pid 0 accepts any single owner. A "-pending"
// reason may still clear; any other reason is final.
func revivedSocketReason(ctx context.Context, r *socketReviver, path string, pid int) string {
	exists, isSocket, err := r.socketNode(path)
	switch {
	case err != nil:
		return "socket-lstat-failed"
	case !exists:
		return "socket-absent-pending"
	case !isSocket:
		return "restored-node-not-a-socket"
	}
	owners, err := r.findServers(ctx, path)
	if err == nil && pid == 0 && len(owners) == 1 {
		pid = owners[0]
	}
	if err != nil || !slices.Equal(owners, []int{pid}) {
		return fmt.Sprintf("restored-socket-owner-mismatch owners=%v", owners)
	}
	if got, err := r.serverPID(ctx, path); err != nil || got != pid {
		return "restored-socket-unresponsive-pending"
	}
	return ""
}

// findTmuxSocketOwners lists the tmux processes holding a unix socket bound to
// path. tmux binds the realpath of its socket directory (/private/tmp on
// macOS), so both spellings match. lsof exits 1 with no output when nothing
// matches; any other failure is returned so the caller fails closed.
func findTmuxSocketOwners(ctx context.Context, path string) ([]int, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "lsof", "-w", "-a", "-U", "-c", "tmux", "-F", "pcn")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("%w: %w", errOwnerProbeUnavailable, err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && stdout.Len() == 0 && strings.TrimSpace(stderr.String()) == "" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lsof: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseLsofSocketOwners(stdout.String(), []string{path, canonicalSocketPath(path)})
}

// lsofStreamType reports whether rest is "STREAM" or "STREAM (STATE)" with an
// uppercase state.
func lsofStreamType(rest string) bool {
	if rest == "STREAM" {
		return true
	}
	state, ok := strings.CutPrefix(rest, "STREAM (")
	if !ok {
		return false
	}
	state, ok = strings.CutSuffix(state, ")")
	return ok && state != "" && strings.Trim(state, "ABCDEFGHIJKLMNOPQRSTUVWXYZ_") == ""
}

// canonicalSocketPath resolves symlinks in the nearest existing ancestor of
// path, since the socket directory itself may have been deleted.
func canonicalSocketPath(path string) string {
	dir, rest := filepath.Clean(path), ""
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// parseLsofSocketOwners returns, in first-seen order, the distinct pids of
// tmux processes holding an fd named one of paths. macOS reports the command
// as "tmux"; Linux tmux sets its proctitle, so lsof reports "tmux: server".
// Name matching is lsofNameMatches.
func parseLsofSocketOwners(out string, paths []string) ([]int, error) {
	var owners []int
	pid, command := 0, ""
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		value := line[1:]
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(value)
			command = ""
		case 'c':
			command = value
		case 'n':
			if pid <= 0 || (command != "tmux" && !strings.HasPrefix(command, "tmux:")) {
				continue
			}
			if match, err := lsofNameMatches(value, paths); err != nil {
				return nil, fmt.Errorf("lsof: pid %d: %w", pid, err)
			} else if match && !slices.Contains(owners, pid) {
				owners = append(owners, pid)
			}
		}
	}
	return owners, nil
}

// lsofNameMatches reports whether an lsof NAME is one of paths. An exact match
// wins; otherwise Linux lsof's appended socket type is accepted after a
// candidate path only as " type=STREAM" (tmux listens on a stream socket),
// optionally followed by a state such as " (LISTEN)"; any other text there
// could be a literal file name and is an error. A file literally named
// path+" type=STREAM" stays indistinguishable; the worst it causes is a
// SIGUSR1 to that tmux server, which only recreates its own socket, and then
// a refusal, since the restored socket at path must answer with the signaled
// pid. lsof escapes non-printable bytes with backslashes, which cannot be
// compared reliably: such a name is an error only when its unescaped prefix
// could still be one of paths, and is ignored otherwise.
func lsofNameMatches(name string, paths []string) (bool, error) {
	for _, path := range paths {
		if name == path {
			return true, nil
		}
		if rest, ok := strings.CutPrefix(name, path+" type="); ok {
			if !lsofStreamType(rest) {
				return false, fmt.Errorf("socket name %q may be %s", name, path)
			}
			return true, nil
		}
	}
	if i := strings.IndexByte(name, '\\'); i >= 0 {
		for _, path := range paths {
			if strings.HasPrefix(path, name[:i]) {
				return false, fmt.Errorf("escaped socket name %q may be %s", name, path)
			}
		}
	}
	return false, nil
}
