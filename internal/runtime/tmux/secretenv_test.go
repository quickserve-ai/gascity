package tmux

import "testing"

func TestIsSecretEnvKey(t *testing.T) {
	secret := []string{
		// The four ga-fhbnmz production exposures.
		"AZURE_OPENAI_API_KEY",
		"GOOGLE_OAUTH_CLIENT_SECRET",
		"BEADS_HOLDER_TOKEN",
		"GC_INSTANCE_TOKEN",
		// Other live credential shapes on this fleet.
		"CLAUDE_CODE_OAUTH_TOKEN",
		"DOLT_REMOTE_PASSWORD",
		"BEADS_DOLT_PASSWORD",
		"AWS_SECRET_ACCESS_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"GITHUB_TOKEN",
		"GH_TOKEN",
		"GC_WORKER_INFERENCE_CODEX_AUTH_JSON",
		"GOOGLE_OAUTH_CLIENT_ID", // OAUTH marker — over-match, deliberately fine
	}
	for _, key := range secret {
		if !IsSecretEnvKey(key) {
			t.Errorf("IsSecretEnvKey(%q) = false, want true", key)
		}
	}
	plain := []string{
		"PATH", "HOME", "USER", "LANG", "LC_ALL", "TZ",
		"GC_RIG", "GC_RIG_ROOT", "GC_PROVIDER", "GC_SESSION_NAME",
		"GC_ALIAS", "GT_PROCESS_NAMES", "GC_CITY_RUNTIME_DIR",
		"BEADS_ACTOR", "CLAUDE_CONFIG_DIR", "TMUX_TMPDIR",
	}
	for _, key := range plain {
		if IsSecretEnvKey(key) {
			t.Errorf("IsSecretEnvKey(%q) = true, want false", key)
		}
	}
}

func TestScrubSecretEnviron(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"AZURE_OPENAI_API_KEY=sk-live-value",
		"HOME=/Users/x",
		"BEADS_HOLDER_TOKEN=abc123",
		"TMUX_TMPDIR=/tmp/sock-root",
		"malformed-no-equals",
	}
	got := scrubSecretEnviron(in)
	want := []string{"PATH=/usr/bin", "HOME=/Users/x", "TMUX_TMPDIR=/tmp/sock-root", "malformed-no-equals"}
	if len(got) != len(want) {
		t.Fatalf("scrubSecretEnviron = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scrubSecretEnviron[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
