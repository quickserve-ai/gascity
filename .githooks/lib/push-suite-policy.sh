#!/usr/bin/env bash
# push-suite-policy.sh: may this host run the full unit suite at push time?
#
# Exit 0: yes, run it (every host without the marker; unchanged behaviour).
# Exit 3: no. This host runs full test suites in CI only, and CI is the gate.
#
# Some hosts cannot afford the suite. On a shared 36 GiB laptop running ~26
# agent seats, one bare pre-push run lasted 6.5 h and took the host to load
# 265 (2026-09-27). Its operator ruled that full suites run in CI only there
# (ga-1zejyh; hook change ga-wirl8l.2). The host opts in with a marker file,
# so every clone and every pusher on that machine gets the same answer and no
# other host changes:
#
#   ${GC_HOST_POLICY_DIR:-$HOME/.gc/host-policy}/test-suites-ci-only
#
# The file's first line, if any, is printed as the reason.
set -euo pipefail

policy_dir="${GC_HOST_POLICY_DIR:-${HOME:-}/.gc/host-policy}"
marker="$policy_dir/test-suites-ci-only"

if [ -n "${HOME:-}${GC_HOST_POLICY_DIR:-}" ] && [ -f "$marker" ]; then
  reason="$(head -n 1 "$marker" 2>/dev/null || true)"
  echo "pre-push: full unit suite SKIPPED on this host: it runs full test suites in CI only (marker $marker)." >&2
  if [ -n "$reason" ]; then
    echo "pre-push: $reason" >&2
  fi
  echo "pre-push: CI is the gate for this push. Run one test file locally if you need a signal." >&2
  exit 3
fi
exit 0
