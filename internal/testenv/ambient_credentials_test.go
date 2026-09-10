package testenv

import (
	"os"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/processenv"
)

// The provider-prefix and AWS-key mirrors must not drift from processenv's
// curated forwarding allowlist: that list is exactly what the agent spawn
// path forwards into sessions, so it is exactly what a test process must not
// inherit ambiently. Entries documented as "beyond the processenv mirror"
// are excluded from the pin.
func TestAmbientCredentialMirrorsProcessenv(t *testing.T) {
	extraPrefixes := map[string]bool{"GC_WORKER_INFERENCE_": true}
	extraKeys := map[string]bool{
		"CLAUDE_CODE_OAUTH_TOKEN": true,
		"DOLTHUB_TOKEN":           true,
		"DOLT_REMOTE_PASSWORD":    true,
		"GH_TOKEN":                true,
		"GITHUB_TOKEN":            true,
	}

	var mirrorPrefixes []string
	for _, p := range ambientCredentialPrefixes {
		if !extraPrefixes[p] {
			mirrorPrefixes = append(mirrorPrefixes, p)
		}
	}
	wantPrefixes := processenv.ProviderCredentialEnvPrefixes()
	sort.Strings(mirrorPrefixes)
	sort.Strings(wantPrefixes)
	if len(mirrorPrefixes) != len(wantPrefixes) {
		t.Fatalf("prefix mirror drifted: testenv has %v, processenv has %v", mirrorPrefixes, wantPrefixes)
	}
	for i := range wantPrefixes {
		if mirrorPrefixes[i] != wantPrefixes[i] {
			t.Fatalf("prefix mirror drifted at %q vs %q", mirrorPrefixes[i], wantPrefixes[i])
		}
	}

	var mirrorKeys []string
	for k := range ambientCredentialExactKeys {
		if !extraKeys[k] {
			mirrorKeys = append(mirrorKeys, k)
		}
	}
	wantKeys := processenv.ProviderCredentialEnvKeys()
	sort.Strings(mirrorKeys)
	sort.Strings(wantKeys)
	if len(mirrorKeys) != len(wantKeys) {
		t.Fatalf("exact-key mirror drifted: testenv has %v, processenv has %v", mirrorKeys, wantKeys)
	}
	for i := range wantKeys {
		if mirrorKeys[i] != wantKeys[i] {
			t.Fatalf("exact-key mirror drifted at %q vs %q", mirrorKeys[i], wantKeys[i])
		}
	}

	// And the classification itself agrees with processenv for everything in
	// the mirrored subset.
	for _, p := range mirrorPrefixes {
		name := p + "PROBE"
		if !processenv.IsProviderCredentialEnv(name) || !isAmbientCredentialEnv(name) {
			t.Fatalf("classifiers disagree on %q", name)
		}
	}
	for _, k := range mirrorKeys {
		if !processenv.IsProviderCredentialEnv(k) || !isAmbientCredentialEnv(k) {
			t.Fatalf("classifiers disagree on %q", k)
		}
	}
}

func TestScrubAmbientCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "fake-anthropic")
	t.Setenv("AZURE_OPENAI_API_KEY", "fake-azure")
	t.Setenv("GITHUB_TOKEN", "fake-gh")
	t.Setenv("GC_WORKER_INFERENCE_CODEX_AUTH_JSON", "fake-json")
	t.Setenv("PATH_LIKE_HARMLESS_VAR", "keep-me")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fake-oauth")

	scrubAmbientCredentials(map[string]bool{"CLAUDE_CODE_OAUTH_TOKEN": true})

	for _, gone := range []string{"ANTHROPIC_API_KEY", "AZURE_OPENAI_API_KEY", "GITHUB_TOKEN", "GC_WORKER_INFERENCE_CODEX_AUTH_JSON"} {
		if v := os.Getenv(gone); v != "" {
			t.Errorf("%s survived the scrub with %q", gone, v)
		}
	}
	if os.Getenv("PATH_LIKE_HARMLESS_VAR") != "keep-me" {
		t.Error("non-credential var was scrubbed")
	}
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "fake-oauth" {
		t.Error("passthrough-kept credential was scrubbed")
	}
}

func TestScrubAmbientCredentialsOptOut(t *testing.T) {
	t.Setenv(AmbientCredentialOptOutVar, "1")
	t.Setenv("ANTHROPIC_API_KEY", "fake-anthropic")

	scrubAmbientCredentials(nil)

	if os.Getenv("ANTHROPIC_API_KEY") != "fake-anthropic" {
		t.Error("opt-out did not preserve the ambient credential")
	}
}
