#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 1 ]; then
	echo "usage: scripts/sign-darwin-local.sh <binary>" >&2
	exit 2
fi

binary=$1
binary_name=$(basename "$binary")
identifier=${GC_SIGN_IDENTIFIER:-com.gascity.gc}

if [ "$(uname -s)" != "Darwin" ]; then
	exit 0
fi

if [ ! -f "$binary" ]; then
	echo "cannot sign missing binary: $binary" >&2
	exit 1
fi

if ! command -v codesign >/dev/null 2>&1; then
	echo "codesign not found; leaving Go linker signature unchanged for $binary_name"
	exit 0
fi

strip_provenance() {
	if command -v xattr >/dev/null 2>&1; then
		xattr -d com.apple.provenance "$binary" 2>/dev/null || true
	fi
}

# try_sign makes one codesign attempt with identity. On failure it leaves
# codesign's own message in sign_error so the caller can report it.
sign_error=""
try_sign() {
	local identity=$1

	if sign_error=$(codesign --force --sign "$identity" --identifier "$identifier" "$binary" 2>&1 </dev/null); then
		strip_provenance
		echo "Signed $binary_name with stable macOS identity: $identity"
		return 0
	fi
	return 1
}

# find_stable_identities prints every identity name that matches a known
# pattern, one per line, in pattern priority order and without duplicates.
# `security find-identity` without -v can list one identity twice: once under
# the matching identities and again under the valid ones.
find_stable_identities() {
	local candidates=$1
	local pattern

	for pattern in 'Apple Development:' 'Developer ID Application:' 'GasCity Dev'; do
		printf '%s\n' "$candidates" | awk -F '"' -v pattern="$pattern" 'index($0, pattern) && $2 != "" {print $2}'
	done | awk '!seen[$0]++'
}

if [ -n "${GC_SIGN_IDENTITY:-}" ]; then
	if try_sign "$GC_SIGN_IDENTITY"; then
		exit 0
	fi
	echo "failed to sign $binary_name with GC_SIGN_IDENTITY=$GC_SIGN_IDENTITY: $sign_error" >&2
	exit 1
fi

identities=""
if command -v security >/dev/null 2>&1; then
	# NO -v. `-v` filters to identities with a TRUSTED chain, which excludes every
	# self-signed local certificate -- including "GasCity Dev", one of the three
	# patterns find_stable_identities searches for. So the auto-detect path could
	# never find the very identity it was written to look for, and every local
	# build silently fell through to the Go linker's ad-hoc signature.
	#
	# That matters because an ad-hoc signature is derived from the binary's
	# CONTENT: its designated requirement is a cdhash that changes on EVERY
	# build, so the macOS firewall (and TCC) can never match a previous grant
	# and re-prompts after each rebuild. Measured on this box 2026-09-16 --
	# two ad-hoc builds gave two different cdhashes, while a certificate-signed
	# build gives `identifier "com.gascity.gc" and certificate root = H"..."`,
	# which is stable across rebuilds. That is the whole point of this script.
	#
	# Without -v an untrusted or expired identity (an old "Apple Development:"
	# cert, say) can match ahead of one that works, so every matching identity
	# is tried in priority order until one signs (ga-0eoxgp).
	candidates=$(security find-identity -p codesigning 2>/dev/null || true)
	identities=$(find_stable_identities "$candidates")
fi

if [ -n "$identities" ]; then
	failures=""
	while IFS= read -r identity <&3; do
		if try_sign "$identity"; then
			exit 0
		fi
		echo "Could not sign $binary_name with $identity; trying the next stable identity." >&2
		failures="$failures  $identity: ${sign_error:-codesign failed}
"
	done 3<<IDENTITIES
$identities
IDENTITIES
	# A stable identity exists but none signed. Falling back to the ad-hoc
	# signature here would be silent: every rebuild changes the cdhash and TCC
	# re-prompts with nothing saying why.
	echo "failed to sign $binary_name with any stable macOS identity. Tried:" >&2
	printf '%s' "$failures" >&2
	echo "Set GC_SIGN_IDENTITY='<certificate name>' to choose one, or fix or remove the failing certificates." >&2
	exit 1
fi

if [ "${GC_ADHOC_SIGN:-0}" = "1" ]; then
	if codesign --force --sign - "$binary" 2>/dev/null; then
		strip_provenance
		echo "Ad-hoc signed $binary_name by explicit opt-in"
	else
		echo "Could not ad-hoc sign $binary_name; leaving Go linker signature unchanged." >&2
	fi
	exit 0
fi

echo "No stable macOS signing identity found; leaving Go linker signature unchanged for $binary_name."
echo "Set GC_SIGN_IDENTITY='<certificate name>' for persistent local TCC grants."
