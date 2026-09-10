// Package processenv centralizes process environment filters shared by CLI and
// API session launch paths.
package processenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/telemetry"
)

// providerCredentialEnvPrefixes lists provider-specific env-var name prefixes
// whose values are treated as agent-provider credentials and forwarded into
// the supervisor's persistent env and spawned agent processes.
//
// The list is curated, not auto-discovered: persistent supervisor env has a
// bounded size, so broad ecosystems such as AWS use exact names in
// providerCredentialEnvKeys to avoid persisting unrelated tooling state.
//
// Keep alphabetised. Documented providers:
//
//	ANTHROPIC_   Anthropic / Claude
//	AZURE_       Azure OpenAI
//	CEREBRAS_    Cerebras
//	COHERE_      Cohere
//	DEEPSEEK_    DeepSeek
//	FIREWORKS_   Fireworks AI
//	GEMINI_      Google Gemini direct API
//	GOOGLE_      Google Cloud / Vertex
//	GROQ_        Groq
//	MISTRAL_     Mistral
//	OLLAMA_      Ollama local and Ollama Cloud
//	OPENAI_      OpenAI-compatible providers
//	OPENROUTER_  OpenRouter
//	TOGETHER_    Together AI
//	VERTEX_      Vertex AI direct
//	XAI_         xAI / Grok
//	XIAOMI_      Xiaomi MiMo
var providerCredentialEnvPrefixes = []string{
	"ANTHROPIC_",
	"AZURE_",
	"CEREBRAS_",
	"COHERE_",
	"DEEPSEEK_",
	"FIREWORKS_",
	"GEMINI_",
	"GOOGLE_",
	"GROQ_",
	"MISTRAL_",
	"OLLAMA_",
	"OPENAI_",
	"OPENROUTER_",
	"TOGETHER_",
	"VERTEX_",
	"XAI_",
	"XIAOMI_",
}

// providerCredentialEnvKeys lists exact provider credential/config env vars for
// providers whose common env namespace is broader than provider auth.
var providerCredentialEnvKeys = map[string]bool{
	"AWS_ACCESS_KEY_ID":                      true,
	"AWS_BEARER_TOKEN_BEDROCK":               true,
	"AWS_CA_BUNDLE":                          true,
	"AWS_CONFIG_FILE":                        true,
	"AWS_CONTAINER_AUTHORIZATION_TOKEN":      true,
	"AWS_CONTAINER_CREDENTIALS_FULL_URI":     true,
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": true,
	"AWS_DEFAULT_REGION":                     true,
	"AWS_EC2_METADATA_DISABLED":              true,
	"AWS_ENDPOINT_URL":                       true,
	"AWS_ENDPOINT_URL_BEDROCK":               true,
	"AWS_PROFILE":                            true,
	"AWS_REGION":                             true,
	"AWS_ROLE_ARN":                           true,
	"AWS_SDK_LOAD_CONFIG":                    true,
	"AWS_SECRET_ACCESS_KEY":                  true,
	"AWS_SESSION_TOKEN":                      true,
	"AWS_SHARED_CREDENTIALS_FILE":            true,
	"AWS_USE_DUALSTACK_ENDPOINT":             true,
	"AWS_USE_FIPS_ENDPOINT":                  true,
	"AWS_WEB_IDENTITY_TOKEN_FILE":            true,
}

// ProviderCredentialEnvPrefixes returns a copy of the curated provider
// credential env-var name prefixes. Exposed so internal/testenv's stdlib-only
// mirror of this classification can be pinned by test instead of drifting.
func ProviderCredentialEnvPrefixes() []string {
	out := make([]string, len(providerCredentialEnvPrefixes))
	copy(out, providerCredentialEnvPrefixes)
	return out
}

// ProviderCredentialEnvKeys returns a copy of the exact provider credential
// env-var names. Exposed for the same mirror-pinning reason as
// ProviderCredentialEnvPrefixes.
func ProviderCredentialEnvKeys() []string {
	out := make([]string, 0, len(providerCredentialEnvKeys))
	for k := range providerCredentialEnvKeys {
		out = append(out, k)
	}
	return out
}

// IsProviderCredentialEnv reports whether key belongs to the curated provider
// credential/config allowlist.
func IsProviderCredentialEnv(key string) bool {
	if providerCredentialEnvKeys[key] {
		return true
	}
	for _, prefix := range providerCredentialEnvPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// ProviderProcessPassthroughEnv returns non-GC process context that provider
// sessions need to start reliably: user/home, provider auth/config, locale,
// time zone, XDG, telemetry, and Claude nesting resets.
func ProviderProcessPassthroughEnv() map[string]string {
	m := make(map[string]string)
	if v := os.Getenv("PATH"); v != "" {
		m["PATH"] = v
	}
	if v := os.Getenv("HOME"); v != "" {
		m["HOME"] = v
	}
	for _, key := range []string{
		"USER",
		"LOGNAME",
		// TZ keeps spawned sessions on the host wall clock so any in-session
		// time reasoning (e.g. `gc order check`, date math in scripts) agrees
		// with the supervisor instead of defaulting to UTC.
		"TZ",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_SUBAGENT_MODEL",
		"CLAUDE_CODE_EFFORT_LEVEL",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
	} {
		if v := os.Getenv(key); v != "" {
			m[key] = v
		}
	}
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE"} {
		if v := os.Getenv(key); v != "" {
			m[key] = v
		}
	}
	if _, ok := m["LC_ALL"]; !ok {
		m["LC_ALL"] = ""
	}
	if _, ok := m["LC_CTYPE"]; !ok {
		m["LC_CTYPE"] = ""
	}
	if m["LANG"] == "" && m["LC_ALL"] == "" && m["LC_CTYPE"] == "" {
		m["LANG"] = "en_US.UTF-8"
	}
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" {
		m["XDG_CONFIG_HOME"] = v
	} else if home := os.Getenv("HOME"); home != "" {
		m["XDG_CONFIG_HOME"] = filepath.Join(home, ".config")
	}
	if v := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); v != "" {
		m["XDG_STATE_HOME"] = v
	} else if home := os.Getenv("HOME"); home != "" {
		m["XDG_STATE_HOME"] = filepath.Join(home, ".local", "state")
	}
	for _, entry := range os.Environ() {
		key, val, ok := strings.Cut(entry, "=")
		if !ok || val == "" {
			continue
		}
		if IsProviderCredentialEnv(key) {
			m[key] = val
		}
	}
	for k, v := range telemetry.OTELEnvMap() {
		m[k] = v
	}
	m["CLAUDECODE"] = ""
	m["CLAUDE_CODE_ENTRYPOINT"] = ""
	m["CODEX_THREAD_ID"] = ""
	m["CODEX_CI"] = ""
	// CLAUDE_CONFIG_DIR selects the Claude ACCOUNT the session bills to. The
	// controller's own ambient value must never decide a managed session's
	// account: an env-less claude provider would silently land on whatever
	// account the controller happens to run under (ga-ai7gz2 — one seat
	// inherited the operator's account, another an unrelated one). Reset it
	// here; a workspace/provider/agent env layer that declares an account
	// overrides this in the later merge, and RequireDeclaredClaudeAccount is
	// the spawn-time guard that turns "claude family, ambient present, none
	// declared" into a loud refusal instead of a silent inheritance.
	m["CLAUDE_CONFIG_DIR"] = ""
	return m
}

// ErrUndeclaredClaudeAccount marks a refused claude-family spawn whose config
// declares no CLAUDE_CONFIG_DIR while the controller carries an ambient one.
var ErrUndeclaredClaudeAccount = errors.New("claude provider declares no CLAUDE_CONFIG_DIR")

// RequireDeclaredClaudeAccount refuses to let a claude-family session launch
// on an inherited account. It errors only when all three hold: the provider
// resolves to the claude family, the controller itself runs with an ambient
// CLAUDE_CONFIG_DIR (so there IS an account to wrongly inherit), and no config
// layer declared one for the session (sessionEnv is the merged session env;
// ProviderProcessPassthroughEnv resets the key, so a non-empty value can only
// come from a declared layer). With no ambient value the vanilla single-account
// setup — claude defaulting to ~/.claude — keeps working untouched. (ga-ai7gz2)
func RequireDeclaredClaudeAccount(providerName, family string, sessionEnv map[string]string) error {
	if family != "claude" {
		return nil
	}
	ambient := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if ambient == "" {
		return nil
	}
	if strings.TrimSpace(sessionEnv["CLAUDE_CONFIG_DIR"]) != "" {
		return nil
	}
	return fmt.Errorf("%w: provider %q resolves to the claude family but no workspace/provider/agent env layer sets CLAUDE_CONFIG_DIR, and the controller runs with an ambient one; declare env.CLAUDE_CONFIG_DIR on the provider so the seat binds to an explicit account instead of silently inheriting the controller's (ga-ai7gz2)", ErrUndeclaredClaudeAccount, providerName)
}
