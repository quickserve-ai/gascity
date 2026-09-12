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
# revision actually links into gc. On carry/operational that is the beads
# fork's fleet build (go.mod `replace github.com/steveyegge/beads =>
# github.com/quickserve-ai/beads <fleet build>`; CARRY.md "Beads pin"). A
# local-directory replace has no revision to build from -- refuse loudly
# rather than guess.
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
  # CGO_ENABLED=1: the fleet's bd (and the tarball this installer replaced) is
  # a CGO build with embedded Dolt; a CGO_ENABLED=0 bd refuses embedded stores
  # outright ("embedded Dolt requires a CGO build"), which broke every test
  # that `bd init`s a store (scaffold_roundtrip_any_bd, the doctor
  # custom-types family) the moment this installer landed. -tags gms_pure_go
  # stays: it is orthogonal to CGO and matches the shipped-image recipe
  # (contrib/k8s/Dockerfile.agent builds CGO_ENABLED=1 with the same tag).
  # Linux runners compile the Dolt cgo deps cleanly (see fork-verify.yml);
  # macOS needs Homebrew icu4c's keg-only headers on the CGO search path,
  # same as the Makefile does for local builds.
  if [[ "$(uname)" == "Darwin" ]]; then
    icu_prefix="$(brew --prefix icu4c 2>/dev/null || true)"
    if [[ -z "$icu_prefix" ]]; then
      brew install -q icu4c >/dev/null 2>&1 || true
      icu_prefix="$(brew --prefix icu4c 2>/dev/null || true)"
    fi
    if [[ -n "$icu_prefix" ]]; then
      export CGO_CPPFLAGS="${CGO_CPPFLAGS:-} -I${icu_prefix}/include"
      export CGO_LDFLAGS="${CGO_LDFLAGS:-} -L${icu_prefix}/lib"
    fi
  fi
  #
  # Build inside the resolved module's own directory instead of
  # `go install ${resolved_path}/cmd/bd@${resolved_version}`: the beads fork
  # declares upstream's module path (`module github.com/steveyegge/beads`), so
  # a path-versioned install of github.com/quickserve-ai/beads fails with
  # "module declares its path as: github.com/steveyegge/beads". `go list -m`
  # points .Dir at the replacement's module-cache tree whenever a replace is
  # present (and at the require pin otherwise); building there uses that
  # module's own go.mod/go.sum, exactly as the path-versioned install did.
  # `go mod download` first so .Dir is populated on a cold runner.
  #
  # Stamp the resolved pin into the version label (contrib/k8s/Dockerfile.agent
  # precedent). With a replace this is the REPLACE version -- the value gc's
  # version_compat preflight compares (beadsModuleVersion returns
  # dep.Replace.Version), and a fleet tag carries no commit token for that
  # gate's commit-scan fallback, so the label must equal it byte-for-byte.
  # Without a replace, the pseudo-version's commit suffix makes `bd version`
  # name the exact revision in CI logs, and the same gate can equate it with
  # go.mod's pin instead of warning on an anonymous "1.1.0 (dev)" — a label
  # byte-identical to the skewed tarball binary this installer exists to
  # replace.
  #
  # The download and the build resolve bd's own module graph, not this repo's,
  # so the go-mod-warm step upstream does not cover them: those checksums are
  # fetched and verified here, on their own trip to sum.golang.org. Same flake
  # class (transient HTTP/2 INTERNAL_ERROR on a checksum tile), same bounded
  # backoff (ga-azybk8). Verification is untouched.
  build_bd_from_module_dir() {
    go mod download "$module" || return $?
    local module_dir
    module_dir="$(go list -m -f '{{.Dir}}' "$module")" || return $?
    if [[ -z "$module_dir" || ! -d "$module_dir" ]]; then
      echo "could not locate the module directory for ${resolved_path}@${resolved_version}" >&2
      return 1
    fi
    CGO_ENABLED=1 go -C "$module_dir" build -tags gms_pure_go \
      -ldflags "-X main.Version=${resolved_version#v}" \
      -o "$target" ./cmd/bd
  }
  install_attempt=1
  install_max=3
  while :; do
    set +e
    build_bd_from_module_dir
    install_status=$?
    set -e
    if ((install_status == 0)); then
      break
    fi
    echo "attempt ${install_attempt}/${install_max}: bd build exited ${install_status}" >&2
    if ((install_attempt >= install_max)); then
      echo "bd build failed after ${install_max} attempts; exiting ${install_status}" >&2
      exit "$install_status"
    fi
    if ((install_attempt == 1)); then
      sleep 5
    else
      sleep 15
    fi
    install_attempt=$((install_attempt + 1))
  done
fi

if [[ ! -x "$target" ]]; then
  echo "bd was not installed at ${target}" >&2
  exit 1
fi

if [[ -n "${GITHUB_PATH:-}" ]]; then
  echo "$bin_dir" >> "$GITHUB_PATH"
fi

"$target" version
