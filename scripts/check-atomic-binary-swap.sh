#!/usr/bin/env bash
# check-atomic-binary-swap: keep gc's build/install paths swapping by atomic rename.
#
# Why this gate exists (ga-l8pur, ga-pmeo1):
# On macOS, writing new bytes into an executable's EXISTING inode leaves the
# kernel holding a cached code-signing decision for that vnode. A process still
# running from that inode is SIGKILLed the next time it faults in a page that no
# longer matches the cached cdhash -- reported as
# `EXC_CRASH / SIGKILL (Code Signature Invalid)`, namespace CODESIGNING. The
# binary then appears to exit 137 with no output. This took the city down
# repeatedly on 2026-08-03/04 and it is silent: it looks like a flaky supervisor.
#
# An atomic `mv` replaces the DIRECTORY ENTRY instead, so a running process keeps
# its own intact inode and the next exec picks up a fresh one.
#
# Measured on darwin 25.0.0: `go build -o` and `go install` both unlink first and
# yield a fresh inode (safe). `cp -f` and `codesign --force` reuse the inode
# (unsafe on a live path). So this gate targets cp/codesign, not the Go toolchain.

set -euo pipefail

cd "$(dirname "$0")/.."

fail=0

note() { printf '%s\n' "$*" >&2; }

# 1. `make build` must not sign or `go build` straight onto $(BUILD_DIR)/$(BINARY).
#    It must stage to a temp path and `mv` it into place.
build_body=$(awk '/^build:/{flag=1;next} /^[a-zA-Z_-]+:/{flag=0} flag' Makefile)

if printf '%s' "$build_body" | grep -qE 'sign-darwin-local\.sh[[:space:]]+"?\$\(BUILD_DIR\)/\$\(BINARY\)'; then
	note "ERROR: 'make build' signs \$(BUILD_DIR)/\$(BINARY) in place."
	note "       codesign --force rewrites the existing inode; sign the temp file"
	note "       BEFORE the atomic rename instead. See ga-pmeo1."
	fail=1
fi

if printf '%s' "$build_body" | grep -qE 'go build .*-o[[:space:]]+"?\$\(BUILD_DIR\)/\$\(BINARY\)'; then
	note "ERROR: 'make build' writes go build output directly to \$(BUILD_DIR)/\$(BINARY)."
	note "       Build to a temp path in the same directory, then 'mv -f' into place."
	fail=1
fi

if ! printf '%s' "$build_body" | grep -qE 'mv -f .*\$\(BUILD_DIR\)/\$\(BINARY\)'; then
	note "ERROR: 'make build' does not swap \$(BUILD_DIR)/\$(BINARY) by atomic rename."
	fail=1
fi

# 2. `make install` must swap by rename and must NOT cp straight onto the live path.
install_body=$(awk '/^install:/{flag=1;next} /^[a-zA-Z_-]+:/{flag=0} flag' Makefile)

if printf '%s' "$install_body" | grep -qE 'cp -f .*"?\$\(INSTALL_DIR\)/\$\(BINARY\)"?[[:space:]]*(;|$)'; then
	note "ERROR: 'make install' copies directly onto \$(INSTALL_DIR)/\$(BINARY)."
	note "       That reuses the live inode and can SIGKILL a running gc."
	fail=1
fi

if ! printf '%s' "$install_body" | grep -qE 'mv -f .*\$\(INSTALL_DIR\)/\$\(BINARY\)'; then
	note "ERROR: 'make install' does not swap \$(INSTALL_DIR)/\$(BINARY) by atomic rename."
	fail=1
fi

# 3. `make install` must verify the installed binary UNPIPED.
#    `gc version | head; echo $?` reports head's status and shows 0 for a binary
#    SIGKILLed before writing a byte -- this nearly caused a false all-clear.
if ! printf '%s' "$install_body" | grep -qE '\$\(INSTALL_DIR\)/\$\(BINARY\)"? version >'; then
	note "ERROR: 'make install' does not exec the installed binary to verify it."
	note "       Redirect to a file (never a pipe) and assert exit 0 AND non-empty output."
	fail=1
fi

if printf '%s' "$install_body" | grep -qE '\$\(BINARY\)"? version[[:space:]]*\|'; then
	note "ERROR: 'make install' pipes the verification exec."
	note "       A pipeline reports the LAST command's status, masking exit 137."
	fail=1
fi

if [ "$fail" -eq 0 ]; then
	echo "check-atomic-binary-swap: OK (build + install swap by atomic rename; install verifies unpiped)"
fi

exit "$fail"
