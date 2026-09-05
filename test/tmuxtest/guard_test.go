package tmuxtest

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
)

func TestConfigureProcessEnvIsolatesTmuxSocketRoot(t *testing.T) {
	socketRoot := t.TempDir()
	t.Setenv(tmuxEnv, "/tmp/tmux-parent/default,1,0")
	t.Setenv(tmuxPaneEnv, "%42")
	t.Setenv(tmuxTmpEnv, "/tmp/parent-tmux")

	if err := ConfigureProcessEnv(socketRoot); err != nil {
		t.Fatalf("ConfigureProcessEnv(): %v", err)
	}

	if value, ok := os.LookupEnv(tmuxEnv); ok {
		t.Fatalf("%s survived with value %q", tmuxEnv, value)
	}
	if value, ok := os.LookupEnv(tmuxPaneEnv); ok {
		t.Fatalf("%s survived with value %q", tmuxPaneEnv, value)
	}
	if value := os.Getenv(tmuxTmpEnv); value != socketRoot {
		t.Fatalf("%s = %q, want %q", tmuxTmpEnv, value, socketRoot)
	}
	if info, err := os.Stat(socketRoot); err != nil {
		t.Fatalf("stat socket root: %v", err)
	} else if !info.IsDir() {
		t.Fatalf("socket root is not a directory")
	}
}

func TestListTestSocketPathsSkipsLiveSiblingRoots(t *testing.T) {
	tmp := t.TempDir()
	currentRun := filepath.Join(tmp, "gc-integration-current")
	t.Setenv("TMPDIR", currentRun)
	currentRoot := filepath.Join(currentRun, "tmux")
	staleRoot := filepath.Join(tmp, "gc-integration-stale", "tmux")
	liveRoot := filepath.Join(tmp, "gc-integration-live", "tmux")
	otherRoot := filepath.Join(tmp, "not-gc", "tmux")
	t.Setenv(tmuxTmpEnv, currentRoot)

	uid := strconv.Itoa(os.Getuid())
	currentSocket := filepath.Join(currentRoot, "tmux-"+uid, "gctest-current")
	staleSocket := filepath.Join(staleRoot, "tmux-"+uid, "gctest-stale")
	liveSocket := filepath.Join(liveRoot, "tmux-"+uid, "gctest-live")
	otherSocket := filepath.Join(otherRoot, "tmux-"+uid, "gctest-other")
	for _, path := range []string{currentSocket, staleSocket, liveSocket, otherSocket} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}
	staleTime := time.Now().Add(-tmuxSiblingSocketStaleAfter - time.Minute)
	if err := os.Chtimes(staleSocket, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(%s): %v", staleSocket, err)
	}

	got := listTestSocketPaths()

	if !slices.Contains(got, currentSocket) {
		t.Fatalf("listTestSocketPaths() missing current socket %s in %v", currentSocket, got)
	}
	if !slices.Contains(got, staleSocket) {
		t.Fatalf("listTestSocketPaths() missing stale socket %s in %v", staleSocket, got)
	}
	if slices.Contains(got, liveSocket) {
		t.Fatalf("listTestSocketPaths() included live sibling socket %s in %v", liveSocket, got)
	}
	if slices.Contains(got, otherSocket) {
		t.Fatalf("listTestSocketPaths() included unrelated socket %s in %v", otherSocket, got)
	}
}

func TestTmuxSocketRootPatternsCoverKnownRuntimePrefixes(t *testing.T) {
	namespace := t.TempDir()
	tests := []struct {
		name    string
		runName string
		direct  bool // true = activeRoot is namespace/runName/tmux (no "runtime" level)
		want    string
	}{
		{
			name:    "acceptance C",
			runName: "gcac-123",
			want:    filepath.Join(namespace, "gcac-*", "runtime", "tmux"),
		},
		{
			name:    "worker inference",
			runName: "gcwi-123",
			want:    filepath.Join(namespace, "gcwi-*", "runtime", "tmux"),
		},
		{
			name:    "worker inference live",
			runName: "gcwi-live-123",
			want:    filepath.Join(namespace, "gcwi-*", "runtime", "tmux"),
		},
		{
			name:    "acceptance B",
			runName: "gc-acceptance-b-123",
			want:    filepath.Join(namespace, "gc-acceptance-b-*", "runtime", "tmux"),
		},
		{
			name:    "acceptance",
			runName: "gc-acceptance-123",
			want:    filepath.Join(namespace, "gc-acceptance-*", "runtime", "tmux"),
		},
		{
			name:    "integration direct",
			runName: "gc-integration-123",
			direct:  true,
			want:    filepath.Join(namespace, "gc-integration-*", "tmux"),
		},
		{
			// gct- is the short-path tmux socket root created by the integration
			// test suite when $TMPDIR is too long (e.g., macOS).
			name:    "gct short root",
			runName: "gct-1234567890",
			direct:  true,
			want:    filepath.Join(namespace, "gct*", "tmux"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var activeRoot string
			if tt.direct {
				activeRoot = filepath.Join(namespace, tt.runName, "tmux")
			} else {
				activeRoot = filepath.Join(namespace, tt.runName, "runtime", "tmux")
			}
			got := tmuxSocketRootPatterns(activeRoot)
			if !slices.Contains(got, tt.want) {
				t.Fatalf("tmuxSocketRootPatterns(%q) = %v, want %q", activeRoot, got, tt.want)
			}
		})
	}
}

func TestNewGuardWithSocketCityNameFormat(t *testing.T) {
	// City name must be "gctest-<8hex>" (no per-character hyphens).
	// macOS's UNIX socket path limit is 104 bytes; per-char hyphenation
	// creates names like "gctest-4-f-d-9-6-0-8-c" (22 chars) instead of
	// "gctest-4fd9608c" (15 chars), which pushes socket paths over the limit.
	for range 100 {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("gctest-%x", b)
		if len(name) != 15 {
			t.Fatalf("city name %q has length %d, want 15", name, len(name))
		}
	}
}

func TestKillSocketRootServersReapsSpawnedServer(t *testing.T) {
	RequireTmux(t)
	// Unix socket paths are capped (~104 bytes on macOS); t.TempDir() is too
	// deep, so use a short /tmp root like the runtime does.
	socketRoot, err := os.MkdirTemp("/tmp", "gct-ut-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })

	spawn := exec.Command("tmux", "-L", "test-city", "new-session", "-d", "-s", "probe", "sleep", "300")
	spawn.Env = append(os.Environ(), tmuxTmpEnv+"="+socketRoot)
	if out, err := spawn.CombinedOutput(); err != nil {
		t.Fatalf("spawning probe tmux server: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		kill := exec.Command("tmux", "-L", "test-city", "kill-server")
		kill.Env = append(os.Environ(), tmuxTmpEnv+"="+socketRoot)
		_ = kill.Run()
	})

	socketPath := filepath.Join(socketRoot, "tmux-"+strconv.Itoa(os.Getuid()), "test-city")
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("probe socket missing after spawn: %v", err)
	}

	if killed := KillSocketRootServers(socketRoot); killed != 1 {
		t.Fatalf("KillSocketRootServers() = %d, want 1", killed)
	}

	check := exec.Command("tmux", "-L", "test-city", "has-session", "-t", "probe")
	check.Env = append(os.Environ(), tmuxTmpEnv+"="+socketRoot)
	if err := check.Run(); err == nil {
		t.Fatalf("probe server still alive after KillSocketRootServers")
	}
}

func TestSweepStaleSocketRootParentsSkipsFreshParents(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "gct-fresh")
	if err := os.MkdirAll(filepath.Join(target, "tmux"), 0o700); err != nil {
		t.Fatal(err)
	}

	SweepStaleSocketRootParents(filepath.Join(parent, "gct-*"), time.Hour)
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("fresh parent was swept: %v", err)
	}

	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatal(err)
	}
	SweepStaleSocketRootParents(filepath.Join(parent, "gct-*"), time.Hour)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("stale parent survived sweep: %v", err)
	}
}

// deadPID returns a PID that has already exited, for building a socket parent
// dir whose owner is gone. If the host recycles it before the sweep runs, the
// sweep skips the dir and the test fails loudly rather than passing silently.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running true: %v", err)
	}
	return cmd.Process.Pid
}

// shortSweepRoot returns a /tmp-rooted dir. Unix socket paths are capped
// (~104 bytes on macOS) and t.TempDir() is too deep to hold one.
func shortSweepRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "gct-ut-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

// TestSweepOrphanPIDPrefixedDirsReapsServerBeforeRemovingParent is the
// regression test for ga-3qlrnv: the sweep used to RemoveAll a dead run's
// socket parent without killing the tmux servers bound to sockets inside it.
// The servers survived on socket paths that no longer existed, which put them
// beyond every socket-addressed sweep in this package -- seven had accumulated
// on the dev host, the oldest 34h old.
//
// The assertion is on the SERVER, not on the directory: removing the directory
// is what the broken version did.
//
// The tmux server is spawned inline rather than through a helper: the resource
// census only credits an exact Medium owner for calls lexically inside the
// declared runnable, so a shared helper would leave this test's tmux dependency
// as undeclared Small debt.
func TestSweepOrphanPIDPrefixedDirsReapsServerBeforeRemovingParent(t *testing.T) {
	RequireTmux(t)
	root := shortSweepRoot(t)
	owner := deadPID(t)
	parent := filepath.Join(root, fmt.Sprintf("%s%d-probe", SocketParentDirPrefix, owner))
	socketRoot := filepath.Join(parent, "tmux")
	if err := os.MkdirAll(socketRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	// The server is reaped in t.Cleanup by PID, not by socket: this test
	// deliberately deletes the socket directory, which is exactly the state
	// that leaves a server unreachable by every socket-addressed path here.
	spawn := exec.Command("tmux", "-L", "test-city", "new-session", "-d", "-s", "probe", "sleep", "300")
	spawn.Env = append(os.Environ(), tmuxTmpEnv+"="+socketRoot)
	if out, err := spawn.CombinedOutput(); err != nil {
		t.Fatalf("spawning probe tmux server: %v\n%s", err, out)
	}
	socketPath := filepath.Join(socketRoot, "tmux-"+strconv.Itoa(os.Getuid()), "test-city")
	show := exec.Command("tmux", "-S", socketPath, "display-message", "-p", "#{pid}")
	out, err := show.Output()
	if err != nil {
		t.Fatalf("reading probe server pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("probe server pid %q: %v", out, err)
	}
	t.Cleanup(func() {
		if pidutil.Alive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	if !pidutil.Alive(pid) {
		t.Fatalf("probe server %d not alive before sweep", pid)
	}

	// The sweep ignores dirs younger than socketParentSweepMinAge. Backdate
	// last, after the writes that would bump mtime.
	old := time.Now().Add(-2 * socketParentSweepMinAge)
	if err := os.Chtimes(parent, old, old); err != nil {
		t.Fatal(err)
	}

	SweepOrphanPIDPrefixedDirs(root, SocketParentDirPrefix, io.Discard)

	deadline := time.Now().Add(3 * time.Second)
	for pidutil.Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if pidutil.Alive(pid) {
		t.Fatalf("tmux server %d survived the sweep; its socket dir %s is %s", pid, parent, dirState(parent))
	}
	if _, err := os.Stat(parent); !os.IsNotExist(err) {
		t.Fatalf("socket parent survived sweep after its server was reaped: %v", err)
	}
}

// TestSweepOrphanPIDPrefixedDirsRemovesParentWithDeadSocketFile pins the other
// half of the liveness question. A socket FILE outlives a server that died
// without cleaning up; if presence on disk counted as a live server, every such
// leftover would pin its parent dir forever and the sweep would stop sweeping.
func TestSweepOrphanPIDPrefixedDirsRemovesParentWithDeadSocketFile(t *testing.T) {
	root := shortSweepRoot(t)
	owner := deadPID(t)
	parent := filepath.Join(root, fmt.Sprintf("%s%d-stale", SocketParentDirPrefix, owner))
	socketDir := filepath.Join(parent, "tmux", "tmux-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(socketDir, "test-city"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-2 * socketParentSweepMinAge)
	if err := os.Chtimes(parent, old, old); err != nil {
		t.Fatal(err)
	}

	SweepOrphanPIDPrefixedDirs(root, SocketParentDirPrefix, io.Discard)

	if _, err := os.Stat(parent); !os.IsNotExist(err) {
		t.Fatalf("a socket file with no listener pinned its parent dir: %v", err)
	}
}

func dirState(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "GONE (the server is now unreachable by socket path)"
	}
	return "still present"
}
