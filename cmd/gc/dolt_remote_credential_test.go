package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// ga-10irxc. The credential used to reach managed dolt only by INHERITANCE from
// whichever process ran the gc command that started it, and the scope watchdog
// froze that environment for every server it later respawned. Every agent shell
// carries zero copies, so an agent-initiated start permanently pinned an
// uncredentialed server. These tests pin the fix: the value comes from the
// city's configured file and does NOT depend on the caller's environment.

// writeCity builds a city with .beads/config.yaml containing body, plus an
// optional password file at the given relative path with the given mode.
func writeCity(t *testing.T, configBody string) string {
	t.Helper()
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if configBody != "" {
		if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte(configBody), 0o644); err != nil {
			t.Fatalf("write config.yaml: %v", err)
		}
	}
	return city
}

func writeSecret(t *testing.T, dir, name, secret string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(secret), mode); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	// WriteFile is subject to umask; force the mode we are actually testing.
	if err := os.Chmod(p, mode); err != nil {
		t.Fatalf("chmod secret: %v", err)
	}
	return p
}

func envValue(env []string, key string) (string, int) {
	prefix := key + "="
	value := ""
	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			value = strings.TrimPrefix(kv, prefix)
			count++
		}
	}
	return value, count
}

// THE BUG ITSELF: caller has no credential, and the server must still get one.
func TestDoltServerEnv_InjectsCredentialWhenCallerHasNone(t *testing.T) {
	city := writeCity(t, "dolt:\n  remote-password-file: secret.txt\n")
	writeSecret(t, city, "secret.txt", "hunter2\n", 0o600)

	// An agent shell: no DOLT_REMOTE_PASSWORD anywhere.
	parent := []string{"PATH=/usr/bin", "HOME=/home/test"}
	if _, n := envValue(parent, doltRemotePasswordEnvKey); n != 0 {
		t.Fatalf("test setup wrong: parent already has the credential")
	}

	out := doltServerEnv(city, parent)

	got, n := envValue(out, doltRemotePasswordEnvKey)
	if n != 1 {
		t.Fatalf("expected exactly 1 %s entry, got %d in %v", doltRemotePasswordEnvKey, n, out)
	}
	if got != "hunter2" {
		t.Fatalf("credential = %q, want %q (trailing newline must be trimmed)", got, "hunter2")
	}
}

// Lineage independence: a STALE inherited value must be replaced by the
// authoritative file, or "which process started it" still matters.
func TestDoltServerEnv_FileOverridesInheritedCredential(t *testing.T) {
	city := writeCity(t, "dolt:\n  remote-password-file: secret.txt\n")
	writeSecret(t, city, "secret.txt", "from-file", 0o600)

	parent := []string{"PATH=/usr/bin", doltRemotePasswordEnvKey + "=stale-inherited"}
	out := doltServerEnv(city, parent)

	got, n := envValue(out, doltRemotePasswordEnvKey)
	if n != 1 {
		t.Fatalf("expected exactly 1 %s entry (no duplicates), got %d in %v", doltRemotePasswordEnvKey, n, out)
	}
	if got != "from-file" {
		t.Fatalf("credential = %q, want the file value; inherited value won", got)
	}
}

// The no-opt-in property that made this safe to ship: with no config, the
// environment must be untouched.
func TestDoltServerEnv_NoConfigLeavesEnvUnchanged(t *testing.T) {
	city := writeCity(t, "") // no config.yaml at all

	parent := []string{"PATH=/usr/bin", doltRemotePasswordEnvKey + "=inherited-value"}
	out := doltServerEnv(city, parent)

	got, n := envValue(out, doltRemotePasswordEnvKey)
	if n != 1 || got != "inherited-value" {
		t.Fatalf("with no config the inherited credential must pass through untouched; got %q (n=%d)", got, n)
	}
}

func TestDoltServerEnv_NoConfigDoesNotInventCredential(t *testing.T) {
	city := writeCity(t, "dolt:\n  disable-event-flush: true\n") // config, but no password file

	parent := []string{"PATH=/usr/bin"}
	out := doltServerEnv(city, parent)

	if _, n := envValue(out, doltRemotePasswordEnvKey); n != 0 {
		t.Fatalf("no password file configured, but a credential appeared: %v", out)
	}
}

// A credential group- or world-readable is refused rather than loaded.
func TestResolveDoltRemotePassword_RefusesPermissiveMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o666} {
		city := writeCity(t, "dolt:\n  remote-password-file: secret.txt\n")
		writeSecret(t, city, "secret.txt", "hunter2", mode)

		secret, reason, ok := resolveDoltRemotePassword(city)
		if ok {
			t.Fatalf("mode %#o: credential was loaded despite being readable by group/others", mode)
		}
		if secret != "" {
			t.Fatalf("mode %#o: refusal must not return the secret", mode)
		}
		if !strings.Contains(reason, "chmod 600") {
			t.Fatalf("mode %#o: reason should tell the operator how to fix it, got %q", mode, reason)
		}
		if strings.Contains(reason, "hunter2") {
			t.Fatalf("mode %#o: reason LEAKED the secret: %q", mode, reason)
		}
	}
}

func TestResolveDoltRemotePassword_AcceptsTightModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o400} {
		city := writeCity(t, "dolt:\n  remote-password-file: secret.txt\n")
		writeSecret(t, city, "secret.txt", "hunter2", mode)

		secret, _, ok := resolveDoltRemotePassword(city)
		if !ok {
			t.Fatalf("mode %#o should be accepted", mode)
		}
		if secret != "hunter2" {
			t.Fatalf("mode %#o: secret = %q", mode, secret)
		}
	}
}

// Every failure mode must be VISIBLE (non-empty reason), never silent.
func TestResolveDoltRemotePassword_FailuresAreVisible(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		city := writeCity(t, "dolt:\n  remote-password-file: nope.txt\n")
		_, reason, ok := resolveDoltRemotePassword(city)
		if ok || reason == "" {
			t.Fatalf("missing file must fail with a reason; ok=%v reason=%q", ok, reason)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		city := writeCity(t, "dolt:\n  remote-password-file: secret.txt\n")
		writeSecret(t, city, "secret.txt", "\n", 0o600)
		_, reason, ok := resolveDoltRemotePassword(city)
		if ok || !strings.Contains(reason, "empty") {
			t.Fatalf("empty file must fail visibly; ok=%v reason=%q", ok, reason)
		}
	})
	t.Run("directory", func(t *testing.T) {
		city := writeCity(t, "dolt:\n  remote-password-file: adir\n")
		if err := os.Mkdir(filepath.Join(city, "adir"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		_, reason, ok := resolveDoltRemotePassword(city)
		if ok || !strings.Contains(reason, "directory") {
			t.Fatalf("directory must fail visibly; ok=%v reason=%q", ok, reason)
		}
	})
	// The not-configured case is NOT a failure and must stay quiet, or every
	// city that never opts in prints a warning on every dolt start.
	t.Run("not configured is silent", func(t *testing.T) {
		city := writeCity(t, "dolt:\n  disable-event-flush: true\n")
		_, reason, ok := resolveDoltRemotePassword(city)
		if ok {
			t.Fatalf("nothing configured, yet ok=true")
		}
		if reason != "" {
			t.Fatalf("not-configured must be silent, got reason %q", reason)
		}
	})
}

// Spaces are legal password characters; only the editor's trailing newline goes.
func TestResolveDoltRemotePassword_TrimsOnlyTrailingNewline(t *testing.T) {
	city := writeCity(t, "dolt:\n  remote-password-file: secret.txt\n")
	writeSecret(t, city, "secret.txt", "  pa ss  \n", 0o600)

	secret, _, ok := resolveDoltRemotePassword(city)
	if !ok {
		t.Fatal("expected success")
	}
	if secret != "  pa ss  " {
		t.Fatalf("secret = %q; leading/trailing spaces must survive, only the newline is trimmed", secret)
	}
}

func TestResolveDoltRemotePassword_AbsolutePathOutsideCity(t *testing.T) {
	outside := t.TempDir()
	p := writeSecret(t, outside, "cred", "abs-secret", 0o600)
	city := writeCity(t, "dolt:\n  remote-password-file: "+p+"\n")

	secret, _, ok := resolveDoltRemotePassword(city)
	if !ok || secret != "abs-secret" {
		t.Fatalf("absolute path should resolve as-is; ok=%v secret=%q", ok, secret)
	}
}

// REGRESSION GUARD for the parser restructure. readDoltConfigFromRoot used to
// RETURN as soon as nested disable-event-flush matched, so a second field
// silently never parsed whenever that key was present. Assert BOTH fields are
// read from one document — this test fails against the original early-return
// shape.
func TestReadDoltConfig_ParsesBothFieldsFromOneDocument(t *testing.T) {
	city := writeCity(t, "dolt:\n  disable-event-flush: true\n  remote-password-file: /tmp/cred\n")

	cfg, ok, err := contract.ReadDoltConfig(fsys.OSFS{}, filepath.Join(city, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("ReadDoltConfig: %v", err)
	}
	if !ok {
		t.Fatal("expected config to report values present")
	}
	if cfg.DisableEventFlush == nil || !*cfg.DisableEventFlush {
		t.Fatalf("disable-event-flush lost: %+v", cfg.DisableEventFlush)
	}
	if cfg.RemotePasswordFile != "/tmp/cred" {
		t.Fatalf("remote-password-file = %q; the early return swallowed it", cfg.RemotePasswordFile)
	}
}

// Same guard for the reversed key order, so the test does not depend on which
// key the parser happens to look at first.
func TestReadDoltConfig_ParsesBothFieldsReversedOrder(t *testing.T) {
	city := writeCity(t, "dolt:\n  remote-password-file: /tmp/cred2\n  disable-event-flush: false\n")

	cfg, _, err := contract.ReadDoltConfig(fsys.OSFS{}, filepath.Join(city, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("ReadDoltConfig: %v", err)
	}
	if cfg.RemotePasswordFile != "/tmp/cred2" {
		t.Fatalf("remote-password-file = %q", cfg.RemotePasswordFile)
	}
	if cfg.DisableEventFlush == nil || *cfg.DisableEventFlush {
		t.Fatalf("disable-event-flush should be false, got %+v", cfg.DisableEventFlush)
	}
}

// The flat dotted form must work too, matching disable-event-flush's contract.
func TestReadDoltConfig_FlatDottedForm(t *testing.T) {
	city := writeCity(t, "dolt.remote-password-file: /tmp/flat\n")

	cfg, _, err := contract.ReadDoltConfig(fsys.OSFS{}, filepath.Join(city, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("ReadDoltConfig: %v", err)
	}
	if cfg.RemotePasswordFile != "/tmp/flat" {
		t.Fatalf("flat dotted form not parsed: %q", cfg.RemotePasswordFile)
	}
}

// doltServerEnv must still do its original job; the credential work must not
// disturb the event-flush behavior it shares a function with.
func TestDoltServerEnv_EventFlushStillAppliedAlongsideCredential(t *testing.T) {
	city := writeCity(t, "dolt:\n  remote-password-file: secret.txt\n")
	writeSecret(t, city, "secret.txt", "s3cr3t", 0o600)

	out := doltServerEnv(city, []string{"PATH=/usr/bin"})

	if v, n := envValue(out, "DOLT_DISABLE_EVENT_FLUSH"); n != 1 || v != "true" {
		t.Fatalf("event-flush default lost: value=%q n=%d", v, n)
	}
	if v, _ := envValue(out, doltRemotePasswordEnvKey); v != "s3cr3t" {
		t.Fatalf("credential missing: %q", v)
	}
}
