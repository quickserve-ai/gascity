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
// the hook limit. A conversation started by hand in the seat's pane, or
// restarted by /compact or /clear inside a gc-launched one, must get a
// bootstrap under the limit naming a file that holds exactly what `gc prime`
// prints for the seat. A gc-launched seat's startup and resume hooks stay as
// they were: no prompt, no file, because the launch delivered it.
func TestDoPrimeWithHook_OversizedPromptBootstrap(t *testing.T) {
	for _, tc := range []struct {
		name, delivered, source string
		wantBootstrap           bool
	}{
		{name: "started by hand", source: "startup", wantBootstrap: true},
		{name: "compact in launched seat", delivered: "1", source: "compact", wantBootstrap: true},
		{name: "clear in launched seat", delivered: "1", source: "clear", wantBootstrap: true},
		{name: "launched seat startup", delivered: "1", source: "startup"},
		{name: "launched seat resume", delivered: "1", source: "resume"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			cityDir := t.TempDir()
			var prompt strings.Builder
			prompt.WriteString("# Worker Context\n\nRun the startup steps below before anything else.\n\n")
			for i := 0; prompt.Len() < 150_000; i++ {
				fmt.Fprintf(&prompt, "Rule %d: a line of seat doctrine the hook could never carry whole.\n", i)
			}
			writePrimeHookTestFile(t, filepath.Join(cityDir, "prompts", "worker.md"), prompt.String())
			writePrimeHookTestFile(t, filepath.Join(cityDir, "city.toml"), "[workspace]\nname = \"gastown\"\n\n[[agent]]\nname = \"worker\"\nprompt_template = \"prompts/worker.md\"\n")
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
				t.Fatalf("hook output is not JSON: %v", err)
			}
			context := out.HookSpecificOutput.AdditionalContext
			path := filepath.Join(cityDir, ".gc", "sessions", "gastown--worker", primeHookPromptFileName)
			got, readErr := os.ReadFile(path)

			if !tc.wantBootstrap {
				if strings.Contains(context, "Worker Context") || !os.IsNotExist(readErr) {
					t.Fatalf("launched seat's %s hook carried the prompt or wrote %s (read err %v):\n%s", tc.source, path, readErr, context)
				}
				return
			}
			if len(context) >= primeHookContextLimit {
				t.Fatalf("bootstrap is %d bytes, want under %d", len(context), primeHookContextLimit)
			}
			for _, wantText := range []string{path, "[gastown] worker", "read that file to its end", "# Worker Context"} {
				if !strings.Contains(context, wantText) {
					t.Errorf("bootstrap missing %q:\n%s", wantText, context)
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

func TestPrimeHookPromptForDelivery(t *testing.T) {
	var stderr bytes.Buffer
	cityDir := t.TempDir()
	if got := primeHookPromptForDelivery(cityDir, "gastown", "worker", "w", "short\n", true, false, "", &stderr); got != "short\n" {
		t.Fatalf("small prompt = %q, want unchanged", got)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("a small prompt wrote state (stat err %v)", err)
	}

	// A file where the runtime root should be makes the prompt file
	// unwritable: the seat gets the whole prompt and the failure is reported.
	writePrimeHookTestFile(t, filepath.Join(cityDir, ".gc"), "not a dir")
	big := strings.Repeat("doctrine line\n", 2000)
	if got := primeHookPromptForDelivery(cityDir, "gastown", "worker", "w", big, true, false, "", &stderr); got != big {
		t.Fatalf("unwritable file: got %d bytes, want the full %d-byte prompt", len(got), len(big))
	}
	if !strings.Contains(stderr.String(), "could not be written") {
		t.Fatalf("stderr = %q, want the write failure reported", stderr.String())
	}

	for _, name := range []string{"", ".", ".."} {
		if _, err := primeHookPromptPath("/city", name); err == nil {
			t.Errorf("primeHookPromptPath(%q) succeeded, want error", name)
		}
	}
	if got := primeHookBootstrap("worker", "/p/prime.md", big, 100); strings.Contains(got, "doctrine line") || !strings.Contains(got, "/p/prime.md") {
		t.Fatalf("bootstrap with no room should keep the path and drop the excerpt:\n%s", got)
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
