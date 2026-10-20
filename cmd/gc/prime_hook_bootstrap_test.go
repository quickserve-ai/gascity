package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// TestPrimeHookFitContextOversizedPrompt covers a capped seat's oversized
// prompt (written to its file, bootstrap under the limit), an unwritable
// runtime directory (a bounded notice to capture `gc prime`), and the cases
// that change nothing: an uncapped provider, a suppressed prompt.
func TestPrimeHookFitContextOversizedPrompt(t *testing.T) {
	big := strings.Repeat("doctrine line\n", 2000)
	const step = "\n\nstep: small"
	var stderr bytes.Buffer

	seat := fitTestSeat(t)
	prompt, suffix := primeHookFitContext(seat, big, false, step, &stderr)
	ctx := fitTestDelivered(seat, prompt, false, suffix)
	if len(ctx) >= primeHookContextLimit || !strings.HasSuffix(ctx, step) {
		t.Fatalf("context %d bytes (want under %d) or step not delivered (stderr %q)", len(ctx), primeHookContextLimit, stderr.String())
	}
	if got, ok := fitTestFile(t, seat, primeHookPromptFileName); !ok || got != big {
		t.Fatalf("prompt file present=%v or content differs", ok)
	}

	for _, tc := range []struct {
		name     string
		capped   bool
		suppress bool
	}{{"uncapped provider", false, false}, {"suppressed prompt", true, true}} {
		seat := fitTestSeat(t)
		seat.capped = tc.capped
		if p, s := primeHookFitContext(seat, big, tc.suppress, step, &stderr); p != big || s != step {
			t.Fatalf("%s: prompt or suffix changed", tc.name)
		}
		if _, err := os.Stat(filepath.Join(seat.cityPath, ".gc")); !os.IsNotExist(err) {
			t.Fatalf("%s wrote state (stat err %v)", tc.name, err)
		}
	}

	seat = fitTestSeat(t)
	writePrimeHookTestFile(t, filepath.Join(seat.cityPath, ".gc"), "not a dir")
	stderr.Reset()
	prompt, suffix = primeHookFitContext(seat, big, false, step, &stderr)
	ctx = fitTestDelivered(seat, prompt, false, suffix)
	if len(ctx) >= primeHookContextLimit || !strings.Contains(ctx, "gc prime worker >") || !strings.HasSuffix(ctx, step) {
		t.Fatalf("failure context is %d bytes or lacks the capture command or the step:\n%.600s", len(ctx), ctx)
	}
	if !strings.Contains(stderr.String(), "could not be written") {
		t.Fatalf("stderr = %q, want the write failure reported", stderr.String())
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
