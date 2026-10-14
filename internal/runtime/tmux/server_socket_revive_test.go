package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// reviveFixture scripts ga-46jb3c's incident: the named socket is gone, tmux
// reports no server, and the injected probe reports the live servers that
// still hold the socket path open.
type reviveFixture struct {
	fe          *fakeExecutor
	servers     []int
	findErr     error
	restores    bool // socket reappears after SIGUSR1
	signaled    []int
	callsAtSig  int
	presentHits int
}

func (f *reviveFixture) tmux() *Tmux {
	return &Tmux{
		cfg:  Config{SocketName: "gc-revive"},
		exec: f.fe,
		serverSocketObserver: func(context.Context, string) error {
			return nil // absent: the existing guard calls this safe to create
		},
		socketReviver: &socketReviver{
			socketPresent: func(string) (bool, error) {
				f.presentHits++
				return f.restores && len(f.signaled) > 0, nil
			},
			findServers: func(context.Context, string) ([]int, error) {
				return f.servers, f.findErr
			},
			signal: func(pid int) error {
				f.signaled = append(f.signaled, pid)
				f.callsAtSig = len(f.fe.calls)
				return nil
			},
			timeout:  50 * time.Millisecond,
			interval: time.Millisecond,
		},
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

func TestParseLsofSocketOwners(t *testing.T) {
	// Shape measured on macOS 27 with tmux 3.7c after the socket was unlinked:
	// the listener and each accepted client fd keep the bound path as NAME.
	out := strings.Join([]string{
		"p27432", "ctmux", "f6", "n/private/tmp/tmux-501/gastown", "f7", "n/private/tmp/tmux-501/gastown",
		"p555", "ctmux", "f3", "n->0x8d3667673bfb9f5c",
		"p777", "ctmux", "f6", "n/private/tmp/tmux-501/gastown-other",
		"p888", "cnottmux", "f4", "n/private/tmp/tmux-501/gastown",
		"p999", "ctmux", "f5", "n/tmp/tmux-501/gastown type=STREAM",
	}, "\n")
	got := parseLsofSocketOwners(out, []string{"/tmp/tmux-501/gastown", "/private/tmp/tmux-501/gastown"})
	if len(got) != 2 || got[0] != 27432 || got[1] != 999 {
		t.Fatalf("owners = %v, want [27432 999]", got)
	}
}
