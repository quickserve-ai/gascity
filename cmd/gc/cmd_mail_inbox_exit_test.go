package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The inbox exit-code contract (pl-8ss, migrated from ga-fc5rdy): an inbox
// that could not be read must never exit like an empty one. Hooks, watchers
// and agents gate on the exit code, so a store that cannot be opened, a
// session lookup that fails, or a message scan that fails must all exit
// non-zero, while "No unread messages" keeps exit 0. Each case drives the real
// CLI entry point so the RunE -> errExit -> process exit code chain is covered,
// not only the helper's return value.

// mailInboxUnreadableStoreScript is an exec beads provider that fails every
// operation the way bd does when its Dolt server refuses the connection. No
// server, port, or credential is involved.
const mailInboxUnreadableStoreScript = `#!/bin/sh
echo "Error: failed to open database: failed to check if database \"hq\" exists on server 127.0.0.1:1: Error 1045 (28000): Access denied for user 'root'" >&2
exit 1
`

func newMailInboxExitCity(t *testing.T) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
}

// useUnreadableMailStore points the city at a beads provider whose every read
// fails after the store itself opens.
func useUnreadableMailStore(t *testing.T) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "unreadable-beads.sh")
	if err := os.WriteFile(script, []byte(mailInboxUnreadableStoreScript), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", script, err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
}

// useUnopenableMailStore selects the bd provider with no bd on PATH, so the
// store open itself fails.
func useUnopenableMailStore(t *testing.T) {
	t.Helper()
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("PATH", t.TempDir())
}

func TestMailInboxExitsNonZeroWhenInboxCannotBeRead(t *testing.T) {
	cases := []struct {
		name       string
		setupStore func(*testing.T)
		identity   map[string]string
		args       []string
		wantStderr string
	}{
		{
			name:       "session lookup fails across identity candidates",
			setupStore: useUnreadableMailStore,
			identity:   map[string]string{"GC_SESSION_ID": "gc-wisp-inbox", "GC_ALIAS": "mayor"},
			wantStderr: `looking up session "gc-wisp-inbox"`,
		},
		{
			name:       "session lookup fails for a single identity",
			setupStore: useUnreadableMailStore,
			identity:   map[string]string{"GC_SESSION_ID": "gc-wisp-inbox"},
			wantStderr: `looking up session "gc-wisp-inbox"`,
		},
		{
			name:       "message scan fails",
			setupStore: useUnreadableMailStore,
			args:       []string{"human"},
			wantStderr: "Access denied",
		},
		{
			name:       "store cannot be opened",
			setupStore: useUnopenableMailStore,
			args:       []string{"human"},
			wantStderr: "bd not found in PATH",
		},
	}
	for _, tc := range cases {
		for _, jsonOut := range []bool{false, true} {
			name := tc.name
			if jsonOut {
				name += " (--json)"
			}
			t.Run(name, func(t *testing.T) {
				newMailInboxExitCity(t)
				tc.setupStore(t)
				for key, value := range tc.identity {
					t.Setenv(key, value)
				}
				args := append([]string{"mail", "inbox"}, tc.args...)
				if jsonOut {
					args = append(args, "--json")
				}

				var stdout, stderr bytes.Buffer
				code := run(args, &stdout, &stderr)

				if code == 0 {
					t.Fatalf("gc %s exited 0 for an unreadable inbox; it must not share the empty inbox's exit code\nstdout: %s\nstderr: %s",
						strings.Join(args, " "), stdout.String(), stderr.String())
				}
				if !strings.Contains(stderr.String(), "gc mail inbox: ") || !strings.Contains(stderr.String(), tc.wantStderr) {
					t.Fatalf("stderr = %q, want the gc mail inbox error naming %q", stderr.String(), tc.wantStderr)
				}
				if strings.Contains(stdout.String(), "No unread messages") {
					t.Fatalf("stdout reports an empty inbox for an unreadable one: %q", stdout.String())
				}
				if !jsonOut {
					return
				}
				var failure jsonSchemaErrorPayload
				if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &failure); err != nil {
					t.Fatalf("--json stdout is not a failure envelope (%v): %q", err, stdout.String())
				}
				if failure.OK || failure.Error.ExitCode != code {
					t.Fatalf("--json failure envelope = %+v, want ok=false with exit_code=%d", failure, code)
				}
			})
		}
	}
}

func TestMailInboxEmptyInboxExitsZero(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		name := "text"
		if jsonOut {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			newMailInboxExitCity(t)
			t.Setenv("GC_BEADS", "file")
			args := []string{"mail", "inbox", "human"}
			if jsonOut {
				args = append(args, "--json")
			}

			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("gc %s = %d, want 0 for a readable empty inbox\nstderr: %s", strings.Join(args, " "), code, stderr.String())
			}
			if !jsonOut {
				if !strings.Contains(stdout.String(), "No unread messages for human") {
					t.Fatalf("stdout = %q, want the empty-inbox line", stdout.String())
				}
				return
			}
			var got mailInboxJSONResult
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
				t.Fatalf("unmarshal --json stdout: %v; output=%s", err, stdout.String())
			}
			if len(got.Messages) != 0 {
				t.Fatalf("messages = %+v, want none", got.Messages)
			}
		})
	}
}
