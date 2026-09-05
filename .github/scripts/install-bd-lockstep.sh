#!/usr/bin/env bash
set -euo pipefail

# Install the standalone `bd` binary built from the exact beads revision this
# repo's go.mod pins, and put it on PATH for subsequent steps.
#
# Why derive rather than pin: gc links the beads library, and that library
# decides the Dolt schema version gc migrates a store to. Tests and orders then
# shell out to a standalone `bd`, which must understand that same schema.
# Installing bd from a release tarball gave it a version axis of its own, and
# the two drifted apart: CI ran gc built from beads schema v59 beside the bd
# v1.1.0 tarball at schema v53, so `bd create` refused the store with
#   schema version mismatch: database is at v59, binary knows up to v53
# and Integration/rest-smoke-2-of-2 went red (ga-yl326d). A version bump would
# have fixed that day's instance and left the skew representable. Deriving the
# install from go.mod removes the second version entirely -- there is nothing
# left to keep in sync, so the two can never skew again.
#
# Note this is a different axis from deps.env's BD_VERSION, which still pins the
# bd *release tarball* that the container image and the minimum-supported
# contract cell install; those deliberately name a published tag.
#
# Usage: install-bd-lockstep.sh [--cache]
#   --cache installs under RUNNER_TOOL_CACHE so a reused self-hosted runner can
#   skip the rebuild on later jobs. Either way the bin directory is appended to
#   GITHUB_PATH, which prepends it to PATH -- so this bd wins over any stale
#   /usr/local/bin/bd left behind on a reused runner.

usage() {
  cat >&2 <<'USAGE'
Usage: install-bd-lockstep.sh [--cache]

Builds bd from the beads revision go.mod pins and adds it to GITHUB_PATH.
Use --cache on self-hosted runners to install under RUNNER_TOOL_CACHE.
USAGE
}

use_cache=false
while (($#)); do
  case "$1" in
    --cache) use_cache=true ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
  shift
done

module="github.com/steveyegge/beads"

# Resolve the pin from the repo checkout, not from wherever the runner happens
# to be, so the answer is this build's beads revision.
cd "${GITHUB_WORKSPACE:-$PWD}"

if ! command -v go >/dev/null 2>&1; then
  echo "go toolchain is required on PATH before installing bd" >&2
  exit 1
fi

# Honour a replace directive: the installed bd must be built from whatever
# revision actually links into gc. scripts/check-gomod-replace.sh forbids
# local-path and pseudo-version replace targets, so a replace can only name a
# released tag -- but refuse loudly rather than guess if that ever changes.
read -r resolved_path resolved_version < <(
  go list -m -f '{{if .Replace}}{{.Replace.Path}} {{if .Replace.Version}}{{.Replace.Version}}{{else}}LOCAL{{end}}{{else}}{{.Path}} {{.Version}}{{end}}' "$module"
)

if [[ -z "$resolved_path" || -z "$resolved_version" ]]; then
  echo "could not resolve $module from go.mod" >&2
  exit 1
fi
if [[ "$resolved_version" == "LOCAL" ]]; then
  echo "go.mod replaces $module with a local directory; refusing to guess which revision to install" >&2
  exit 1
fi

echo "go.mod pins ${resolved_path}@${resolved_version}; building bd from that revision"

version_slug="${resolved_version//[^A-Za-z0-9._-]/_}"
if $use_cache; then
  cache_root="${RUNNER_TOOL_CACHE:-$HOME/.local}"
  bin_dir="${cache_root}/gascity-bd-lockstep/${version_slug}/bin"
else
  bin_dir="${BD_INSTALL_BIN_DIR:-${HOME}/.local/bin}"
fi
mkdir -p "$bin_dir"

target="${bin_dir}/bd"
if $use_cache && [[ -x "$target" ]]; then
  echo "Reusing cached bd ${resolved_version} at ${target}"
else
  # CGO_ENABLED=0 with -tags gms_pure_go is beads' own documented rebuild recipe
  # (it is the command bd prints in its schema-skew error) and matches how this
  # workflow already builds bd from source for the cross-version contract cells.
  # It also keeps the build hermetic: the cgo path needs ICU headers the runners
  # do not carry.
  GOBIN="$bin_dir" CGO_ENABLED=0 go install -tags gms_pure_go \
    "${resolved_path}/cmd/bd@${resolved_version}"
fi

if [[ ! -x "$target" ]]; then
  echo "bd was not installed at ${target}" >&2
  exit 1
fi

if [[ -n "${GITHUB_PATH:-}" ]]; then
  echo "$bin_dir" >> "$GITHUB_PATH"
fi

"$target" version
