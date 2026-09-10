#!/usr/bin/env bash
#
# test-sign-staged.sh — asserts the load-bearing properties of sign-staged.sh:
# graceful no-identity fallback, staged-only signing (refuse the live path),
# and arg/shape guards. The actual-signing path needs a trusted code-signing
# cert (per-box operator setup), so it is exercised only when one is available
# and otherwise SKIPPED with a reason — never silently passed. (ga-f9dnub)

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
sign="$here/sign-staged.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0; fail=0; skip=0
ok()   { printf '  ok   %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  FAIL %s\n' "$1"; fail=$((fail+1)); }
skipt(){ printf '  skip %s (%s)\n' "$1" "$2"; skip=$((skip+1)); }

# A throwaway "staged binary".
printf 'int main(){return 0;}\n' > "$tmp/x.c"
cc -o "$tmp/staged-gc" "$tmp/x.c" 2>/dev/null || cp /bin/echo "$tmp/staged-gc"

# 1. No identity configured anywhere -> graceful no-op, exit 0, file unchanged.
before="$(shasum -a 256 "$tmp/staged-gc" | cut -d' ' -f1)"
if env -u GC_SIGNING_IDENTITY -u GC_CODESIGN_IDENTITY GC_CITY_PATH="$tmp/nope" HOME="$tmp/nohome" \
     "$sign" "$tmp/staged-gc" >/dev/null 2>&1; then
  after="$(shasum -a 256 "$tmp/staged-gc" | cut -d' ' -f1)"
  [[ "$before" == "$after" ]] && ok "no-identity: exits 0 and leaves the binary untouched" \
                              || bad "no-identity: binary was modified despite no identity"
else
  bad "no-identity: non-zero exit (must be a graceful no-op)"
fi

# 2. Missing arg -> usage error, non-zero.
if "$sign" >/dev/null 2>&1; then bad "missing arg: should have failed"; else ok "missing arg: fails with usage"; fi

# 3. Nonexistent staged path -> error, non-zero.
if "$sign" "$tmp/does-not-exist" >/dev/null 2>&1; then bad "missing file: should have failed"; else ok "missing file: fails"; fi

# 4. Staged-only guard: with an identity set, refuse a path that IS the live gc.
#    Simulate a live gc on PATH and point the staged path at it.
mkdir -p "$tmp/bin"; cp "$tmp/staged-gc" "$tmp/bin/gc"
if PATH="$tmp/bin:$PATH" GC_SIGNING_IDENTITY="dummy-identity" \
     "$sign" "$tmp/bin/gc" >/dev/null 2>&1; then
  bad "staged-only guard: signed the live gc path (must refuse)"
else
  ok "staged-only guard: refuses the live gc path even with an identity set"
fi

# 5. Real signing path — only if a trusted code-signing identity exists here.
id="$(security find-identity -p codesigning -v 2>/dev/null | grep -oE '[0-9A-F]{40}' | head -n1 || true)"
if [[ -n "$id" ]]; then
  if GC_SIGNING_IDENTITY="$id" "$sign" "$tmp/staged-gc" >/dev/null 2>&1; then
    codesign --verify --strict "$tmp/staged-gc" >/dev/null 2>&1 \
      && ok "real sign: staged binary verifies after signing" \
      || bad "real sign: verify failed after signing"
  else
    bad "real sign: signing failed with a valid identity present"
  fi
else
  skipt "real sign path" "no trusted code-signing identity on this box — run the tcc-signing wizard"
fi

printf '\n%d passed, %d failed, %d skipped\n' "$pass" "$fail" "$skip"
[[ "$fail" -eq 0 ]]
