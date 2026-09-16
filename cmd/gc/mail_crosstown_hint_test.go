package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestCrossTownRecipientHint(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "qlandia"},
		Rigs:      []config.Rig{{Name: "qcore"}},
	}
	contexts := filepath.Join(t.TempDir(), "contexts.toml")
	if err := os.WriteFile(contexts, []byte("[[context]]\n  name = \"westeros\"\n  url = \"https://hub.example:8443\"\n  city = \"westeros\"\n"), 0o600); err != nil {
		t.Fatalf("write contexts: %v", err)
	}

	hint := crossTownRecipientHint(cfg, "gastown/woodhouse", contexts)
	for _, want := range []string{`"gastown" is not this city (qlandia)`, "--context westeros", "[for <rig>/<name>]"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q; got %q", want, hint)
		}
	}

	// Negative controls: a local rig and a bare name are ordinary typos, not
	// cross-town addresses, and must not be told to use the hub.
	for _, recipient := range []string{"qcore/nobody", "nobody", "qlandia/mayor", "controller/"} {
		if got := crossTownRecipientHint(cfg, recipient, contexts); got != "" {
			t.Errorf("crossTownRecipientHint(%q) = %q, want empty", recipient, got)
		}
	}

	// Several contexts: one complete command per context, never a "|" join
	// that a shell would run as a pipeline.
	multi := filepath.Join(t.TempDir(), "multi.toml")
	if err := os.WriteFile(multi, []byte("[[context]]\n  name = \"prod\"\n  url = \"https://a.example:8443\"\n  city = \"a\"\n[[context]]\n  name = \"staging\"\n  url = \"https://b.example:8443\"\n  city = \"b\"\n"), 0o600); err != nil {
		t.Fatalf("write contexts: %v", err)
	}
	got := crossTownRecipientHint(cfg, "gastown/woodhouse", multi)
	if strings.Contains(got, "prod|staging") {
		t.Errorf("hint joins contexts into a pipeline: %q", got)
	}
	for _, want := range []string{"\n  gc mail send --context prod ", "\n  gc mail send --context staging "} {
		if !strings.Contains(got, want) {
			t.Errorf("multi-context hint missing %q; got %q", want, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "mayor") || strings.Contains(got, "skill") {
		t.Errorf("hint names a pack-specific role or skill: %q", got)
	}

	// No registry: the hint still fires, with a placeholder instead of a name.
	missing := filepath.Join(t.TempDir(), "absent.toml")
	if got := crossTownRecipientHint(cfg, "gastown/woodhouse", missing); !strings.Contains(got, "--context <hub context>") {
		t.Errorf("hint without registry = %q, want placeholder context", got)
	}
}

// The refusal site must actually print the hint: a helper nobody calls would
// pass TestCrossTownRecipientHint and change nothing a sender sees.
func TestCmdMailSendForeignTownRecipientPrintsHubHint(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_AGENT", "")

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityPath)
	if _, err := openCityStoreAt(cityPath); err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdMailSend([]string{"gastown/woodhouse"}, false, false, "human", "", "Subject", "Body", &stdout, &stderr); code == 0 {
		t.Fatalf("cmdMailSend() = 0, want refusal; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `unknown recipient "gastown/woodhouse"`) {
		t.Fatalf("stderr = %q, want the unchanged unknown-recipient line", stderr.String())
	}
	if !strings.Contains(stderr.String(), "cross-town agent mail goes through the hub") {
		t.Fatalf("stderr = %q, want the cross-town hub hint", stderr.String())
	}

	// Control: an unknown bare name is refused WITHOUT the hint.
	stderr.Reset()
	if code := cmdMailSend([]string{"nobody"}, false, false, "human", "", "Subject", "Body", &stdout, &stderr); code == 0 {
		t.Fatalf("cmdMailSend(nobody) = 0, want refusal")
	}
	if strings.Contains(stderr.String(), "goes through the hub") {
		t.Fatalf("bare unknown recipient got the hub hint: %q", stderr.String())
	}
}
