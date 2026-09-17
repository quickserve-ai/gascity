//go:build integration

package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	bracketedPasteStart = "\x1b[200~"
	bracketedPasteEnd   = "\x1b[201~"
)

// TestNudgeSessionDeliversLongClaudeNudgeAsOneBracketedPaste is the real-tmux
// half of ga-6qfgdo. It drives the real NudgeSession against an isolated tmux
// server (the package test socket under TestMain's private TMUX_TMPDIR) and a
// pane that behaves like Claude Code at the byte level: it enables bracketed
// paste mode (ESC[?2004h), puts its tty in raw mode, and records every input
// byte it receives.
//
// A 2.6KB multi-line nudge must arrive as exactly ONE ESC[200~ ... ESC[201~
// frame whose body is the whole message, followed only by the submit. Sent as
// unbracketed keystrokes instead, it reaches Claude Code as several 1022-byte
// reads with no frame, and all but the last read are lost.
func TestNudgeSessionDeliversLongClaudeNudgeAsOneBracketedPaste(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	record := filepath.Join(dir, "pane-input.bin")
	reader := filepath.Join(dir, "bracketed-reader.sh")
	script := strings.Join([]string{
		`printf '\033[?2004h'`,
		`stty raw -echo`,
		`printf ready > ` + shellQuote(ready),
		`exec cat > ` + shellQuote(record),
	}, "\n") + "\n"
	if err := os.WriteFile(reader, []byte(script), 0o600); err != nil {
		t.Fatalf("writing reader script: %v", err)
	}

	sessionName := fmt.Sprintf("gt-test-nudge-bracketed-%d", time.Now().UnixNano()%100000)
	_ = tm.KillSession(sessionName)
	// GC_PROVIDER=claude selects the claude send path, no pre-submit Escape,
	// and submit confirmation, as on a live claude seat.
	if err := tm.NewSessionWithCommandAndEnv(sessionName, dir, "sh "+shellQuote(reader), map[string]string{
		"GC_PROVIDER": "claude",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })
	waitForFileContents(t, ready, 10*time.Second)

	var b strings.Builder
	b.WriteString("HEAD-SENTINEL-START ga-6qfgdo bracketed paste probe\n")
	for line := 0; b.Len() < 2600; line++ {
		fmt.Fprintf(&b, "line %03d: relayed context a seat must receive in full\n", line)
	}
	b.WriteString("TAIL-SENTINEL-END")
	message := b.String()

	err := tm.NudgeSession(sessionName, message)
	// The reader never renders a busy indicator, so the claude submit
	// confirmation cannot observe the turn start; that verdict is not under test.
	if err != nil && !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("NudgeSession: %v", err)
	}
	t.Logf("NudgeSession(%d bytes, %d newlines) returned %v", len(message), strings.Count(message, "\n"), err)

	got := waitForBracketedPasteSubmit(t, record, 10*time.Second)

	if starts, ends := strings.Count(got, bracketedPasteStart), strings.Count(got, bracketedPasteEnd); starts != 1 || ends != 1 {
		t.Fatalf("pane received %d ESC[200~ and %d ESC[201~ markers, want exactly one bracketed paste frame; %d bytes received, head %q",
			starts, ends, len(got), headOf(got, 80))
	}
	start := strings.Index(got, bracketedPasteStart)
	end := strings.Index(got, bracketedPasteEnd)
	if end < start {
		t.Fatalf("ESC[201~ at %d precedes ESC[200~ at %d", end, start)
	}

	// tmux's paste-buffer turns each LF into CR on output (it has no -r here),
	// and Claude Code's paste handler turns CR back into LF, so the frame body
	// must be the message with exactly that substitution: every byte and every
	// line break present, nothing added.
	body := got[start+len(bracketedPasteStart) : end]
	if want := strings.ReplaceAll(message, "\n", "\r"); body != want {
		t.Fatalf("bracketed paste body (%d bytes, head %q, tail %q) != message (%d bytes, head %q, tail %q)",
			len(body), headOf(body, 40), tailOf(body, 40), len(want), headOf(want, 40), tailOf(want, 40))
	}
	if gotLines, wantLines := strings.Count(body, "\r"), strings.Count(message, "\n"); gotLines != wantLines {
		t.Fatalf("bracketed paste body carries %d line breaks, message has %d", gotLines, wantLines)
	}

	// Outside the frame only NudgeSession's own keys may appear: the C-u that
	// clears a detached pane's stale draft before the paste, and the submit
	// Enter (re-sent while unconfirmed) after it. Any message text out here
	// means part of the nudge went out as keystrokes.
	if before := got[:start]; strings.Trim(before, "\x15") != "" {
		t.Fatalf("bytes before the paste frame = %q, want only C-u", before)
	}
	after := got[end+len(bracketedPasteEnd):]
	if after == "" || strings.Trim(after, "\r") != "" {
		t.Fatalf("bytes after the paste frame = %q, want one or more submit Enters (CR)", after)
	}
}

// TestNudgeSessionKeepsKeystrokesWhenPaneHasNoBracketedPaste is the real-tmux
// half of the bracket_paste_flag guard. Its reader never sends ESC[?2004h, so
// tmux reports #{bracket_paste_flag}=0 for the pane. `paste-buffer -p` into such
// a pane adds no markers and writes each newline as a CR ("line one\rline
// two\r..."), so every line would submit on its own. A multi-line claude nudge
// must instead keep the send-keys path: the pane receives the message with its
// line feeds intact, preceded only by the C-u and followed only by the submit.
func TestNudgeSessionKeepsKeystrokesWhenPaneHasNoBracketedPaste(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	record := filepath.Join(dir, "pane-input.bin")
	reader := filepath.Join(dir, "plain-reader.sh")
	script := strings.Join([]string{
		`stty raw -echo`,
		`printf ready > ` + shellQuote(ready),
		`exec cat > ` + shellQuote(record),
	}, "\n") + "\n"
	if err := os.WriteFile(reader, []byte(script), 0o600); err != nil {
		t.Fatalf("writing reader script: %v", err)
	}

	sessionName := fmt.Sprintf("gt-test-nudge-nobracket-%d", time.Now().UnixNano()%100000)
	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, dir, "sh "+shellQuote(reader), map[string]string{
		"GC_PROVIDER": "claude",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(sessionName) })
	waitForFileContents(t, ready, 10*time.Second)

	if flag, err := tm.run("display-message", "-t", sessionName, "-p", "#{bracket_paste_flag}"); err != nil || strings.TrimSpace(flag) == "1" {
		t.Fatalf("reader pane #{bracket_paste_flag} = %q, %v; want bracketed paste off", flag, err)
	}

	const tail = "TAIL-SENTINEL line four"
	message := "HEAD-SENTINEL line one\nline two\nline three\n" + tail
	err := tm.NudgeSession(sessionName, message)
	if err != nil && !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("NudgeSession: %v", err)
	}

	got := waitForSubmitAfter(t, record, tail, 10*time.Second)
	if strings.Contains(got, bracketedPasteStart) || strings.Contains(got, bracketedPasteEnd) {
		t.Fatalf("pane without bracketed paste received paste markers: %q", got)
	}
	at := strings.Index(got, message)
	if at < 0 {
		t.Fatalf("pane did not receive the nudge with its line feeds intact (a raw paste turns each into a CR that submits the line): got %q", got)
	}
	if before := got[:at]; strings.Trim(before, "\x15") != "" {
		t.Fatalf("bytes before the nudge = %q, want only C-u", before)
	}
	if after := got[at+len(message):]; after == "" || strings.Trim(after, "\r") != "" {
		t.Fatalf("bytes after the nudge = %q, want one or more submit Enters (CR)", after)
	}
}

// waitForSubmitAfter polls the reader's record until marker is followed by a
// CR, then returns it; on timeout it returns what arrived.
func waitForSubmitAfter(t *testing.T, path, marker string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, _ := os.ReadFile(path)
		got := string(data)
		if at := strings.LastIndex(got, marker); at >= 0 && strings.Contains(got[at:], "\r") {
			return got
		}
		if time.Now().After(deadline) {
			t.Logf("no submit after %q within %s", marker, timeout)
			return got
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitForBracketedPasteSubmit polls the reader's record until it holds a
// closing ESC[201~ followed by at least one submit CR, then returns it. If that
// never happens it returns what arrived, so the caller's frame assertions report
// the actual bytes instead of a bare timeout.
func waitForBracketedPasteSubmit(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, _ := os.ReadFile(path)
		got := string(data)
		if end := strings.LastIndex(got, bracketedPasteEnd); end >= 0 && strings.Contains(got[end:], "\r") {
			return got
		}
		select {
		case <-timer.C:
			t.Logf("no bracketed paste frame followed by a submit within %s", timeout)
			return got
		case <-ticker.C:
		}
	}
}

func headOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
