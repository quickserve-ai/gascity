#!/bin/sh

# Dolt ENOSPC restart guard. The single implementation shared by both callers
# that restart Dolt: `gc dolt restart` (dolt/commands/restart/run.sh) and the
# auto-recovery path, gc-beads-bd.sh op_recover.
#
# Restarting Dolt under ENOSPC does not free disk space, and the recovery cycle
# itself amplifies the failure: every restart triggers a fresh conjoin that can
# drop another partial nbs_table_* file in the backup remote, accelerating the
# disk-full feedback loop. See gastownhall/gascity#2158.
#
# Inputs, set by the caller as shell variables:
#   DATA_DIR  the Dolt data directory. The volume holding it is measured live.
#   LOG_FILE  the Dolt server log. Unset or unreadable means no log evidence.
#
# Environment:
#   GC_DOLT_RESTART_MIN_FREE_MB        refuse while the data volume has less
#                                      than this many MB free (default 1024;
#                                      0 disables the live check)
#   GC_DOLT_RESTART_ENOSPC_WINDOW_MIN  an ENOSPC log line blocks only while its
#                                      time= stamp is within this many minutes
#                                      of now (default 60)

ENOSPC_DEFAULT_MIN_FREE_MB=1024
ENOSPC_DEFAULT_WINDOW_MIN=60
# The tail bounds how much log is read. It decides nothing on its own: with a
# quiet log, 1000 lines spanned 8 days (the 7/22 -> 7/30 case that blocked
# recovery on week-old evidence).
ENOSPC_LOG_TAIL_LINES=1000
ENOSPC_LOG_SIGNATURE='no space left on device|copy_file_range:.*no space|ENOSPC'

# recovery_should_skip_due_to_enospc returns 0 (true) when a Dolt restart must
# be refused and 1 (false) when it may proceed.
#
# Refusal conditions, checked independently:
#   1. Free space on the Dolt data volume, measured now with df, is below
#      GC_DOLT_RESTART_MIN_FREE_MB. A live reading cannot go stale. A volume
#      that cannot be measured refuses.
#   2. An ENOSPC line in the log tail carries a time= stamp within
#      GC_DOLT_RESTART_ENOSPC_WINDOW_MIN minutes of now. A line whose stamp is
#      missing or unparseable counts as recent and is reported as an
#      unparseable timestamp, so it never passes silently.
#
# On refusal it sets ENOSPC_REFUSAL_REASON (one line) and ENOSPC_REFUSAL_DETAIL
# (indented lines: the live free-space reading, the blocking-line count with the
# newest and oldest blocking stamps, and the log path) for the caller to print.
recovery_should_skip_due_to_enospc() {
    ENOSPC_REFUSAL_REASON=""
    ENOSPC_REFUSAL_DETAIL=""
    _enospc_refuse=false

    _enospc_min_mb=${GC_DOLT_RESTART_MIN_FREE_MB:-$ENOSPC_DEFAULT_MIN_FREE_MB}
    _enospc_window_min=${GC_DOLT_RESTART_ENOSPC_WINDOW_MIN:-$ENOSPC_DEFAULT_WINDOW_MIN}
    for _enospc_setting in "GC_DOLT_RESTART_MIN_FREE_MB=$_enospc_min_mb" \
        "GC_DOLT_RESTART_ENOSPC_WINDOW_MIN=$_enospc_window_min"; do
        if ! _enospc_is_count "${_enospc_setting#*=}"; then
            _enospc_add_reason "invalid $_enospc_setting (want a whole number)"
            _enospc_add_detail "fix the setting; the ENOSPC guard cannot evaluate the disk without it"
            return 0
        fi
    done

    if _enospc_measure_free_mb; then
        _enospc_free_line="live free space: $_enospc_free_mb MB on the volume holding $_enospc_probe (minimum $_enospc_min_mb MB)"
        if [ "$_enospc_free_mb" -lt "$_enospc_min_mb" ]; then
            _enospc_refuse=true
            _enospc_add_reason "Dolt data volume has $_enospc_free_mb MB free, below the $_enospc_min_mb MB minimum"
        fi
    else
        _enospc_refuse=true
        _enospc_free_line="live free space: unmeasurable ($_enospc_measure_error)"
        _enospc_add_reason "free space on the Dolt data volume could not be measured"
    fi
    _enospc_add_detail "$_enospc_free_line"

    _enospc_recent=0
    _enospc_unparseable=0
    if [ -n "${LOG_FILE:-}" ] && [ -r "$LOG_FILE" ]; then
        _enospc_scan_log
    fi

    _enospc_recent_line="ENOSPC log lines stamped within the last $_enospc_window_min min: $_enospc_recent"
    if [ "$_enospc_recent" -gt 0 ]; then
        _enospc_refuse=true
        _enospc_add_reason "Dolt log shows $_enospc_recent ENOSPC line(s) stamped within the last $_enospc_window_min min"
        _enospc_recent_line="$_enospc_recent_line (newest $_enospc_newest_stamp, $(_enospc_age "$_enospc_newest_epoch"); oldest $_enospc_oldest_stamp, $(_enospc_age "$_enospc_oldest_epoch"))"
    fi
    _enospc_add_detail "$_enospc_recent_line"
    if [ "$_enospc_unparseable" -gt 0 ]; then
        _enospc_refuse=true
        _enospc_add_reason "Dolt log shows $_enospc_unparseable ENOSPC line(s) with an unparseable timestamp"
        _enospc_add_detail "ENOSPC log lines with an unparseable timestamp, counted as recent: $_enospc_unparseable"
    fi
    if [ -n "${LOG_FILE:-}" ]; then
        _enospc_add_detail "log: $LOG_FILE (last $ENOSPC_LOG_TAIL_LINES lines scanned)"
    fi

    [ "$_enospc_refuse" = true ]
}

# _enospc_is_count succeeds for a whole number without a leading zero, the only
# form shell arithmetic reads the same way everywhere.
_enospc_is_count() {
    case $1 in
        '' | *[!0-9]* | 0?*) return 1 ;;
    esac
    return 0
}

_enospc_add_reason() {
    if [ -n "$ENOSPC_REFUSAL_REASON" ]; then
        ENOSPC_REFUSAL_REASON="$ENOSPC_REFUSAL_REASON; $1"
    else
        ENOSPC_REFUSAL_REASON=$1
    fi
}

_enospc_add_detail() {
    if [ -n "$ENOSPC_REFUSAL_DETAIL" ]; then
        ENOSPC_REFUSAL_DETAIL="$ENOSPC_REFUSAL_DETAIL
  $1"
    else
        ENOSPC_REFUSAL_DETAIL="  $1"
    fi
}

# _enospc_measure_free_mb sets _enospc_free_mb to the MB available on the
# volume holding DATA_DIR. A data dir that does not exist yet is measured at
# its nearest existing ancestor, which sits on the volume it will be created
# on. On failure it sets _enospc_measure_error and returns 1.
_enospc_measure_free_mb() {
    _enospc_free_mb=""
    _enospc_probe=${DATA_DIR:-}
    if [ -z "$_enospc_probe" ]; then
        _enospc_measure_error="Dolt data directory unknown"
        return 1
    fi
    while [ ! -e "$_enospc_probe" ]; do
        _enospc_probe=$(dirname "$_enospc_probe")
    done
    _enospc_avail_kb=$(df -Pk "$_enospc_probe" 2>/dev/null | awk 'NR == 2 { print $4 }')
    if ! _enospc_is_count "$_enospc_avail_kb"; then
        _enospc_measure_error="df -Pk $_enospc_probe gave no available-space reading"
        return 1
    fi
    _enospc_free_mb=$((_enospc_avail_kb / 1024))
}

# _enospc_scan_log counts the ENOSPC lines in the log tail that block a
# restart: _enospc_recent for lines stamped inside the window (tracking the
# newest and oldest of them), _enospc_unparseable for lines whose stamp is
# missing or unreadable. Stamps are parsed once per distinct value.
_enospc_scan_log() {
    _enospc_newest_epoch=""
    _enospc_newest_stamp=""
    _enospc_oldest_epoch=""
    _enospc_oldest_stamp=""
    # Dolt's logrus lines open with an RFC3339 stamp:
    #   time="2026-09-18T07:06:15-07:00" level=error msg="..."
    # A line without one becomes "-", which never parses.
    _enospc_stamps=$(tail -n "$ENOSPC_LOG_TAIL_LINES" "$LOG_FILE" 2>/dev/null \
        | grep -E "$ENOSPC_LOG_SIGNATURE" \
        | sed -e 's/^[[:space:]]*time="\([^"]*\)".*$/\1/' -e 't' -e 's/.*/-/')
    [ -n "$_enospc_stamps" ] || return 0

    _enospc_now=$(date +%s)
    _enospc_cutoff=$((_enospc_now - _enospc_window_min * 60))
    while read -r _enospc_count _enospc_stamp; do
        [ -n "$_enospc_count" ] || continue
        if _enospc_epoch=$(_enospc_stamp_epoch "$_enospc_stamp"); then
            [ "$_enospc_epoch" -ge "$_enospc_cutoff" ] || continue
            _enospc_recent=$((_enospc_recent + _enospc_count))
            if [ -z "$_enospc_newest_epoch" ] || [ "$_enospc_epoch" -gt "$_enospc_newest_epoch" ]; then
                _enospc_newest_epoch=$_enospc_epoch
                _enospc_newest_stamp=$_enospc_stamp
            fi
            if [ -z "$_enospc_oldest_epoch" ] || [ "$_enospc_epoch" -lt "$_enospc_oldest_epoch" ]; then
                _enospc_oldest_epoch=$_enospc_epoch
                _enospc_oldest_stamp=$_enospc_stamp
            fi
        else
            _enospc_unparseable=$((_enospc_unparseable + _enospc_count))
        fi
    done <<EOF
$(printf '%s\n' "$_enospc_stamps" | sort | uniq -c)
EOF
}

# _enospc_stamp_epoch prints the Unix epoch of an RFC3339 stamp with a Z or
# numeric offset (fractional seconds allowed), or returns 1. The shape is
# checked in the shell and normalized to YYYY-MM-DDTHH:MM:SS+HHMM, which GNU
# date -d, BSD date -j -f and python3 all read.
_enospc_stamp_epoch() {
    _enospc_rest=${1#????-??-??T??:??:??}
    [ "$_enospc_rest" != "$1" ] || return 1
    _enospc_base=${1%"$_enospc_rest"}
    case $_enospc_base in
        [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]) ;;
        *) return 1 ;;
    esac
    case $_enospc_rest in
        .*)
            _enospc_frac=${_enospc_rest%%[Z+-]*}
            case $_enospc_frac in
                . | .*[!0-9]*) return 1 ;;
            esac
            _enospc_rest=${_enospc_rest#"$_enospc_frac"}
            ;;
    esac
    case $_enospc_rest in
        Z) _enospc_offset=+0000 ;;
        [+-][0-9][0-9]:[0-9][0-9]) _enospc_offset="${_enospc_rest%:*}${_enospc_rest#*:}" ;;
        [+-][0-9][0-9][0-9][0-9]) _enospc_offset=$_enospc_rest ;;
        *) return 1 ;;
    esac
    _enospc_normalized="$_enospc_base$_enospc_offset"
    _enospc_parsed=$(date -u -d "$_enospc_normalized" '+%s' 2>/dev/null \
        || date -u -j -f '%Y-%m-%dT%H:%M:%S%z' "$_enospc_normalized" '+%s' 2>/dev/null \
        || python3 -c 'import datetime,sys; print(int(datetime.datetime.strptime(sys.argv[1], "%Y-%m-%dT%H:%M:%S%z").timestamp()))' "$_enospc_normalized" 2>/dev/null) \
        || return 1
    _enospc_is_count "$_enospc_parsed" || return 1
    printf '%s\n' "$_enospc_parsed"
}

# _enospc_age renders how long ago an epoch was, for the refusal detail.
_enospc_age() {
    _enospc_elapsed=$((_enospc_now - $1))
    if [ "$_enospc_elapsed" -lt 0 ]; then
        printf 'ahead of the local clock'
    else
        printf '%s min ago' "$((_enospc_elapsed / 60))"
    fi
}
