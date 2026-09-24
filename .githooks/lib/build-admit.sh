#!/usr/bin/env bash
# Shared by pre-commit and pre-push (ga-wirl8l.2). One heavy Go build per host:
# where the host runs the go-build-admit gate, the hooks' compile steps go
# THROUGH it at GOMAXPROCS=2 and go -p=2. Unbounded hook builds thrashed a
# shared host on 2026-09-24 (5 compiles at ~6 GB each, swap +6-7 GB).
#
# Opt-in per clone, never guessed from $HOME:
#   git config gascity.buildAdmit /path/to/go-build-admit.py
# or GO_BUILD_ADMIT=/path for one invocation. Clones without either behave
# exactly as before. A configured gate that is missing, or that does not
# admit (75) or aborts for disk (74), fails the hook with the reason; a hook
# never silently skips its checks. `--no-verify` remains the recorded bypass.

build_admit_path() {
  printf '%s' "${GO_BUILD_ADMIT:-$(git config --get gascity.buildAdmit 2>/dev/null || true)}"
}

# build_admit_check: returns 0 when no gate is configured or it is usable;
# otherwise prints the reason and returns 1.
build_admit_check() {
  local admit
  admit="$(build_admit_path)"
  [ -z "$admit" ] && return 0
  if [ ! -x "$admit" ]; then
    echo "githooks: the build gate $admit is missing or not executable; refusing." >&2
    echo "githooks: fix the path, or unset gascity.buildAdmit / GO_BUILD_ADMIT." >&2
    return 1
  fi
  return 0
}

# gated CMD...: run CMD through the gate (capped) when one is configured,
# else run it unchanged. Returns CMD's code, or the gate's 75/74 with a reason.
gated() {
  local admit rc
  admit="$(build_admit_path)"
  if [ -z "$admit" ]; then
    "$@"
    return
  fi
  set +e
  "$admit" --wait-s "${GO_BUILD_ADMIT_WAIT_S:-1800}" -- \
    env GOMAXPROCS=2 GOFLAGS="${GOFLAGS:+$GOFLAGS }-p=2" "$@"
  rc=$?
  set -e
  case "$rc" in
    75) echo "githooks: the build gate did not admit '$*' (another heavy build, or low disk); nothing ran. Retry later." >&2 ;;
    74) echo "githooks: the build gate aborted '$*' for low disk. Retry once space recovers." >&2 ;;
  esac
  return "$rc"
}
