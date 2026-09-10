package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Secrets must never reach the tmux SERVER's process-table entry (ga-fhbnmz).
//
// `new-session -e KEY=VALUE` is a documented, normally-harmless call: the
// client process exits in milliseconds. But when no server is listening, that
// same invocation FORKS THE SERVER, and the daemon keeps the full client argv
// — every -e pair — in its process-table entry for the life of the city. Four
// live credentials sat in `ps -eo command` output that way, readable by every
// process running as this user without touching a credential file. The same
// fork also seeds the server's GLOBAL ENVIRONMENT (and its own process
// environment) from the forking client, so the daemon retains ambient
// credentials on a second surface (`tmux show-environment -g`).
//
// The fix is to make sure the server is never forked by a session-env
// command: when the socket is cold, an inert ANCHOR new-session — carrying
// no env, a clean argv, and a secret-scrubbed process environment — forks
// the daemon first and holds it alive (a bare `start-server` cannot do this:
// tmux's default exit-empty makes a session-less server exit immediately,
// measured in this package's integration test). The real new-session then
// runs against the live server as an ordinary millisecond-lived client, and
// the anchor is killed. Session env delivery via -e is untouched.
//
// Residual exposure, accepted deliberately: the -e client argv is still
// readable for the few milliseconds the client lives, and the session
// environment remains queryable via the (0700, same-user) tmux socket. Both
// are documented on ga-fhbnmz; the persistent, casually-captured surface —
// the server that any `ps` paste photographs — is what this closes.

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
// that don't still observe the anchor call through executeCtx.
type envExecutor interface {
	executeCtxEnv(ctx context.Context, args []string, env []string) (string, error)
}

// serverAnchorLifetime bounds an anchor session's own life so a crash
// between forking the server and killing the anchor cannot leave it behind
// forever. Generous, because under heavy host load the real new-session that
// follows can itself take a while.
const serverAnchorLifetime = "120"

// startServerInert makes sure the tmux server is already running before a
// session-env command is issued, so that command can never be the one that
// forks the daemon. On a cold socket it forks the server via an inert anchor
// session — clean argv, no -e pairs, secret-scrubbed process environment —
// and returns a cleanup that kills the anchor once the caller's real session
// exists (or failed; either way the server is back to today's semantics).
// Against a live server it is a no-op returning a no-op cleanup.
//
// Best-effort by design: any genuine server problem is surfaced by the
// new-session call that follows, with its full error mapping (ErrNoServer,
// ErrSessionExists, ...) intact. The one uncovered window — the server
// exiting between this check and the caller's new-session — degrades to
// exactly today's behavior, never to something worse.
//
// Skipped when no socket name is configured, mirroring probeServerAlive:
// that is the ad-hoc/default-server case, where cold-starting the USER'S
// tmux server as a side effect would be wrong. Every managed city runs on a
// named socket.
func (t *Tmux) startServerInert() (cleanup func()) {
	cleanup = func() {}
	if t.cfg.SocketName == "" {
		return cleanup
	}
	// Only a COLD socket needs the anchor; against a live server an extra
	// session create/kill per spawn would be pointless churn for hooks and
	// observers.
	if _, err := t.run("has-session", "-t", "="+probeSessionName); !errors.Is(err, ErrNoServer) {
		return cleanup
	}
	anchor := fmt.Sprintf("gc-srv-anchor-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), tmuxSubprocessTimeout)
	defer cancel()
	args := []string{
		"-u", "-L", t.cfg.SocketName,
		"new-session", "-d", "-s", anchor,
		"/bin/sh", "-c", "sleep " + serverAnchorLifetime,
	}
	var err error
	if ee, ok := t.exec.(envExecutor); ok {
		_, err = ee.executeCtxEnv(ctx, args, scrubSecretEnviron(os.Environ()))
	} else {
		_, err = t.exec.executeCtx(ctx, args)
	}
	if err != nil {
		return cleanup
	}
	return func() {
		_, _ = t.run("kill-session", "-t", "="+anchor)
	}
}
