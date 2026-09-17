package tmux

import (
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// startProcessNamedArgv0 starts a live throwaway process whose argv[0] is name,
// and returns its PID as a string. processMatchesNames reads `ps -o comm=` and
// then `ps -o args=`, matching filepath.Base(argv[0]) against the provider name
// set, so this is enough to make the pane look like it is running that provider
// without launching anything real.
func startProcessNamedArgv0(t *testing.T, name string) string {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep binary to borrow for an argv[0] fixture: %v", err)
	}
	cmd := &exec.Cmd{Path: sleep, Args: []string{name, "30"}}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting a process with argv[0]=%q: %v", name, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return strconv.Itoa(cmd.Process.Pid)
}

// TestNudgeSendPathConfirmsBracketedPasteByAgentLiveness covers the sub-3.7
// fallback (ga-p93v6w). tmux added #{bracket_paste_flag} in 3.7; westeros runs
// 3.4 and qlandia 3.6a, so on both peer towns the flag renders empty and the
// ga-6qfgdo paste fix was inert -- every claude nudge over 512 bytes kept the
// keystroke path that loses the message HEAD.
//
// Where the flag cannot answer, the nudge path asks instead whether the pane's
// AGENT IS ALIVE: a running claude sits at its prompt with bracketed paste on,
// while the pane that reports no agent is the bare-shell pane where raw
// CR-separated lines would RUN. So a live agent is confirmation enough to
// paste, and no agent is a refusal.
func TestNudgeSendPathConfirmsBracketedPasteByAgentLiveness(t *testing.T) {
	text := multiLineNudge(900)
	tests := []struct {
		name      string
		flag      string
		argv0     string // the pane process's argv[0]; "" starts no process
		wantPaste bool
		wantProbe bool // whether the liveness probe (#{pane_pid}) should run
	}{
		{
			name:      "unreadable flag with a live claude pastes",
			flag:      "",
			argv0:     "claude",
			wantPaste: true,
			wantProbe: true,
		},
		{
			name:      "unreadable flag with a bare shell keeps send-keys",
			flag:      "",
			argv0:     "bash",
			wantPaste: false,
			wantProbe: true,
		},
		{
			name:      "flag 0 refuses even with a live claude",
			flag:      "0",
			argv0:     "claude",
			wantPaste: false,
			wantProbe: false,
		},
		{
			name:      "flag 1 pastes without paying for the probe",
			flag:      "1",
			argv0:     "claude",
			wantPaste: true,
			wantProbe: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fe := &bracketedPasteExecutor{provider: "claude", pasteFlag: tt.flag}
			if tt.argv0 != "" {
				fe.panePID = startProcessNamedArgv0(t, tt.argv0)
			}
			tm := NewTmuxWithConfig(DefaultConfig())
			tm.exec = fe

			if err := tm.sendKeysLiteralWithRetry("%1", text, time.Second); err != nil {
				t.Fatalf("sendKeysLiteralWithRetry() = %v, want nil", err)
			}

			pastes := fe.callsWith("paste-buffer")
			literal := fe.literalSends()
			if tt.wantPaste {
				if len(pastes) != 1 || len(literal) != 0 {
					t.Fatalf("flag %q, pane argv[0] %q: paste-buffer calls = %d, send-keys -l calls = %d, want 1 and 0; calls: %q",
						tt.flag, tt.argv0, len(pastes), len(literal), fe.calls)
				}
				// The point of the paste is that the HEAD survives, so assert
				// the buffer carried the whole message, not just its tail.
				if len(fe.loaded) != 1 || fe.loaded[0] != text {
					t.Fatalf("flag %q: loaded buffers = %d, want exactly one equal to the %d-byte message", tt.flag, len(fe.loaded), len(text))
				}
			} else {
				if len(pastes) != 0 || len(fe.callsWith("load-buffer")) != 0 {
					t.Fatalf("flag %q, pane argv[0] %q: nudge pasted into a pane that was not confirmed to bracket it, which submits it line by line; calls: %q",
						tt.flag, tt.argv0, fe.calls)
				}
				if len(literal) != 1 || literal[0] != text {
					t.Fatalf("flag %q, pane argv[0] %q: send-keys -l texts = %d call(s), want exactly one carrying the whole text; calls: %q",
						tt.flag, tt.argv0, len(literal), fe.calls)
				}
			}

			probes := fe.callsWith("#{pane_pid}")
			if tt.wantProbe && len(probes) == 0 {
				t.Fatalf("flag %q: no #{pane_pid} read, so the liveness fallback never ran; calls: %q", tt.flag, fe.calls)
			}
			if !tt.wantProbe && len(probes) != 0 {
				t.Fatalf("flag %q: %d #{pane_pid} read(s), but a readable flag is authoritative and must not pay for the probe; calls: %q",
					tt.flag, len(probes), fe.calls)
			}
		})
	}
}

// TestStartupSendPathNeverConfirmsByAgentLiveness is the regression test for the
// trap this change had to avoid. The startup path delivers a seat's ROLE PROMPT
// through the same sendLiteralText, and it does so while the agent is
// deliberately not running yet -- waiting for the TUI is what its retry loop is
// for. So the agent-liveness fallback must be scoped to the NUDGE path: applied
// to startup, it would refuse a sub-4096-byte prompt on every sub-3.7 server
// (westeros, qlandia) and strand every seat without its role.
//
// Here the pane HAS a live claude, so the nudge path would paste. The startup
// path must still keystroke, and must not even read the pane PID.
func TestStartupSendPathNeverConfirmsByAgentLiveness(t *testing.T) {
	text := multiLineNudge(900)
	fe := &bracketedPasteExecutor{provider: "claude", pasteFlag: ""}
	fe.panePID = startProcessNamedArgv0(t, "claude")
	tm := NewTmuxWithConfig(DefaultConfig())
	tm.exec = fe

	if err := tm.sendStartupKeysLiteralWithRetry("%1", text, "claude", time.Second); err != nil {
		t.Fatalf("sendStartupKeysLiteralWithRetry() = %v, want nil", err)
	}

	if pastes := fe.callsWith("paste-buffer"); len(pastes) != 0 {
		t.Fatalf("startup delivery used the paste path on an unreadable flag; the nudge-only fallback leaked into startup; calls: %q", fe.calls)
	}
	if literal := fe.literalSends(); len(literal) != 1 || literal[0] != text {
		t.Fatalf("startup delivery send-keys -l texts = %d call(s), want exactly one carrying the whole prompt; calls: %q", len(literal), fe.calls)
	}
	if probes := fe.callsWith("#{pane_pid}"); len(probes) != 0 {
		t.Fatalf("startup delivery read #{pane_pid} %d time(s): it must not consult agent liveness at all; calls: %q", len(probes), fe.calls)
	}
}

// TestNudgeSendPathLeavesTheOversizedBandUnchanged pins the band boundary of
// this change, and documents a hazard it deliberately does NOT fix. Text over
// maxSendKeysLiteralLen cannot go as `send-keys -l` at all, so it has always
// been pasted with no flag check whatsoever -- blind, on every tmux version.
// That is how every seat's role prompt reaches a sub-3.7 pane today, and it is
// why blind pasting is not a new risk anyone is introducing here.
//
// It is also an unfixed hole: above 4096 bytes a pane with the mode OFF still
// gets raw CR-separated lines. Narrowing that needs its own change, with the
// startup path's needs weighed in; this test exists so that shipping the
// confirmation for the 512-4096 band is not mistaken for having closed it.
func TestNudgeSendPathLeavesTheOversizedBandUnchanged(t *testing.T) {
	text := sizedNudge(maxSendKeysLiteralLen + 1)
	for _, flag := range []string{"", "0"} {
		fe := &bracketedPasteExecutor{provider: "claude", pasteFlag: flag}
		tm := NewTmuxWithConfig(DefaultConfig())
		tm.exec = fe

		if err := tm.sendKeysLiteralWithRetry("%1", text, time.Second); err != nil {
			t.Fatalf("flag %q: sendKeysLiteralWithRetry() = %v, want nil", flag, err)
		}
		if pastes := fe.callsWith("paste-buffer"); len(pastes) != 1 {
			t.Fatalf("flag %q: paste-buffer calls = %d, want 1 (the oversized band pastes unconditionally); calls: %q", flag, len(pastes), fe.calls)
		}
		if reads := fe.callsWith("#{bracket_paste_flag}"); len(reads) != 0 {
			t.Fatalf("flag %q: the oversized band read the flag %d time(s); it does not consult it today, so a change in behavior needs its own test and bead", flag, len(reads))
		}
	}
}
