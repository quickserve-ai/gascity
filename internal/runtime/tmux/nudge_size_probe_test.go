//go:build integration && nudgeprobe

package tmux

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestNudgeSizeProbe measures, from the RECEIVING side, whether a nudge of a
// given size reaches a real Claude Code TUI whole (pl-7tq). It is a manual
// measurement harness, not a regression test: it needs a real `claude` binary
// and never runs in CI (build tag nudgeprobe).
//
// The receiver is a claude started by GC_NUDGE_PROBE_RECEIVER (a launch
// script) with a UserPromptSubmit hook that copies the hook's stdin JSON into
// GC_NUDGE_PROBE_SINK and exits 2, which blocks the prompt: no model call is
// made. The hook's "prompt" field is exactly what claude would have sent, so
// it is the ground truth for what arrived.
//
// Every payload carries its index in the first 20 bytes and in the last 30,
// so a whole, head-cut or tail-cut arrival is each identifiable. A cut is
// judged from the HEAD: a head-cut message still ends perfectly.
//
//	GC_NUDGE_PROBE_RECEIVER=/path/run-rx.sh GC_NUDGE_PROBE_SINK=/path/sink \
//	GC_NUDGE_PROBE_OUT=/path/table.tsv \
//	go test -tags integration,nudgeprobe -run TestNudgeSizeProbe -v -timeout 60m ./internal/runtime/tmux/
func TestNudgeSizeProbe(t *testing.T) {
	receiver := os.Getenv("GC_NUDGE_PROBE_RECEIVER")
	sink := os.Getenv("GC_NUDGE_PROBE_SINK")
	if receiver == "" || sink == "" {
		t.Skip("GC_NUDGE_PROBE_RECEIVER and GC_NUDGE_PROBE_SINK must be set")
	}
	sizes := []int{512, 1024, 1400, 2048, 4096, 4097, 8192}
	if v := os.Getenv("GC_NUDGE_PROBE_SIZES"); v != "" {
		sizes = nil
		for _, f := range strings.Split(v, ",") {
			var n int
			if _, err := fmt.Sscanf(f, "%d", &n); err != nil {
				t.Fatalf("GC_NUDGE_PROBE_SIZES %q: %v", v, err)
			}
			sizes = append(sizes, n)
		}
	}
	reps := 3
	// Each path is a GC_PROVIDER value on the receiver session. "claude"
	// takes the claude send path (bracketed paste above 512 bytes or with a
	// newline, when the pane brackets pastes). Any other value takes the
	// generic path: `send-keys -l` keystrokes up to maxSendKeysLiteralLen and
	// paste-buffer above it, the path a non-claude family gets and the path a
	// claude pane gets when its bracketed-paste mode cannot be confirmed.
	paths := []string{"claude", "keystrokes"}
	if v := os.Getenv("GC_NUDGE_PROBE_PATHS"); v != "" {
		paths = strings.Split(v, ",")
	}

	var out *os.File
	if p := os.Getenv("GC_NUDGE_PROBE_OUT"); p != "" {
		f, err := os.Create(p)
		if err != nil {
			t.Fatalf("creating %s: %v", p, err)
		}
		defer f.Close() //nolint:errcheck
		out = f
		fmt.Fprintln(out, "path\tshape\tsent_bytes\trep\tarrived\thead\ttail\twhole\trecv_bytes\tframes\tsend_err\trecv_head\trecv_tail") //nolint:errcheck
	}

	tm := testTmux()
	idx := 0
	for _, path := range paths {
		provider := path
		if path == "keystrokes" {
			provider = "probe-generic"
		}
		session := fmt.Sprintf("gt-test-nudge-probe-%s-%d", path, time.Now().UnixNano()%100000)
		if err := tm.NewSessionWithCommandAndEnv(session, filepath.Dir(sink), receiver, map[string]string{"GC_PROVIDER": provider}); err != nil {
			t.Fatalf("starting receiver: %v", err)
		}
		waitForProbeReceiver(t, tm, session)
		// A freshly drawn prompt can drop the first keystrokes it is sent, so
		// warm the receiver until one short nudge round-trips.
		for w := 0; w < 8; w++ {
			clearProbeSink(t, sink)
			_ = tm.NudgeSession(session, "warmup")
			if p, ok := waitForProbePrompt(sink, 10*time.Second); ok && strings.Contains(p, "warmup") {
				break
			}
		}
		for _, size := range sizes {
			for _, multiline := range []bool{false, true} {
				shape := "single"
				if multiline {
					shape = "multi"
				}
				for rep := 1; rep <= reps; rep++ {
					idx++
					msg := probePayload(idx, size, multiline)
					clearProbeSink(t, sink)
					sendErr := tm.NudgeSession(session, msg)
					prompt, arrived := waitForProbePrompt(sink, 20*time.Second)
					r := judgeProbeArrival(msg, prompt, arrived)
					if keep := os.Getenv("GC_NUDGE_PROBE_KEEP"); keep != "" && arrived {
						_ = os.WriteFile(filepath.Join(keep, fmt.Sprintf("%s-%s-%d-%d.txt", path, shape, len(msg), rep)), []byte(prompt), 0o600)
					}
					errText := ""
					if sendErr != nil {
						errText = sendErr.Error()
						if errors.Is(sendErr, ErrNudgeSubmitUnconfirmed) || errors.Is(sendErr, ErrNudgeSubmitDeliveredUnobserved) {
							// The blocking hook leaves the pane idle, so the
							// submit confirmation cannot observe a turn.
							errText = "unconfirmed(expected)"
						}
					}
					t.Logf("%s %s %5d #%d: arrived=%v head=%v tail=%v whole=%v recv=%d frames=%d err=%s head=%q tail=%q",
						path, shape, len(msg), rep, arrived, r.head, r.tail, r.whole, r.recvBytes, r.frames, errText, r.recvHead, r.recvTail)
					if out != nil {
						fmt.Fprintf(out, "%s\t%s\t%d\t%d\t%v\t%v\t%v\t%v\t%d\t%d\t%s\t%q\t%q\n", //nolint:errcheck
							path, shape, len(msg), rep, arrived, r.head, r.tail, r.whole, r.recvBytes, r.frames, errText, r.recvHead, r.recvTail)
					}
					if !arrived {
						// Leave nothing drafted for the next send.
						_, _ = tm.run("send-keys", "-t", session, "Escape")
						time.Sleep(300 * time.Millisecond)
						_, _ = tm.run("send-keys", "-t", session, "C-u")
					}
					time.Sleep(500 * time.Millisecond)
				}
			}
		}
		_ = tm.KillSession(session)
	}
}

// probePayload builds a size-byte payload whose first 20 bytes carry
// "P<idx>-HEAD|" and whose last 30 carry "|P<idx>-TAIL-<size>". The filler
// names its own byte offset every 8 bytes, so a cut point is readable from
// the first bytes that arrived.
func probePayload(idx, size int, multiline bool) string {
	head := fmt.Sprintf("P%04d-HEAD|", idx)
	tail := fmt.Sprintf("|P%04d-TAIL-%05d", idx, size)
	var b strings.Builder
	b.WriteString(head)
	line := b.Len()
	for b.Len() < size-len(tail) {
		if multiline && line >= 56 {
			b.WriteString("\n")
			line = 0
			continue
		}
		fmt.Fprintf(&b, "o%06d ", b.Len())
		line += 8
	}
	s := b.String()
	if len(s) > size-len(tail) {
		s = s[:size-len(tail)]
	}
	return s + tail
}

type probeResult struct {
	head, tail, whole  bool
	recvBytes, frames  int
	recvHead, recvTail string
}

func judgeProbeArrival(sent, prompt string, arrived bool) probeResult {
	if !arrived {
		return probeResult{}
	}
	// A paste reaches claude with each LF as CR, which its paste handler turns
	// back into LF; normalise both so only content differences count.
	norm := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n") }
	got := norm(unwrapPastedContent(prompt))
	want := norm(sent)
	head := want[:strings.Index(want, "|")+1]
	tail := want[strings.LastIndex(want, "|"):]
	return probeResult{
		head:      strings.HasPrefix(strings.TrimSpace(got), head),
		tail:      strings.HasSuffix(strings.TrimSpace(got), tail),
		whole:     strings.TrimSpace(got) == strings.TrimSpace(want),
		recvBytes: len(prompt),
		frames:    strings.Count(prompt, "<pasted_content id="),
		recvHead:  headOf(prompt, 24),
		recvTail:  tailOf(prompt, 30),
	}
}

// unwrapPastedContent replaces each <pasted_content id="..."> frame Claude
// Code wraps around a paste with the frame's body, keeping any text between
// frames, so a message that arrived as several pastes is judged by its bytes.
func unwrapPastedContent(prompt string) string {
	var b strings.Builder
	rest := prompt
	for {
		open := strings.Index(rest, "<pasted_content id=")
		if open < 0 {
			b.WriteString(rest)
			return b.String()
		}
		bodyStart := strings.Index(rest[open:], ">\n")
		closeRel := strings.Index(rest[open:], "\n</pasted_content id=")
		if bodyStart < 0 || closeRel < bodyStart+2 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:open])
		b.WriteString(rest[open+bodyStart+2 : open+closeRel])
		after := rest[open+closeRel+1:]
		end := strings.Index(after, ">")
		if end < 0 {
			return b.String()
		}
		rest = after[end+1:]
	}
}

func waitForProbeReceiver(t *testing.T, tm *Tmux, session string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		lines, err := tm.CapturePaneLines(session, 50)
		if err == nil && strings.Contains(strings.Join(lines, "\n"), "❯") {
			if flag, _ := tm.paneBracketPasteFlag(session); flag == "1" {
				time.Sleep(2 * time.Second)
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	lines, _ := tm.CapturePaneLines(session, 50)
	t.Fatalf("receiver claude never reached its prompt with bracketed paste on:\n%s", strings.Join(lines, "\n"))
}

func clearProbeSink(t *testing.T, sink string) {
	t.Helper()
	entries, err := os.ReadDir(sink)
	if err != nil {
		t.Fatalf("reading sink: %v", err)
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(sink, e.Name()))
	}
}

// waitForProbePrompt returns the prompt of the first capture that lands in
// sink. A nudge that never submits (or is dropped) returns arrived=false.
func waitForProbePrompt(sink string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(sink)
		var names []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		if len(names) > 0 {
			data, err := os.ReadFile(filepath.Join(sink, names[0]))
			if err == nil {
				var in struct {
					Prompt string `json:"prompt"`
				}
				if json.Unmarshal(data, &in) == nil {
					return in.Prompt, true
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", false
}
