package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// indexedNudgeBody returns a size-byte message that names idx in its first and
// last bytes, so a test can tell a whole delivery from a head- or tail-cut one.
func indexedNudgeBody(idx, size int) string {
	head := fmt.Sprintf("N%02d-HEAD|", idx)
	tail := fmt.Sprintf("|N%02d-TAIL", idx)
	return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
}

func TestPackNudgeInjectForHookBudgetKeepsEverythingThatFits(t *testing.T) {
	items := []queuedNudge{
		{ID: "a", Source: "session", Message: indexedNudgeBody(1, 3000)},
		{ID: "b", Source: "session", Message: indexedNudgeBody(2, 3000)},
	}
	content, delivered, deferred := packNudgeInjectForHookBudget("clock\n", items, "step\n", claudeHookContextMaxChars)
	if len(delivered) != 2 || len(deferred) != 0 {
		t.Fatalf("delivered %d, deferred %d; want 2 and 0", len(delivered), len(deferred))
	}
	if want := "clock\n" + formatNudgeInjectOutput(items) + "step\n"; content != want {
		t.Fatalf("content differs from the unbudgeted payload:\n got %q\nwant %q", headOf(content, 80), headOf(want, 80))
	}
}

// A batch the hook output cannot carry whole is split: the items that fit go
// now and the rest wait for the next prompt, instead of Claude Code swapping
// the whole output for a head preview and a file path (pl-7tq).
func TestPackNudgeInjectForHookBudgetDefersItemsPastTheBudget(t *testing.T) {
	items := []queuedNudge{
		{ID: "a", Source: "session", Message: indexedNudgeBody(1, 6000)},
		{ID: "b", Source: "session", Message: indexedNudgeBody(2, 6000)},
	}
	content, delivered, deferred := packNudgeInjectForHookBudget("clock\n", items, "", claudeHookContextMaxChars)
	if len(content) > claudeHookContextMaxChars {
		t.Fatalf("hook output is %d bytes, over the %d Claude Code attaches whole", len(content), claudeHookContextMaxChars)
	}
	if len(delivered) != 1 || delivered[0].ID != "a" {
		t.Fatalf("delivered = %v, want only the first item", queuedNudgeIDs(delivered))
	}
	if len(deferred) != 1 || deferred[0].ID != "b" {
		t.Fatalf("deferred = %v, want the second item", queuedNudgeIDs(deferred))
	}
	if !strings.Contains(content, items[0].Message) || strings.Contains(content, "N02-HEAD") {
		t.Fatalf("content must carry item a whole and nothing of item b: %q", headOf(content, 120))
	}
}

// The active-step reminder comes back on every prompt; a queued nudge is
// delivered once. When both cannot fit, the reminder waits.
func TestPackNudgeInjectForHookBudgetDropsTheStepReminderBeforeANudge(t *testing.T) {
	items := []queuedNudge{{ID: "a", Source: "session", Message: indexedNudgeBody(1, 4000)}}
	step := "<system-reminder>\n" + strings.Repeat("step detail ", 600) + "\n</system-reminder>\n"
	content, delivered, deferred := packNudgeInjectForHookBudget("clock\n", items, step, claudeHookContextMaxChars)
	if len(delivered) != 1 || len(deferred) != 0 {
		t.Fatalf("delivered %d, deferred %d; want the nudge delivered", len(delivered), len(deferred))
	}
	if len(content) > claudeHookContextMaxChars {
		t.Fatalf("hook output is %d bytes, over %d", len(content), claudeHookContextMaxChars)
	}
	if strings.Contains(content, "step detail") {
		t.Fatalf("step reminder was kept although it pushes the output past the budget")
	}
	if !strings.Contains(content, items[0].Message) {
		t.Fatalf("nudge not delivered whole")
	}
}

// A single item that cannot fit on its own is still delivered, so the queue
// makes progress: Claude Code then saves it to a file and tells the agent
// where, which is loud. The sender refuses such a message before it is
// queued (see TestDeliverSessionNudgeRefusesOversizeForClaude).
func TestPackNudgeInjectForHookBudgetAlwaysDeliversTheFirstItem(t *testing.T) {
	items := []queuedNudge{
		{ID: "a", Source: "session", Message: indexedNudgeBody(1, 12000)},
		{ID: "b", Source: "session", Message: "short"},
	}
	_, delivered, deferred := packNudgeInjectForHookBudget("", items, "", claudeHookContextMaxChars)
	if len(delivered) != 1 || delivered[0].ID != "a" || len(deferred) != 1 {
		t.Fatalf("delivered %v deferred %v; want a now, b later", queuedNudgeIDs(delivered), queuedNudgeIDs(deferred))
	}
}

func TestPackNudgeInjectForHookBudgetZeroBudgetIsUnbounded(t *testing.T) {
	items := []queuedNudge{
		{ID: "a", Source: "session", Message: indexedNudgeBody(1, 9000)},
		{ID: "b", Source: "session", Message: indexedNudgeBody(2, 9000)},
	}
	content, delivered, deferred := packNudgeInjectForHookBudget("p", items, "s", 0)
	if len(delivered) != 2 || len(deferred) != 0 || content != "p"+formatNudgeInjectOutput(items)+"s" {
		t.Fatalf("an unbudgeted hook format must carry the whole batch")
	}
}

// End to end through `gc nudge drain --inject` in Claude's raw hook format:
// two queued 6 KB nudges cannot share one 10,000-character hook output, so
// the first drain carries one whole and leaves the other queued, and the next
// drain carries the other.
func TestCmdNudgeDrainInjectSplitsABatchOverTheClaudeHookBudget(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	writeNamedSessionCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "Session: worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"state":        string(session.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("store.Create session: %v", err)
	}
	messages := []string{indexedNudgeBody(1, 6000), indexedNudgeBody(2, 6000)}
	for i, msg := range messages {
		item := newQueuedNudgeWithOptions("worker", msg, "session", time.Now().Add(-time.Minute+time.Duration(i)*time.Second), queuedNudgeOptions{
			SessionID: created.ID,
		})
		if err := enqueueQueuedNudgeWithStore(cityDir, beads.NudgesStore{Store: store}, item); err != nil {
			t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
		}
	}

	var got []string
	for pass := 1; pass <= 2; pass++ {
		var stdout, stderr bytes.Buffer
		if code := cmdNudgeDrainWithFormat([]string{created.ID}, true, "", &stdout, &stderr); code != 0 {
			t.Fatalf("drain pass %d = %d; stderr=%s", pass, code, stderr.String())
		}
		out := stdout.String()
		if len(out) > claudeHookContextMaxChars {
			t.Fatalf("drain pass %d wrote %d bytes of hook output, over the %d Claude Code attaches whole", pass, len(out), claudeHookContextMaxChars)
		}
		got = append(got, out)
	}
	for i, msg := range messages {
		if !strings.Contains(got[i], msg) {
			t.Fatalf("drain pass %d does not carry nudge %d whole (head %q)", i+1, i+1, headOf(got[i], 200))
		}
	}
	if strings.Contains(got[0], "N02-HEAD") {
		t.Fatalf("first drain carried part of the second nudge")
	}
}

func headOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// A Claude seat can receive a nudge through the queued hook path whatever
// delivery mode the sender asked for (a busy or asleep seat is queued), and
// that path cuts past maxClaudeNudgeMessageBytes. So the sender is refused
// before anything is queued or typed, with the limit named, instead of being
// told "Queued" or "Nudged" about a message the seat will not see whole.
func TestDeliverSessionNudgeRefusesOversizeForClaude(t *testing.T) {
	for _, mode := range []nudgeDeliveryMode{nudgeDeliveryImmediate, nudgeDeliveryWaitIdle, nudgeDeliveryQueue} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			dir := t.TempDir()
			store := openNudgeBeadStore(dir)
			fake := runtime.NewFake()
			target := nudgeTarget{
				cityPath:    dir,
				agent:       config.Agent{Name: "worker"},
				resolved:    &config.ResolvedProvider{Name: "claude"},
				sessionName: "sess-worker",
			}
			message := indexedNudgeBody(1, maxClaudeNudgeMessageBytes+1)

			var stdout, stderr bytes.Buffer
			code := deliverSessionNudgeWithWorker(target, store, fake, message, mode, false, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("oversize nudge exited 0; stdout=%q", stdout.String())
			}
			if !strings.Contains(stderr.String(), fmt.Sprint(maxClaudeNudgeMessageBytes)) {
				t.Fatalf("stderr = %q, want the %d-byte limit named", stderr.String(), maxClaudeNudgeMessageBytes)
			}
			if len(fake.Calls) != 0 {
				t.Fatalf("provider was called for a refused nudge: %#v", fake.Calls)
			}
			pending, inFlight, _, err := listQueuedNudges(dir, "worker", time.Now())
			if err != nil {
				t.Fatalf("listQueuedNudges: %v", err)
			}
			if len(pending)+len(inFlight) != 0 {
				t.Fatalf("refused nudge was queued")
			}
		})
	}
}

// The limit is exact: a message of maxClaudeNudgeMessageBytes is accepted.
// Other families are not held to it; their limits are unmeasured.
func TestDeliverSessionNudgeAcceptsTheClaudeLimitAndOtherFamilies(t *testing.T) {
	for _, tc := range []struct {
		provider string
		size     int
	}{
		{"claude", maxClaudeNudgeMessageBytes},
		{"codex", maxClaudeNudgeMessageBytes + 1},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			dir := t.TempDir()
			store := openNudgeBeadStore(dir)
			fake := runtime.NewFake()
			target := nudgeTarget{
				cityPath:    dir,
				agent:       config.Agent{Name: "worker"},
				resolved:    &config.ResolvedProvider{Name: tc.provider},
				sessionName: "sess-worker",
			}
			var stdout, stderr bytes.Buffer
			code := deliverSessionNudgeWithWorker(target, store, fake, indexedNudgeBody(1, tc.size), nudgeDeliveryQueue, false, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, stderr.String())
			}
			pending, _, _, err := listQueuedNudges(dir, "worker", time.Now())
			if err != nil {
				t.Fatalf("listQueuedNudges: %v", err)
			}
			if len(pending) != 1 {
				t.Fatalf("pending = %d, want the nudge queued", len(pending))
			}
		})
	}
}
