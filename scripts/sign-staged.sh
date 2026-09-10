#!/usr/bin/env bash
#
# sign-staged.sh — code-sign a STAGED gc binary with this box's local signing
# identity, so one macOS TCC "Allow" survives every rebuild (ga-f9dnub).
#
# gc is ad-hoc signed by default, so its code identity (cdhash) changes on every
# build and macOS re-prompts for permission each time. A stable code-signing
# certificate moves TCC's stored designated requirement off the cdhash and onto
# the cert, so the grant re-matches across rebuilds. The certificate is per-box
# operator setup (mint it with .gc/staging/tcc-signing-*/wizard.sh, or any local
# self-signed code-signing cert trusted for code signing); this script only
# consumes it.
#
# CONTRACT
#   sign-staged.sh <staged-binary-path>
#   - With a signing identity configured: code-signs the staged file in place.
#   - With NO identity configured: a graceful NO-OP (exit 0), leaving the file
#     ad-hoc exactly as today. This is load-bearing — the same carry/operational
#     commit deploys on a box that has never minted a cert without breaking
#     `make install`. Do not turn the absent-identity case into an error.
#
# Identity resolution (first wins):
#   $GC_SIGNING_IDENTITY                         — identity (SHA-1 or cert name)
#   $GC_CODESIGN_IDENTITY                         — alias the wizard writes
#   ~/.gc/gc-codesign-identity.env               — KEY=VALUE file (wizard output,
#   $GC_CITY_PATH/.gc/secrets/gc-codesign-identity.env)   symlinked/real)
# Optional alongside the identity:
#   GC_CODESIGN_ID_NAME   codesign --identifier   (default: com.gastown.gc)
#   GC_CODESIGN_KEYCHAIN  keychain to search      (default: login.keychain-db)
#
# HARD CONSTRAINTS (ga-l8pur / ga-4v3ckk, restated by woodhouse on ga-f9dnub):
#   - Sign the STAGED file only, never the live install target. `codesign
#     --force` on the running path rewrites the live inode and can invalidate the
#     launchd code requirement mid-flight (the supervisor-SIGKILL class). This
#     script refuses a path that is the current gc on PATH or under go/bin.
#   - The FIRST signed deploy on a box changes gc's signing identity, which is
#     exactly when launchd's cached launch constraints bite: that deploy needs the
#     full supervisor re-registration (bootout → bootstrap[retry] → enable →
#     kickstart). That sequencing is the caller's/deploy's job, not this script's.

set -euo pipefail

die() { printf 'sign-staged: %s\n' "$1" >&2; exit 1; }

[[ $# -eq 1 ]] || die "usage: sign-staged.sh <staged-binary-path>"
staged="$1"
[[ -f "$staged" ]] || die "no file at staged path: $staged"

# --- resolve identity (graceful no-op when absent) -------------------------
env_file=""
for candidate in \
    "$HOME/.gc/gc-codesign-identity.env" \
    "${GC_CITY_PATH:-}/.gc/secrets/gc-codesign-identity.env"; do
  [[ -n "$candidate" && -f "$candidate" ]] && { env_file="$candidate"; break; }
done
if [[ -n "$env_file" ]]; then
  # Only import the three keys we use; tolerate comments/blank lines.
  while IFS='=' read -r k v; do
    case "$k" in
      GC_CODESIGN_IDENTITY|GC_SIGNING_IDENTITY|GC_CODESIGN_ID_NAME|GC_CODESIGN_KEYCHAIN)
        v="${v%\"}"; v="${v#\"}"
        [[ -z "${!k:-}" ]] && printf -v "$k" '%s' "$v" ;;
    esac
  done < "$env_file"
fi

identity="${GC_SIGNING_IDENTITY:-${GC_CODESIGN_IDENTITY:-}}"
if [[ -z "$identity" ]]; then
  echo "sign-staged: no signing identity configured — leaving $staged ad-hoc (no-op)."
  echo "             (mint one with .gc/staging/tcc-signing-*/wizard.sh to stop the TCC re-prompt; ga-f9dnub)"
  exit 0
fi

# --- refuse to sign a live/running path ------------------------------------
abs() { cd "$(dirname "$1")" >/dev/null 2>&1 && printf '%s/%s' "$(pwd -P)" "$(basename "$1")"; }
staged_abs="$(abs "$staged")"
live_gc=""; command -v gc >/dev/null 2>&1 && live_gc="$(abs "$(command -v gc)")"
for forbidden in "$live_gc" "$HOME/go/bin/gc" "$HOME/.local/bin/gc"; do
  [[ -n "$forbidden" && "$staged_abs" == "$(abs "$forbidden" 2>/dev/null || echo "$forbidden")" ]] \
    && die "refusing to sign the LIVE gc path ($staged) — sign the staged copy before the rename (ga-l8pur)."
done

command -v codesign >/dev/null 2>&1 || die "codesign not found (is this macOS?)"

id_name="${GC_CODESIGN_ID_NAME:-com.gastown.gc}"
keychain="${GC_CODESIGN_KEYCHAIN:-login.keychain-db}"

echo "sign-staged: signing $staged as '$id_name' with identity $identity (keychain $keychain)"
codesign --force --sign "$identity" --identifier "$id_name" --keychain "$keychain" "$staged" \
  || die "codesign failed — check the cert is trusted for code signing and the key ACL allows codesign (security set-key-partition-list)."

# Verify the signature took and is internally consistent (not a trust/Gatekeeper
# check — just that the staged file carries the cert-based identity we expect).
codesign --verify --strict "$staged" \
  || die "post-sign verify failed for $staged"
echo "sign-staged: ok — $(codesign -dvvv "$staged" 2>&1 | grep -E '^Authority=' | head -n1 || echo 'signed')"
