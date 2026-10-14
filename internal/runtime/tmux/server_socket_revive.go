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
	socketPresent func(path string) (bool, error)
	findServers   func(ctx context.Context, path string) ([]int, error)
	signal        func(pid int) error
	timeout       time.Duration
	interval      time.Duration
}

// productionSocketReviver uses lstat, lsof and SIGUSR1. Detection is lsof
// because macOS keeps a unix socket's bound name after the file is unlinked:
// `lsof -a -U -c tmux -F pcn` still lists the server's listener fd under the
// original path (measured on macOS 27, tmux 3.7c; see ga-46jb3c).
var productionSocketReviver = &socketReviver{
	socketPresent: func(path string) (bool, error) {
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	},
	findServers: findTmuxSocketOwners,
	signal: func(pid int) error {
		return signalPID(pid, processSignalUsr1)
	},
	timeout:  2 * time.Second,
	interval: 50 * time.Millisecond,
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

// reviveOrphanedServer runs in the new-session preflight after tmux reported
// no server and the socket observation called path safe to create. nil means
// creating a server is safe: the socket file exists (a stale socket, handled
// exactly as before), no live tmux server holds path (including a server that
// was deliberately stopped, which has no process left), or one did and
// SIGUSR1 restored its socket. Any uncertainty refuses with ErrServerDegraded
// so the caller retries next tick instead of founding a second server.
func (t *Tmux) reviveOrphanedServer(path string) error {
	r := t.reviver()
	if r == nil {
		return nil
	}
	present, err := r.socketPresent(path)
	if err != nil {
		return fmt.Errorf("%w: protocol=orphaned-server path=%s reason=socket-lstat: %w", ErrServerDegraded, path, err)
	}
	if present {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	pids, err := r.findServers(ctx, path)
	if err != nil {
		return fmt.Errorf("%w: protocol=orphaned-server path=%s reason=owner-detection-failed: %w", ErrServerDegraded, path, err)
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
	if err := r.signal(pid); err != nil {
		return fmt.Errorf("%w: protocol=orphaned-server path=%s pid=%d reason=sigusr1-failed: %w", ErrServerDegraded, path, pid, err)
	}
	deadline := time.Now().Add(r.timeout)
	for {
		if present, err := r.socketPresent(path); err == nil && present {
			log.Printf("tmux socket %s was missing while server pid %d lived; sent SIGUSR1, socket restored", path, pid)
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(r.interval)
	}
	log.Printf("tmux socket %s was missing while server pid %d lived; sent SIGUSR1, socket NOT restored: refusing new-session", path, pid)
	return fmt.Errorf("%w: protocol=orphaned-server path=%s pid=%d reason=socket-not-restored", ErrServerDegraded, path, pid)
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
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && stdout.Len() == 0 && strings.TrimSpace(stderr.String()) == "" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lsof: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseLsofSocketOwners(stdout.String(), []string{path, canonicalSocketPath(path)}), nil
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
// processes whose command is exactly "tmux" holding an fd named one of paths.
// Linux lsof may suffix the name with " type=STREAM ...".
func parseLsofSocketOwners(out string, paths []string) []int {
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
			name, _, _ := strings.Cut(value, " ")
			if pid > 0 && command == "tmux" && slices.Contains(paths, name) && !slices.Contains(owners, pid) {
				owners = append(owners, pid)
			}
		}
	}
	return owners
}
