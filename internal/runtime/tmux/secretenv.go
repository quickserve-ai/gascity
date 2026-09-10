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
// This is the availability half of the mechanism; the security half is the
// caller passing tmux's client -N flag ("do not start the server even if
// the command would normally do so") on every -e-bearing new-session. With
// -N, any window this probe cannot close — the server exiting between the
// check and the new-session, a racing creator killing the last session, an
// anchor that failed to start — turns into an ordinary ErrNoServer on the
// session call (retried by ensureFreshSession), never into a daemon forked
// with secrets on its command line. Anchor errors are therefore deliberately
// swallowed here: the -N'd call is the enforcement point.
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
	// Only a COLD socket needs the anchor. Order matters: ErrNoCurrentTarget
	// (a live server holding zero sessions — persistent under gc's
	// exit-empty-off default) WRAPS ErrNoServer, so it must be recognized as
	// "alive" before the ErrNoServer check. Any other error (timeout,
	// degraded) is indeterminate: skip the anchor and let the -N'd session
	// call decide — it fails closed rather than forking.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), newSessionProbeTimeout)
	_, err := t.runCtx(probeCtx, "has-session", "-t", "="+probeSessionName)
	probeCancel()
	if errors.Is(err, ErrNoCurrentTarget) || !errors.Is(err, ErrNoServer) {
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
	var anchorErr error
	if ee, ok := t.exec.(envExecutor); ok {
		_, anchorErr = ee.executeCtxEnv(ctx, args, scrubSecretEnviron(os.Environ()))
	} else {
		_, anchorErr = t.exec.executeCtx(ctx, args)
	}
	if anchorErr != nil {
		return cleanup
	}
	return func() {
		_, _ = t.run("kill-session", "-t", "="+anchor)
	}
}
