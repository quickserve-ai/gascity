// Package doltauth resolves Dolt credentials from scoped files and env overrides.
package doltauth

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// Resolved holds the effective Dolt auth values for a scope.
type Resolved struct {
	User                    string
	Password                string
	CredentialsFileOverride string
}

// AuthScopeRoot returns the scope root that owns credentials for the target.
func AuthScopeRoot(cityRoot, scopeRoot string, target contract.DoltConnectionTarget) string {
	if filepath.Clean(scopeRoot) == filepath.Clean(cityRoot) {
		return cityRoot
	}
	if target.EndpointOrigin == contract.EndpointOriginExplicit {
		return scopeRoot
	}
	return cityRoot
}

// Resolve returns the effective Dolt auth for a scope and target.
// Ambient BEADS_DOLT_PASSWORD is an intentional fallback for operators and
// non-bd callers, after scope-local .beads/.env and before credentials files.
func Resolve(scopeRoot, fallbackUser, host string, port int) Resolved {
	overridePath := strings.TrimSpace(os.Getenv("BEADS_CREDENTIALS_FILE"))
	return Resolved{
		User:                    resolveUser(fallbackUser, host, port),
		Password:                resolvePassword(scopeRoot, host, port, overridePath),
		CredentialsFileOverride: overridePath,
	}
}

// ResolveFromEnv returns effective Dolt auth using projected environment values.
// Projected BEADS_DOLT_PASSWORD is treated like an already-resolved fallback;
// callers that switch auth scopes must clear stale projected passwords first.
func ResolveFromEnv(scopeRoot, fallbackUser string, env map[string]string) Resolved {
	return resolveFromEnv(scopeRoot, fallbackUser, env, true)
}

// ResolveScopedFromEnv returns effective Dolt auth for a GC-selected scope.
// Unlike ResolveFromEnv, it does not fall back to ambient BEADS_DOLT_PASSWORD;
// scoped GC projections must not let one external rig password contaminate
// another scope's managed city/HQ connection.
func ResolveScopedFromEnv(scopeRoot, fallbackUser string, env map[string]string) Resolved {
	return resolveFromEnv(scopeRoot, fallbackUser, env, false)
}

func resolveFromEnv(scopeRoot, fallbackUser string, env map[string]string, allowAmbientBeadsPassword bool) Resolved {
	host := strings.TrimSpace(env["GC_DOLT_HOST"])
	port, ok := projectedPort(env)
	if host == "" && ok {
		host = "127.0.0.1"
	}
	if !ok {
		port = 0
	}
	overridePath := strings.TrimSpace(env["BEADS_CREDENTIALS_FILE"])
	if overridePath == "" {
		overridePath = strings.TrimSpace(os.Getenv("BEADS_CREDENTIALS_FILE"))
	}
	envPass := strings.TrimSpace(env["BEADS_DOLT_PASSWORD"])
	return Resolved{
		User:                    resolveUser(fallbackUser, host, port),
		Password:                resolvePasswordWithEnv(envPass, scopeRoot, host, port, overridePath, allowAmbientBeadsPassword),
		CredentialsFileOverride: overridePath,
	}
}

// AmbientIdentityAppliesTo reports whether the ambient GC_DOLT_USER /
// GC_DOLT_PASSWORD override may be applied to the endpoint being resolved.
// Exported for the cmd/gc paths that dial Dolt directly (the managed-Dolt SQL
// helpers, the bd store bridge) and read the ambient password themselves: they
// must decline it on the same provable endpoint mismatch (gc-49ho).
//
// THE AMBIENT IDENTITY TRAVELS WITH THE AMBIENT ENDPOINT (ga-3qvmjj). gc already
// resolves host and port PER STORE and ignores an ambient endpoint that does not
// match the store being opened — that is the "ignoring ambient Dolt host/port
// override for external target" warning in cmd_bd.go. The identity had no such
// guard, so the endpoint was resolved per store while the credential was
// resolved per process, and the two disagreed.
//
// What that cost, measured 2026-08-27 after the qcore hub flip: an agent session
// projected for the hub (GC_DOLT_HOST=100.71.23.94, GC_DOLT_PORT=3307,
// GC_DOLT_USER=cherub) could not open the LOCAL city store at all —
//
//	failed to check if database "hq" exists on server 127.0.0.1:51361:
//	Error 1045 (28000): Access denied for user 'cherub'
//
// because no "cherub" exists on the managed local server. Note what that error
// proves: the endpoint had ALREADY correctly resolved to 127.0.0.1:51361, so the
// ambient 3307 was being ignored while the credential that came with it was not.
// hq is the coordination plane, so the blast radius was the agent's mail, its
// ga- beads, its hook queue and its work queue — it presents as the agent
// dropping out of the city, not as a connection error.
//
// The same hazard was already recognized for the PASSWORD one scope up: see
// ResolveScopedFromEnv, "scoped GC projections must not let one external rig
// password contaminate another scope's managed city/HQ connection". This closes
// the same class for the user.
//
// A BARE OVERRIDE STILL APPLIES EVERYWHERE. When the ambient environment names
// no endpoint, GC_DOLT_USER is a deliberate operator override — the documented
// behavior that doltauth reads it via os.Getenv rather than from the resolution
// map — and it keeps working unchanged. Likewise when the target endpoint is
// unknown there is nothing to disagree with, so the override applies. Only an
// ambient identity that PROVABLY belongs to a different endpoint is declined.
func AmbientIdentityAppliesTo(targetHost string, targetPort int) bool {
	ambientHost := strings.TrimSpace(os.Getenv("GC_DOLT_HOST"))
	ambientPort := strings.TrimSpace(os.Getenv("GC_DOLT_PORT"))
	if ambientHost == "" && ambientPort == "" {
		return true
	}
	if strings.TrimSpace(targetHost) == "" && targetPort == 0 {
		return true
	}
	if ambientHost != "" && strings.TrimSpace(targetHost) != "" &&
		!sameDoltHost(ambientHost, targetHost) {
		return false
	}
	if ambientPort != "" && targetPort != 0 && ambientPort != strconv.Itoa(targetPort) {
		return false
	}
	return true
}

// sameDoltHost compares two spellings of the same endpoint. "localhost" and
// "127.0.0.1" name the same server and both spellings appear across the config
// surface, so a literal comparison would decline a legitimate override.
func sameDoltHost(a, b string) bool {
	norm := func(h string) string {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "localhost" || h == "::1" {
			return "127.0.0.1"
		}
		return h
	}
	return norm(a) == norm(b)
}

func resolveUser(fallbackUser, targetHost string, targetPort int) string {
	if user := strings.TrimSpace(os.Getenv("GC_DOLT_USER")); user != "" &&
		AmbientIdentityAppliesTo(targetHost, targetPort) {
		return user
	}
	return strings.TrimSpace(fallbackUser)
}

func resolvePassword(scopeRoot, host string, port int, overridePath string) string {
	return resolvePasswordWithEnv("", scopeRoot, host, port, overridePath, true)
}

func resolvePasswordWithEnv(envPass, scopeRoot, host string, port int, overridePath string, allowAmbientBeadsPassword bool) string {
	// The ambient PASSWORD is half of the ambient identity and is declined on
	// the same provable endpoint mismatch as the ambient user
	// (AmbientIdentityAppliesTo). Guarding the user alone is not enough, and the
	// way it fails is actively misleading: a password present at all forces the
	// credentialed path, so with the user correctly declined the connection
	// falls back to the local default and the error moves from
	//
	//	Access denied for user 'cherub'   ->   Access denied for user 'root'
	//
	// which reads as an unrelated bug rather than as the same leak one variable
	// further along. Measured by lana on 2026-08-27 against the first fix
	// (5b70f17f4): with a real crew env, `env -u GC_DOLT_PASSWORD` alone
	// restored hq while `env -u GC_DOLT_USER` alone did not.
	//
	// root on the managed local server has NO password, so supplying one is not
	// merely redundant — it is what makes the connection fail.
	ambientApplies := AmbientIdentityAppliesTo(host, port)
	if pass := strings.TrimSpace(os.Getenv("GC_DOLT_PASSWORD")); pass != "" && ambientApplies {
		return pass
	}
	if pass := ReadStoreLocalPassword(scopeRoot); pass != "" {
		return pass
	}
	if envPass != "" {
		return envPass
	}
	if allowAmbientBeadsPassword {
		if pass := strings.TrimSpace(os.Getenv("BEADS_DOLT_PASSWORD")); pass != "" && ambientApplies {
			return pass
		}
	}
	host = strings.TrimSpace(host)
	if host == "" || port <= 0 {
		return ""
	}
	lookupPath := overridePath
	if lookupPath == "" {
		lookupPath = DefaultCredentialsPath()
	}
	if lookupPath == "" {
		return ""
	}
	return ReadCredentialsPassword(lookupPath, host, port)
}

// ReadStoreLocalPassword returns the BEADS_DOLT_PASSWORD from a scope-local .beads/.env file.
func ReadStoreLocalPassword(scopeRoot string) string {
	if strings.TrimSpace(scopeRoot) == "" {
		return ""
	}
	return readSimpleEnvValue(filepath.Join(scopeRoot, ".beads", ".env"), "BEADS_DOLT_PASSWORD")
}

func readSimpleEnvValue(path, key string) string {
	f, err := os.Open(path) //nolint:gosec // path is derived from scope roots
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	value := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		name, raw, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		value = strings.TrimSpace(raw)
		if len(value) >= 2 {
			if (value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"') {
				if unquoted, err := strconv.Unquote(value); err == nil {
					value = unquoted
				} else {
					value = value[1 : len(value)-1]
				}
			}
		}
	}
	return value
}

func projectedPort(env map[string]string) (int, bool) {
	port := strings.TrimSpace(env["GC_DOLT_PORT"])
	if port == "" {
		return 0, false
	}
	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

// DefaultCredentialsPath returns the default beads credentials file path for the current OS.
func DefaultCredentialsPath() string {
	if runtime.GOOS == "windows" {
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			return filepath.Join(appData, "beads", "credentials")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".config", "beads", "credentials")
}

// ReadCredentialsPassword returns the password for the given host:port from a beads credentials file.
func ReadCredentialsPassword(path, host string, port int) string {
	f, err := os.Open(path) //nolint:gosec // path comes from env or os.UserHomeDir
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	sectionKey := host + ":" + strconv.Itoa(port)
	inSection := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := line[1 : len(line)-1]
			if section == sectionKey {
				inSection = true
			} else if inSection {
				break
			}
			continue
		}
		if !inSection {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "password" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
