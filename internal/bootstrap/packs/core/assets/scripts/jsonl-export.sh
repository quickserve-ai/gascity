#!/usr/bin/env bash
# jsonl-export — export every bead scope to JSONL and push to a git archive.
#
# Core exec order. All operations are deterministic: `gc bd export` per scope
# (the city and each rig, reached through `gc bd`, so bd picks the transport),
# jq record-count comparisons against the spike threshold, git
# add/commit/push. No LLM judgment needed.
#
# The archive holds bd's native export format, one issue per line with its
# labels, dependencies and comments embedded, so a snapshot restores with
# `gc bd import <file>`.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

# When this run began, which is when the order's timeout started counting,
# and an id for this run's repack attempt that no other run shares, even one
# started in the same second (see begin_archive_repack).
RUN_STARTED_AT="$(date +%s)"
REPACK_ATTEMPT_ID="$RUN_STARTED_AT-$$-$RANDOM"

CITY="${GC_CITY_PATH:-${GC_CITY:-.}}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# CITY_ABS is read by scope_bd.sh.
# shellcheck disable=SC2034
CITY_ABS="$(cd "$CITY" 2>/dev/null && pwd -P || printf '%s\n' "$CITY")"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/scope_bd.sh"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/order_outcome.sh"

# jq is a hard dependency: count_jsonl_rows below relies on it, and a missing
# jq would silently zero every record count and could mask spikes on a stale
# baseline. Fail loud at startup instead.
if ! command -v jq >/dev/null 2>&1; then
    echo "jsonl-export: jq is required but not found in PATH" >&2
    exit 1
fi
# Print the whole number in the environment variable named $1, or warn and
# print the default $2 when it is not one or falls outside $3..$4. A
# malformed override must not reach a shell comparison or a git -c setting,
# and the maximum keeps out a value git cannot take (pack.threads and
# gc.autoPackLimit are signed ints there), which would fail the repack
# instead of falling back.
jsonl_uint_knob() {
    local name="$1"
    local default="$2"
    local min="$3"
    local max="$4"
    local value="${!name:-$default}"
    case "$value" in
        ''|*[!0-9]*) ;;
        *)
            if [ "${#value}" -le 18 ] && [ "$((10#$value))" -ge "$min" ] && [ "$((10#$value))" -le "$max" ]; then
                printf '%s\n' "$((10#$value))"
                return
            fi
            ;;
    esac
    echo "jsonl-export: ignoring $name=$value (want a whole number from $min to $max); using $default" >&2
    printf '%s\n' "$default"
}

# Print the bytes in the git size $1 (a whole number with no leading zero and
# an optional k, m or g, as git reads it), or fail when it is not one. Nine
# digits keep the product inside bash's 64-bit arithmetic.
jsonl_git_size_bytes() {
    local factor=1
    [[ "$1" =~ ^([1-9][0-9]{0,8})([kKmMgG]?)$ ]] || return 1
    case "${BASH_REMATCH[2]}" in
        k|K) factor=1024 ;;
        m|M) factor=1048576 ;;
        g|G) factor=1073741824 ;;
    esac
    printf '%s\n' "$((BASH_REMATCH[1] * factor))"
}

# Print the git size ("512m", "4g") in the environment variable named $1, or
# warn and print the default $2 when it is not one or falls outside the git
# sizes $3..$4.
jsonl_git_size_knob() {
    local name="$1"
    local default="$2"
    local min="$3"
    local max="$4"
    local value="${!name:-$default}"
    local bytes
    if bytes=$(jsonl_git_size_bytes "$value") \
        && [ "$bytes" -ge "$(jsonl_git_size_bytes "$min")" ] \
        && [ "$bytes" -le "$(jsonl_git_size_bytes "$max")" ]; then
        printf '%s\n' "$value"
        return
    fi
    echo "jsonl-export: ignoring $name=$value (want a git size from $min to $max); using $default" >&2
    printf '%s\n' "$default"
}

PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/core}"
LEGACY_PACK_STATE_DIR="${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/maintenance"
LEGACY_PACK_ARCHIVE_REPO="$LEGACY_PACK_STATE_DIR/jsonl-archive"
LEGACY_ARCHIVE_REPO="$CITY/.gc/jsonl-archive"
LEGACY_STATE_FILE="$CITY/.gc/jsonl-export-state.json"

# Configurable via environment (defaults match the old formula).
SPIKE_THRESHOLD="${GC_JSONL_SPIKE_THRESHOLD:-20}"  # percentage (0-100)
# Skip the percentage spike check when the previous record count is below
# this absolute floor — small-N percentages are noise. A fresh bead store
# growing 10→309 records during stand-up legitimately trips a 20% delta on
# every cycle; raising the floor to 100 keeps the check meaningful on real
# data while suppressing stand-up flares. Set to 0 to disable.
MIN_PREV_FOR_SPIKE_CHECK="${GC_JSONL_MIN_PREV_FOR_SPIKE:-100}"
MAX_PUSH_FAILURES="${GC_JSONL_MAX_PUSH_FAILURES:-3}"
MAX_REPACK_FAILURES="${GC_JSONL_MAX_REPACK_FAILURES:-3}"
# Each knob below has a maximum far past any useful setting and inside what
# git and bash arithmetic take; a value above it warns and falls back.
# A million loose objects is far past git's own 6700-object trigger.
REPACK_LOOSE_CEILING="$(jsonl_uint_knob GC_JSONL_REPACK_LOOSE_CEILING 512 0 1000000)"
# Loose objects are packed above this many KiB on disk (count-objects -v
# "size:") as well as above REPACK_LOOSE_CEILING objects. Each snapshot adds
# one full-size blob per store, so a large store crosses any byte budget far
# below any object count: 512 loose snapshots of a 961 MB store are ~100 GiB
# of zlib. 512 MiB is two or three such snapshots between repacks; the
# maximum is 1 TiB.
REPACK_LOOSE_KIB_CEILING="$(jsonl_uint_knob GC_JSONL_REPACK_LOOSE_KIB_CEILING 524288 0 1073741824)"
# Delta compression for large exports. git never delta-compresses a blob
# above core.bigFileThreshold (default 512 MiB): `git add` streams it whole
# into a pack of its own, and pack-objects never tries it as a delta. A store
# past that size costs a full copy per snapshot (gc-kt7i: 247 copies of one
# 961 MB store were 45.97 GiB of a 46.23 GiB archive, where two consecutive
# copies pack as a 202 MB base and a 27 KB delta). The threshold is raised on
# every archive git call that writes or packs objects (ARCHIVE_PACK_CONFIG
# below); a command-line -c outranks the archive's own config. An export
# above it is still stored whole, so the run warns about each one before
# staging. git reads it as an unsigned long. The minimum is git's own 512m
# (lower would deltify less than git does); the maximum, 64g, is far above
# any blob worth a delta.
ARCHIVE_BIG_FILE_THRESHOLD="$(jsonl_git_size_knob GC_JSONL_BIG_FILE_THRESHOLD 4g 512m 64g)"
# The delta search's window, which sets its memory. Objects reach the window
# sorted by path and then size, so a store's consecutive snapshots are
# neighbours. pack.window=2 keeps up to two predecessors to try each object
# against (pack-objects hands find_deltas window+1 slots,
# builtin/pack-objects.c:3355, git 2.50.1) instead of git's ten.
# pack.windowMemory is per thread, and git never stores a blob whole for
# lack of it: find_deltas evicts candidates only `while (mem_usage >
# window_memory_limit && count > 1)` (:2913), so it narrows the window to
# one predecessor and still compares. Peak memory is about windowMemory plus
# 2.5 times the largest blob (the blob, its candidate and the candidate's
# delta index); with a window of two at 2g it is about 3.5 times the largest
# blob, ~3.4 GB for a 961 MB export, on one thread (two threads hold two
# windows). The window is fixed because that bound rests on it; the window
# memory is a git size from 256m to 256g (an unsigned long in git).
ARCHIVE_PACK_WINDOW=2
ARCHIVE_PACK_WINDOW_MEMORY="$(jsonl_git_size_knob GC_JSONL_PACK_WINDOW_MEMORY 2g 256m 256g)"
# pack.threads is a signed int in git; maximum 64.
ARCHIVE_PACK_THREADS="$(jsonl_uint_knob GC_JSONL_PACK_THREADS 1 1 64)"
# gc --auto folds the archive into one pack when its packs exceed this many
# (0 leaves the pack count unchecked, as in git). Every incremental repack
# writes one pack holding a whole copy of each store it touches, so in steady
# state the limit bounds both the whole copies on disk between
# consolidations and the delta search a consolidation runs. git's default of
# 50 puts about 50 whole copies of a 961 MB store into one search, which nears
# the order's timeout. It is a trigger, not a cap on the work (see the gc
# --auto call). gc.autoPackLimit is a signed int in git; maximum 10000.
REPACK_PACK_LIMIT="$(jsonl_uint_knob GC_JSONL_REPACK_PACK_LIMIT 10 0 10000)"
# The order's timeout (orders/jsonl-export.toml, timeout = "30m", kept equal
# by a test): the controller kills a run that reaches it. A repack marker
# older than this means its run died mid-repack (begin_archive_repack), and
# no run lives longer, which bounds the prune grace below.
JSONL_ORDER_TIMEOUT_SECONDS=1800
# The explicit repack in repack_archive_objects prunes unreachable loose
# objects older than this many minutes (the reasoning is at the prune). The
# minimum, 35, is above the order's 30-minute timeout: an object younger
# than the grace may belong to a run still alive, and only the timeout
# bounds how long one lives. Maximum one week.
ARCHIVE_PRUNE_GRACE_MINUTES="$(jsonl_uint_knob GC_JSONL_PRUNE_GRACE_MINUTES 60 35 10080)"
# Every archive git call that writes or packs objects carries these: add,
# commit, gc, repack, prune, fetch, rebase and push. gc --auto adds its
# trigger settings in repack_archive_objects. None of them reaches the
# remote: over ssh or https it is another machine, and for a local bare
# origin git clears GIT_CONFIG_PARAMETERS with the rest of local_repo_env
# (connect.c). A remote that repacks with delta reuse keeps the deltas it
# received; core.bigFileThreshold in the remote's own config matters only
# for the delta searches it runs itself.
#
# An archive that already holds whole copies in ONE pack keeps them: git never
# retries two whole objects of the same pack as a delta pair without
# --no-reuse-delta. Repair it once, by hand, never from this order (on the
# gc-kt7i host: 46.42 GiB to 916 MiB in 48 minutes at 9.8 GB RSS):
#   git -C <archive> -c core.bigFileThreshold=4g repack -a -d -l -f \
#       --window=2 --depth=10 --threads=2 --window-memory=8g --no-write-bitmap-index
ARCHIVE_PACK_CONFIG=(
    -c "core.bigFileThreshold=$ARCHIVE_BIG_FILE_THRESHOLD"
    -c "pack.threads=$ARCHIVE_PACK_THREADS"
    -c "pack.windowMemory=$ARCHIVE_PACK_WINDOW_MEMORY"
    -c "pack.window=$ARCHIVE_PACK_WINDOW"
)
# Only the script's own gc --auto step (and the explicit repack behind it)
# may pack the archive, inside the run. commit, fetch and rebase run git's
# `maintenance run --auto` when they finish (builtin/commit.c:1935,
# builtin/fetch.c:2679, builtin/rebase.c:566 in git 2.50.1), which detaches
# by default: a gc it starts inherits the pack settings above and outlives
# the order's kill of the run's process group. maintenance.auto=false stops
# that call (run-command.c:1820), and gc.auto=0 stops any gc --auto it
# reaches anyway: need_to_gc returns when gc.auto <= 0, before its
# pack-count check (builtin/gc.c:635). push carries both too.
ARCHIVE_NO_AUTO_GC=(
    -c gc.auto=0
    -c maintenance.auto=false
)
# An escalation suppresses repeats for this long, then re-alerts. Bounding the
# silence by TIME, not by a marker, means a stale marker (a clear that failed
# to persist) can never mute a later streak for more than this window.
REPACK_REESCALATE_SECONDS="${GC_JSONL_REPACK_REESCALATE_SECONDS:-86400}"
PUSH_RETRY_DELAY_MIN="${GC_JSONL_PUSH_RETRY_DELAY_MIN:-1}"
PUSH_RETRY_DELAY_SPAN="${GC_JSONL_PUSH_RETRY_DELAY_SPAN:-4}"
SCRUB="${GC_JSONL_SCRUB:-true}"
ARCHIVE_REPO="${GC_JSONL_ARCHIVE_REPO:-$PACK_STATE_DIR/jsonl-archive}"
# Re-log the archive mode at least this often (seconds) even without a mode
# transition, so operators who missed the first line still see the current
# configuration. Default one week.
MODE_RELOG_INTERVAL_SECONDS="${GC_JSONL_MODE_RELOG_INTERVAL:-604800}"

# Cached archive mode ("push" or "local-only"). Resolved once on the first
# get_archive_mode call and reused thereafter so every push checkpoint in a
# single run sees a consistent value even if an operator adds or removes the
# origin remote mid-run.
ARCHIVE_MODE=""

resolve_escalate_script() {
    local candidate
    local pack
    local system_packs="${GC_SYSTEM_PACKS_DIR:-$CITY/.gc/system/packs}"

    if [ -n "${GC_ESCALATE_SCRIPT:-}" ]; then
        printf '%s\n' "$GC_ESCALATE_SCRIPT"
        return
    fi
    for pack in ${GC_ESCALATE_SEARCH_PACKS:-gastown maintenance bd core}; do
        candidate="$system_packs/$pack/assets/scripts/escalate.sh"
        if [ -x "$candidate" ]; then
            printf '%s\n' "$candidate"
            return
        fi
    done
    printf '%s\n' "$SCRIPT_DIR/escalate.sh"
}

ESCALATE_SCRIPT="$(resolve_escalate_script)"

maintenance_done() {
    local summary="$1${BIG_EXPORT_WARNINGS:+, warning: $BIG_EXPORT_WARNINGS}"
    local target="${GC_MAINTENANCE_DONE_TARGET:-}"

    [ -n "$target" ] || return 0
    gc session nudge "$target" "MAINTENANCE_DONE: $summary" 2>/dev/null || true
}

# Count issue records in an archived snapshot. Current snapshots are gc bd export
# JSONL (one record per line; memory records carry "_type":"memory" and are
# not issues). Snapshots written before the switch to gc bd export are a single
# `dolt sql -r json` object ({"rows":[...]}, or {} when empty); they are still
# read so the first run after the switch compares against the old baseline
# instead of tripping the spike check. Falls back to 0 on empty/missing/
# unparseable input; jq parse errors are forwarded to stderr so a corrupt
# archive surfaces in operator logs instead of being silently scored as zero.
count_jsonl_rows() {
    jq -s -r '
        if length == 0 then 0
        elif length == 1 and (.[0] | type) == "object" and ((.[0] | has("rows")) or .[0] == {}) then
            ((.[0].rows // []) | length)
        else
            map(select(type == "object" and ((._type // "issue") == "issue"))) | length
        end' || echo "0"
}

push_retry_delay_seconds() {
    awk -v seed="$RANDOM$$" -v min="$PUSH_RETRY_DELAY_MIN" -v span="$PUSH_RETRY_DELAY_SPAN" \
        'BEGIN{srand(seed); printf "%.2f", min + rand() * span}'
}

# select_archived_records filters a `gc bd export --all` stream down to what the
# archive keeps. `--all` is used because plain `gc bd export` also drops role
# beads and templates, which the archive has always carried. Always dropped:
# wisps-plane rows (ephemeral, or no-history wisps stamped "wisp_plane"),
# which never belonged to the durable issue archive. Memory records
# ("_type":"memory") are always kept. With scrub=true, issue records are also
# filtered by the archive's scrub rules: test pollution, system issue types
# (message, event, wisp, agent), gc:/order: system titles and sling
# auto-convoys. Every line must be a JSON object; anything else fails the
# scope rather than scoring as zero rows.
select_archived_records() {
    local scrub="$1"
    jq -c --arg scrub "$scrub" '
        if type != "object" then error("gc bd export line is not a JSON object") else . end
        | select(
            ((._type // "issue") == "memory") or (
                ((._type // "issue") == "issue")
                and ((.ephemeral // false) | not)
                and ((.wisp_plane // false) | not)
                and (
                    $scrub != "true" or (
                        ((.title // "") | test("^(Test Issue|test_)") | not) and
                        (
                            (
                                (.id // "") == "bd-1" or
                                (.id // "") == "bd-abc12" or
                                ((.id // "") | test("^(testdb_|beads_t)"))
                            ) | not
                        ) and
                        ((.issue_type // "") | test("^(message|event|wisp|agent)$") | not) and
                        ((.title // "") | test("^(gc:|order:)") | not) and
                        ((((.issue_type // "") == "convoy") and ((.title // "") | test("^sling-"))) | not)
                    )
                )
            )
        )
    '
}

normalize_pending_spike_alert_state() {
    jq -c '
        (.pending_spike_alerts //= {}) |
        if (.pending_spike_alert? | type) == "object" and ((.pending_spike_alert.database // "") != "") then
            .pending_spike_alerts[.pending_spike_alert.database] = (.pending_spike_alerts[.pending_spike_alert.database] // .pending_spike_alert)
        else
            .
        end |
        del(.pending_spike_alert) |
        if .pending_spike_alerts == {} then
            del(.pending_spike_alerts)
        else
            .
        end
    '
}

read_state_object() {
    local path="$1"

    jq -c '
        if type == "object" then
            .
        else
            error("state root must be a JSON object")
        end
    ' "$path" 2>/dev/null
}

read_state_json() {
    if [ -f "$STATE_FILE" ] && read_state_object "$STATE_FILE"; then
        return
    fi
    if [ -f "$STATE_FILE_BACKUP" ] && read_state_object "$STATE_FILE_BACKUP"; then
        if [ -f "$STATE_FILE" ]; then
            echo "jsonl-export: state file malformed; using last-known-good backup" >&2
        else
            echo "jsonl-export: state file missing; using last-known-good backup" >&2
        fi
        return
    fi
    if [ -f "$STATE_FILE" ]; then
        echo "jsonl-export: state file malformed; resetting to empty state" >&2
    fi
    echo '{}'
}

write_state_file_atomically() {
    local path="$1"
    local label="$2"
    local content="$3"
    local tmpfile

    if ! tmpfile=$(mktemp "${path}.tmp.XXXXXX"); then
        echo "jsonl-export: creating temporary $label failed" >&2
        return 1
    fi
    if ! printf '%s\n' "$content" > "$tmpfile"; then
        echo "jsonl-export: writing temporary $label failed" >&2
        rm -f "$tmpfile"
        return 1
    fi
    if ! mv -f "$tmpfile" "$path"; then
        echo "jsonl-export: replacing $label failed" >&2
        rm -f "$tmpfile"
        return 1
    fi
}

write_state_json() {
    if ! write_state_file_atomically "$STATE_FILE" "state file" "$1"; then
        return 1
    fi
    if ! write_state_file_atomically "$STATE_FILE_BACKUP" "state backup" "$1"; then
        echo "jsonl-export: state backup update failed; continuing with primary state only" >&2
    fi
}

set_consecutive_push_failures() {
    local count="$1"
    write_state_json "$(read_state_json | jq -c --argjson count "$count" '.consecutive_push_failures = $count')"
}

mark_push_failure_escalated() {
    write_state_json "$(read_state_json | jq -c '.push_failure_escalated = true')"
}

clear_push_failure_escalation() {
    write_state_json "$(read_state_json | jq -c 'del(.push_failure_escalated)')"
}

# Truncate push stderr before persisting it to state so the state file stays
# small regardless of how verbose git/network errors get. The head of the
# output is almost always the actionable message.
truncate_push_stderr_for_state() {
    local raw="$1"
    local max_bytes=512

    if [ -z "$raw" ]; then
        printf '%s' ""
        return
    fi
    printf '%s' "$raw" | LC_ALL=C awk -v max="$max_bytes" '
        BEGIN { total = 0 }
        {
            line = $0
            if (NR > 1) {
                line = "\n" line
            }
            len = length(line)
            if (total + len > max) {
                remaining = max - total
                if (remaining > 0) {
                    printf "%s", substr(line, 1, remaining)
                }
                printf "..."
                exit
            }
            printf "%s", line
            total += len
        }
    '
}

# Record a successful push in state so `gc doctor` can surface a timestamp for
# the archive health check. Clears any stale stderr from previous failures and
# any prior escalation marker so the next failure-cycle escalates fresh.
record_archive_push_success() {
    local now
    now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    write_state_json "$(
        read_state_json \
            | jq -c \
                --arg now "$now" \
                '.consecutive_push_failures = 0
                 | del(.pending_archive_push)
                 | del(.push_failure_escalated)
                 | .last_push_at = $now
                 | del(.last_push_stderr)'
    )"
}

set_pending_archive_push() {
    write_state_json "$(read_state_json | jq -c '.pending_archive_push = true')"
}

clear_pending_archive_push() {
    write_state_json "$(read_state_json | jq -c 'del(.pending_archive_push)')"
}

has_pending_archive_push() {
    [ "$(read_state_json | jq -r '.pending_archive_push // false')" = "true" ]
}

refresh_archive_remote_main() {
    git "${ARCHIVE_PACK_CONFIG[@]}" "${ARCHIVE_NO_AUTO_GC[@]}" fetch origin main -q 2>/dev/null
}

archive_has_local_only_commits_from_tracking() {
    local merge_base

    if ! git rev-parse --verify refs/remotes/origin/main >/dev/null 2>&1; then
        return 1
    fi
    merge_base=$(git merge-base refs/remotes/origin/main HEAD 2>/dev/null) || return 1
    [ "$(git rev-list --count "$merge_base..HEAD" 2>/dev/null || echo "0")" -gt 0 ]
}

archive_has_local_only_commits() {
    if refresh_archive_remote_main >/dev/null 2>&1; then
        archive_has_local_only_commits_from_tracking
        return
    fi
    if archive_has_local_only_commits_from_tracking; then
        echo "jsonl-export: fetch failed while checking deferred archive push; using existing origin/main tracking ref" >&2
        return 0
    fi
    return 1
}

# Detect the archive's push mode from the live state of its remotes rather
# than from a cached state field. Operators opt into off-box backup by adding
# an `origin` remote; removing it reverts to local-only on the next run with
# no extra command. The result is memoized in ARCHIVE_MODE on first call so
# every push checkpoint within a single run agrees, even if the remote changes
# mid-run.
resolve_archive_mode() {
    if [ -n "$ARCHIVE_MODE" ]; then
        return
    fi
    if [ -d "$ARCHIVE_REPO/.git" ] \
        && git -C "$ARCHIVE_REPO" remote get-url origin >/dev/null 2>&1; then
        ARCHIVE_MODE="push"
    else
        ARCHIVE_MODE="local-only"
    fi
}

get_archive_mode() {
    resolve_archive_mode
    echo "$ARCHIVE_MODE"
}

should_attempt_push() {
    resolve_archive_mode
    [ "$ARCHIVE_MODE" = "push" ]
}

# Log the archive mode on transitions and re-log weekly so operators who
# missed the first line still see the current configuration. State fields
# last_logged_mode and last_logged_at drive the re-log interval.
log_archive_mode_if_needed() {
    local current_mode
    local state_json
    local last_logged_mode
    local last_logged_at
    local stale_push_failures
    local stale_push_escalation
    local now
    local now_ts
    local last_ts
    local should_log=0
    local message

    resolve_archive_mode
    current_mode="$ARCHIVE_MODE"
    state_json=$(read_state_json)
    last_logged_mode=$(printf '%s\n' "$state_json" | jq -r '.last_logged_mode // empty')
    last_logged_at=$(printf '%s\n' "$state_json" | jq -r '.last_logged_at // empty')
    stale_push_failures=$(printf '%s\n' "$state_json" | jq -r '.consecutive_push_failures // 0')
    stale_push_escalation=$(printf '%s\n' "$state_json" | jq -r '.push_failure_escalated // false')
    now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    now_ts=$(date -u +%s)

    if [ "$current_mode" != "$last_logged_mode" ]; then
        should_log=1
    elif [ -z "$last_logged_at" ]; then
        should_log=1
    else
        last_ts=$(jq -n -r --arg ts "$last_logged_at" '$ts | try fromdateiso8601 catch 0')
        if [ "$last_ts" = "0" ] || [ "$((now_ts - last_ts))" -gt "$MODE_RELOG_INTERVAL_SECONDS" ]; then
            should_log=1
        fi
    fi

    if [ "$should_log" -eq 0 ] && [ "$current_mode" = "local-only" ] && { [ "$stale_push_failures" != "0" ] || [ "$stale_push_escalation" = "true" ]; }; then
        write_state_json "$(printf '%s\n' "$state_json" | jq -c '.consecutive_push_failures = 0 | del(.push_failure_escalated)')"
        return 0
    fi

    if [ "$should_log" -eq 0 ]; then
        return 0
    fi

    if [ "$current_mode" = "push" ]; then
        message="jsonl-export: archive running in push mode (origin configured; will push commits to remote)"
    else
        message="jsonl-export: archive running in local-only mode (no origin remote; commits stay on this host — off-box backup disabled)"
    fi
    echo "$message" >&2

    # On entering local-only mode, clear consecutive_push_failures so a later
    # return to push mode starts from a clean counter. Without this, a
    # push→local-only→push round-trip (operator removes then re-adds origin)
    # would carry the old failure count forward and could trigger a premature
    # HIGH escalation on the very first failure after origin returns.
    # pending_archive_push is intentionally NOT cleared here — it correctly
    # tracks that local commits still need to be pushed once origin returns.
    # shellcheck disable=SC2016  # $mode/$at are jq variables, not bash
    local jq_filter='.last_logged_mode = $mode | .last_logged_at = $at'
    if [ "$current_mode" = "local-only" ]; then
        jq_filter="$jq_filter | .consecutive_push_failures = 0 | del(.push_failure_escalated)"
    fi

    write_state_json "$(
        printf '%s\n' "$state_json" \
            | jq -c --arg mode "$current_mode" --arg at "$now" "$jq_filter"
    )"
}

set_pending_spike_alert() {
    local db="$1"
    local prev_count="$2"
    local current_count="$3"
    local delta="$4"
    local threshold="$5"

    write_state_json "$(
        read_state_json \
            | normalize_pending_spike_alert_state \
            | jq -c \
            --arg db "$db" \
            --argjson prev_count "$prev_count" \
            --argjson current_count "$current_count" \
            --argjson delta "$delta" \
            --argjson threshold "$threshold" \
            '.pending_spike_alerts[$db] = {
                database: $db,
                prev_count: $prev_count,
                current_count: $current_count,
                delta: $delta,
                threshold: $threshold
            }'
    )"
}

clear_pending_spike_alert() {
    local db="${1:-}"

    if [ -z "$db" ]; then
        write_state_json "$(read_state_json | jq -c 'del(.pending_spike_alert, .pending_spike_alerts)')"
        return
    fi

    write_state_json "$(
        read_state_json \
            | normalize_pending_spike_alert_state \
            | jq -c --arg db "$db" '
                del(.pending_spike_alerts[$db]) |
                if (.pending_spike_alerts // {}) == {} then
                    del(.pending_spike_alerts)
                else
                    .
                end
            '
    )"
}

send_spike_alert() {
    local db="$1"
    local prev_count="$2"
    local current_count="$3"
    local delta="$4"
    local threshold="$5"

    "$ESCALATE_SCRIPT" \
        --subject "ESCALATION: JSONL spike detected [HIGH]" \
        --message "Database: $db, prev: $prev_count, current: $current_count, delta: ${delta}%, threshold: ${threshold}%" \
        2>/dev/null
}

retry_pending_spike_alert() {
    local state_json
    local updated_state_json
    local state_changed=0
    local alert_json
    local pending_alerts=()
    local db
    local prev_count
    local current_count
    local delta
    local threshold

    state_json=$(read_state_json | normalize_pending_spike_alert_state)
    updated_state_json="$state_json"
    while IFS= read -r alert_json; do
        [ -n "$alert_json" ] || continue
        pending_alerts+=("$alert_json")
    done < <(
        printf '%s\n' "$state_json" \
            | jq -c '.pending_spike_alerts // {} | to_entries | sort_by(.key) | .[].value'
    )
    if [ "${#pending_alerts[@]}" -eq 0 ]; then
        return
    fi

    for alert_json in "${pending_alerts[@]}"; do
        db=$(printf '%s\n' "$alert_json" | jq -r '.database // empty')
        if [ -z "$db" ]; then
            continue
        fi
        prev_count=$(printf '%s\n' "$alert_json" | jq -r '.prev_count // 0')
        current_count=$(printf '%s\n' "$alert_json" | jq -r '.current_count // 0')
        delta=$(printf '%s\n' "$alert_json" | jq -r '.delta // 0')
        threshold=$(printf '%s\n' "$alert_json" | jq -r '.threshold // 0')

        if send_spike_alert "$db" "$prev_count" "$current_count" "$delta" "$threshold"; then
            updated_state_json=$(
                printf '%s\n' "$updated_state_json" \
                    | jq -c --arg db "$db" '
                        del(.pending_spike_alerts[$db]) |
                        if (.pending_spike_alerts // {}) == {} then
                            del(.pending_spike_alerts)
                        else
                            .
                        end
                    '
            )
            state_changed=1
            continue
        fi
        echo "jsonl-export: pending spike alert delivery failed for $db" >&2
    done

    if [ "$state_changed" -eq 1 ]; then
        write_state_json "$updated_state_json"
    fi
}

# Retain only the last ~20 lines of stderr so an extremely chatty failure
# doesn't drown the escalation body.
truncate_stderr_context() {
    local raw="$1"

    [ -z "$raw" ] && return 0
    printf '%s\n' "$raw" | tail -n 20
}

push_archive_main() {
    local consecutive
    local fetch_err
    local rebase_err
    local push_err
    local push_attempt
    local push_succeeded

    record_archive_push_failure() {
        local message="$1"
        local stderr_context="$2"
        local body
        local stderr_display
        local already_escalated
        local state_stderr=""

        echo "$message" >&2
        if [ -n "$stderr_context" ]; then
            state_stderr=$(truncate_push_stderr_for_state "$stderr_context")
        fi
        consecutive=$(read_state_json | jq -r '.consecutive_push_failures // 0' || echo "0")
        consecutive=$((consecutive + 1))
        write_state_json "$(
            read_state_json \
                | jq -c \
                    --argjson count "$consecutive" \
                    --arg stderr "$state_stderr" \
                    '.consecutive_push_failures = $count
                     | .pending_archive_push = true
                     | if $stderr == "" then del(.last_push_stderr) else .last_push_stderr = $stderr end'
        )"

        already_escalated=$(read_state_json | jq -r '.push_failure_escalated // false' || echo "false")
        if [ "$consecutive" -ge "$MAX_PUSH_FAILURES" ] && [ "$already_escalated" != "true" ]; then
            stderr_display=$(truncate_stderr_context "$stderr_context")
            if [ -z "$stderr_display" ]; then
                stderr_display="(no stderr captured)"
            fi
            body=$(cat <<ESCALATION
Order: jsonl-export
Archive: $ARCHIVE_REPO
Consecutive failures: $consecutive (threshold: $MAX_PUSH_FAILURES)

Last git push stderr:
$stderr_display

Remediation:
- Check remote: git -C $ARCHIVE_REPO remote -v
- Verify remote is reachable and credentials are valid
- Temporarily suppress: export GC_JSONL_MAX_PUSH_FAILURES=99
- See docs/getting-started/troubleshooting.md#jsonl-archive-push-failures
ESCALATION
)
            if "$ESCALATE_SCRIPT" \
                --subject "ESCALATION: JSONL push failed [HIGH]" \
                --message "$body" \
                2>/dev/null; then
                mark_push_failure_escalated
            fi
        fi

        return 1
    }

    # Branch on the actual git exit status, not on whether stderr is non-empty.
    # Successful git commands can emit benign stderr (e.g. "warning: redirecting
    # to https://...", credential-helper notes, protocol upgrade hints) which
    # would otherwise misclassify the run as a failure and falsely escalate.
    if ! fetch_err=$(git "${ARCHIVE_PACK_CONFIG[@]}" "${ARCHIVE_NO_AUTO_GC[@]}" fetch origin main -q 2>&1 >/dev/null); then
        if git rev-parse --verify refs/remotes/origin/main >/dev/null 2>&1; then
            record_archive_push_failure \
                "jsonl-export: fetching origin/main failed" \
                "$fetch_err"
            return 1
        fi
        echo "jsonl-export: origin/main missing; attempting initial push bootstrap" >&2
    fi

    if git rev-parse --verify refs/remotes/origin/main >/dev/null 2>&1; then
        if ! git merge-base --is-ancestor refs/remotes/origin/main HEAD >/dev/null 2>&1; then
            if ! rebase_err=$(git "${ARCHIVE_PACK_CONFIG[@]}" "${ARCHIVE_NO_AUTO_GC[@]}" rebase refs/remotes/origin/main 2>&1 >/dev/null); then
                git "${ARCHIVE_PACK_CONFIG[@]}" "${ARCHIVE_NO_AUTO_GC[@]}" rebase --abort >/dev/null 2>&1 || true
                record_archive_push_failure \
                    "jsonl-export: rebase onto origin/main failed during archive push recovery" \
                    "$rebase_err"
                return 1
            fi
        fi
        if ! archive_has_local_only_commits_from_tracking; then
            record_archive_push_success
            return 0
        fi
    fi

    # Retry-with-backoff for transient push failures. Concurrent rigs pushing
    # to the same local bare archive can race on the ref-update lock or produce
    # non-fast-forward (a sibling rig commits between our fetch and our push).
    # Retry up to 3 times with jitter; re-fetch and rebase before each retry to
    # absorb any new commits. Delay bounds default to 1-5s and are overridden
    # in tests to keep failure-path coverage fast.
    push_succeeded=false
    for push_attempt in 1 2 3; do
        if push_err=$(git "${ARCHIVE_PACK_CONFIG[@]}" "${ARCHIVE_NO_AUTO_GC[@]}" push origin main -q 2>&1 >/dev/null); then
            push_succeeded=true
            if [ "$push_attempt" -gt 1 ]; then
                echo "jsonl-export: push succeeded on retry attempt $push_attempt" >&2
            fi
            break
        fi

        if [ "$push_attempt" -lt 3 ]; then
            sleep "$(push_retry_delay_seconds)"

            # Refresh origin tracking before retry — a sibling rig may have
            # moved the ref while we slept.
            if fetch_err=$(git "${ARCHIVE_PACK_CONFIG[@]}" "${ARCHIVE_NO_AUTO_GC[@]}" fetch origin main -q 2>&1 >/dev/null); then
                if git rev-parse --verify refs/remotes/origin/main >/dev/null 2>&1 \
                    && ! git merge-base --is-ancestor refs/remotes/origin/main HEAD >/dev/null 2>&1; then
                    if ! rebase_err=$(git "${ARCHIVE_PACK_CONFIG[@]}" "${ARCHIVE_NO_AUTO_GC[@]}" rebase refs/remotes/origin/main 2>&1 >/dev/null); then
                        git "${ARCHIVE_PACK_CONFIG[@]}" "${ARCHIVE_NO_AUTO_GC[@]}" rebase --abort >/dev/null 2>&1 || true
                        record_archive_push_failure \
                            "jsonl-export: rebase onto origin/main failed during retry $push_attempt" \
                            "$rebase_err"
                        return 1
                    fi
                fi
            fi
            # If fetch failed, fall through and retry the push anyway —
            # origin may just be momentarily unavailable.
        fi
    done

    if [ "$push_succeeded" != "true" ]; then
        record_archive_push_failure \
            "jsonl-export: pushing archive main failed after 3 attempts" \
            "$push_err"
        return 1
    fi

    record_archive_push_success
    return 0
}

commit_archive_snapshot() {
    local message="$1"
    local context="$2"

    if ! GIT_AUTHOR_NAME="Gas Town Daemon" \
        GIT_AUTHOR_EMAIL="daemon@gastown.local" \
        GIT_COMMITTER_NAME="Gas Town Daemon" \
        GIT_COMMITTER_EMAIL="daemon@gastown.local" \
        git "${ARCHIVE_PACK_CONFIG[@]}" "${ARCHIVE_NO_AUTO_GC[@]}" commit -q -m "$message"; then
        echo "jsonl-export: $context commit failed" >&2
        return 1
    fi
    # Both callers run this function as the left operand of ||, where bash
    # ignores errexit for the whole body, so nothing in the repack below can
    # end the export; it is never fatal (the snapshot is already committed).
    if begin_archive_repack; then
        repack_archive_objects
        end_archive_repack
    fi
    return 0
}

# The repack step runs under a marker kept in the state file this script
# already owns, repack_in_flight = {run_started_at, id}: a timestamp and an
# attempt id in that JSON, not a lock or PID file. run_started_at is when
# the RUN began, because the order's timeout counts from there: once
# JSONL_ORDER_TIMEOUT_SECONDS have passed since it, that run is dead (the
# controller kills it), however recently its repack began. id is
# REPACK_ATTEMPT_ID, so a run clears only its own marker, even against one
# started in the same second. reconcile_repack_marker counts a dead run's
# marker as a repack failure, through the usual path so it counts and
# escalates, and clears it; it runs early in every run, so a run that dies
# mid-repack is counted even if the runs after it have nothing to commit.
# A live marker of another run (a manual `gc order run` is not single-flight)
# is left alone, and this run skips its repack: one repack at a time,
# without a lock. The read, check and write of the state are not atomic
# across processes, so two overlapping manual runs can still race here.
# Once the failures reach MAX_REPACK_FAILURES, the step is skipped until
# REPACK_REESCALATE_SECONDS have passed since the last attempt: a repack the
# timeout keeps killing then costs one killed run per window instead of every
# run, and the snapshot still commits and pushes.

# Count a dead run's repack marker as a repack failure and clear it. A
# marker whose run started within the timeout is left alone.
reconcile_repack_marker() {
    local marker
    local started
    local id
    local now
    marker=$(read_state_json | jq -c '.repack_in_flight // empty' 2>/dev/null) || marker=""
    [ -n "$marker" ] || return 0
    started=$(printf '%s\n' "$marker" | jq -r '.run_started_at? | numbers' 2>/dev/null) || started=""
    case "$started" in ''|*[!0-9]*) started="" ;; esac
    id=$(printf '%s\n' "$marker" | jq -r '.id? | strings' 2>/dev/null) || id=""
    now=$(date +%s)
    if [ -n "$started" ] && [ "$started" -le "$now" ] && [ "$((now - started))" -lt "$JSONL_ORDER_TIMEOUT_SECONDS" ]; then
        return 0
    fi
    record_archive_repack_failure "step=repack exit=killed repack_in_flight id=${id:-unknown} run_started_at=${started:-unknown}: that run passed the order's ${JSONL_ORDER_TIMEOUT_SECONDS}s timeout with its repack unfinished, so it died mid-repack"
    if ! write_state_json "$(read_state_json | jq -c --argjson m "$marker" 'if .repack_in_flight == $m then del(.repack_in_flight) else . end')"; then
        echo "jsonl-export: could not clear the dead run's repack marker; the next run counts it again" >&2
    fi
}

# Succeed when this run should repack, after writing its marker.
begin_archive_repack() {
    local now
    local state_json
    local marker
    local consecutive
    local last_attempt
    now=$(date +%s)
    # A run that died since this run's own reconcile is counted here.
    reconcile_repack_marker
    state_json=$(read_state_json)
    marker=$(printf '%s\n' "$state_json" | jq -c '.repack_in_flight // empty' 2>/dev/null) || marker=""
    if [ -n "$marker" ]; then
        echo "jsonl-export: archive repack skipped: another run holds the repack marker ($marker) and has not passed the order's timeout; the snapshot commits and pushes as usual" >&2
        return 1
    fi
    consecutive=$(printf '%s\n' "$state_json" | jq -r '.consecutive_repack_failures // 0 | numbers' 2>/dev/null) || consecutive=0
    case "$consecutive" in ''|*[!0-9]*) consecutive=0 ;; esac
    last_attempt=$(printf '%s\n' "$state_json" | jq -r '.last_repack_attempt_at | numbers' 2>/dev/null) || last_attempt=""
    case "$last_attempt" in ''|*[!0-9]*) last_attempt="" ;; esac
    if [ "$consecutive" -ge "$MAX_REPACK_FAILURES" ] && [ -n "$last_attempt" ] \
        && [ "$last_attempt" -le "$now" ] && [ "$((now - last_attempt))" -lt "$REPACK_REESCALATE_SECONDS" ]; then
        echo "jsonl-export: archive repack skipped: $consecutive consecutive failures (threshold $MAX_REPACK_FAILURES) and the last attempt was $((now - last_attempt))s ago; the next comes once ${REPACK_REESCALATE_SECONDS}s have passed. The snapshot commits and pushes as usual" >&2
        return 1
    fi
    if ! write_state_json "$(read_state_json | jq -c --argjson started "$RUN_STARTED_AT" --arg id "$REPACK_ATTEMPT_ID" --argjson now "$now" \
        '.repack_in_flight = {run_started_at: $started, id: $id} | .last_repack_attempt_at = $now')"; then
        echo "jsonl-export: could not record the repack attempt in state; if this run dies mid-repack, nothing counts it" >&2
    fi
    return 0
}

# Clear the repack marker if its id is this run's attempt.
end_archive_repack() {
    if ! write_state_json "$(read_state_json | jq -c --arg id "$REPACK_ATTEMPT_ID" 'if (.repack_in_flight.id? // null) == $id then del(.repack_in_flight) else . end')"; then
        echo "jsonl-export: could not clear this run's repack marker; once its run passes the order's timeout, a later run counts it as a death" >&2
    fi
}

# Pack the archive after a snapshot commit, and judge the result. Every
# failure is recorded in state and escalated by record_archive_repack_failure;
# nothing here can end the export (see commit_archive_snapshot).
repack_archive_objects() {
    # Every snapshot commit leaves one new loose blob per exported store (a
    # full issues.jsonl, tens of MiB each, or far more for a large store).
    # The commit's own auto-maintenance is off (ARCHIVE_NO_AUTO_GC), and its
    # trigger, gc.auto's default of 6700 loose objects, would be several GiB
    # of loose data at this blob size anyway. Repack here with a much lower
    # trigger: gc.auto=256 fires roughly every ~128 commits, so each repack
    # handles a few hundred MiB, not the whole history; autoDetach off so
    # the repack finishes inside this order's run instead of a detached child
    # that outlives it; gc.autoPackLimit at REPACK_PACK_LIMIT, so the packs
    # those repacks write are folded together (and their whole copies
    # deltified) long before git's default of 50. The first run against an
    # archive that is already far behind packs the whole backlog at once,
    # which is why the order's timeout is 30m. Never fatal: the snapshot is
    # already committed, and a failed repack costs disk, not data. Never
    # silent either: a repack that keeps failing lets loose objects pile up
    # toward git's own trigger, so failures are counted in state and
    # escalated.
    # Exit status alone is not proof: gc --auto returns 0 without packing when
    # its sampled estimate (one of the 256 loose-object fan-out directories)
    # misses the trigger, when another git gc holds the repository, when the
    # pre-auto-gc hook declines, or when the pack directory is unusable; and
    # it counts objects, not bytes. So the loose objects are measured exactly,
    # by count and by KiB on disk: above REPACK_LOOSE_CEILING (default twice
    # the gc.auto trigger) or REPACK_LOOSE_KIB_CEILING after a gc --auto that
    # exited 0, they are packed explicitly with the incremental repack gc
    # --auto would have run, the unreachable ones past a short grace are
    # pruned, and only that result is judged, on both.
    # It runs inside commit_archive_snapshot, whose callers run it as the
    # left operand of ||, where bash ignores errexit for the whole body, so a
    # failing command here cannot end the export. Every failure below is
    # still handled explicitly, so the snapshot is marked for push even if a
    # caller drops the ||.
    local repack_err
    local repack_rc=0
    local repack_step="gc --auto"
    local count_out
    local loose
    local loose_kib
    local gc_pid
    local gc_log
    local gc_log_head
    local gc_holder
    local pre_auto_gc
    local fallback_err
    local prune_err
    local pruned=0
    local unreachable
    local summary
    # Read the loose objects' count and KiB on disk; either is empty when
    # count-objects did not report it.
    read_loose_count() {
        count_out=$(git count-objects -v 2>&1) || count_out="count-objects failed: $count_out"
        loose=$(printf '%s\n' "$count_out" | awk '/^count:/ {print $2}')
        loose_kib=$(printf '%s\n' "$count_out" | awk '/^size:/ {print $2}')
        case "$loose" in ''|*[!0-9]*) loose="" ;; esac
        case "$loose_kib" in ''|*[!0-9]*) loose_kib="" ;; esac
    }
    # Succeed when both readings exist and both are within their ceilings.
    loose_within_ceilings() {
        [ -n "$loose" ] && [ -n "$loose_kib" ] \
            && [ "$loose" -le "$REPACK_LOOSE_CEILING" ] \
            && [ "$loose_kib" -le "$REPACK_LOOSE_KIB_CEILING" ]
    }
    # Succeed when both readings exist and either is over its ceiling. An
    # unreadable reading is neither; the post-condition below fails on it.
    loose_over_a_ceiling() {
        [ -n "$loose" ] && [ -n "$loose_kib" ] && ! loose_within_ceilings
    }
    # Print how many loose objects git prune would remove with no grace at
    # all: the unreachable ones, one "<id> <type>" line each in its dry run.
    # Asked right after the prune, every one of them is younger than the
    # grace. Reachability, not age, is what marks them as garbage: a repack
    # that exits 0 but packs nothing leaves reachable loose objects that are
    # just as young.
    count_unreachable_loose_objects() {
        local listing
        listing=$(git prune --dry-run --expire=now 2>/dev/null) || return 1
        printf '%s\n' "$listing" | grep -cE '^[0-9a-f]{40}([0-9a-f]{24})? '
    }
    read_loose_count
    if loose_over_a_ceiling; then
        echo "jsonl-export: archive holds $loose loose objects in $loose_kib KiB (ceilings $REPACK_LOOSE_CEILING objects, $REPACK_LOOSE_KIB_CEILING KiB); packing them now, which takes minutes on a large backlog" >&2
    fi
    # gc.autoPackLimit is a trigger, not a bound on the work: once the packs
    # exceed it, this gc --auto folds ALL of them together in this run. An
    # archive that arrives holding many packs of whole copies (written
    # before these settings) runs that whole delta search at once, inside
    # the order's timeout. Whole copies that already share a pack are never
    # retried as a delta pair without -f, and when the memory git estimates
    # for the repack is not available and gc.bigPackThreshold is unset, gc
    # also keeps the largest pack out of the consolidation altogether
    # (--keep-largest-pack), so the whole copies inside it stay unrepaired.
    # The remedy is the one-time repair by hand above ARCHIVE_PACK_CONFIG;
    # for such an archive it is a precondition of deploying these settings.
    repack_err=$(git "${ARCHIVE_PACK_CONFIG[@]}" -c gc.auto=256 -c gc.autoDetach=false -c "gc.autoPackLimit=$REPACK_PACK_LIMIT" gc --auto --quiet 2>&1 >/dev/null) || repack_rc=$?
    read_loose_count
    if [ "$repack_rc" -eq 0 ] && loose_over_a_ceiling; then
        # The explicit repack stands in for the auto-gc that did not fire, so
        # it keeps auto-gc's two courtesies. A deferral is neither a success
        # nor a failure; the next commit retries.
        # 1. A failing pre-auto-gc hook vetoes it, as it vetoes gc --auto.
        #    The hook is found the way git finds it (rev-parse --git-path
        #    resolves core.hooksPath) and run directly, which needs no
        #    particular git version; an unresolvable path means no veto. When
        #    gc --auto was due and the hook already declined, the hook runs a
        #    second time here, and a consistent hook declines again.
        pre_auto_gc=$(git rev-parse --git-path hooks/pre-auto-gc 2>/dev/null) || pre_auto_gc=""
        if [ -n "$pre_auto_gc" ] && [ -f "$pre_auto_gc" ] && [ -x "$pre_auto_gc" ] && ! "$pre_auto_gc" >/dev/null 2>&1; then
            echo "jsonl-export: archive repack deferred: the pre-auto-gc hook ($pre_auto_gc) declined it; the next commit retries" >&2
            return 0
        fi
        # 2. It defers to a git gc that holds the repository. It only reads
        #    gc.pid (in the common git dir); this script writes no lock or
        #    PID file. A gc that starts between this check and the repack can
        #    still race it. The archive is private to this script, so that
        #    takes a human's gc or an overlapping export, and a repack that
        #    fails in the race is judged below like any other failure.
        gc_pid=$(git rev-parse --git-path gc.pid 2>/dev/null) || gc_pid=".git/gc.pid"
        if gc_holder=$(git_gc_holder "$gc_pid"); then
            echo "jsonl-export: archive repack deferred: another git gc holds $gc_pid ($gc_holder); the next commit retries" >&2
            return 0
        fi
        # An incremental repack cannot write a bitmap index: an inherited
        # repack.writeBitmaps / pack.writeBitmaps makes it exit 128 on every
        # snapshot, and the loose objects would never be packed.
        repack_step="repack -d -l --no-write-bitmap-index"
        fallback_err=$(git "${ARCHIVE_PACK_CONFIG[@]}" repack -d -l -q --no-write-bitmap-index 2>&1 >/dev/null) || repack_rc=$?
        # A repack packs only reachable objects. Unreachable loose ones (a
        # snapshot blob discarded after a failed add or commit, or one left
        # by a run killed mid-add) stay loose and count toward the KiB
        # ceiling, so two or three such blobs of a large store would fail
        # the post-condition on every run. Prune them with a grace far
        # shorter than git's two weeks, but longer than the order's timeout.
        # Nothing serializes runs: a manual `gc order run` does not wait for
        # one in flight, and the cooldown counts from a run's start. What
        # bounds a run is the timeout, which kills it by
        # JSONL_ORDER_TIMEOUT_SECONDS, and this run's own objects are
        # committed (reachable) before its repack. So an unreachable object
        # younger than the grace may still belong to a live run and is never
        # pruned, and one older is garbage. gc --auto is left alone: its own
        # prune keeps git's two weeks.
        if [ "$repack_rc" -eq 0 ]; then
            repack_step="prune --expire=$ARCHIVE_PRUNE_GRACE_MINUTES.minutes.ago"
            prune_err=$(git "${ARCHIVE_PACK_CONFIG[@]}" prune --expire="$ARCHIVE_PRUNE_GRACE_MINUTES.minutes.ago" 2>&1 >/dev/null) || repack_rc=$?
            fallback_err="${fallback_err:+$fallback_err
}$prune_err"
            [ "$repack_rc" -eq 0 ] && pruned=1
        fi
        # Keep gc --auto's stderr too: its warnings (unreachable loose
        # objects, for one) explain why the fallback was needed.
        repack_err="${repack_err:+$repack_err
}$fallback_err"
        read_loose_count
    fi
    if [ "$repack_rc" -eq 0 ] && loose_within_ceilings; then
        record_archive_repack_success
        return 0
    fi
    # Still over a ceiling after a repack and a prune that both succeeded:
    # when every loose object left is unreachable, it is garbage the grace
    # still protects (a discarded blob can now sit up to the grace and hold
    # the KiB ceiling over), waiting its turn rather than a failed repack, so
    # log it and record no failure. Anything reachable left loose is a
    # repack that did not pack, and fails below.
    if [ "$repack_rc" -eq 0 ] && [ "$pruned" -eq 1 ] && [ -n "$loose" ] \
        && unreachable=$(count_unreachable_loose_objects) && [ "$unreachable" -eq "$loose" ]; then
        echo "jsonl-export: $loose loose objects ($loose_kib KiB) remain over a ceiling, all unreachable and younger than the ${ARCHIVE_PRUNE_GRACE_MINUTES}-minute prune grace; a later run prunes them" >&2
        record_archive_repack_success
        return 0
    fi
    if [ -z "$loose" ] || [ -z "$loose_kib" ]; then
        repack_err="$repack_err
$count_out"
    fi
    gc_log=$(git rev-parse --git-path gc.log 2>/dev/null) || gc_log=".git/gc.log"
    if [ -f "$gc_log" ]; then
        gc_log_head=$(head -c 400 "$gc_log" 2>&1) || gc_log_head="(unreadable: $gc_log_head)"
        repack_err="$repack_err
gc.log: $gc_log_head"
    fi
    # The summary goes first and last: the state file keeps the first 512
    # bytes and the escalation mail keeps the last 20 lines.
    summary="step=$repack_step exit=$repack_rc loose_objects=${loose:-unknown} (ceiling $REPACK_LOOSE_CEILING) loose_kib=${loose_kib:-unknown} (ceiling $REPACK_LOOSE_KIB_CEILING)"
    if [ -n "$repack_err" ]; then
        record_archive_repack_failure "$summary
$repack_err
$summary"
    else
        record_archive_repack_failure "$summary"
    fi
    return 0
}

# Print who holds git gc's repository lock and succeed, or fail when nobody
# does. $1 is the gc.pid path (rev-parse --git-path gc.pid, which is in the
# common git dir). git serializes gc through gc.pid ("<pid> <host>") and treats
# it as held while the file is younger than 12 hours and its host is another
# machine or its pid is alive here (builtin/gc.c). This mirrors that test.
# Liveness is read with ps, which, like git's EPERM rule, counts another
# user's live process as alive, where kill -0 would call it dead.
git_gc_holder() {
    local pid_file="$1"
    local pid=""
    local host=""
    [ -f "$pid_file" ] || return 1
    [ -n "$(find "$pid_file" -mmin -720 2>/dev/null)" ] || return 1
    read -r pid host < "$pid_file" 2>/dev/null || [ -n "$pid" ] || return 1
    case "$pid" in ''|*[!0-9]*) return 1 ;; esac
    if [ -n "$host" ] && [ "$host" != "$(hostname 2>/dev/null)" ]; then
        printf 'pid %s on %s' "$pid" "$host"
        return 0
    fi
    if [ -n "$(ps -p "$pid" -o pid= 2>/dev/null)" ]; then
        printf 'pid %s' "$pid"
        return 0
    fi
    return 1
}

# Clear the repack failure streak. Writes state only when there is a streak
# to clear, so the common path (gc --auto finds nothing to do) costs no write.
record_archive_repack_success() {
    local state_json
    state_json=$(read_state_json)
    if [ "$(printf '%s\n' "$state_json" | jq -r '(.consecutive_repack_failures // 0) > 0 or has("last_repack_stderr") or has("repack_failure_escalated") or has("last_repack_escalation_error")')" != "true" ]; then
        return 0
    fi
    if ! write_state_json "$(printf '%s\n' "$state_json" | jq -c 'del(.consecutive_repack_failures) | del(.last_repack_stderr) | del(.repack_failure_escalated) | del(.last_repack_escalation_error)')"; then
        echo "jsonl-export: repack succeeded but clearing the failure streak did not persist; the next successful commit retries, and escalation dedupe is time-bounded" >&2
    fi
    return 0
}

# Count a failed repack and escalate once per failure streak when the count
# reaches MAX_REPACK_FAILURES. gc --auto is a no-op below the loose-object
# threshold, so a failure only happens when a repack was actually due, and
# every later commit retries it: a streak means the archive is growing.
record_archive_repack_failure() {
    local stderr_context="$1"
    local consecutive
    local already_escalated
    local stderr_display
    local body

    echo "jsonl-export: archive repack failed (non-fatal; loose objects keep accumulating until it succeeds)" >&2
    consecutive=$(read_state_json | jq -r '.consecutive_repack_failures // 0' || echo "0")
    case "$consecutive" in ''|*[!0-9]*) consecutive=0 ;; esac
    consecutive=$((consecutive + 1))
    # The streak is the only durable record, and the conditions that fail a
    # repack (full or unwritable disk) also fail this write. If it cannot be
    # persisted, the count can never reach the threshold, so escalate NOW
    # instead of waiting on a counter that will not move.
    local state_persisted=1
    if ! write_state_json "$(
        read_state_json \
            | jq -c \
                --argjson count "$consecutive" \
                --arg stderr "$(truncate_push_stderr_for_state "$stderr_context")" \
                '.consecutive_repack_failures = $count
                 | if $stderr == "" then del(.last_repack_stderr) else .last_repack_stderr = $stderr end'
    )"; then
        state_persisted=0
        echo "jsonl-export: could not persist the repack failure streak; escalating now" >&2
    fi

    if [ "$state_persisted" = 1 ]; then
        already_escalated=$(read_state_json | jq -r --argjson now "$(date +%s)" --argjson win "$REPACK_REESCALATE_SECONDS" \
            '(.repack_failure_escalated // null) as $e | if ($e | type) == "number" then ($e <= $now and ($now - $e) < $win) else false end' || echo "false")
        if [ "$consecutive" -lt "$MAX_REPACK_FAILURES" ] || [ "$already_escalated" = "true" ]; then
            return 0
        fi
    fi
    stderr_display=$(truncate_stderr_context "$stderr_context")
    if [ -z "$stderr_display" ]; then
        stderr_display="(no stderr captured)"
    fi
    body=$(cat <<ESCALATION
Order: jsonl-export
Archive: $ARCHIVE_REPO
Consecutive repack failures: $consecutive (threshold: $MAX_REPACK_FAILURES)

Last git gc stderr:
$stderr_display

Every snapshot commit adds full-size loose blobs; until a repack succeeds the
archive grows by tens of MiB per commit.

Remediation:
- Check free disk and the loose-object count: git -C $ARCHIVE_REPO count-objects -vH
- Run the repack by hand to see the full error, with the settings that keep large exports delta-compressed: git -C $ARCHIVE_REPO ${ARCHIVE_PACK_CONFIG[*]} gc
- Temporarily suppress: export GC_JSONL_MAX_REPACK_FAILURES=99
ESCALATION
)
    # A failed delivery must be distinguishable from the repack failure it
    # reports: log it like the spike-alert path does, and keep it in state
    # (order stderr is persisted nowhere). The marker stays unset, so the next
    # failing commit retries the escalation.
    local escalate_err
    if escalate_err=$("$ESCALATE_SCRIPT" \
        --subject "ESCALATION: JSONL archive repack failing [HIGH]" \
        --message "$body" 2>&1 >/dev/null); then
        if ! write_state_json "$(read_state_json | jq -c --argjson now "$(date +%s)" '.repack_failure_escalated = $now | del(.last_repack_escalation_error)')"; then
            echo "jsonl-export: repack escalation delivered but its dedupe marker did not persist; the next failing commit re-sends it" >&2
        fi
    else
        echo "jsonl-export: repack failure escalation delivery failed (retrying on the next failing commit)" >&2
        if ! write_state_json "$(
            read_state_json \
                | jq -c --arg err "$(truncate_push_stderr_for_state "${escalate_err:-(no stderr)}")" \
                    '.last_repack_escalation_error = $err'
        )"; then
            echo "jsonl-export: could not record the escalation delivery failure in state" >&2
        fi
    fi
    return 0
}

discard_failed_db_outputs() {
    local db="$1"

    # Put the scope's directory and flat mirror back exactly as HEAD has them
    # (or remove them when HEAD never had them). Nothing committed is lost.
    git -C "$ARCHIVE_REPO" reset -q -- "$db" "$db.jsonl" >/dev/null 2>&1 || true
    if git -C "$ARCHIVE_REPO" cat-file -e "HEAD:$db" 2>/dev/null; then
        git -C "$ARCHIVE_REPO" checkout -q HEAD -- "$db" >/dev/null 2>&1 || true
        git -C "$ARCHIVE_REPO" clean -q -fd -- "$db" >/dev/null 2>&1 || true
    else
        rm -rf "${ARCHIVE_REPO:?}/$db"
    fi
    if git -C "$ARCHIVE_REPO" cat-file -e "HEAD:$db.jsonl" 2>/dev/null; then
        git -C "$ARCHIVE_REPO" checkout -q HEAD -- "$db.jsonl" >/dev/null 2>&1 || true
    else
        rm -f "$ARCHIVE_REPO/$db.jsonl"
    fi
}

discard_staged_archive_outputs() {
    local path

    if [ "${#STAGE_PATHS[@]}" -eq 0 ]; then
        return
    fi

    git reset -q -- "${STAGE_PATHS[@]}" >/dev/null 2>&1 || true
    for path in "${STAGE_PATHS[@]}"; do
        if git cat-file -e "HEAD:$path" 2>/dev/null; then
            git restore --source=HEAD --staged --worktree -- "$path" >/dev/null 2>&1 || true
            git clean -fd -- "$path" >/dev/null 2>&1 || true
            continue
        fi
        rm -rf "$path"
    done
}

# State file for tracking consecutive push failures.
STATE_FILE="$PACK_STATE_DIR/jsonl-export-state.json"
LEGACY_PACK_STATE_FILE="$LEGACY_PACK_STATE_DIR/jsonl-export-state.json"

if [ -z "${GC_JSONL_ARCHIVE_REPO:-}" ] && [ ! -d "$ARCHIVE_REPO/.git" ]; then
    if [ -d "$LEGACY_PACK_ARCHIVE_REPO/.git" ]; then
        ARCHIVE_REPO="$LEGACY_PACK_ARCHIVE_REPO"
    elif [ -d "$LEGACY_ARCHIVE_REPO/.git" ]; then
        ARCHIVE_REPO="$LEGACY_ARCHIVE_REPO"
    fi
fi
if [ ! -e "$STATE_FILE" ] && [ -e "$LEGACY_PACK_STATE_FILE" ]; then
    STATE_FILE="$LEGACY_PACK_STATE_FILE"
elif [ ! -e "$STATE_FILE" ] && [ -e "$LEGACY_STATE_FILE" ]; then
    STATE_FILE="$LEGACY_STATE_FILE"
fi
STATE_FILE_BACKUP="${STATE_FILE}.bak"
mkdir -p "$(dirname "$STATE_FILE")"

log_archive_mode_if_needed
retry_pending_spike_alert
# Before any path that ends the run: a run that died mid-repack is counted
# even when this run has nothing to commit.
reconcile_repack_marker

# The first export in the bd format moves a scope's snapshot files from the
# old `dolt sql -r json` layout ({"rows":[...]} issues.jsonl, the flat
# <db>.jsonl mirror and the per-table comments/config/dependencies/labels/
# metadata files) into <db>/legacy/ with `git mv`. History is kept and nothing
# is deleted; normal archive retention ages the legacy copies out.
LEGACY_TABLE_FILES="issues comments config dependencies labels metadata"

archived_file_is_legacy_format() {
    local path="$1"
    local head

    git -C "$ARCHIVE_REPO" cat-file -e "HEAD:$path" 2>/dev/null || return 1
    head=$(git -C "$ARCHIVE_REPO" show "HEAD:$path" 2>/dev/null | head -c 16 | tr -d '[:space:]')
    case "$head" in
        '{"rows"'* | '{}') return 0 ;;
    esac
    return 1
}

move_legacy_snapshot_files() {
    local db="$1"
    local table
    local moved=0

    if ! archived_file_is_legacy_format "$db/issues.jsonl"; then
        return 0
    fi
    mkdir -p "$ARCHIVE_REPO/$db/legacy"
    for table in $LEGACY_TABLE_FILES; do
        if git -C "$ARCHIVE_REPO" cat-file -e "HEAD:$db/$table.jsonl" 2>/dev/null; then
            git -C "$ARCHIVE_REPO" mv -f "$db/$table.jsonl" "$db/legacy/$table.jsonl" || return 1
            moved=1
        fi
    done
    if archived_file_is_legacy_format "$db.jsonl"; then
        git -C "$ARCHIVE_REPO" mv -f "$db.jsonl" "$db/legacy/$db.jsonl" || return 1
        moved=1
    fi
    if [ "$moved" -eq 1 ]; then
        echo "jsonl-export: moved the pre-bd-export snapshot of $db to $db/legacy/" >&2
    fi
}

# Ensure archive repo exists.
if [ ! -d "$ARCHIVE_REPO/.git" ]; then
    mkdir -p "$ARCHIVE_REPO"
    git -C "$ARCHIVE_REPO" init -q 2>/dev/null || true
fi

TOTAL_EXPORTED=0
TOTAL_DBS=0
FAILED_DBS=""
FAILED_DB_COUNT=0
HALTED=0
STAGE_PATHS=()
HALT_DB=""
HALT_PREV_COUNT=0
HALT_CURRENT_COUNT=0
HALT_DELTA=0
# Exports above the big-file threshold, named in the run's summary.
BIG_EXPORT_WARNINGS=""
SCOPE_SCRUB_WHERE=""
if [ "$SCRUB" = "true" ]; then
    SCOPE_SCRUB_WHERE="WHERE issue_type NOT IN ('message', 'event', 'wisp', 'agent') AND title NOT LIKE 'gc:%' AND title NOT LIKE 'order:%' AND NOT (issue_type = 'convoy' AND title LIKE 'sling-%')"
fi

record_failed_db() {
    FAILED_DB_COUNT=$((FAILED_DB_COUNT + 1))
    FAILED_DBS="${FAILED_DBS}$1
"
}

# read_source_issue_count counts the scope's durable issues with the same
# scrub rules, straight from the store, so a drop spike in the export can be
# checked against the source of truth.
read_source_issue_count() {
    local db="$1"
    local output
    local count

    if ! output=$(scope_sql_read csv "SELECT COUNT(*) AS row_count FROM \`$db\`.issues $SCOPE_SCRUB_WHERE" 2>/dev/null); then
        return 1
    fi
    count=$(printf '%s\n' "$output" | tail -n 1 | tr -d '\r')
    case "$count" in
        ''|*[!0-9]*)
            return 1
            ;;
    esac
    printf '%s\n' "$count"
}

should_halt_for_jsonl_spike() {
    local db="$1"
    local prev_count="$2"
    local current_count="$3"
    local threshold="$4"
    local source_count
    local source_drop

    # Growth spikes are still suspicious. Only drop spikes can be suppressed by
    # checking the store behind the passive JSONL export.
    if [ "$current_count" -ge "$prev_count" ]; then
        return 0
    fi

    if ! source_count=$(read_source_issue_count "$db"); then
        echo "jsonl-export: source-of-truth count unavailable for $db; preserving JSONL spike halt" >&2
        return 0
    fi

    if [ "$source_count" -ge "$prev_count" ]; then
        echo "jsonl-export: suppressing JSONL drop spike for $db; source count $source_count >= previous $prev_count" >&2
        return 1
    fi

    source_drop=$(( (prev_count - source_count) * 100 / prev_count ))
    if [ "$source_drop" -le "$threshold" ]; then
        echo "jsonl-export: suppressing JSONL drop spike for $db; source drop ${source_drop}% <= ${threshold}%" >&2
        return 1
    fi

    return 0
}

# export_scope exports the current scope into <archive>/<db>/issues.jsonl
# (plus the flat <db>.jsonl mirror). Returns 1 when the scope failed; its
# outputs are then restored to HEAD.
export_scope() {
    local DB="$SCOPE_DB"
    local db_dir="$ARCHIVE_REPO/$DB"
    local raw_tmp
    local out
    local filtered_tmp

    if ! move_legacy_snapshot_files "$DB"; then
        echo "jsonl-export: moving the legacy snapshot of $DB to $DB/legacy/ failed" >&2
        discard_failed_db_outputs "$DB"
        return 1
    fi
    mkdir -p "$db_dir"

    # Step 1: gc bd export of the whole scope (read-only; one consistent snapshot).
    raw_tmp=$(mktemp "$db_dir/export.jsonl.tmp.XXXXXX")
    if ! out=$(scope_bd export --all -o "$raw_tmp" 2>&1); then
        echo "jsonl-export: gc bd export failed for $SCOPE_LABEL ($DB): $out" >&2
        rm -f "$raw_tmp"
        discard_failed_db_outputs "$DB"
        return 1
    fi

    # Step 2: keep the archived records (see select_archived_records). A
    # malformed export fails the scope so it cannot become the new baseline.
    filtered_tmp=$(mktemp "$db_dir/issues.jsonl.tmp.XXXXXX")
    if ! select_archived_records "$SCRUB" <"$raw_tmp" >"$filtered_tmp"; then
        echo "jsonl-export: gc bd export for $SCOPE_LABEL ($DB) is not valid JSONL" >&2
        rm -f "$raw_tmp" "$filtered_tmp"
        discard_failed_db_outputs "$DB"
        return 1
    fi
    rm -f "$raw_tmp"
    mv -f "$filtered_tmp" "$db_dir/issues.jsonl"

    # The flat <db>.jsonl mirrors the per-db snapshot for readers of the
    # older flat layout.
    if ! cp -f "$db_dir/issues.jsonl" "$ARCHIVE_REPO/$DB.jsonl" 2>/dev/null; then
        discard_failed_db_outputs "$DB"
        return 1
    fi
    return 0
}

# Every scope is visited through bd: the city first, then each rig.
SCOPE_SPECS="city"
if RIG_NAMES=$(core_rig_names); then
    while IFS= read -r rig_name; do
        [ -n "$rig_name" ] || continue
        SCOPE_SPECS="$SCOPE_SPECS
rig $rig_name"
    done <<< "$RIG_NAMES"
else
    echo "jsonl-export: gc rig list failed; exporting the city scope only" >&2
    order_outcome_scope_skipped "rigs" "rig list unavailable"
fi
trap order_outcome_write EXIT

VISITED_DBS=""
while IFS= read -r SCOPE_SPEC; do
    [ -n "$SCOPE_SPEC" ] || continue
    # shellcheck disable=SC2086 # "city" or "rig <name>"
    scope_select $SCOPE_SPEC
    if ! scope_resolve_db; then
        if [ "$SCOPE_NOT_BD" -eq 1 ]; then
            echo "jsonl-export: $SCOPE_LABEL is not a bd bead store; nothing to export there"
            order_outcome_scope_skipped "$SCOPE_LABEL" "not a bd bead store"
            continue
        fi
        echo "jsonl-export: $SCOPE_LABEL unreachable through gc bd: $SCOPE_LAST_ERROR" >&2
        TOTAL_DBS=$((TOTAL_DBS + 1))
        record_failed_db "$SCOPE_LABEL"
        order_outcome_scope_skipped "$SCOPE_LABEL" "bead store unreachable"
        continue
    fi
    case "
$VISITED_DBS
" in
        *"
$SCOPE_DB
"*)
            continue
            ;;
    esac
    VISITED_DBS="${VISITED_DBS}${SCOPE_DB}
"
    DB="$SCOPE_DB"
    TOTAL_DBS=$((TOTAL_DBS + 1))

    if ! export_scope; then
        record_failed_db "$DB"
        order_outcome_scope_skipped "$SCOPE_LABEL" "export failed"
        continue
    fi

    # Count records from the final persisted payload (post-scrub) so commit
    # messages and maintenance summaries reflect what was actually archived.
    CURRENT_COUNT=$(count_jsonl_rows < "$ARCHIVE_REPO/$DB/issues.jsonl")
    TOTAL_EXPORTED=$((TOTAL_EXPORTED + CURRENT_COUNT))

    STAGE_PATHS+=("$DB" "$DB.jsonl")

    # Step 3: Spike detection — compare record counts against previous commit.
    PREV_COUNT=0
    if git -C "$ARCHIVE_REPO" cat-file -e "HEAD:$DB/issues.jsonl" 2>/dev/null; then
        PREV_COUNT=$(git -C "$ARCHIVE_REPO" show "HEAD:$DB/issues.jsonl" 2>/dev/null | count_jsonl_rows || echo "0")
    fi

    # Skip the percentage check on the first run (no prior commit) and when
    # the previous count is below the absolute floor — a 1→2 swing is 100% but
    # meaningless on a tiny database. The PREV_COUNT > 0 guard also avoids the
    # division by zero when the floor is set to 0 to disable the small-N skip.
    if [ "$PREV_COUNT" -gt 0 ] && [ "$PREV_COUNT" -ge "$MIN_PREV_FOR_SPIKE_CHECK" ]; then
        FILTERED_COUNT="$CURRENT_COUNT"
        DELTA=$(( (FILTERED_COUNT - PREV_COUNT) * 100 / PREV_COUNT ))
        if [ "$DELTA" -lt 0 ]; then
            DELTA=$(( -DELTA ))
        fi
        if [ "$DELTA" -gt "$SPIKE_THRESHOLD" ] && should_halt_for_jsonl_spike "$DB" "$PREV_COUNT" "$FILTERED_COUNT" "$SPIKE_THRESHOLD"; then
            HALTED=1
            HALT_DB="$DB"
            HALT_PREV_COUNT="$PREV_COUNT"
            HALT_CURRENT_COUNT="$FILTERED_COUNT"
            HALT_DELTA="$DELTA"
            echo "jsonl-export: HALTED — spike in $DB (${DELTA}% > ${SPIKE_THRESHOLD}%)"
            break
        fi
    fi
done <<EOF
$SCOPE_SPECS
EOF

if [ "$TOTAL_DBS" -eq 0 ]; then
    order_outcome_set skipped "no bd bead store to export"
elif [ "$FAILED_DB_COUNT" -ge "$TOTAL_DBS" ]; then
    order_outcome_set skipped "no bead scope could be exported"
fi

cd "$ARCHIVE_REPO"
if [ "${#STAGE_PATHS[@]}" -gt 0 ]; then
    # git stores an export above core.bigFileThreshold whole, whatever the
    # pack settings: name each one, with its size, on stderr and in the
    # run's summary.
    big_file_threshold_bytes=$(jsonl_git_size_bytes "$ARCHIVE_BIG_FILE_THRESHOLD")
    for stage_path in "${STAGE_PATHS[@]}"; do
        [ -f "$stage_path/issues.jsonl" ] || continue
        export_bytes=$(wc -c "./$stage_path/issues.jsonl" | awk '{print $1}')
        case "$export_bytes" in ''|*[!0-9]*) continue ;; esac
        if [ "$export_bytes" -gt "$big_file_threshold_bytes" ]; then
            big_export="$stage_path/issues.jsonl is $export_bytes bytes, above the big-file threshold ($ARCHIVE_BIG_FILE_THRESHOLD); stored whole"
            echo "jsonl-export: warning: $big_export" >&2
            BIG_EXPORT_WARNINGS="${BIG_EXPORT_WARNINGS:+$BIG_EXPORT_WARNINGS; }$big_export"
        fi
    done
    if ! git "${ARCHIVE_PACK_CONFIG[@]}" add -A -- "${STAGE_PATHS[@]}"; then
        discard_staged_archive_outputs
        echo "jsonl-export: staging archive outputs failed" >&2
        exit 1
    fi
fi

# On HALT we still commit the new export so PREV_COUNT advances on the next
# run — otherwise the same spike re-fires every cooldown and floods the inbox
# (#1547 root cause #3). Push is skipped, so the spike snapshot stays local
# until a later successful non-HALT run pushes the archive forward.
if [ "$HALTED" -eq 1 ]; then
    if ! git diff --cached --quiet 2>/dev/null; then
        EXPORTED_DBS=$((TOTAL_DBS - FAILED_DB_COUNT))
        commit_archive_snapshot \
            "[HALT] backup $(date -u +%Y-%m-%dT%H:%M:%SZ): exported=$EXPORTED_DBS/$TOTAL_DBS records=$TOTAL_EXPORTED (spike detected; push skipped)" \
            "HALT baseline" || {
            discard_staged_archive_outputs
            exit 1
        }
        # A full disk (the repack above can cause one) must not end the
        # run before the spike alert: the next run re-detects the snapshot
        # as a local-only commit.
        if ! set_pending_archive_push; then
            echo "jsonl-export: could not mark the HALT snapshot pending for push; the next run re-detects it as a local-only commit" >&2
        fi
    fi
    spike_alert_recorded=1
    if ! set_pending_spike_alert "$HALT_DB" "$HALT_PREV_COUNT" "$HALT_CURRENT_COUNT" "$HALT_DELTA" "$SPIKE_THRESHOLD"; then
        spike_alert_recorded=0
        echo "jsonl-export: could not record the spike alert in state; sending it now, but a failed send cannot be retried from state" >&2
    fi
    if send_spike_alert "$HALT_DB" "$HALT_PREV_COUNT" "$HALT_CURRENT_COUNT" "$HALT_DELTA" "$SPIKE_THRESHOLD"; then
        # Nothing to clear when the record never landed, and on a full disk
        # the clear is one more failing write: under errexit it would end the
        # run before maintenance_done.
        if [ "$spike_alert_recorded" -eq 1 ] && ! clear_pending_spike_alert "$HALT_DB"; then
            echo "jsonl-export: the spike alert was sent but could not be cleared from state; the next run may send it once more" >&2
        fi
    elif [ "$spike_alert_recorded" -eq 1 ]; then
        echo "jsonl-export: spike alert delivery failed; will retry from state" >&2
    else
        echo "jsonl-export: spike alert delivery failed and was never recorded in state; it will not be retried" >&2
    fi
    maintenance_done "jsonl — HALTED on spike detection"
    exit 0
fi

if git diff --cached --quiet 2>/dev/null; then
    if has_pending_archive_push || archive_has_local_only_commits; then
        if should_attempt_push; then
            PUSH_STATUS="ok"
            if ! push_archive_main; then
                PUSH_STATUS="failed"
            fi
        else
            PUSH_STATUS="skipped (local-only)"
        fi
        if [ -n "$FAILED_DBS" ]; then
            EXPORTED_DBS=$((TOTAL_DBS - FAILED_DB_COUNT))
            SUMMARY="jsonl — exported $EXPORTED_DBS/$TOTAL_DBS, records: $TOTAL_EXPORTED, push: $PUSH_STATUS, failed: $(printf '%s' "$FAILED_DBS" | tr '\n' ' ')"
        else
            SUMMARY="jsonl — no changes, push: $PUSH_STATUS"
        fi
        maintenance_done "$SUMMARY"
        echo "jsonl-export: $SUMMARY"
        exit 0
    fi
    if [ -n "$FAILED_DBS" ]; then
        EXPORTED_DBS=$((TOTAL_DBS - FAILED_DB_COUNT))
        SUMMARY="jsonl — exported $EXPORTED_DBS/$TOTAL_DBS, records: $TOTAL_EXPORTED, push: skipped, failed: $(printf '%s' "$FAILED_DBS" | tr '\n' ' ')"
        maintenance_done "$SUMMARY"
        echo "jsonl-export: $SUMMARY"
        exit 0
    fi
    # No changes.
    maintenance_done "jsonl — no changes"
    exit 0
fi

EXPORTED_DBS=$((TOTAL_DBS - FAILED_DB_COUNT))
commit_archive_snapshot \
    "backup $(date -u +%Y-%m-%dT%H:%M:%SZ): exported=$EXPORTED_DBS/$TOTAL_DBS records=$TOTAL_EXPORTED" \
    "archive snapshot" || {
    discard_staged_archive_outputs
    exit 1
}
# A full disk (the repack above can cause one) must not end the run before
# the push and the summary: the next run re-detects the snapshot as a
# local-only commit.
if ! set_pending_archive_push; then
    echo "jsonl-export: could not mark the snapshot pending for push; pushing now, and the next run re-detects it as a local-only commit" >&2
fi

if should_attempt_push; then
    PUSH_STATUS="ok"
    if ! push_archive_main; then
        PUSH_STATUS="failed"
    fi
else
    PUSH_STATUS="skipped (local-only)"
fi

SUMMARY="jsonl — exported $EXPORTED_DBS/$TOTAL_DBS, records: $TOTAL_EXPORTED, push: $PUSH_STATUS"
if [ -n "$FAILED_DBS" ]; then
    SUMMARY="$SUMMARY, failed: $(printf '%s' "$FAILED_DBS" | tr '\n' ' ')"
fi

maintenance_done "$SUMMARY"
echo "jsonl-export: $SUMMARY"
