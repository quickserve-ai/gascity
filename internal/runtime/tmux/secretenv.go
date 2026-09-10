package tmux

import (
	"context"
	"strings"
)

// Secrets must never reach the tmux SERVER (ga-fhbnmz).
//
// A tmux server keeps both the argv and the environment of the client that
// forked it. The argv half is how four live credentials sat in `ps` output: a
// `new-session -e KEY=VALUE` that founded the server left every -e pair in the
// daemon's process-table entry. Staging secret values through a private command
// file closes it ([Tmux.runNewSession]). The environment half remains: the
// server keeps the founding client's process environment, as its own and as its
// global environment (`tmux show-environment -g`), for as long as the city
// runs, so gc's ambient credentials would outlive the command that passed them.
//
// So on a named socket every tmux client that can found the server runs with a
// secret-scrubbed process environment ([Tmux.runCtx], [tmuxArgsCanStartServer]).
// Session env delivery is untouched: a session receives its declared values
// through -e or the staged command file, never by server inheritance.

// secretEnvKeyMarkers classifies env var NAMES as secret-bearing for the
// server-environment scrub. Matching is case-insensitive substring on the
// key. Over-matching is safe: a scrubbed var is only absent from the tmux
// SERVER's inherited environment, and every managed session receives its env
// explicitly via -e / set-environment, never by server inheritance.
var secretEnvKeyMarkers = []string{
	"TOKEN",
	"SECRET",
	"PASSWORD",
	"PASSWD",
	"API_KEY",
	"APIKEY",
	"ACCESS_KEY",
	"CREDENTIAL",
	"PRIVATE_KEY",
	"OAUTH",
	"AUTH_JSON",
}

// IsSecretEnvKey reports whether an env var name is secret-classified and
// must be withheld from the tmux server's inherited environment.
func IsSecretEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, marker := range secretEnvKeyMarkers {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// scrubSecretEnviron returns environ (os.Environ() form, "KEY=VALUE") with
// secret-classified entries removed.
func scrubSecretEnviron(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if key, _, ok := strings.Cut(kv, "="); ok && IsSecretEnvKey(key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// envExecutor is the optional executor extension that runs a tmux command
// with an explicit child environment. realExecutor implements it; test fakes
// that do not implement it still observe the call through executeCtx.
type envExecutor interface {
	executeCtxEnv(ctx context.Context, args []string, env []string) (string, error)
}
