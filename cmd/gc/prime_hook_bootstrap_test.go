package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestDoPrimeWithHook_OversizedPromptBootstrap drives the SessionStart hook
// for a seat whose prompt (~150 KB, the shape of a real seat prompt) is past
// the hook limit. A Claude conversation started by hand in the seat's pane,
// or restarted by /compact or /clear inside a gc-launched one, must get a
// bootstrap under the limit naming a file that holds exactly what `gc prime`
// prints for the seat. A gc-launched seat's startup and resume
// hooks stay as they were (no prompt, no file: the launch delivered it), and
// a provider that does not cap hook context still gets its prompt whole.
func TestDoPrimeWithHook_OversizedPromptBootstrap(t *testing.T) {
	for _, tc := range []struct {
		name, provider, delivered, source string
		wantBootstrap, wantWhole          bool
	}{
		{name: "started by hand", provider: "claude", source: "startup", wantBootstrap: true},
		{name: "compact in launched seat", provider: "claude", delivered: "1", source: "compact", wantBootstrap: true},
		{name: "clear in launched seat", provider: "claude", delivered: "1", source: "clear", wantBootstrap: true},
		{name: "launched seat startup", provider: "claude", delivered: "1", source: "startup"},
		{name: "launched seat resume", provider: "claude", delivered: "1", source: "resume"},
		{name: "uncapped provider", provider: "codex", source: "startup", wantWhole: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := t.TempDir()
			var prompt strings.Builder
			prompt.WriteString("# Worker Context\n\nRun the startup steps below before anything else.\n\n")
			lastRule := ""
			for i := 0; prompt.Len() < 150_000; i++ {
				lastRule = fmt.Sprintf("Rule %d: a line of seat doctrine the hook could never carry whole.\n", i)
				prompt.WriteString(lastRule)
			}
			writePrimeHookTestFile(t, filepath.Join(cityDir, "prompts", "worker.md"), prompt.String())
			writePrimeHookTestFile(t, filepath.Join(cityDir, "city.toml"), "[workspace]\nname = \"gastown\"\n\n[providers."+tc.provider+"]\nbase = \"builtin:"+tc.provider+"\"\n\n[[agent]]\nname = \"worker\"\nprovider = \""+tc.provider+"\"\nprompt_template = \"prompts/worker.md\"\n")
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_AGENT", "worker")
			t.Setenv("GC_ALIAS", "worker")
			t.Setenv("GC_TEMPLATE", "worker")
			t.Setenv("GC_SESSION_NAME", "gastown--worker")
			t.Setenv("GC_SESSION_ID", createPrimeHookSession(t, cityDir, "gastown--worker", "worker"))
			t.Setenv(managedSessionHookEnv, "1")
			t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
			t.Setenv(startupPromptDeliveredEnv, tc.delivered)

			withPrimeHookStdin(t)
			var want, wantErr bytes.Buffer
			if code := doPrimeWithMode(nil, &want, &wantErr, false, false); code != 0 {
				t.Fatalf("gc prime = %d, want 0; stderr=%q", code, wantErr.String())
			}

			// Claude Code passes the source on stdin, not in the environment.
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(writer, "{\"hook_event_name\":\"SessionStart\",\"source\":%q}\n", tc.source) //nolint:errcheck // pipe write in test
			_ = writer.Close()
			primeStdin = func() *os.File { return reader }
			t.Cleanup(func() { _ = reader.Close() })

			var stdout, stderr bytes.Buffer
			if code := doPrimeWithHookFormat(nil, &stdout, &stderr, true, hookOutputFormatCodex, false); code != 0 {
				t.Fatalf("gc prime --hook = %d, want 0; stderr=%q", code, stderr.String())
			}
			var out struct {
				HookSpecificOutput struct {
					AdditionalContext string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatalf("hook output is not JSON: %v (stderr %q)", err, stderr.String())
			}
			context := out.HookSpecificOutput.AdditionalContext
			path := filepath.Join(cityDir, ".gc", "sessions", "gastown--worker", primeHookPromptFileName)
			got, readErr := os.ReadFile(path)

			if !tc.wantBootstrap {
				if !os.IsNotExist(readErr) {
					t.Fatalf("%s hook wrote %s (read err %v)", tc.name, path, readErr)
				}
				hasWhole := strings.Contains(context, "# Worker Context") && strings.Contains(context, strings.TrimSuffix(lastRule, "\n")) && !strings.Contains(context, "read each file named above")
				if hasWhole != tc.wantWhole {
					t.Fatalf("%s hook carries the whole prompt = %v, want %v (%d bytes, ends %q)", tc.name, hasWhole, tc.wantWhole, len(context), context[max(0, len(context)-200):])
				}
				return
			}
			if len(context) >= primeHookContextLimit {
				t.Fatalf("bootstrap is %d bytes, want under %d", len(context), primeHookContextLimit)
			}
			for _, wantText := range []string{path, "[gastown] worker", "read each file named above to its end", "# Worker Context"} {
				if !strings.Contains(context, wantText) {
					t.Errorf("bootstrap missing %q (stderr %q):\n%.800s", wantText, stderr.String(), context)
				}
			}
			if readErr != nil {
				t.Fatalf("reading prompt file: %v", readErr)
			}
			if !bytes.Equal(got, want.Bytes()) {
				t.Fatalf("prompt file (%d bytes) differs from gc prime output (%d bytes)", len(got), want.Len())
			}
		})
	}
}

// TestDoPrimeWithHook_GenericFallbackBoundsContext drives the SessionStart
// hook down the generic-prompt branch (a seat with no prompt template and no
// builtin prompt, and a hook under an alias that names no agent) with a 12 KB
// assignment. The delivered context must stay under the limit, with the
// assignment in the seat's context file.
func TestDoPrimeWithHook_GenericFallbackBoundsContext(t *testing.T) {
	for _, tc := range []struct{ name, alias string }{
		{name: "agent without prompt", alias: "worker"},
		{name: "alias naming no agent", alias: "helper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := t.TempDir()
			writePrimeHookTestFile(t, filepath.Join(cityDir, "city.toml"), "[workspace]\nname = \"gastown\"\nprovider = \"claude\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n\n[[agent]]\nname = \"worker\"\n")
			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatalf("openCityStoreAt: %v", err)
			}
			mol := mustCreateInProgressStore(t, store, beads.Bead{Title: "Formula: mol-worker", Type: "molecule", Assignee: tc.alias})
			assignment := strings.Repeat("assignment detail line\n", 540) // ~12 KB
			mustCreateInProgressStore(t, store, beads.Bead{Title: "Step 1: the big one", Description: assignment, Type: "step", Assignee: tc.alias, ParentID: mol.ID})
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_AGENT", tc.alias)
			t.Setenv("GC_ALIAS", tc.alias)
			t.Setenv("GC_SESSION_NAME", "gastown--"+tc.alias)
			t.Setenv("GC_SESSION_ID", createPrimeHookSession(t, cityDir, "gastown--"+tc.alias, tc.alias))
			t.Setenv(managedSessionHookEnv, "1")
			t.Setenv("GC_HOOK_EVENT_NAME", "SessionStart")
			withPrimeHookStdin(t)

			var stdout, stderr bytes.Buffer
			if code := doPrimeWithHookFormat(nil, &stdout, &stderr, true, hookOutputFormatCodex, false); code != 0 {
				t.Fatalf("gc prime --hook = %d, want 0; stderr=%q", code, stderr.String())
			}
			var out struct {
				HookSpecificOutput struct {
					AdditionalContext string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatalf("hook output is not JSON: %v (stderr %q)", err, stderr.String())
			}
			context := out.HookSpecificOutput.AdditionalContext
			if len(context) >= primeHookContextLimit {
				t.Fatalf("generic-fallback context is %d bytes, want under %d (stderr %q)", len(context), primeHookContextLimit, stderr.String())
			}
			got, err := os.ReadFile(filepath.Join(cityDir, ".gc", "sessions", "gastown--"+tc.alias, primeHookContextFileName))
			if err != nil || !strings.Contains(string(got), strings.TrimSpace(assignment)) {
				t.Fatalf("context file does not hold the assignment (%d bytes, err %v)", len(got), err)
			}
		})
	}
}

// fitTestSeat is a capped seat in a fresh city directory.
func fitTestSeat(t *testing.T) primeHookSeat {
	return primeHookSeat{cityPath: t.TempDir(), cityName: "gastown", agentName: "worker", sessionName: "gastown--worker", capped: true}
}

func fitTestFile(t *testing.T, seat primeHookSeat, name string) (string, bool) {
	t.Helper()
	dir, err := primeHookSeatDir(seat.cityPath, seat.sessionName)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

// fitTestDelivered is the context writePrimePromptWithFormat would deliver.
func fitTestDelivered(seat primeHookSeat, prompt string, suppress bool, suffix string) string {
	if suppress {
		prompt = ""
	}
	return prependHookBeacon(seat.cityName, seat.agentName, prompt) + suffix
}

// TestPrimeHookFitContextBoundsEveryBranch covers the complete delivered
// context staying under the limit: an oversized prompt, an oversized
// assignment beside it, an oversized assignment on a launched seat whose
// prompt is suppressed, and an unwritable runtime directory.
func TestPrimeHookFitContextBoundsEveryBranch(t *testing.T) {
	big := strings.Repeat("doctrine line\n", 2000)
	bigStep := "\n\n## Current step\n" + strings.Repeat("assignment detail\n", 1000)
	for _, tc := range []struct {
		name, prompt, suffix string
		suppress             bool
		wantPrompt, wantStep bool // files written
		wantInline           bool // suffix delivered in the context itself
	}{
		{name: "oversized prompt", prompt: big, suffix: "\n\nstep: small", wantPrompt: true, wantInline: true},
		{name: "oversized prompt and assignment", prompt: big, suffix: bigStep, wantPrompt: true, wantStep: true},
		{name: "suppressed prompt, oversized assignment", prompt: big, suffix: bigStep, suppress: true, wantStep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seat := fitTestSeat(t)
			var stderr bytes.Buffer
			prompt, suffix, delivered := primeHookFitContext(seat, tc.prompt, tc.suppress, tc.suffix, &stderr)
			ctx := fitTestDelivered(seat, prompt, tc.suppress, suffix)
			if !delivered || len(ctx) >= primeHookContextLimit {
				t.Fatalf("delivered=%v, context %d bytes; want delivered under %d (stderr %q)", delivered, len(ctx), primeHookContextLimit, stderr.String())
			}
			if got, ok := fitTestFile(t, seat, primeHookPromptFileName); ok != tc.wantPrompt || (ok && got != tc.prompt) {
				t.Errorf("prompt file present=%v (want %v) or content differs", ok, tc.wantPrompt)
			}
			if got, ok := fitTestFile(t, seat, primeHookContextFileName); ok != tc.wantStep || (ok && got != tc.suffix) {
				t.Errorf("context file present=%v (want %v) or content differs", ok, tc.wantStep)
			}
			if strings.Contains(ctx, tc.suffix) != tc.wantInline {
				t.Errorf("suffix inline = %v, want %v", !tc.wantInline, tc.wantInline)
			}
		})
	}

	t.Run("write failure", func(t *testing.T) {
		seat := fitTestSeat(t)
		writePrimeHookTestFile(t, filepath.Join(seat.cityPath, ".gc"), "not a dir")
		for _, suffix := range []string{"\n\nstep: small", bigStep} {
			var stderr bytes.Buffer
			prompt, got, delivered := primeHookFitContext(seat, big, false, suffix, &stderr)
			ctx := fitTestDelivered(seat, prompt, false, got)
			if len(ctx) >= primeHookContextLimit || !strings.Contains(ctx, "gc prime worker >") {
				t.Fatalf("failure context is %d bytes or lacks the capture command:\n%.600s", len(ctx), ctx)
			}
			if wantDelivered := suffix != bigStep; delivered != wantDelivered || strings.Contains(ctx, suffix) != wantDelivered {
				t.Fatalf("suffix %d bytes: delivered=%v, want %v", len(suffix), delivered, wantDelivered)
			}
			if !strings.Contains(stderr.String(), "could not be written") {
				t.Fatalf("stderr = %q, want the write failure reported", stderr.String())
			}
		}
	})
}

// TestPrimeHookWriteFailureNoticeFits: the notice that replaces an
// unwritable context fits the budget whatever the error says (an overlong
// session name yields a long ENAMETOOLONG path).
func TestPrimeHookWriteFailureNoticeFits(t *testing.T) {
	seat := fitTestSeat(t)
	seat.sessionName = strings.Repeat("s", 20_000)
	var stderr bytes.Buffer
	prompt, suffix, delivered := primeHookFitContext(seat, strings.Repeat("doctrine line\n", 2000), false, "\n\nstep: small", &stderr)
	ctx := fitTestDelivered(seat, prompt, false, suffix)
	if len(ctx) >= primeHookContextLimit || !strings.Contains(ctx, "gc prime worker >") || !delivered {
		t.Fatalf("failure context is %d bytes (delivered %v), want under %d with the capture command (stderr %d bytes)", len(ctx), delivered, primeHookContextLimit, stderr.Len())
	}

	budget := primeHookContextLimit - primeHookBootstrapMargin
	notice := primeHookWriteFailureNotice("worker", 140_000, errors.New(strings.Repeat("e", 50_000)), budget)
	if len(notice) > budget || !strings.Contains(notice, "gc prime worker >") {
		t.Fatalf("notice for a 50 KB error is %d bytes, want at most %d with the capture command", len(notice), budget)
	}
}

// TestPrimeHookFitContextFiles covers what the seat directory holds: an
// uncapped provider and a fitting prompt write nothing, a later fitting
// prompt removes the files an earlier oversized one left, distinct session
// names never share a directory, and concurrent hooks leave one whole file.
func TestPrimeHookFitContextFiles(t *testing.T) {
	big := strings.Repeat("doctrine line\n", 2000)
	var stderr bytes.Buffer

	seat := fitTestSeat(t)
	seat.capped = false
	if p, s, _ := primeHookFitContext(seat, big, false, "x", &stderr); p != big || s != "x" {
		t.Fatal("uncapped provider: prompt or suffix changed")
	}
	if _, err := os.Stat(filepath.Join(seat.cityPath, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("uncapped provider wrote state (stat err %v)", err)
	}

	seat.capped = true
	primeHookFitContext(seat, big, false, "\n\n"+big, &stderr)
	if _, ok := fitTestFile(t, seat, primeHookContextFileName); !ok {
		t.Fatal("oversized hook wrote no context file")
	}
	if p, s, _ := primeHookFitContext(seat, "short\n", false, "", &stderr); p != "short\n" || s != "" {
		t.Fatalf("fitting prompt changed: %q %q", p, s)
	}
	for _, name := range []string{primeHookPromptFileName, primeHookContextFileName} {
		if _, ok := fitTestFile(t, seat, name); ok {
			t.Errorf("stale %s left after a fitting prompt", name)
		}
	}

	a, errA := primeHookSeatDir("/city", "a/b")
	b, errB := primeHookSeatDir("/city", "a--b")
	if errA != nil || errB != nil || a == b || filepath.Dir(a) != filepath.Dir(b) {
		t.Fatalf("session dirs a/b=%q a--b=%q (errs %v %v): want distinct siblings", a, b, errA, errB)
	}
	for _, name := range []string{"", ".", ".."} {
		if _, err := primeHookSeatDir("/city", name); err == nil {
			t.Errorf("primeHookSeatDir(%q) succeeded, want error", name)
		}
	}

	prompts := make([]string, 8)
	var wg sync.WaitGroup
	for i := range prompts {
		prompts[i] = strings.Repeat(fmt.Sprintf("writer %d line\n", i), 1000)
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			primeHookFitContext(seat, p, false, "", &bytes.Buffer{})
		}(prompts[i])
	}
	wg.Wait()
	got, _ := fitTestFile(t, seat, primeHookPromptFileName)
	whole := false
	for _, p := range prompts {
		whole = whole || got == p
	}
	if !whole {
		t.Fatalf("after concurrent hooks the prompt file (%d bytes) is no writer's whole prompt", len(got))
	}
}

// TestPrimeHookFitContextRefusesSymlinkedAncestor: a symlink anywhere below
// the city root (here .gc/sessions) must not redirect the write, or the
// stale-file removal, outside it.
func TestPrimeHookFitContextRefusesSymlinkedAncestor(t *testing.T) {
	seat := fitTestSeat(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(seat.cityPath, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(seat.cityPath, ".gc", "sessions")); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "gastown--worker", primeHookPromptFileName)
	writePrimeHookTestFile(t, victim, "not the city's file")

	var stderr bytes.Buffer
	prompt, suffix, _ := primeHookFitContext(seat, strings.Repeat("doctrine line\n", 2000), false, "", &stderr)
	if !strings.Contains(stderr.String(), "is a symlink") || !strings.Contains(prompt+suffix, "gc prime worker >") {
		t.Fatalf("write through a symlinked ancestor was not refused: stderr=%q", stderr.String())
	}
	primeHookFitContext(seat, "short\n", false, "", &stderr)
	if got, err := os.ReadFile(victim); err != nil || string(got) != "not the city's file" {
		t.Fatalf("file outside the city was changed or removed: %q, %v", got, err)
	}
}

func writePrimeHookTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
