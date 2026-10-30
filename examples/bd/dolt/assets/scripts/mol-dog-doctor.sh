#!/usr/bin/env bash
# mol-dog-doctor — probe Dolt server health and report findings.
#
# Converted from the former mol-dog-doctor formula. All checks are read-only: SQL probe,
# PROCESSLIST count, disk usage, orphan DB detection, backup artifact freshness.
# No LLM judgment needed — runs inline in the controller.
#
# Runs as an exec order (no LLM, no agent, no wisp).
#
# RPO note: BACKUP_STALE_S (default 43200 = 12h = 2x backup interval) is the
# threshold at which backup age (the newer of the backup remote's manifest and
# the last successful sync's stamp) triggers a [WARN: backup stale] advisory.
# With 6h backup syncs and fail-closed journal corruption recovery, maximum data
# loss on journal corruption without manual intervention is one 6h backup interval.
# If BACKUP_STALE_S exceeds 2x the backup interval, a single missed backup cycle
# is undetected. Keep BACKUP_STALE_S <= 2x the configured backup order
# interval (`interval` in orders/mol-dog-backup.toml, currently 6h).
set -euo pipefail

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
. "$PACK_DIR/assets/scripts/latency.sh"
. "$PACK_DIR/assets/scripts/advisory_state.sh"
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
# Where mol-dog-backup.sh syncs, spelled the way it spells it: the name it
# registers a backup's URL under and writes into a sync stamp.
BACKUP_SYNC_DEST_CITY=$(cd "$GC_CITY_PATH" 2>/dev/null && pwd -P) || BACKUP_SYNC_DEST_CITY="$GC_CITY_PATH"
BACKUP_SYNC_DEST="${GC_BACKUP_ARTIFACT_DIR:-$BACKUP_SYNC_DEST_CITY/.dolt-backup}"
while [ "${BACKUP_SYNC_DEST%/}" != "$BACKUP_SYNC_DEST" ] && [ "$BACKUP_SYNC_DEST" != "/" ]; do
    BACKUP_SYNC_DEST="${BACKUP_SYNC_DEST%/}"
done
# Advisory dedup state (#3409): records the signature of the last-sent [MEDIUM]
# advisory so a persistent condition collapses into one rolling alert instead of
# a fresh bead every 5-min tick. DOLT_STATE_DIR is set by runtime.sh.
ADVISORY_STATE_FILE="${GC_DOCTOR_ADVISORY_STATE_FILE:-$DOLT_STATE_DIR/doctor-advisory-state}"
# Advisory mailbox hygiene: the subject the [MEDIUM] escalation sends under, the
# prefix the superseded-advisory sweep matches (severity-agnostic on purpose, so
# it also catches older sends under a different severity tag), and the recipient
# those sends go to. The recipient is the same env-var-with-default the core
# escalate.sh resolves (RECIPIENT="${GC_ESCALATION_RECIPIENT:-human}"); a pack
# that overrides escalate.sh with a different recipient makes the sweep a
# harmless no-op. The CRITICAL "server unreachable" escalation uses a different
# subject and is never matched by the prefix, so outage mail is never swept.
ADVISORY_SUBJECT="Dolt health advisory [MEDIUM]"
ADVISORY_SUBJECT_PREFIX="Dolt health advisory"
ADVISORY_RECIPIENT="${GC_ESCALATION_RECIPIENT:-human}"

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

# Age of the newest *restorable* backup for a database, which is the mtime of
# the remote's manifest and never the mtime of the newest file of any kind.
# `dolt backup sync` writes chunk data first and adopts it by rewriting the
# manifest last, so a sync killed in between leaves chunk files newer than
# anything the manifest references. Measured on a live city: an hq remote whose
# newest chunk landed at 21:04 carried a manifest still reading 15:04, and the
# manifest did not reference that chunk. Reading file mtimes there reports a
# six-hour-old backup as nine minutes old.
#
# A db-named directory is the remote the backup order configures
# (file://$BACKUP_ARTIFACT_DIR/<db>), so it is dated only by its own root
# manifest, the same file `gc dolt health` reads. With no manifest it is a
# remote whose first sync never completed: it holds nothing restorable and
# reports 0. Nothing else is read for it, because the prefix arms of
# backup_path_matches_db also accept a sibling such as <db>-dev/manifest or a
# moved-aside <db>.old/manifest, and either would date this database by a
# backup that is not its own.
#
# Only layouts without that directory fall back to the prefix-matched files,
# because BACKUP_ARTIFACT_DIR may point at something that is not a Dolt remote
# at all. A matched manifest still wins there and, with none, the newest
# matching file is the only age there is to read. The prefix arms can still
# credit a sibling's files on that path.
backup_commit_mtime_for_db() {
    db_name="$1"
    db_remote_dir="$BACKUP_ARTIFACT_DIR/$db_name"
    if [ -d "$db_remote_dir" ]; then
        if [ -f "$db_remote_dir/manifest" ]; then
            file_mtime "$db_remote_dir/manifest"
        else
            printf '0\n'
        fi
        return 0
    fi
    newest_mtime=0
    manifest_mtime=0
    while IFS= read -r -d '' backup_path; do
        backup_rel_path="${backup_path#$BACKUP_ARTIFACT_DIR/}"
        if backup_path_matches_db "$db_name" "$backup_rel_path"; then
            backup_mtime=$(file_mtime "$backup_path")
            case "$backup_path" in
                */manifest)
                    if [ "$backup_mtime" -gt "$manifest_mtime" ]; then
                        manifest_mtime="$backup_mtime"
                    fi
                    ;;
                *)
                    if [ "$backup_mtime" -gt "$newest_mtime" ]; then
                        newest_mtime="$backup_mtime"
                    fi
                    ;;
            esac
        fi
    done < <(find "$BACKUP_ARTIFACT_DIR" -type f -print0 2>/dev/null)
    if [ "$manifest_mtime" -gt 0 ]; then
        printf '%s\n' "$manifest_mtime"
    else
        printf '%s\n' "$newest_mtime"
    fi
}

# Succeed when the backup backup_commit_mtime_for_db dates for a database is a
# Dolt remote's: its db-named directory's manifest or, without that directory,
# a matching manifest. Only then can the local sync-success stamp (ga-g3p5rm)
# speak for it: the stamp proves a `dolt backup sync`, and a bare-file
# artifact dir has no Dolt remote for a sync to have written.
backup_has_manifest_for_db() {
    db_name="$1"
    if [ -d "$BACKUP_ARTIFACT_DIR/$db_name" ]; then
        [ -f "$BACKUP_ARTIFACT_DIR/$db_name/manifest" ]
        return
    fi
    while IFS= read -r -d '' backup_path; do
        if backup_path_matches_db "$db_name" "${backup_path#$BACKUP_ARTIFACT_DIR/}"; then
            return 0
        fi
    done < <(find "$BACKUP_ARTIFACT_DIR" -type f -name manifest -print0 2>/dev/null)
    return 1
}

append_backup_stale() {
    backup_stale_item="$1"
    if [ -n "$BACKUP_STALE_ITEMS" ]; then
        BACKUP_STALE_ITEMS="$BACKUP_STALE_ITEMS, $backup_stale_item"
    else
        BACKUP_STALE_ITEMS="$backup_stale_item"
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

# --- Step 1: Probe connectivity and measure latency ---

PROBE_START_MS=$(now_ms)
if ! dolt_sql -q "SELECT active_branch()" >/dev/null 2>&1; then
    if send_escalation \
        "ESCALATION: Dolt server unreachable on port $PORT [CRITICAL]" \
        "Doctor probe failed: server did not respond to active_branch() query."; then
        dolt_notify_done "doctor — server: UNREACHABLE (escalated)"
        echo "doctor: server unreachable on port $PORT (escalated)"
    else
        dolt_notify_done "doctor — server: UNREACHABLE (escalation failed)"
        echo "doctor: server unreachable on port $PORT (escalation failed)"
    fi
    exit 0
fi
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

# Succeed when a database has a Dolt backup registered: named <db>-backup, or at
# this database's URL under the sync destination under any name, because `bd
# backup init` names its backup `default`. `dolt backup -v` prints "name url
# {params}" with the URL unquoted, so the URL is what lies between the first field
# and the params, spaces included.
backup_registered_for_db() {
    (cd "$2" && run_bounded 30 dolt backup -v 2>/dev/null) \
        | BACKUP_NAME="$1-backup" BACKUP_URL="file://$BACKUP_SYNC_DEST/$1" awk '
            { n = $1; sub(/^[^[:space:]]+[[:space:]]+/, ""); sub(/[[:space:]]+\{.*\}[[:space:]]*$/, "") }
            n == ENVIRON["BACKUP_NAME"] || $0 == ENVIRON["BACKUP_URL"] { found = 1 }
            END { exit !found }'
}

# Backup freshness: check each database's newest restorable backup, dated by
# the newer of the backup remote's manifest (backup_commit_mtime_for_db) and
# the last successful sync's stamp (ga-g3p5rm). See the loop below.
# Every user database is in scope. DBs without a registered backup
# are reported as a coverage gap rather than silently excluded —
# the exclusion is how unconfigured production DBs went unbacked-up until
# journal corruption made them unrecoverable (#3176). mol-dog-backup.sh
# auto-configures the remote on its next run, so this warning self-heals
# unless the backup dog itself is failing.
BACKUP_ELIGIBLE_DBS=""
BACKUP_STALE_ITEMS=""
for db in $USER_DBS; do
    db_dir="$DOLT_DATA_DIR/$db"
    if [ -d "$db_dir/.dolt" ]; then
        if backup_registered_for_db "$db" "$db_dir"; then
            BACKUP_ELIGIBLE_DBS="$BACKUP_ELIGIBLE_DBS $db"
        else
            append_backup_stale "$db backup remote missing"
        fi
    fi
done
BACKUP_ELIGIBLE_DBS=$(printf '%s\n' "$BACKUP_ELIGIBLE_DBS" | tr ' ' '\n' | grep -v '^$' || true)

BACKUP_STALE=""
if [ -n "$BACKUP_ELIGIBLE_DBS" ]; then
    if [ ! -d "$BACKUP_ARTIFACT_DIR" ]; then
        BACKUP_STALE=" [WARN: backup artifact dir missing]"
    else
        NOW_S=$(date +%s)
        for db in $BACKUP_ELIGIBLE_DBS; do
            # A Dolt remote's backup is dated by the newer of two proofs that a
            # sync completed:
            #   - the remote's manifest (backup_commit_mtime_for_db, upstream
            #     #7381), rewritten last by a sync that adopted new data, so
            #     the orphan chunks a KILLED sync leaves never date it;
            #   - the local sync-success stamp (ga-g3p5rm), written whenever
            #     `dolt backup sync` exits 0. An up-to-date sync does not
            #     rewrite the manifest, so the manifest alone would page on
            #     every idle database.
            # Nothing to date is "missing", whatever the stamp says. A
            # bare-file artifact dir (no manifest) keeps upstream's dating by
            # its newest file, and the stamp does not count there. A stale
            # remote with no stamp at all says so: no sync of it was ever
            # recorded as successful.
            BACKUP_COMMIT_MTIME=$(backup_commit_mtime_for_db "$db")
            if [ "$BACKUP_COMMIT_MTIME" -le 0 ]; then
                append_backup_stale "$db backup missing"
                continue
            fi
            # The stamp counts only while a manifest exists: a bare-file
            # artifact dir (no manifest) is dated by its newest file alone, as
            # upstream dates it, so a fresh stamp cannot make a stale chunk
            # read fresh where no Dolt remote was ever synced.
            # Guarded for `set -euo pipefail`: a missing stamp is normal (a
            # database not synced since the stamp shipped), and an unguarded
            # pipeline over a nonexistent file would abort the doctor pass.
            BACKUP_STAMP_COUNTS=0
            BACKUP_SYNCED_AT=0
            if backup_has_manifest_for_db "$db"; then
                BACKUP_STAMP_COUNTS=1
                if [ -f "$LOCAL_BACKUP_FRESHNESS_DIR/$db" ]; then
                    # A sync to another destination says nothing about this one.
                    if [ "$(sed -n 's/^artifact_dir=//p' "$LOCAL_BACKUP_FRESHNESS_DIR/$db" 2>/dev/null | head -1 || true)" = "$BACKUP_SYNC_DEST" ]; then
                        BACKUP_SYNCED_AT=$(sed -n 's/^synced_at_epoch=//p' \
                            "$LOCAL_BACKUP_FRESHNESS_DIR/$db" 2>/dev/null | head -1 || true)
                    fi
                fi
            fi
            case "$BACKUP_SYNCED_AT" in ''|*[!0-9]*) BACKUP_SYNCED_AT=0 ;; esac
            BACKUP_FRESH_AT="$BACKUP_COMMIT_MTIME"
            if [ "$BACKUP_SYNCED_AT" -gt "$BACKUP_FRESH_AT" ]; then
                BACKUP_FRESH_AT="$BACKUP_SYNCED_AT"
            fi
            BACKUP_AGE=$((NOW_S - BACKUP_FRESH_AT))
            if [ "$BACKUP_AGE" -gt "$BACKUP_STALE_S" ]; then
                if [ "$BACKUP_STAMP_COUNTS" -eq 1 ] && [ "$BACKUP_SYNCED_AT" -le 0 ]; then
                    append_backup_stale "$db backup is $((BACKUP_AGE / 3600))h old (no successful sync recorded)"
                else
                    append_backup_stale "$db backup is $((BACKUP_AGE / 3600))h old"
                fi
            fi
        done
    fi
fi
if [ -n "$BACKUP_STALE_ITEMS" ]; then
    BACKUP_STALE="$BACKUP_STALE [WARN: backup freshness: $BACKUP_STALE_ITEMS]"
fi

# --- Step 3: Compose report and escalate if critical ---

WARNINGS="${LATENCY_WARN}${CONN_WARN}${ORPHAN_WARN}${BACKUP_STALE}"
if [ -n "$WARNINGS" ]; then
    # Dedup (#3409): key on which conditions are active — not their tick-volatile
    # values (exact latency ms, connection count, backup age) — and re-send only
    # when that set changes. Record after a successful send so a failed
    # escalation retries next tick. The CRITICAL "server unreachable" path above
    # is never deduped, so a true outage always alerts.
    ADVISORY_SIG=""
    if [ -n "$LATENCY_WARN" ]; then ADVISORY_SIG="${ADVISORY_SIG}latency "; fi
    if [ -n "$CONN_WARN" ]; then ADVISORY_SIG="${ADVISORY_SIG}conn "; fi
    if [ -n "$ORPHAN_WARN" ]; then ADVISORY_SIG="${ADVISORY_SIG}orphan "; fi
    if [ -n "$BACKUP_STALE" ]; then ADVISORY_SIG="${ADVISORY_SIG}backup "; fi
    if advisory_changed "$ADVISORY_SIG" "$ADVISORY_STATE_FILE"; then
        # Sweep superseded advisories before the fresh send: the new advisory
        # carries the full current status block, so older snapshots (and any
        # backlog minted before the dedup existed) are stale the moment it
        # lands. If the send below then fails, the sig stays unrecorded and the
        # next tick retries both the sweep (a no-op) and the send.
        advisory_archive_superseded "$ADVISORY_SUBJECT_PREFIX" "$ADVISORY_RECIPIENT"
        if send_escalation \
            "$ADVISORY_SUBJECT" \
            "Latency: ${LATENCY_MS}ms${LATENCY_WARN}
Connections: ${CONN_COUNT}/${CONN_MAX}${CONN_WARN}
Disk: ${DISK_USAGE}
Orphan DBs: ${ORPHAN_COUNT}${ORPHAN_WARN}${BACKUP_STALE}"; then
            advisory_record "$ADVISORY_SIG" "$ADVISORY_STATE_FILE"
        fi
    fi
else
    # Healthy: the condition cleared, so no advisory should stay open. Sweep
    # only when a prior advisory was recorded (state file present), so steady
    # healthy ticks cost no extra gc call; then forget the signature so a
    # future condition re-alerts.
    if [ -f "$ADVISORY_STATE_FILE" ]; then
        advisory_archive_superseded "$ADVISORY_SUBJECT_PREFIX" "$ADVISORY_RECIPIENT"
    fi
    advisory_clear "$ADVISORY_STATE_FILE"
fi

SUMMARY="doctor — server: ok, latency: ${LATENCY_MS}ms, conns: ${CONN_COUNT}/${CONN_MAX}, disk: ${DISK_USAGE}, orphans: ${ORPHAN_COUNT}"
dolt_notify_done "$SUMMARY"
echo "doctor: $SUMMARY"
