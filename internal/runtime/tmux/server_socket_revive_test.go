package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// reviveFixture scripts ga-46jb3c's incident: the named socket is gone, tmux
// reports no server, and the injected probe reports the live servers that
// still hold the socket path open.
type reviveFixture struct {
	mu          sync.Mutex
	fe          *fakeExecutor
	servers     []int // owners before the signal
	preSignal   []int // owners at the pre-signal re-check (nil: servers)
	postSignal  []int // owners once restored (nil: servers)
	findErr     error
	restores    bool   // a node reappears after SIGUSR1
	restoredAs  string // "" (socket) or "file"
	answerPID   int    // pid the revived server reports (0: servers[0])
	signaled    []int
	callsAtSig  int
	findCalls   int
	presentHits int
}

func (f *reviveFixture) reviver() *socketReviver {
	return &socketReviver{
		socketNode: func(string) (bool, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.presentHits++
			if !f.restores || len(f.signaled) == 0 {
				return false, false, nil
			}
			return true, f.restoredAs == "", nil
		},
		findServers: func(context.Context, string) ([]int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.findCalls++
			switch {
			case f.findErr != nil:
				return nil, f.findErr
			case len(f.signaled) > 0 && f.postSignal != nil:
				return f.postSignal, nil
			case len(f.signaled) == 0 && f.findCalls > 1 && f.preSignal != nil:
				return f.preSignal, nil
			}
			return f.servers, nil
		},
		signal: func(pid int) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.signaled = append(f.signaled, pid)
			f.callsAtSig = len(f.fe.calls)
			return nil
		},
		serverPID: func(context.Context, string) (int, error) {
			if f.answerPID != 0 {
				return f.answerPID, nil
			}
			return f.servers[0], nil
		},
		timeout:  50 * time.Millisecond,
		interval: time.Millisecond,
	}
}

func (f *reviveFixture) tmux() *Tmux {
	return &Tmux{
		cfg:  Config{SocketName: "gc-revive"},
		exec: f.fe,
		serverSocketObserver: func(context.Context, string) error {
			return nil // absent: the existing guard calls this safe to create
		},
		socketReviver: f.reviver(),
	}
}

func newReviveFixture() *reviveFixture {
	return &reviveFixture{fe: probeAssertSet([]string{"", "", ""}, []error{ErrNoServer, nil, nil})}
}

func issuedNewSession(fe *fakeExecutor) bool {
	for _, call := range fe.calls {
		for _, arg := range call {
			if arg == "new-session" {
				return true
			}
		}
	}
	return false
}

func TestNewSessionRevivesLiveServerWhoseSocketWasUnlinked(t *testing.T) {
	f := newReviveFixture()
	f.servers = []int{27432}
	f.restores = true

	if err := f.tmux().NewSession("gc-seat", ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if len(f.signaled) != 1 || f.signaled[0] != 27432 {
		t.Fatalf("signaled = %v, want SIGUSR1 to the orphaned server 27432 once", f.signaled)
	}
	if f.callsAtSig != 1 {
		t.Fatalf("tmux calls before SIGUSR1 = %d, want only the has-session probe", f.callsAtSig)
	}
	if f.presentHits == 0 {
		t.Fatal("socket was never re-checked after SIGUSR1")
	}
	if len(f.fe.calls) < 2 || f.fe.calls[1][3] != "new-session" {
		t.Fatalf("calls = %#v, want new-session against the revived server", f.fe.calls)
	}
}

func TestNewSessionRefusesWhenRevivedSocketDoesNotReturn(t *testing.T) {
	f := newReviveFixture()
	f.servers = []int{27432}

	err := f.tmux().NewSession("gc-seat", "")
	if !errors.Is(err, ErrServerDegraded) || errors.Is(err, ErrNoServer) {
		t.Fatalf("err = %v, want ErrServerDegraded (not ErrNoServer)", err)
	}
	if !strings.Contains(err.Error(), "pid=27432") {
		t.Fatalf("err = %q, want it to name the server pid", err)
	}
	if len(f.signaled) != 1 {
		t.Fatalf("signaled = %v, want one SIGUSR1", f.signaled)
	}
	if issuedNewSession(f.fe) {
		t.Fatalf("calls = %#v: new-session would found a second server", f.fe.calls)
	}
}

func TestNewSessionCreatesServerWhenNoLiveServerOwnsSocket(t *testing.T) {
	f := newReviveFixture()

	if err := f.tmux().NewSession("gc-seat", ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if len(f.signaled) != 0 {
		t.Fatalf("signaled = %v, want none for a stopped server", f.signaled)
	}
	if !issuedNewSession(f.fe) {
		t.Fatalf("calls = %#v, want new-session", f.fe.calls)
	}
}

func TestNewSessionFailsClosedWhenOrphanDetectionIsUncertain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		servers []int
		findErr error
	}{
		{name: "probe-error", findErr: errors.New("lsof: exec format error")},
		{name: "two-servers", servers: []int{27432, 26334}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviveFixture()
			f.servers, f.findErr = tc.servers, tc.findErr
			err := f.tmux().NewSession("gc-seat", "")
			if !errors.Is(err, ErrServerDegraded) {
				t.Fatalf("err = %v, want ErrServerDegraded", err)
			}
			if len(f.signaled) != 0 || issuedNewSession(f.fe) {
				t.Fatalf("signaled=%v calls=%#v, want neither", f.signaled, f.fe.calls)
			}
		})
	}
}

// Review fix 2: lsof not installed must not refuse every cold start.
func TestNewSessionCreatesServerWhenOwnerProbeIsNotInstalled(t *testing.T) {
	f := newReviveFixture()
	f.findErr = fmt.Errorf("%w: exec: \"lsof\": executable file not found in $PATH", errOwnerProbeUnavailable)
	if err := f.tmux().NewSession("gc-seat", ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if len(f.signaled) != 0 || !issuedNewSession(f.fe) {
		t.Fatalf("signaled=%v calls=%#v, want new-session and no signal", f.signaled, f.fe.calls)
	}
}

func TestFindTmuxSocketOwnersDistinguishesMissingFromFailingLsof(t *testing.T) {
	ctx := context.Background()
	t.Setenv("PATH", t.TempDir())
	if _, err := findTmuxSocketOwners(ctx, "/tmp/tmux-501/gastown"); !errors.Is(err, errOwnerProbeUnavailable) {
		t.Fatalf("missing lsof: err = %v, want errOwnerProbeUnavailable", err)
	}

	dir := t.TempDir()
	script := "#!/bin/sh\necho 'lsof: WARNING: something broke' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "lsof"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	_, err := findTmuxSocketOwners(ctx, "/tmp/tmux-501/gastown")
	if err == nil || errors.Is(err, errOwnerProbeUnavailable) {
		t.Fatalf("failing lsof: err = %v, want a fail-closed error", err)
	}
}

// Review fix 4: two callers finding the same orphan signal it once, whether
// the second waits on the lock or arrives after the socket is back.
func TestConcurrentReviveSignalsOrphanOnce(t *testing.T) {
	f := newReviveFixture()
	f.servers, f.restores = []int{27432}, true
	tm := f.tmux()
	path := "/tmp/tmux-test/gc-revive-concurrent"
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = tm.reviveOrphanedServer(path)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if len(f.signaled) != 1 {
		t.Fatalf("signaled = %v, want exactly one SIGUSR1", f.signaled)
	}
}

// Review fixes 5 and 6: never signal a reused pid; never accept a restored
// path that is not the revived server's socket.
func TestReviveRefusesUnprovenOwnership(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(*reviveFixture)
		wantSignal bool
		reason     string
	}{
		{name: "pid-reused-before-signal", setup: func(f *reviveFixture) { f.preSignal = []int{} }, reason: "owner-changed-before-signal"},
		{name: "restored-node-not-socket", setup: func(f *reviveFixture) { f.restoredAs = "file" }, wantSignal: true, reason: "restored-node-not-a-socket"},
		{name: "restored-socket-wrong-owner", setup: func(f *reviveFixture) { f.postSignal = []int{26334} }, wantSignal: true, reason: "restored-socket-owner-mismatch"},
		{name: "restored-socket-answers-other-pid", setup: func(f *reviveFixture) { f.answerPID = 26334 }, wantSignal: true, reason: "restored-socket-unresponsive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviveFixture()
			f.servers, f.restores = []int{27432}, true
			tc.setup(f)
			err := f.tmux().NewSession("gc-seat", "")
			if !errors.Is(err, ErrServerDegraded) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("err = %v, want ErrServerDegraded with %q", err, tc.reason)
			}
			if (len(f.signaled) > 0) != tc.wantSignal || issuedNewSession(f.fe) {
				t.Fatalf("signaled=%v calls=%#v", f.signaled, f.fe.calls)
			}
		})
	}
}

func TestParseLsofSocketOwners(t *testing.T) {
	paths := []string{"/tmp/tmux-501/gastown", "/private/tmp/tmux-501/gastown"}
	// macOS 27 / tmux 3.7c after the socket was unlinked: the listener and
	// each accepted client fd keep the bound path as NAME; clients do not.
	// Linux tmux sets its proctitle, and Linux lsof appends " type=...".
	out := strings.Join([]string{
		"p27432", "ctmux", "f6", "n/private/tmp/tmux-501/gastown", "f7", "n/private/tmp/tmux-501/gastown",
		"p555", "ctmux", "f3", "n->0x8d3667673bfb9f5c",
		"p777", "ctmux", "f6", "n/private/tmp/tmux-501/gastown-other",
		"p888", "cnottmux", "f4", "n/private/tmp/tmux-501/gastown",
		"p999", "ctmux: server", "f5", "n/tmp/tmux-501/gastown type=STREAM (LISTEN)",
	}, "\n")
	got, err := parseLsofSocketOwners(out, paths)
	if err != nil || len(got) != 2 || got[0] != 27432 || got[1] != 999 {
		t.Fatalf("owners = %v, %v; want [27432 999]", got, err)
	}

	spaced := "/tmp/my dir/tmux-501/gastown"
	got, err = parseLsofSocketOwners("p42\nctmux: server\nf5\nn"+spaced+" type=STREAM\n", []string{spaced})
	if err != nil || len(got) != 1 || got[0] != 42 {
		t.Fatalf("spaced path owners = %v, %v; want [42]", got, err)
	}
	if got, err := parseLsofSocketOwners("p42\nctmux\nf5\nn/tmp/tmux-501/gas\\x09town\n", paths); err == nil {
		t.Fatalf("escaped name owners = %v, want a fail-closed error", got)
	}
}
