#!/usr/bin/env bash
# check-termination-seam.sh [repo-root] [--emit-baseline] [--self-test]
#
# The thirteenth unrecorded ending must be unwritable.
#
# ga-ksac39 makes every session ending countable — including the good one, because
# a ratio with an incomplete denominator is worse than no ratio. The seam is
# runtime.StopRecorded, which takes a MANDATORY termination record. This guard is
# what stops a thirteenth call site from going straight to Provider.Stop and
# silently leaving its ending uncounted.
#
# WHY A FENCE AND NOT A CONVENTION. Go will not let an interface method be
# unexported across packages, so Provider.Stop cannot be made unreachable by the
# compiler. And the enumeration cannot be trusted to a human: the first census of
# this exact question found 7 sites, and its own second pass found 3 more
# (session_lifecycle_parallel.go x2, doctor/checks.go x2, api/huma_handlers_rigs.go).
# An enumerated guarantee undercovers its surface. Growth is the violation.
#
# WHAT IT SCANS: non-test .go under cmd/ and internal/, excluding the provider
# IMPLEMENTATIONS themselves (internal/runtime/<provider>/ and the package's own
# fake/adapter/conformance helpers), for `.Stop(` WITH A NON-EMPTY ARGUMENT,
# minus the shapes allowlisted in scripts/termination-seam-patterns.txt.
#
# THE DISCRIMINATOR IS THE SIGNATURE, NOT THE RECEIVER NAME, since 2026-09-19.
# runtime.Provider.Stop takes one argument — the session name. A timer or ticker
# is Stop() with none; a worker handle is Stop(ctx) and funnels through the seam
# itself. Until this scan inverted, it matched a VOCABULARY of six receiver
# spellings, which made the fence an enumeration of the very thing it was built
# to catch: `runtimeProvider.Stop(name)` passed both the scan and its own
# self-test, and a real provider delegation in cmd/gc/status_provider.go had gone
# unseen since the fence was written. The Codex review of PR #106 named it.
#
# The old file also justified the narrowness with a claim that was FALSE: that a
# false negative "costs one uncounted site, which the unclassified budget then
# surfaces". An unrecorded call writes no row, so it cannot raise the unclassified
# share — it is missing from every share, denominator included. Nothing catches
# it but this fence. See the pattern file for the full note.
#
# EXIT: 0 no new violations. 1 new violations (they are printed). 2 the guard
# could not run — NEVER "clean". A guard that cannot run must not answer "pass";
# that is how a blind instrument reads as a green one.
set -u

ROOT=""
EMIT=0
SELFTEST=0
for arg in "$@"; do
	case "$arg" in
	--emit-baseline) EMIT=1 ;;
	--self-test) SELFTEST=1 ;;
	-*) printf 'unknown flag: %s\n' "$arg" >&2; exit 2 ;;
	*) ROOT="$arg" ;;
	esac
done
if [ -z "$ROOT" ]; then
	ROOT=$(cd "$(dirname "$0")/.." 2>/dev/null && pwd) || { echo "cannot resolve repo root" >&2; exit 2; }
fi
[ -d "$ROOT" ] || { printf 'not a directory: %s\n' "$ROOT" >&2; exit 2; }

PATTERNS="$ROOT/scripts/termination-seam-patterns.txt"
BASELINE="$ROOT/scripts/termination-seam-baseline.txt"
[ -f "$PATTERNS" ] || { printf 'missing pattern file: %s\n' "$PATTERNS" >&2; exit 2; }

# scan prints "<path>:<line>:<trimmed source>" for every candidate call.
# Excluded: _test.go; the provider implementations (they ARE Stop); the seam
# file itself; and the conformance/fake helpers that drive providers directly.
scan() {
	local dir="$1" pat
	pat=$(grep -vE '^\s*(#|$)' "$PATTERNS" | paste -sd'|' -) || return 2
	[ -n "$pat" ] || return 2
	# Match every .Stop( with a non-empty argument, then SUBTRACT the allowlisted
	# shapes. The subtraction is what makes a new provider receiver spelling a
	# violation by default instead of an omission nobody notices.
	(cd "$dir" && grep -rnE '\.Stop\([^)]' --include='*.go' cmd internal 2>/dev/null) |
		grep -v '_test\.go:' |
		grep -vE '^internal/runtime/(termination|fake|seam_adapter|beacon)\.go:' |
		grep -vE '^internal/runtime/(tmux|subprocess|exec|ssh|k8s|acp|herdr|auto|t3bridge|runtimetest|hybrid|registry|proctable|runtimecontract|runtimecapability|rppcheck)/' |
		grep -vE ':[0-9]+:\s*//' |
		grep -vE "$pat" |
		sed -E 's/^([^:]+):([0-9]+):[[:space:]]*/\1:\2:/'
}

if [ "$SELFTEST" = 1 ]; then
	# The control: aim the instrument at a KNOWN POSITIVE before trusting any
	# negative it reports. A guard that has only ever been seen answering
	# "clean" has not been shown to see anything at all.
	tmp=$(mktemp -d) || exit 2
	trap 'rm -rf "$tmp"' EXIT
	mkdir -p "$tmp/scripts" "$tmp/cmd/gc" "$tmp/internal"
	cp "$PATTERNS" "$tmp/scripts/" || exit 2
	# The planted call uses a receiver spelling the OLD vocabulary-based fence
	# did NOT know, so this control proves the widening rather than re-proving
	# what the narrow version already caught.
	cat > "$tmp/cmd/gc/planted.go" <<'PLANT'
package main

func planted() {
	if err := runtimeProvider.Stop(name); err != nil {
		_ = err
	}
}
PLANT
	got=$(scan "$tmp") || { echo "SELF-TEST: scan failed" >&2; exit 2; }
	if printf '%s' "$got" | grep -q 'cmd/gc/planted.go:4:'; then
		echo "SELF-TEST PASS: the guard sees a planted direct Provider.Stop call"
		exit 0
	fi
	echo "SELF-TEST FAIL: the guard did NOT see a planted violation — it is blind, and a 'clean' result from it means nothing" >&2
	printf 'scanned output was: %s\n' "$got" >&2
	exit 2
fi

found=$(scan "$ROOT")
rc=$?
[ "$rc" -le 1 ] || { echo "scan failed" >&2; exit 2; }

if [ "$EMIT" = 1 ]; then
	printf '%s\n' "$found" | sed '/^$/d' | sort > "$BASELINE" || exit 2
	printf 'wrote %s (%s entries)\n' "$BASELINE" "$(grep -c . "$BASELINE" 2>/dev/null || echo 0)"
	exit 0
fi

[ -f "$BASELINE" ] || { printf 'missing baseline: %s (run with --emit-baseline)\n' "$BASELINE" >&2; exit 2; }

# Compare on path+source, NOT on line number: an unrelated edit above a
# baselined call shifts its line and would otherwise read as a brand-new
# violation. Keying on the line number is how a guard becomes noise and then
# becomes ignored.
key() { sed -E 's/^([^:]+):[0-9]+:(.*)$/\1\t\2/' | sed '/^$/d' | sort; }
new=$(comm -23 <(printf '%s\n' "$found" | key) <(key < "$BASELINE"))

if [ -n "$new" ]; then
	echo "NEW direct Provider.Stop call(s) outside the termination seam:"
	printf '%s\n' "$new" | sed 's/^/  /'
	cat <<'WHY'

Every session ending must be countable, including the good one (ga-ksac39).
Route this through runtime.StopRecorded(p, name, runtime.Termination{...}) and
say WHY the session is ending. If you genuinely cannot classify it, pass
Kind: runtime.KindUnclassified — that is counted and budgeted, and it is the
honest answer. What it must not do is end silently.

If this call is genuinely not a session termination, add its receiver to the
exclusions in scripts/check-termination-seam.sh and say why in the commit.
WHY
	exit 1
fi
echo "termination seam: no new direct Provider.Stop calls"
exit 0
