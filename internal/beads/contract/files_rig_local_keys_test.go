package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestEnsureCanonicalConfigPreservesNestedDoltSiblings pins the guarantee
// ga-dwobeb depends on: a rig may carry its OWN tuning inside the nested dolt:
// mapping — pool-read-timeout, pool-write-timeout, remote-password-file — and a
// canonicalization pass must not drop it while setting the keys it does own.
//
// The existing unknown-key test covers a FLAT top-level key. This one covers the
// harder shape, where the rig's keys are siblings of a key the canonicalizer
// writes (dolt.disable-event-flush) inside the same nested mapping — the case
// where a "rebuild the dolt: block" refactor would silently wipe rig-local
// tuning. remote-password-file is load-bearing for authed Dolt, so losing it
// breaks the store rather than merely reverting a timeout.
//
// The input is the live city rig's .beads/config.yaml shape, and the pass runs
// three times because init/reload runs it repeatedly.
func TestEnsureCanonicalConfigPreservesNestedDoltSiblings(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	live := strings.Join([]string{
		"issue_prefix: ga",
		"issue-prefix: ga",
		"dolt.auto-start: false",
		"dolt:",
		"    disable-event-flush: true",
		"    pool-read-timeout: 120s",
		"    pool-write-timeout: 120s",
		"    remote-password-file: /tmp/dolt-remote-password",
		"export.auto: false",
		"backup.enabled: false",
		"gc.endpoint_origin: managed_city",
		"gc.endpoint_status: verified",
		"dolt.mode: server",
		"dolt.local-only: true",
		"",
	}, "\n")
	if err := fs.WriteFile(path, []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := EnsureCanonicalConfig(fs, path, ConfigState{
			IssuePrefix:    "ga",
			EndpointOrigin: EndpointOriginManagedCity,
			EndpointStatus: EndpointStatusVerified,
		}); err != nil {
			t.Fatalf("pass %d: EnsureCanonicalConfig() error = %v", i, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"pool-read-timeout: 120s",
		"pool-write-timeout: 120s",
		"remote-password-file: /tmp/dolt-remote-password",
		"dolt.mode: server",
		"dolt.local-only: true",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("canonicalization dropped rig-local key %q:\n%s", want, text)
		}
	}
}

// TestEnsureCanonicalConfigFallbackPreservesRigLocalKeys covers the same
// guarantee on the PARSE-FAILURE branch (ensureCanonicalConfigFallback), which
// rewrites by line replacement rather than editing the parsed document. A rig
// whose config.yaml stops parsing must still keep its pool timeouts, since that
// is exactly when a degraded store most needs the longer deadline.
func TestEnsureCanonicalConfigFallbackPreservesRigLocalKeys(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// A tab indent makes this unparseable YAML, forcing the fallback branch.
	broken := "issue_prefix: ga\n\tdolt.auto-start: true\ndolt.pool-read-timeout: 120s\ndolt.pool-write-timeout: 120s\n"
	if err := fs.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCanonicalConfig(fs, path, ConfigState{
		IssuePrefix:    "ga",
		EndpointOrigin: EndpointOriginManagedCity,
		EndpointStatus: EndpointStatusVerified,
	}); err != nil {
		t.Fatalf("EnsureCanonicalConfig() on an unparseable file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"pool-read-timeout: 120s", "pool-write-timeout: 120s"} {
		if !strings.Contains(text, want) {
			t.Errorf("fallback path dropped rig-local key %q:\n%s", want, text)
		}
	}
}
