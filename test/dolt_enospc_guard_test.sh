#!/usr/bin/env bash
# Test: the Dolt ENOSPC restart guard (examples/bd/assets/scripts/dolt-enospc.sh)
# and both of its callers: `gc dolt restart` (examples/bd/dolt/commands/restart/
# run.sh) and gc-beads-bd auto-recovery (op_recover in
# examples/bd/assets/scripts/gc-beads-bd.sh).
#
# The pre-fix guard refused on any ENOSPC line in the last 1000 log lines. With
# a quiet log those lines spanned 8 days, so week-old evidence blocked recovery
# on a healthy disk. The guard now measures the disk live and only counts log
# lines stamped inside a time window.
#
# Acceptance criteria (every case runs against a fake Dolt log and a stubbed df):
#   1. ENOSPC lines 8 days old + healthy disk -> does NOT refuse.
#      CONTROL: the pre-fix detector DOES refuse on the same fixture.
#   2. ENOSPC line 10 minutes old -> refuses, prints the count and timestamp.
#   3. No ENOSPC lines + free space below the minimum -> refuses, prints the
#      free-space reading.
#   4. ENOSPC line with an unparseable stamp -> refuses, says "unparseable".
#   5. No refusal output contains "--force".
#   6. Both callers act on the guard's verdict; gc-beads-bd without its sibling
#      helper fails closed instead of running a second copy of the detector.
#   7. Nothing that goes wrong inside the guard lets a restart through: an
#      invalid or overflowing setting, an unmeasurable disk, a NUL byte in the
#      log, and a here-document the shell cannot write all refuse.
#
# Run in CI by TestDoltENOSPCGuardShellHarness (examples/bd/dolt).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
HELPER="$REPO_ROOT/examples/bd/assets/scripts/dolt-enospc.sh"
BEADS_BD="$REPO_ROOT/examples/bd/assets/scripts/gc-beads-bd.sh"
DOLT_PACK="$REPO_ROOT/examples/bd/dolt"
RESTART="$DOLT_PACK/commands/restart/run.sh"
FAILED=0

pass() { printf '\033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }

for f in "$HELPER" "$BEADS_BD" "$RESTART"; do
    if [ ! -f "$f" ]; then
        printf 'ERROR: %s not found\n' "$f" >&2
        exit 1
    fi
done

# The guard's tunables must come from each case, never from the caller's shell.
unset GC_DOLT_RESTART_MIN_FREE_MB GC_DOLT_RESTART_ENOSPC_WINDOW_MIN
unset GC_DOLT_HOST GC_DOLT_DATA_DIR GC_DOLT_LOG_FILE GC_PACK_STATE_DIR GC_CITY_RUNTIME_DIR

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# Stub df: reports FAKE_DF_AVAIL_KB available. The mount point carries a space,
# and FAKE_DF_FS can give the filesystem name one too (an SMB share), so the
# harness proves the guard finds the Available column from the capacity field
# rather than by a fixed position. FAKE_DF_AVAIL_KB=fail reproduces a df that
# cannot measure.
STUB_BIN="$WORK/bin"
mkdir -p "$STUB_BIN"
cat > "$STUB_BIN/df" <<'EOF'
#!/bin/sh
[ "${FAKE_DF_AVAIL_KB:?}" = fail ] && exit 1
printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n'
printf '%s 104857600 1000 %s 1%% /Volumes/Fake Disk\n' "${FAKE_DF_FS:-/dev/fake}" "$FAKE_DF_AVAIL_KB"
EOF
chmod +x "$STUB_BIN/df"

HEALTHY_KB=$((50 * 1024 * 1024)) # 50 GB
LOW_KB=$((512 * 1024))           # 512 MB, under the 1024 MB default
DATA_DIR_FIXTURE="$WORK/city/.beads/dolt"
mkdir -p "$DATA_DIR_FIXTURE"

NOW=$(date +%s)

# stamp <epoch>: Dolt's logrus stamp, local time with a colon offset
# (time="2026-09-18T07:06:15-07:00").
stamp() {
    local raw
    raw=$(TZ=America/Los_Angeles date -d "@$1" '+%Y-%m-%dT%H:%M:%S%z' 2>/dev/null \
        || TZ=America/Los_Angeles date -r "$1" '+%Y-%m-%dT%H:%M:%S%z')
    printf '%s:%s\n' "${raw%??}" "${raw: -2}"
}

# stamp_utc_frac <epoch>: the RFC3339Nano form with a Z suffix.
stamp_utc_frac() {
    local raw
    raw=$(date -u -d "@$1" '+%Y-%m-%dT%H:%M:%S' 2>/dev/null || date -u -r "$1" '+%Y-%m-%dT%H:%M:%S')
    printf '%s.123456789Z\n' "$raw"
}

enospc_line() {
    printf 'time="%s" level=error msg="error writing chunk" error="write /data/.dolt/noms/journal.idx: no space left on device"\n' "$1"
}

quiet_line() {
    printf 'time="%s" level=info msg="Server ready. Accepting connections."\n' "$1"
}

# write_log <name> <line>... : a fake Dolt log under $WORK, path on stdout.
write_log() {
    local path="$WORK/logs/$1.log"
    shift
    mkdir -p "$WORK/logs"
    printf '%s\n' "$@" > "$path"
    printf '%s\n' "$path"
}

EIGHT_DAYS_AGO=$((NOW - 8 * 86400))
TEN_MIN_AGO=$((NOW - 600))
TWENTY_MIN_AGO=$((NOW - 1200))

LOG_OLD=$(write_log old \
    "$(enospc_line "$(stamp "$EIGHT_DAYS_AGO")")" \
    "$(enospc_line "$(stamp $((EIGHT_DAYS_AGO + 1)))")" \
    "$(quiet_line "$(stamp $((NOW - 3600 * 5)))")")
LOG_RECENT=$(write_log recent \
    "$(enospc_line "$(stamp "$EIGHT_DAYS_AGO")")" \
    "$(enospc_line "$(stamp "$TEN_MIN_AGO")")")
LOG_NONE=$(write_log none "$(quiet_line "$(stamp "$TEN_MIN_AGO")")")
LOG_UNPARSEABLE=$(write_log unparseable \
    "$(enospc_line yesterday)" \
    "fatal: no space left on device")
LOG_MANY_RECENT=$(write_log many-recent \
    "$(enospc_line "$(stamp "$EIGHT_DAYS_AGO")")" \
    "$(enospc_line "$(stamp "$TWENTY_MIN_AGO")")" \
    "$(enospc_line "$(stamp "$TEN_MIN_AGO")")" \
    "$(enospc_line "$(stamp "$TEN_MIN_AGO")")")
LOG_UTC_FRAC=$(write_log utc-frac "$(enospc_line "$(stamp_utc_frac "$TEN_MIN_AGO")")")
NOW_STAMP=$(stamp "$NOW")
{
    enospc_line "$(stamp "$TEN_MIN_AGO")"
    for _ in $(seq 1 1001); do quiet_line "$NOW_STAMP"; done
} > "$WORK/logs/buried.log"
LOG_BURIED="$WORK/logs/buried.log"
# A NUL byte makes grep treat the log as binary: GNU grep then hides the lines
# after it and BSD grep reports one phantom match.
{
    enospc_line "$(stamp "$EIGHT_DAYS_AGO")"
    printf 'time="%s" level=warning msg="torn write \000 recovered"\n' "$NOW_STAMP"
} > "$WORK/logs/nul-old.log"
LOG_NUL_OLD="$WORK/logs/nul-old.log"
{
    printf 'time="%s" level=warning msg="torn write \000 recovered"\n' "$NOW_STAMP"
    enospc_line "$(stamp "$TEN_MIN_AGO")"
} > "$WORK/logs/nul-recent.log"
LOG_NUL_RECENT="$WORK/logs/nul-recent.log"

# run_guard <log> <avail_kb> [VAR=value...]: the helper alone under POSIX sh.
# Prints VERDICT=refuse|proceed, then the reason and detail lines.
run_guard() {
    local log="$1" avail="$2"
    shift 2
    # shellcheck disable=SC2016 # the child sh expands these, not this shell
    env PATH="$STUB_BIN:$PATH" FAKE_DF_AVAIL_KB="$avail" \
        LOG_FILE="$log" DATA_DIR="$DATA_DIR_FIXTURE" "$@" \
        sh -c '
            . "$1"
            if recovery_should_skip_due_to_enospc; then
                echo "VERDICT=refuse"
            else
                echo "VERDICT=proceed"
            fi
            echo "REASON=$ENOSPC_REFUSAL_REASON"
            printf "%s\n" "$ENOSPC_REFUSAL_DETAIL"
        ' guard "$HELPER"
}

# old_detector <log>: the pre-fix detector, verbatim from dolt-enospc.sh before
# this change. It is the CONTROL: the fixture that the new guard passes must
# still trip it, or the fixture proves nothing.
old_detector() {
    LOG_FILE="$1" sh -c '
recovery_should_skip_due_to_enospc() {
    [ -n "${LOG_FILE:-}" ] && [ -r "$LOG_FILE" ] || return 1
    tail -n 1000 "$LOG_FILE" 2>/dev/null \
        | grep -qE "no space left on device|copy_file_range:.*no space|ENOSPC" \
        || return 1
    return 0
}
recovery_should_skip_due_to_enospc'
}

# recorded_ops <file>: the ops a fake backend recorded, on one line. No file
# means nothing reached the backend.
recorded_ops() {
    if [ -f "$1" ]; then
        tr '\n' ' ' < "$1"
    fi
}

# run_restart <log> <avail_kb> [args...]: gc dolt restart with a fake
# gc-beads-bd that records ops. The fake sits in a dir without the helper, so
# run.sh resolves the real helper from the bd pack, as it does in production.
# Prints RC=<code>, OPS=<recorded ops>, then the command's output.
run_restart() {
    local log="$1" avail="$2"
    shift 2
    local city="$WORK/restart-city"
    rm -rf "$city"
    mkdir -p "$city/.gc/scripts"
    cat > "$city/.gc/scripts/gc-beads-bd.sh" <<EOF
#!/bin/sh
printf '%s\n' "\$1" >> "$city/ops.log"
exit 0
EOF
    chmod +x "$city/.gc/scripts/gc-beads-bd.sh"
    local rc=0 out
    out=$(env PATH="$STUB_BIN:$PATH" FAKE_DF_AVAIL_KB="$avail" \
        GC_CITY_PATH="$city" GC_PACK_DIR="$DOLT_PACK" \
        GC_DOLT_LOG_FILE="$log" GC_DOLT_DATA_DIR="$DATA_DIR_FIXTURE" \
        sh "$RESTART" "$@" 2>&1) || rc=$?
    printf 'RC=%s\nOPS=%s\n%s\n' "$rc" "$(recorded_ops "$city/ops.log")" "$out"
}

# run_recover <log> <avail_kb> [with-helper|without-helper]: gc-beads-bd
# op_recover from the real script's prelude, with the recovery backend stubbed
# to record that it ran. with-helper places the real dolt-enospc.sh beside the
# prelude, as the pack ships it; without-helper exercises the fail-closed
# fallback. Prints RC=<code>, OPS=<recorded ops>, then the output.
run_recover() {
    local log="$1" avail="$2" layout="${3:-with-helper}"
    local dir="$WORK/recover-$layout"
    rm -rf "$dir"
    mkdir -p "$dir"
    if [ "$layout" = with-helper ]; then
        cp "$HELPER" "$dir/dolt-enospc.sh"
    fi
    awk '/^# --- Main ---$/ { exit } { print }' "$BEADS_BD" > "$dir/harness.sh"
    cat >> "$dir/harness.sh" <<EOF
is_remote() { return 1; }
load_recover_managed_from_gc() {
    printf 'recover_managed\n' >> "$dir/ops.log"
    GC_RECOVER_PORT=3311
    GC_RECOVER_DIAGNOSED_READ_ONLY=false
    return 0
}
LOG_FILE="$log"
DATA_DIR="$DATA_DIR_FIXTURE"
op_recover
EOF
    local rc=0 out
    out=$(env PATH="$STUB_BIN:$PATH" FAKE_DF_AVAIL_KB="$avail" sh "$dir/harness.sh" 2>&1) || rc=$?
    printf 'RC=%s\nOPS=%s\n%s\n' "$rc" "$(recorded_ops "$dir/ops.log")" "$out"
}

has() { printf '%s' "$1" | grep -qF -- "$2"; }

# rc_is <output> <code>: the exit code a run_restart/run_recover call recorded.
rc_is() { [ "$(printf '%s\n' "$1" | sed -n 's/^RC=//p')" = "$2" ]; }

# ops_are <output> <ops>: the recorded backend ops, exactly.
ops_are() { [ "$(printf '%s\n' "$1" | sed -n 's/^OPS=//p' | sed 's/ *$//')" = "$2" ]; }
# no_ops <output>: nothing reached the backend.
no_ops() { ops_are "$1" ""; }

# check_no_force <case> <output>: criterion 5, applied to every refusal.
check_no_force() {
    if has "$2" "--force"; then
        fail "$1: refusal output mentions --force: $2"
    fi
}

OLD_STAMP=$(stamp "$EIGHT_DAYS_AGO")
TEN_MIN_STAMP=$(stamp "$TEN_MIN_AGO")
TWENTY_MIN_STAMP=$(stamp "$TWENTY_MIN_AGO")

# --- The helper alone ---

# T1: 8-day-old ENOSPC + healthy disk -> proceed; the CONTROL still refuses.
out=$(run_guard "$LOG_OLD" "$HEALTHY_KB")
if has "$out" "VERDICT=proceed"; then
    pass "T1: ENOSPC lines 8 days old + healthy disk -> guard does not refuse"
else
    fail "T1: ENOSPC lines 8 days old + healthy disk -> expected proceed; got: $out"
fi
if old_detector "$LOG_OLD"; then
    pass "T1 CONTROL: pre-fix detector refuses on the same 8-day-old fixture"
else
    fail "T1 CONTROL: pre-fix detector did not refuse, so the fixture proves nothing"
fi

# T2: 10-minute-old ENOSPC -> refuse with the count and the stamp.
out=$(run_guard "$LOG_RECENT" "$HEALTHY_KB")
if has "$out" "VERDICT=refuse" \
    && has "$out" "ENOSPC log lines stamped within the last 60 min: 1 (newest $TEN_MIN_STAMP, " \
    && has "$out" "; oldest $TEN_MIN_STAMP, " && has "$out" " min ago)" \
    && has "$out" "live free space: 51200 MB"; then
    pass "T2: ENOSPC line 10 min old -> refuses, prints count, stamp and free space"
else
    fail "T2: ENOSPC line 10 min old -> expected refusal with count+stamp; got: $out"
fi
check_no_force T2 "$out"

# T3: no ENOSPC + low disk -> refuse with the free-space reading.
out=$(run_guard "$LOG_NONE" "$LOW_KB")
if has "$out" "VERDICT=refuse" \
    && has "$out" "Dolt data volume has 512 MB free, below the 1024 MB minimum" \
    && has "$out" "live free space: 512 MB on the volume holding $DATA_DIR_FIXTURE (minimum 1024 MB)" \
    && has "$out" "ENOSPC log lines stamped within the last 60 min: 0"; then
    pass "T3: no ENOSPC lines + free space below minimum -> refuses, prints free space"
else
    fail "T3: no ENOSPC + low disk -> expected refusal with free-space reading; got: $out"
fi
check_no_force T3 "$out"

# T4: unparseable and missing stamps count as recent and say so.
out=$(run_guard "$LOG_UNPARSEABLE" "$HEALTHY_KB")
if has "$out" "VERDICT=refuse" \
    && has "$out" "2 ENOSPC line(s) with an unparseable timestamp" \
    && has "$out" "ENOSPC log lines with an unparseable timestamp, counted as recent: 2"; then
    pass "T4: ENOSPC lines with unparseable/missing stamps -> refuses, says unparseable"
else
    fail "T4: unparseable stamp -> expected refusal naming unparseable; got: $out"
fi
check_no_force T4 "$out"

# T5: several blocking lines -> count all of them, newest and oldest by time,
# and ignore the 8-day-old line.
out=$(run_guard "$LOG_MANY_RECENT" "$HEALTHY_KB")
if has "$out" "VERDICT=refuse" \
    && has "$out" "within the last 60 min: 3 (newest $TEN_MIN_STAMP, " \
    && has "$out" "; oldest $TWENTY_MIN_STAMP, " \
    && ! has "$out" "$OLD_STAMP"; then
    pass "T5: 3 recent ENOSPC lines + 1 stale -> counts 3, newest/oldest correct"
else
    fail "T5: expected count 3 with newest/oldest; got: $out"
fi
check_no_force T5 "$out"

# T6: the RFC3339Nano Z form parses as a real stamp, not as unparseable.
out=$(run_guard "$LOG_UTC_FRAC" "$HEALTHY_KB")
if has "$out" "VERDICT=refuse" && has "$out" "within the last 60 min: 1" && ! has "$out" "unparseable"; then
    pass "T6: Z-suffixed fractional stamp parses and counts as recent"
else
    fail "T6: Z/fractional stamp -> expected a parsed recent line; got: $out"
fi

# T7: the window and minimum are tunable; 0 disables the live check.
out=$(run_guard "$LOG_RECENT" "$HEALTHY_KB" GC_DOLT_RESTART_ENOSPC_WINDOW_MIN=5)
if has "$out" "VERDICT=proceed"; then
    pass "T7: 10-min-old ENOSPC with a 5-min window -> proceeds"
else
    fail "T7: 5-min window -> expected proceed; got: $out"
fi
out=$(run_guard "$LOG_NONE" "$LOW_KB" GC_DOLT_RESTART_MIN_FREE_MB=0)
if has "$out" "VERDICT=proceed"; then
    pass "T7: GC_DOLT_RESTART_MIN_FREE_MB=0 disables the live disk check"
else
    fail "T7: MIN_FREE_MB=0 -> expected proceed; got: $out"
fi

# T8: an unusable tunable or an unmeasurable disk refuses and says why.
out=$(run_guard "$LOG_NONE" "$HEALTHY_KB" GC_DOLT_RESTART_MIN_FREE_MB=2G)
if has "$out" "VERDICT=refuse" && has "$out" "invalid GC_DOLT_RESTART_MIN_FREE_MB=2G" \
    && has "$out" "live free space: 51200 MB"; then
    pass "T8: invalid GC_DOLT_RESTART_MIN_FREE_MB -> refuses, names it, still reads the disk"
else
    fail "T8: invalid tunable -> expected refusal naming it with the free-space reading; got: $out"
fi
check_no_force T8 "$out"
out=$(run_guard "$LOG_NONE" fail)
if has "$out" "VERDICT=refuse" && has "$out" "live free space: unmeasurable"; then
    pass "T8: df cannot measure the data volume -> refuses, says unmeasurable"
else
    fail "T8: df failure -> expected refusal; got: $out"
fi
check_no_force T8 "$out"

# T9: the 1000-line tail still bounds the work; a line past it is not read.
out=$(run_guard "$LOG_BURIED" "$HEALTHY_KB")
if has "$out" "VERDICT=proceed"; then
    pass "T9: recent ENOSPC buried past the 1000-line tail -> not read"
else
    fail "T9: buried line -> expected proceed; got: $out"
fi

# T10: no log file and a healthy disk -> proceed.
out=$(run_guard "$WORK/logs/absent.log" "$HEALTHY_KB")
if has "$out" "VERDICT=proceed"; then
    pass "T10: no Dolt log + healthy disk -> proceeds"
else
    fail "T10: absent log -> expected proceed; got: $out"
fi

# T11: settings too long for shell arithmetic refuse instead of erroring open.
out=$(run_guard "$LOG_NONE" "$HEALTHY_KB" GC_DOLT_RESTART_MIN_FREE_MB=99999999999999999999)
if has "$out" "VERDICT=refuse" && has "$out" "invalid GC_DOLT_RESTART_MIN_FREE_MB=99999999999999999999"; then
    pass "T11: 20-digit GC_DOLT_RESTART_MIN_FREE_MB -> refuses, names the setting"
else
    fail "T11: overflowing minimum -> expected refusal; got: $out"
fi
check_no_force T11 "$out"
out=$(run_guard "$LOG_RECENT" "$HEALTHY_KB" GC_DOLT_RESTART_ENOSPC_WINDOW_MIN=300000000000000000)
if has "$out" "VERDICT=refuse" && has "$out" "invalid GC_DOLT_RESTART_ENOSPC_WINDOW_MIN=300000000000000000" \
    && has "$out" "live free space: 51200 MB"; then
    pass "T11: overflowing GC_DOLT_RESTART_ENOSPC_WINDOW_MIN -> refuses, still reads the disk"
else
    fail "T11: overflowing window -> expected refusal; got: $out"
fi
check_no_force T11 "$out"

# T12: a NUL byte in the log neither hides a recent line nor invents one.
out=$(run_guard "$LOG_NUL_OLD" "$HEALTHY_KB")
if has "$out" "VERDICT=proceed"; then
    pass "T12: NUL byte + only 8-day-old ENOSPC -> proceeds (no phantom match)"
else
    fail "T12: NUL byte + stale ENOSPC -> expected proceed; got: $out"
fi
out=$(run_guard "$LOG_NUL_RECENT" "$HEALTHY_KB")
if has "$out" "VERDICT=refuse" && has "$out" "within the last 60 min: 1 (newest $TEN_MIN_STAMP, "; then
    pass "T12: NUL byte before a 10-min-old ENOSPC -> still refuses on it"
else
    fail "T12: NUL byte + recent ENOSPC -> expected refusal; got: $out"
fi

# T13: a filesystem name with a space does not shift the Available column.
out=$(run_guard "$LOG_NONE" "$LOW_KB" FAKE_DF_FS="//user@nas/dolt share")
if has "$out" "VERDICT=refuse" && has "$out" "live free space: 512 MB"; then
    pass "T13: spaced filesystem name -> reads the Available column correctly"
else
    fail "T13: spaced filesystem name -> expected a 512 MB reading; got: $out"
fi

# T14: a here-document the shell cannot write fails closed. Shells that spool
# here-documents to a temp file (bash 3.2, macOS /bin/sh) hit the write error;
# shells that use a pipe read the line and refuse on it. Both must refuse.
# shellcheck disable=SC2016 # the child sh expands these, not this shell
out=$(env PATH="$STUB_BIN:$PATH" FAKE_DF_AVAIL_KB="$HEALTHY_KB" \
    LOG_FILE="$LOG_RECENT" DATA_DIR="$DATA_DIR_FIXTURE" \
    sh -c '
        trap "" XFSZ
        ulimit -f 0
        . "$1"
        if recovery_should_skip_due_to_enospc; then
            echo "VERDICT=refuse"
        else
            echo "VERDICT=proceed"
        fi
        printf "%s\n" "$ENOSPC_REFUSAL_DETAIL"
    ' guard "$HELPER" 2>&1) || true
if has "$out" "VERDICT=refuse"; then
    pass "T14: here-document write failure -> still refuses"
else
    fail "T14: here-document write failure -> expected refusal; got: $out"
fi

# --- Caller 1: gc dolt restart ---

out=$(run_restart "$LOG_OLD" "$HEALTHY_KB")
if rc_is "$out" 0 && ops_are "$out" "stop start"; then
    pass "R1: restart with 8-day-old ENOSPC + healthy disk -> stops and starts"
else
    fail "R1: restart with stale ENOSPC -> expected stop start; got: $out"
fi

out=$(run_restart "$LOG_RECENT" "$HEALTHY_KB")
if rc_is "$out" 1 && no_ops "$out" \
    && has "$out" "gc dolt restart: refusing restart: Dolt log shows 1 ENOSPC line(s) stamped within the last 60 min" \
    && has "$out" "newest $TEN_MIN_STAMP" && has "$out" "live free space: 51200 MB"; then
    pass "R2: restart with 10-min-old ENOSPC -> refuses before stop, prints evidence"
else
    fail "R2: restart with recent ENOSPC -> expected refusal and no ops; got: $out"
fi
check_no_force R2 "$out"

out=$(run_restart "$LOG_NONE" "$LOW_KB")
if rc_is "$out" 1 && no_ops "$out" && has "$out" "live free space: 512 MB"; then
    pass "R3: restart with low disk -> refuses before stop, prints free space"
else
    fail "R3: restart with low disk -> expected refusal; got: $out"
fi
check_no_force R3 "$out"

out=$(run_restart "$LOG_UNPARSEABLE" "$HEALTHY_KB")
if rc_is "$out" 1 && no_ops "$out" && has "$out" "unparseable timestamp"; then
    pass "R4: restart with unparseable ENOSPC stamp -> refuses, says unparseable"
else
    fail "R4: restart with unparseable stamp -> expected refusal; got: $out"
fi
check_no_force R4 "$out"

out=$(run_restart "$LOG_RECENT" "$HEALTHY_KB" --force)
if rc_is "$out" 0 && ops_are "$out" "stop start" && has "$out" "--force set; restarting despite the ENOSPC guard"; then
    pass "R5: restart --force with recent ENOSPC -> operator override still restarts"
else
    fail "R5: restart --force -> expected stop start; got: $out"
fi

# --- Caller 2: gc-beads-bd op_recover (auto-recovery) ---

out=$(run_recover "$LOG_OLD" "$HEALTHY_KB")
if rc_is "$out" 0 && ops_are "$out" "recover_managed"; then
    pass "A1: auto-recovery with 8-day-old ENOSPC + healthy disk -> recovers"
else
    fail "A1: auto-recovery with stale ENOSPC -> expected recovery; got: $out"
fi

out=$(run_recover "$LOG_RECENT" "$HEALTHY_KB")
if rc_is "$out" 1 && no_ops "$out" \
    && has "$out" "skipping dolt recovery: Dolt log shows 1 ENOSPC line(s) stamped within the last 60 min" \
    && has "$out" "newest $TEN_MIN_STAMP" && has "$out" "live free space: 51200 MB" \
    && has "$out" "dolt recovery skipped: ENOSPC guard"; then
    pass "A2: auto-recovery with 10-min-old ENOSPC -> skipped, prints evidence"
else
    fail "A2: auto-recovery with recent ENOSPC -> expected skip; got: $out"
fi
check_no_force A2 "$out"

out=$(run_recover "$LOG_NONE" "$LOW_KB")
if rc_is "$out" 1 && no_ops "$out" && has "$out" "live free space: 512 MB"; then
    pass "A3: auto-recovery with low disk -> skipped, prints free space"
else
    fail "A3: auto-recovery with low disk -> expected skip; got: $out"
fi
check_no_force A3 "$out"

out=$(run_recover "$LOG_UNPARSEABLE" "$HEALTHY_KB")
if rc_is "$out" 1 && no_ops "$out" && has "$out" "unparseable timestamp"; then
    pass "A4: auto-recovery with unparseable ENOSPC stamp -> skipped, says unparseable"
else
    fail "A4: auto-recovery with unparseable stamp -> expected skip; got: $out"
fi
check_no_force A4 "$out"

out=$(run_recover "$LOG_OLD" "$HEALTHY_KB" without-helper)
if rc_is "$out" 1 && no_ops "$out" && has "$out" "ENOSPC guard helper not found"; then
    pass "A5: gc-beads-bd without its sibling helper -> recovery fails closed"
else
    fail "A5: missing helper -> expected fail-closed skip; got: $out"
fi
check_no_force A5 "$out"

[ "$FAILED" -eq 0 ] && exit 0 || exit 1
