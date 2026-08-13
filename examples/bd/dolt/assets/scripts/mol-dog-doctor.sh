#!/usr/bin/env bash
# mol-dog-doctor — probe Dolt server health and report findings.
#
# Converted from the former mol-dog-doctor formula. All checks are read-only: SQL probe,
# PROCESSLIST count, disk usage, orphan DB detection, backup artifact freshness.
# No LLM judgment needed — runs inline in the controller.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
. "$PACK_DIR/assets/scripts/latency.sh"
. "$PACK_DIR/assets/scripts/_notify.sh"

PORT="$GC_DOLT_PORT"
HOST="${GC_DOLT_HOST:-127.0.0.1}"
USER="${GC_DOLT_USER:-root}"
# Latency warn threshold in milliseconds. GC_DOCTOR_LATENCY_WARN_MS takes
# precedence; otherwise derive from the legacy seconds knob (default 1s ->
# 1000ms) for backward compatibility.
LATENCY_WARN_MS="${GC_DOCTOR_LATENCY_WARN_MS:-$(( ${GC_DOCTOR_LATENCY_WARN_S:-1} * 1000 ))}"
CONN_WARN_PCT="${GC_DOCTOR_CONN_WARN_PCT:-80}"
BACKUP_STALE_S="${GC_DOCTOR_BACKUP_STALE_S:-43200}"  # 2x 6h backup interval
BACKUP_ARTIFACT_DIR="${GC_BACKUP_ARTIFACT_DIR:-$GC_CITY_PATH/.dolt-backup}"

dolt_sql() {
    DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" \
        run_bounded 10 \
        dolt --host "$HOST" --port "$PORT" --user "$USER" --no-tls sql "$@"
}

# CONN_MAX: explicit override > server @@GLOBAL.max_connections > fallback.
if [ -n "${GC_DOCTOR_CONN_MAX:-}" ]; then
    CONN_MAX="$GC_DOCTOR_CONN_MAX"
else
    _server_max=$(dolt_sql -r csv -q "SELECT @@GLOBAL.max_connections" 2>/dev/null | tail -1 || true)
    case "${_server_max:-}" in
        ''|*[!0-9]*) CONN_MAX=256 ;;
        *) CONN_MAX="$_server_max" ;;
    esac
    unset _server_max
fi

file_mtime() {
    file_path="$1"
    file_mtime_value=$(stat -c %Y "$file_path" 2>/dev/null \
        || stat -f %m "$file_path" 2>/dev/null || echo "0")
    case "$file_mtime_value" in
        ''|*[!0-9]*) file_mtime_value=0 ;;
    esac
    printf '%s\n' "$file_mtime_value"
}

backup_path_matches_db() {
    db_name="$1"
    backup_rel_path="$2"
    case "$backup_rel_path" in
        "$db_name"|"$db_name"/*|"$db_name".*|"$db_name"-*|*"/$db_name"|*"/$db_name"/*|*"/$db_name".*|*"/$db_name"-*)
            return 0
            ;;
    esac
    return 1
}

newest_backup_mtime_for_db() {
    db_name="$1"
    newest_mtime=0
    while IFS= read -r -d '' backup_path; do
        backup_rel_path="${backup_path#$BACKUP_ARTIFACT_DIR/}"
        if backup_path_matches_db "$db_name" "$backup_rel_path"; then
            backup_mtime=$(file_mtime "$backup_path")
            if [ "$backup_mtime" -gt "$newest_mtime" ]; then
                newest_mtime="$backup_mtime"
            fi
        fi
    done < <(find "$BACKUP_ARTIFACT_DIR" -type f -print0 2>/dev/null)
    printf '%s\n' "$newest_mtime"
}

append_backup_stale() {
    backup_stale_item="$1"
    if [ -n "$BACKUP_STALE_ITEMS" ]; then
        BACKUP_STALE_ITEMS="$BACKUP_STALE_ITEMS, $backup_stale_item"
    else
        BACKUP_STALE_ITEMS="$backup_stale_item"
    fi
    # Second argument is the LATCH SIGNATURE token — a stable identity for this
    # item, deliberately distinct from the human-readable text above. The prose
    # carries a drifting age ("hq backup is 13h old" becomes "14h old" an hour
    # later); a signature built from it would change every hour, never match the
    # marker, and latch nothing. See the advisory-latch block below.
    backup_stale_sig_token="${2:-unclassified}"
    if [ -n "$BACKUP_STALE_SIG_ITEMS" ]; then
        BACKUP_STALE_SIG_ITEMS="$BACKUP_STALE_SIG_ITEMS,$backup_stale_sig_token"
    else
        BACKUP_STALE_SIG_ITEMS="$backup_stale_sig_token"
    fi
}

send_escalation() {
    local subject="$1"
    local message="$2"
    local err
    if ! err=$(dolt_escalate "$subject" "$message" 2>&1 >/dev/null); then
        if [ -n "$err" ]; then
            echo "doctor: escalation failed: $err" >&2
        else
            echo "doctor: escalation failed" >&2
        fi
        return 1
    fi
}

# --- Advisory latch (ga-466izs) ---
#
# WHY THIS EXISTS — this advisory had no latch and no dedupe. Any warning that
# is STICKY rather than transient produced one operator mail every 5 minutes for
# as long as the condition held: 288/day. Measured 2026-08-13, 43 MEDIUM mails
# in 4h once ga-g3p5rm's fail-closed backup check began (correctly) reporting
# UNVERIFIED, and again the same day on sustained latency.
#
# The mail volume is not the failure. Alarm fatigue is: an advisory that arrives
# 288x/day trains the operator to ignore the channel, so the NEXT genuine MEDIUM
# is invisible (ga-mlvzm3 — the human mailbox is already a dead-letter box).
#
# Four properties, three of them copied from the sibling that already solved
# this (packs/cherub-law/assets/scripts/dolt-mirror-alarm.sh):
#
#   PER CLASS, not aggregate. latency / connections / orphans / backup latch
#   independently. Latching the aggregate would let one sticky backup warning
#   suppress a genuinely NEW latency warning — trading a mail storm for a
#   blind spot, which is a worse bug than the one being fixed.
#
#   SIGNATURE, not mere existence. A materially different incident still pages
#   while a repeat of the same one stays quiet.
#
#   BANDED, not raw — the trap that makes a naive signature useless. Latency
#   reads 1054ms, then 1010ms, then 3160ms; connection counts and backup ages
#   drift every cycle. A signature carrying the raw measurement changes every
#   pass, never matches its marker, and suppresses NOTHING — the fix would read
#   as correct and latch zero mails. So signatures carry a severity BAND or a
#   stable identity (<db>:<kind>): a steady incident is one signature, while a
#   materially WORSENING one crosses a band and re-pages.
#
#   HEARTBEAT. A latched incident re-sends every GC_DOCTOR_ADVISORY_REPEAT_S
#   (default 6h) so a long-running condition is never silently forgotten.
#   Worst case 4 mails/day instead of 288.
#
# Latch ONLY after mail is actually delivered (the sibling's rule — an
# undelivered alert that latches is a lost alert), and re-arm a class the moment
# it stops warning, so a condition that clears and recurs pages again.
DOCTOR_LATCH_DIR="${GC_DOCTOR_LATCH_DIR:-$PACK_STATE_DIR/doctor-advisory-latch}"
ADVISORY_REPEAT_S="${GC_DOCTOR_ADVISORY_REPEAT_S:-21600}"
ADVISORY_ACTIVE=""
ADVISORY_ACTIVE_CLASSES=""

# advisory_band <value> <threshold> -> warn | high | severe
# Collapses a drifting measurement into a stable severity band so that only a
# materially worse incident changes the signature.
advisory_band() {
    ab_value="$1"
    ab_threshold="$2"
    case "$ab_value" in ''|*[!0-9]*) ab_value=0 ;; esac
    case "$ab_threshold" in ''|*[!0-9]*) ab_threshold=0 ;; esac
    if [ "$ab_threshold" -le 0 ]; then
        printf 'warn\n'
        return 0
    fi
    if [ "$ab_value" -ge $((ab_threshold * 5)) ]; then
        printf 'severe\n'
    elif [ "$ab_value" -ge $((ab_threshold * 2)) ]; then
        printf 'high\n'
    else
        printf 'warn\n'
    fi
}

# advisory_mark_active <class> <signature>
advisory_mark_active() {
    ADVISORY_ACTIVE="${ADVISORY_ACTIVE}$1 $2
"
    ADVISORY_ACTIVE_CLASSES="$ADVISORY_ACTIVE_CLASSES $1"
}

# advisory_class_should_send <class> <signature>
# 0 = this class justifies a mail (new, changed, or heartbeat due), 1 = latched.
advisory_class_should_send() {
    acs_marker="$DOCTOR_LATCH_DIR/$1"
    if [ ! -f "$acs_marker" ]; then
        return 0
    fi
    acs_prev=$(sed -n '1p' "$acs_marker" 2>/dev/null || true)
    if [ "$acs_prev" != "$2" ]; then
        return 0
    fi
    acs_age=$(( $(date +%s) - $(file_mtime "$acs_marker") ))
    if [ "$acs_age" -ge "$ADVISORY_REPEAT_S" ]; then
        return 0
    fi
    return 1
}

# advisory_latch <class> <signature>
# Best-effort: a marker write must never fail an otherwise-delivered advisory.
# tmp+mv keeps a concurrent read from seeing a torn file.
advisory_latch() {
    mkdir -p "$DOCTOR_LATCH_DIR" 2>/dev/null || return 0
    printf '%s\n' "$2" > "$DOCTOR_LATCH_DIR/$1.tmp" 2>/dev/null || {
        rm -f "$DOCTOR_LATCH_DIR/$1.tmp" 2>/dev/null
        return 0
    }
    mv -f "$DOCTOR_LATCH_DIR/$1.tmp" "$DOCTOR_LATCH_DIR/$1" 2>/dev/null || return 0
}

# advisory_rearm <class> — drop the marker so the next occurrence pages.
advisory_rearm() {
    rm -f "$DOCTOR_LATCH_DIR/$1" 2>/dev/null || true
}

# --- Step 1: Probe connectivity and measure latency ---

PROBE_START_MS=$(now_ms)
if ! dolt_sql -q "SELECT active_branch()" >/dev/null 2>&1; then
    # The CRITICAL path is latched on the same terms as the MEDIUM advisory
    # below: an unreachable server is the stickiest condition this script can
    # observe, so without a latch a multi-hour outage mails the operator every
    # 5 minutes. The heartbeat still re-pages it, and a delivery failure does
    # not latch, so an outage is never silently swallowed.
    if advisory_class_should_send unreachable "port:$PORT"; then
        if send_escalation \
            "ESCALATION: Dolt server unreachable on port $PORT [CRITICAL]" \
            "Doctor probe failed: server did not respond to active_branch() query."; then
            advisory_latch unreachable "port:$PORT"
            dolt_notify_done "doctor — server: UNREACHABLE (escalated)"
            echo "doctor: server unreachable on port $PORT (escalated)"
        else
            dolt_notify_done "doctor — server: UNREACHABLE (escalation failed)"
            echo "doctor: server unreachable on port $PORT (escalation failed)"
        fi
    else
        dolt_notify_done "doctor — server: UNREACHABLE (alert latched)"
        echo "doctor: server unreachable on port $PORT (alert latched)"
    fi
    exit 0
fi
advisory_rearm unreachable
PROBE_END_MS=$(now_ms)
LATENCY_MS=$((PROBE_END_MS - PROBE_START_MS))
LATENCY_WARN=""
if latency_should_warn "$LATENCY_MS" "$LATENCY_WARN_MS"; then
    LATENCY_WARN=" [WARN: latency ${LATENCY_MS}ms >= threshold ${LATENCY_WARN_MS}ms]"
fi

# --- Step 2: Check resource conditions ---

CONN_COUNT=$(dolt_sql -r csv -q "SELECT COUNT(*) FROM information_schema.PROCESSLIST" 2>/dev/null \
    | tail -1 || echo "0")
CONN_WARN=""
CONN_WARN_AT=$(( (CONN_MAX * CONN_WARN_PCT) / 100 ))
if [ "${CONN_COUNT:-0}" -ge "$CONN_WARN_AT" ]; then
    CONN_WARN=" [WARN: ${CONN_COUNT} connections >= ${CONN_WARN_PCT}% of max ${CONN_MAX}]"
fi

# Disk usage of Dolt data directory.
DISK_USAGE=$(du -sh "$DOLT_DATA_DIR" 2>/dev/null | cut -f1 || echo "unknown")

# Orphan database detection.
ALL_DBS=$(dolt_sql -r csv -q "SHOW DATABASES" 2>/dev/null | tail -n +2 || true)
ORPHAN_PATTERNS="^(testdb_|beads_t|beads_pt|beads_vr|doctest_|doctortest_)"
SYSTEM_DBS="^(information_schema|mysql|dolt_cluster|__gc_probe|performance_schema|sys)$"
USER_DBS=$(printf '%s\n' "$ALL_DBS" | grep -viE "$SYSTEM_DBS" || true)
ORPHANS=$(printf '%s\n' "$USER_DBS" | grep -iE "$ORPHAN_PATTERNS" || true)
ORPHAN_COUNT=$(printf '%s\n' "$ORPHANS" | awk 'NF {count++} END {print count + 0}')
ORPHAN_WARN=""
if [ "${ORPHAN_COUNT:-0}" -gt 0 ]; then
    ORPHAN_WARN=" [WARN: $ORPHAN_COUNT orphan DBs detected — run gc dolt cleanup]"
fi

# Backup freshness: check newest backup artifact per database.
# Every user database is in scope. DBs without a configured <db>-backup
# remote are reported as a coverage gap rather than silently excluded —
# the exclusion is how unconfigured production DBs went unbacked-up until
# journal corruption made them unrecoverable (#3176). mol-dog-backup.sh
# auto-configures the remote on its next run, so this warning self-heals
# unless the backup dog itself is failing.
BACKUP_ELIGIBLE_DBS=""
BACKUP_STALE_ITEMS=""
BACKUP_STALE_SIG_ITEMS=""
for db in $USER_DBS; do
    db_dir="$DOLT_DATA_DIR/$db"
    if [ -d "$db_dir/.dolt" ]; then
        if (cd "$db_dir" && run_bounded 30 dolt backup 2>/dev/null | awk '{print $1}' | grep -qx "${db}-backup"); then
            BACKUP_ELIGIBLE_DBS="$BACKUP_ELIGIBLE_DBS $db"
        else
            append_backup_stale "$db backup remote missing" "$db:remote-missing"
        fi
    fi
done
BACKUP_ELIGIBLE_DBS=$(printf '%s\n' "$BACKUP_ELIGIBLE_DBS" | tr ' ' '\n' | grep -v '^$' || true)

BACKUP_STALE=""
if [ -n "$BACKUP_ELIGIBLE_DBS" ]; then
    if [ ! -d "$BACKUP_ARTIFACT_DIR" ]; then
        BACKUP_STALE=" [WARN: backup artifact dir missing]"
        BACKUP_STALE_SIG_ITEMS="artifact-dir:missing"
    else
        NOW_S=$(date +%s)
        for db in $BACKUP_ELIGIBLE_DBS; do
            # ga-g3p5rm: age comes from the sync-success stamp, not from the
            # newest FILE mtime under the artifact dir. newest_backup_mtime_for_db
            # is satisfied by the orphan chunks a KILLED sync leaves behind, so
            # the advisory inherited health's false green. FAIL CLOSED — treat a
            # missing stamp as "never proven", not as fresh.
            # Guarded for `set -euo pipefail`: a MISSING stamp is the normal
            # case (and the whole point of failing closed), but an unguarded
            # pipeline over a nonexistent file exits non-zero and would abort
            # the doctor pass.
            NEWEST_BACKUP_MTIME=0
            if [ -f "$LOCAL_BACKUP_FRESHNESS_DIR/$db" ]; then
                NEWEST_BACKUP_MTIME=$(sed -n 's/^synced_at_epoch=//p' \
                    "$LOCAL_BACKUP_FRESHNESS_DIR/$db" 2>/dev/null | head -1 || true)
            fi
            case "$NEWEST_BACKUP_MTIME" in ''|*[!0-9]*) NEWEST_BACKUP_MTIME=0 ;; esac
            if [ "$NEWEST_BACKUP_MTIME" -le 0 ]; then
                if [ "$(newest_backup_mtime_for_db "$db")" -gt 0 ]; then
                    # Artifacts exist but no sync ever proved itself: exactly the
                    # hq shape — gigabytes on disk, none of it known-good.
                    append_backup_stale "$db backup UNVERIFIED (artifacts present, no successful sync recorded)" "$db:unverified"
                else
                    append_backup_stale "$db backup missing" "$db:missing"
                fi
                continue
            fi
            BACKUP_AGE=$((NOW_S - NEWEST_BACKUP_MTIME))
            if [ "$BACKUP_AGE" -gt "$BACKUP_STALE_S" ]; then
                append_backup_stale "$db backup is $((BACKUP_AGE / 3600))h old" "$db:stale"
            fi
        done
    fi
fi
if [ -n "$BACKUP_STALE_ITEMS" ]; then
    BACKUP_STALE="$BACKUP_STALE [WARN: backup freshness: $BACKUP_STALE_ITEMS]"
fi

# --- Step 3: Compose report and escalate if critical ---

WARNINGS="${LATENCY_WARN}${CONN_WARN}${ORPHAN_WARN}${BACKUP_STALE}"

# Register each warning class with a signature that is stable while the incident
# is unchanged. See the advisory-latch block above for why these are banded
# rather than carrying the raw measurement.
if [ -n "$LATENCY_WARN" ]; then
    advisory_mark_active latency "over:$(advisory_band "$LATENCY_MS" "$LATENCY_WARN_MS")"
fi
if [ -n "$CONN_WARN" ]; then
    advisory_mark_active connections "over:$(advisory_band "$CONN_COUNT" "$CONN_WARN_AT")"
fi
if [ -n "$ORPHAN_WARN" ]; then
    advisory_mark_active orphans "count:$(advisory_band "$ORPHAN_COUNT" 10)"
fi
if [ -n "$BACKUP_STALE" ]; then
    # Sorted so that a change in SHOW DATABASES ordering is not mistaken for a
    # new incident.
    advisory_mark_active backup "$(printf '%s' "$BACKUP_STALE_SIG_ITEMS" \
        | tr ',' '\n' | sort | tr '\n' ',' | sed 's/,$//')"
fi

if [ -n "$WARNINGS" ]; then
    ADVISORY_SEND=false
    ADVISORY_LATCHED_CLASSES=""
    while IFS=' ' read -r advisory_class advisory_signature; do
        [ -n "$advisory_class" ] || continue
        if advisory_class_should_send "$advisory_class" "$advisory_signature"; then
            ADVISORY_SEND=true
        else
            ADVISORY_LATCHED_CLASSES="$ADVISORY_LATCHED_CLASSES $advisory_class"
        fi
    done <<EOF
$ADVISORY_ACTIVE
EOF
    if [ "$ADVISORY_SEND" = true ]; then
        if send_escalation \
            "Dolt health advisory [MEDIUM]" \
            "Latency: ${LATENCY_MS}ms${LATENCY_WARN}
Connections: ${CONN_COUNT}/${CONN_MAX}${CONN_WARN}
Disk: ${DISK_USAGE}
Orphan DBs: ${ORPHAN_COUNT}${ORPHAN_WARN}${BACKUP_STALE}"; then
            # Latch only what was actually delivered. On failure nothing is
            # latched, so the next cycle retries rather than going quiet.
            while IFS=' ' read -r advisory_class advisory_signature; do
                [ -n "$advisory_class" ] || continue
                advisory_latch "$advisory_class" "$advisory_signature"
            done <<EOF
$ADVISORY_ACTIVE
EOF
        fi
    else
        echo "doctor: advisory suppressed — warning classes latched:${ADVISORY_LATCHED_CLASSES}"
    fi
fi

# Re-arm every class that is not currently warning, so a condition that clears
# and later recurs pages again instead of staying silently latched.
for advisory_class in latency connections orphans backup; do
    case " $ADVISORY_ACTIVE_CLASSES " in
        *" $advisory_class "*) ;;
        *) advisory_rearm "$advisory_class" ;;
    esac
done

SUMMARY="doctor — server: ok, latency: ${LATENCY_MS}ms, conns: ${CONN_COUNT}/${CONN_MAX}, disk: ${DISK_USAGE}, orphans: ${ORPHAN_COUNT}"
dolt_notify_done "$SUMMARY"
echo "doctor: $SUMMARY"
