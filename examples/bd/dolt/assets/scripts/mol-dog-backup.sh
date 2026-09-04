#!/usr/bin/env bash
# mol-dog-backup — sync Dolt databases to backup remotes and offsite storage.
#
# Converted from the former mol-dog-backup formula. All operations are deterministic:
# dolt backup sync per DB, rsync backup artifacts to offsite path. No LLM judgment needed.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
. "$PACK_DIR/assets/scripts/_notify.sh"

PORT="$GC_DOLT_PORT"
HOST="${GC_DOLT_HOST:-127.0.0.1}"
USER="${GC_DOLT_USER:-root}"
OFFSITE_PATH="${GC_BACKUP_OFFSITE_PATH:-}"
BACKUP_ARTIFACT_DIR="${GC_BACKUP_ARTIFACT_DIR:-$GC_CITY_PATH/.dolt-backup}"
SYSTEM_DBS="^(information_schema|mysql|dolt_cluster|__gc_probe|performance_schema|sys)$"
MIN_DOLT_BACKUP_VERSION="2.1.0"
BACKUP_LOCK_FILE="${GC_DOLT_BACKUP_LOCK_FILE:-$GC_CITY_PATH/.gc/runtime/packs/dolt/backup-sync.lock}"
BACKUP_LOCK_WAIT_SECONDS="${GC_DOLT_BACKUP_LOCK_WAIT_SECONDS:-5}"
# Per-database `dolt backup sync` bound, seconds (ga-g3p5rm). Was a hardcoded
# 120 that hq could never finish inside. The order's own cadence is 6h, so a
# 30-minute ceiling still leaves ample headroom while remaining bounded — this
# holds the backup flock, so it must never be unbounded.
BACKUP_SYNC_TIMEOUT="${GC_DOLT_BACKUP_SYNC_TIMEOUT_SECONDS:-1800}"
case "$BACKUP_SYNC_TIMEOUT" in ''|*[!0-9]*|0) BACKUP_SYNC_TIMEOUT=1800 ;; esac
# Copy-on-write snapshot staging dir (ga-npnoeo). MUST live outside
# DOLT_DATA_DIR — anything under the data dir is discovered by the running
# sql-server as a database — and on the same filesystem, or the clone fails.
BACKUP_SNAPSHOT_DIR="${GC_DOLT_BACKUP_SNAPSHOT_DIR:-$PACK_STATE_DIR/backup-snapshot}"
BACKUP_SNAPSHOT_MODE="${GC_DOLT_BACKUP_SNAPSHOT:-auto}"
BACKUP_PRUNE_ORPHANS="${GC_DOLT_BACKUP_PRUNE_ORPHANS:-1}"
# ga-l9smko. When 1, a run that produced NO off-box copy and carries no operator
# waiver exits non-zero, so its history row differs from a run that actually
# copied off-box. DEFAULT 0, and that default is an evidenced choice rather than
# timidity: an order that is red on every cycle for a condition its owner cannot
# discharge is what got qcore/origin PARKED, and the park is what made a real
# outage invisible for three days (ga-8bt85j). The UNCONDITIONAL half of this
# change is what removes the false green — a real-failure exit, a stamp only a
# real copy writes, and readers that cannot go green without one. This knob is
# the city's policy call on top of that, one line in [[orders.overrides]] env.
OFFSITE_REQUIRED="${GC_BACKUP_OFFSITE_REQUIRED:-0}"
case "$OFFSITE_REQUIRED" in 1|true|TRUE|yes|YES) OFFSITE_REQUIRED=1 ;; *) OFFSITE_REQUIRED=0 ;; esac

dolt_sql() {
    DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" \
        run_bounded 30 \
        dolt --host "$HOST" --port "$PORT" --user "$USER" --no-tls sql "$@"
}

dolt_version_at_least() {
    current="${1#v}"
    minimum="$2"
    current="${current%%+*}"
    minimum="${minimum%%+*}"
    case "$current" in
        *-*) return 1 ;;
    esac
    IFS=. read -r cur_major cur_minor cur_patch <<EOF
$current
EOF
    IFS=. read -r min_major min_minor min_patch <<EOF
$minimum
EOF
    for part in "$cur_major" "$cur_minor" "$cur_patch" "$min_major" "$min_minor" "$min_patch"; do
        case "$part" in
            ''|*[!0-9]*) return 1 ;;
        esac
    done
    cur_major=$((10#$cur_major))
    cur_minor=$((10#$cur_minor))
    cur_patch=$((10#$cur_patch))
    min_major=$((10#$min_major))
    min_minor=$((10#$min_minor))
    min_patch=$((10#$min_patch))
    if [ "$cur_major" -ne "$min_major" ]; then
        [ "$cur_major" -gt "$min_major" ]
        return $?
    fi
    if [ "$cur_minor" -ne "$min_minor" ]; then
        [ "$cur_minor" -gt "$min_minor" ]
        return $?
    fi
    [ "$cur_patch" -ge "$min_patch" ]
}

append_failed_db() {
    db_failure="$1"
    FAILED=$((FAILED + 1))
    if [ -n "$FAILED_DBS" ]; then
        FAILED_DBS="$FAILED_DBS, $db_failure"
    else
        FAILED_DBS="$db_failure"
    fi
}

acquire_backup_lock() {
    case "$BACKUP_LOCK_WAIT_SECONDS" in
        ''|*[!0-9]*) BACKUP_LOCK_WAIT_SECONDS=5 ;;
    esac
    if ! command -v flock >/dev/null 2>&1; then
        SUMMARY="backup — flock-missing"
        dolt_escalate \
            "Dolt backup: flock missing for backup sync [HIGH]" \
            "Skipping backup sync because flock is unavailable; concurrent dolt backup sync can overload the shared sql-server." \
            2>/dev/null || true
        dolt_notify_done "$SUMMARY"
        echo "backup: $SUMMARY"
        exit 1
    fi

    mkdir -p "$(dirname "$BACKUP_LOCK_FILE")"
    exec 9>"$BACKUP_LOCK_FILE"
    if ! flock -w "$BACKUP_LOCK_WAIT_SECONDS" 9; then
        SUMMARY="backup — skipped: already running"
        dolt_notify_done "$SUMMARY"
        echo "backup: $SUMMARY"
        exit 0
    fi
}

# --- Step 1: Preflight Dolt version before backup sync ---

DOLT_VERSION="$(dolt version 2>/dev/null | awk 'NR == 1 {print $NF}' || true)"
if ! dolt_version_at_least "$DOLT_VERSION" "$MIN_DOLT_BACKUP_VERSION"; then
    dolt_escalate \
        "Dolt backup: dolt-too-old for backup sync [HIGH]" \
        "Skipping backup sync: dolt version ${DOLT_VERSION:-unknown} is below required ${MIN_DOLT_BACKUP_VERSION}. Gas City requires this managed Dolt floor before backup sync." \
        2>/dev/null || true
    SUMMARY="backup — dolt-too-old: ${DOLT_VERSION:-unknown}, required: $MIN_DOLT_BACKUP_VERSION"
    dolt_notify_done "$SUMMARY"
    echo "backup: $SUMMARY"
    exit 1
fi

acquire_backup_lock

# --- Step 2: Sync databases to backup remotes ---

# If GC_BACKUP_DATABASES is set, use it; otherwise auto-discover every user
# database in the data dir. Discovery used to require an existing <db>-backup
# remote, silently excluding unconfigured DBs from backup coverage — which is
# how production DBs ended up unrecoverable after journal corruption (#3176:
# beads_hq had no named remote, so it was never synced). DBs without the
# remote now get one auto-configured below.
if [ -n "${GC_BACKUP_DATABASES:-}" ]; then
    DATABASES=$(echo "$GC_BACKUP_DATABASES" | tr ',' '\n' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | grep -v '^$' || true)
else
    ALL_DBS=$(dolt_sql -r csv -q "SHOW DATABASES" 2>/dev/null | tail -n +2 | \
        grep -viE "$SYSTEM_DBS" || true)
    DATABASES=""
    for db in $ALL_DBS; do
        if [ -d "$DOLT_DATA_DIR/$db/.dolt" ]; then
            DATABASES="$DATABASES $db"
        fi
    done
    DATABASES=$(echo "$DATABASES" | tr ' ' '\n' | grep -v '^$' || true)
fi

if [ -z "$DATABASES" ]; then
    echo "backup: no databases found, skipping"
    exit 0
fi

# ensure_backup_remote guarantees db has a named <db>-backup remote, creating
# one under the backup artifact dir when missing. Auto-configuration is logged
# loudly so operators can see when coverage was established rather than
# assumed. Returns 1 when the remote cannot be configured.
ensure_backup_remote() {
    remote_db="$1"
    remote_db_dir="$DOLT_DATA_DIR/$remote_db"
    [ -d "$remote_db_dir/.dolt" ] || return 0 # sync loop reports not-found
    if (cd "$remote_db_dir" && run_bounded 30 dolt backup 2>/dev/null | awk '{print $1}' | grep -qx "${remote_db}-backup"); then
        return 0
    fi
    remote_url="file://$BACKUP_ARTIFACT_DIR/$remote_db"
    mkdir -p "$BACKUP_ARTIFACT_DIR/$remote_db"
    if (cd "$remote_db_dir" && run_bounded 30 dolt backup add "${remote_db}-backup" "$remote_url" >/dev/null 2>&1); then
        echo "backup: auto-configured missing backup remote ${remote_db}-backup -> $remote_url"
        return 0
    fi
    return 1
}

# --- Copy-on-write snapshot staging (ga-npnoeo) ---
#
# `dolt backup sync` run inside a live database directory does NOT touch files
# directly: the CLI finds the sql-server marker at the DATA DIR root
# ($DOLT_DATA_DIR/.dolt/sql-server.info) and proxies the whole operation
# through the running server. Proof: `dolt sql -q "show databases"` from inside
# .beads/dolt/hq lists as, qcore and mysql — databases that directory knows
# nothing about.
#
# That proxy is the reason hq could not be backed up at all. The sync then has
# to stream ~11 GB through one server connection, and the connection dies long
# before the data does — observed three ways on 2026-08-12/13: a 97s server
# crash, a ~7min `Error 1105: connection was closed` with the server surviving,
# and repeated timeout kills. Raising the timeout cannot fix it, because the
# limit is the connection window, not the clock. The same one-connection
# ceiling is why the mirror push fails (ga-nqm4gv) — one root cause, two planes.
#
# A copy of the database OUTSIDE the data dir has no server marker above it, so
# dolt falls back to direct file access and no connection exists to drop. On
# APFS `cp -c` clones by reference: measured 11 GB in 0.051s at zero disk cost,
# and the sync that had never once completed finished in 8s.
#
# The snapshot is crash-consistent, not transactionally consistent. For a
# content-addressed, append-mostly store that yields a valid slightly-older
# root — strictly better than the 7-day-old alternative it replaces.
#
# Every guard below is written against a HOSTILE value of
# GC_DOLT_BACKUP_SNAPSHOT_DIR, because this function's output feeds an `rm -rf`.
# Lexical prefix tests are not enough: they cannot see through `..` or symlinks,
# and they treat an ANCESTOR of the data dir as unrelated to it. Both mistakes
# were live in the first version of this change — setting the override to the
# data dir made the cleanup delete a live database.
snapshot_root_is_safe() {
    [ -n "$BACKUP_SNAPSHOT_DIR" ] || return 1
    case "$BACKUP_SNAPSHOT_DIR" in
        /*) : ;;
        *) return 1 ;;   # relative roots resolve against an unknown cwd
    esac
    mkdir -p "$BACKUP_SNAPSHOT_DIR" 2>/dev/null || return 1
    snapshot_canon="$(canonical_path "$BACKUP_SNAPSHOT_DIR")" || return 1
    data_canon="$(canonical_path "$DOLT_DATA_DIR")" || return 1
    [ -n "$snapshot_canon" ] && [ -n "$data_canon" ] || return 1
    [ "$snapshot_canon" != "/" ] || return 1
    [ "$snapshot_canon" != "$data_canon" ] || return 1
    # Beneath the data dir: the sql-server would discover the clones as
    # databases.
    case "$snapshot_canon/" in
        "$data_canon"/*) return 1 ;;
    esac
    # ABOVE the data dir: the stale-snapshot sweep deletes every direct child
    # of the root, so an ancestor root would delete the data dir itself.
    case "$data_canon/" in
        "$snapshot_canon"/*) return 1 ;;
    esac
    return 0
}

# discard_db_snapshot removes a staged clone. It resolves the path first and
# requires it to be a PROPER DESCENDANT of a snapshot root that is itself safe,
# so neither a mis-set root nor a database named `../victim` can turn this into
# an rm -rf of live data.
discard_db_snapshot() {
    snapshot_path="$1"
    [ -n "$snapshot_path" ] || return 0
    # Reject traversal before resolving: $db reaches this from operator-set
    # GC_BACKUP_DATABASES.
    case "$snapshot_path" in
        *..*) return 0 ;;
    esac
    [ -d "$snapshot_path" ] || return 0
    snapshot_root_is_safe || return 0
    discard_canon="$(canonical_path "$snapshot_path")" || return 0
    root_canon="$(canonical_path "$BACKUP_SNAPSHOT_DIR")" || return 0
    [ -n "$discard_canon" ] && [ -n "$root_canon" ] || return 0
    [ "$discard_canon" != "$root_canon" ] || return 0
    case "$discard_canon/" in
        "$root_canon"/*) : ;;
        *) return 0 ;;
    esac
    rm -rf "$discard_canon" 2>/dev/null || true
}

# clone_db_snapshot stages a copy-on-write clone. Both `cp -c` (APFS) and
# `cp --reflink=always` (btrfs/xfs) FAIL rather than silently degrading to a
# byte copy, which is what we want: a real 11 GB copy every 6h would be worse
# than the bug. When no CoW clone is possible the caller falls back to the
# in-place path and the run degrades to previous behaviour instead of breaking.
clone_db_snapshot() {
    clone_src="$1"
    clone_dest="$2"
    discard_db_snapshot "$clone_dest"
    # If the destination survived cleanup, ABORT rather than clone into it.
    # `cp -R src existing-dir` succeeds by nesting src INSIDE it, so the sync
    # would then run from the stale outer copy and report success — a false
    # green backed by old data, which is the exact failure class this whole
    # change exists to remove.
    if [ -e "$clone_dest" ]; then
        return 1
    fi
    if run_bounded 120 cp -c -R "$clone_src" "$clone_dest" 2>/dev/null; then
        return 0
    fi
    discard_db_snapshot "$clone_dest"
    if [ -e "$clone_dest" ]; then
        return 1
    fi
    if run_bounded 120 cp --reflink=always -R "$clone_src" "$clone_dest" 2>/dev/null; then
        return 0
    fi
    discard_db_snapshot "$clone_dest"
    return 1
}

# prune_backup_orphans deletes table files the backup manifest does not
# reference. A killed sync leaves its partial chunks behind, and nothing ever
# reclaimed them: 46.89 GB had accumulated by 2026-08-13, and the failed 07:02Z
# run alone left 2.58 GB. That bloat is self-reinforcing — it consumed the disk
# headroom the next sync needed, and a full disk is what crashed the server
# mid-rescue at 98%.
#
# FAILS CLOSED. Nothing is deleted unless the manifest parses AND every table
# file it references is present on disk; a backup we cannot fully verify is one
# we do not touch.
prune_backup_orphans() {
    prune_db="$1"
    if [ "$BACKUP_PRUNE_ORPHANS" = "0" ]; then
        return 0
    fi
    prune_dir="$BACKUP_ARTIFACT_DIR/$prune_db"
    prune_manifest="$prune_dir/manifest"
    [ -s "$prune_manifest" ] || return 0

    # manifest: version:__DOLT__:lock:root:appendix:(hash:count)*
    # Everything after the appendix is (table file hash, chunk count) pairs.
    # Do NOT match "any 32-char hash" — the lock, root and the all-zeros
    # appendix sentinel all look like table file hashes and are not.
    # A manifest is ONE line. `read` sees only the first, so a second line
    # would carry table file references we never parse — and every file they
    # reference would then look like an orphan and be deleted. Refuse outright
    # rather than prune from a partial view.
    # (A trailing newline is fine; only real content past line 1 is a problem.)
    prune_extra="$(sed -n '2,$p' "$prune_manifest" 2>/dev/null | tr -d '[:space:]' || true)"
    if [ -n "$prune_extra" ]; then
        echo "backup: $prune_db — orphan prune SKIPPED (manifest is not a single line)"
        return 0
    fi

    # `read` exits non-zero on a file with no trailing newline — which is
    # exactly how Dolt writes the manifest — but still assigns every field, so
    # the magic check below is the real validation. Swallowing the status here
    # is deliberate: `|| return 0` would skip the prune on every real manifest.
    IFS=: read -r _ prune_magic _ _ _ prune_rest < "$prune_manifest" || true
    [ "$prune_magic" = "__DOLT__" ] || return 0

    prune_referenced=" "
    prune_pairs=0
    prune_malformed=0
    while [ -n "$prune_rest" ]; do
        prune_hash="${prune_rest%%:*}"
        case "$prune_rest" in
            *:*) prune_rest="${prune_rest#*:}" ;;
            *) prune_rest="" ;;
        esac
        # A hash with no count after it means a truncated manifest.
        if [ -z "$prune_rest" ]; then
            if [ -n "$prune_hash" ]; then
                prune_malformed=1
            fi
            break
        fi
        prune_count="${prune_rest%%:*}"
        case "$prune_rest" in
            *:*) prune_rest="${prune_rest#*:}" ;;
            *) prune_rest="" ;;
        esac
        case "$prune_count" in
            ''|*[!0-9]*) prune_malformed=1; break ;;
        esac
        # Dolt table file hashes are 32 base32 characters. Anything else is a
        # manifest we do not understand — including `../sentinel`, which would
        # otherwise resolve to a real file and validate.
        case "$prune_hash" in
            *[!0-9a-v]*) prune_malformed=1; break ;;
        esac
        if [ "${#prune_hash}" -ne 32 ]; then
            prune_malformed=1
            break
        fi
        if [ ! -f "$prune_dir/$prune_hash.darc" ]; then
            prune_malformed=1
            break
        fi
        prune_referenced="$prune_referenced$prune_hash "
        prune_pairs=$((prune_pairs + 1))
    done

    if [ "$prune_malformed" -ne 0 ]; then
        echo "backup: $prune_db — orphan prune SKIPPED (manifest unreadable or references a missing table file)"
        return 0
    fi
    if [ "$prune_pairs" -eq 0 ]; then
        # A manifest that references nothing cannot authorise deleting
        # anything. "No referenced files" means unverified, never "everything
        # in this directory is an orphan" — that reading would empty a backup.
        return 0
    fi

    prune_removed=0
    prune_freed=0
    for prune_file in "$prune_dir"/*.darc; do
        [ -f "$prune_file" ] || continue
        prune_base="${prune_file##*/}"
        prune_base="${prune_base%.darc}"
        case "$prune_referenced" in
            *" $prune_base "*) continue ;;
        esac
        # Only reap things that actually look like Dolt table files. Anything
        # else in here is not ours to delete.
        case "$prune_base" in
            *[!0-9a-v]*) continue ;;
        esac
        if [ "${#prune_base}" -ne 32 ]; then
            continue
        fi
        # Never reap anything newer than the manifest we just validated.
        if [ "$prune_file" -nt "$prune_manifest" ]; then
            continue
        fi
        prune_size="$(stat -f%z "$prune_file" 2>/dev/null || stat -c%s "$prune_file" 2>/dev/null || echo 0)"
        case "$prune_size" in ''|*[!0-9]*) prune_size=0 ;; esac
        if rm -f "$prune_file" 2>/dev/null; then
            prune_removed=$((prune_removed + 1))
            prune_freed=$((prune_freed + prune_size))
        fi
    done

    if [ "$prune_removed" -gt 0 ]; then
        echo "backup: $prune_db — pruned $prune_removed unreferenced table file(s), $((prune_freed / 1048576)) MB reclaimed ($prune_pairs referenced files verified present)"
    fi
}

SNAPSHOT_ENABLED=0
if [ "$BACKUP_SNAPSHOT_MODE" != "off" ] && snapshot_root_is_safe; then
    SNAPSHOT_ENABLED=1
    # Reap clones stranded by a previous crashed run before staging new ones.
    for stale_snapshot in "$BACKUP_SNAPSHOT_DIR"/*; do
        [ -d "$stale_snapshot" ] || continue
        discard_db_snapshot "$stale_snapshot"
    done
fi

TOTAL=$(printf '%s\n' "$DATABASES" | awk 'NF {count++} END {print count + 0}')
SYNCED=0
FAILED=0
FAILED_DBS=""

for db in $DATABASES; do
    if ! ensure_backup_remote "$db"; then
        append_failed_db "$db(backup add failed)"
        continue
    fi
    db_dir="$DOLT_DATA_DIR/$db"
    if [ ! -d "$db_dir/.dolt" ]; then
        append_failed_db "$db(not found)"
        continue
    fi
    # ga-g3p5rm: the bound used to be a hardcoded 120s with no override, which
    # no database larger than a couple of GB could ever finish. hq (5.4 GB /
    # 53k commits) writes ~1 GB per ~30s, so it was killed partway on EVERY run
    # from the moment it outgrew the bound — abandoning gigabytes of unreachable
    # chunks and never advancing its manifest. as and qcore were small enough to
    # finish, which is why only hq silently rotted.
    #
    # Keep it BOUNDED (this holds the backup flock and competes for I/O with the
    # whole city) but size it so a real database can complete, and let the
    # operator raise it without editing the pack.
    sync_stderr="$(mktemp -t dolt-backup-sync 2>/dev/null || printf '%s' "/tmp/dolt-backup-sync.$$")"
    # Sync from a CoW clone so dolt uses direct file access instead of proxying
    # through the sql-server connection the data cannot fit inside. Falls back
    # to the in-place path when no clone is possible.
    sync_dir="$db_dir"
    sync_mode="in-place"
    sync_ok=0
    # Only ever names a path under the snapshot root, and only when snapshots
    # are actually enabled — this value reaches an `rm -rf`.
    db_snapshot=""
    if [ "$SNAPSHOT_ENABLED" -eq 1 ]; then
        db_snapshot="$BACKUP_SNAPSHOT_DIR/$db"
        if clone_db_snapshot "$db_dir" "$db_snapshot"; then
            sync_dir="$db_snapshot"
            sync_mode="snapshot"
        else
            echo "backup: $db — copy-on-write clone unavailable, falling back to in-place sync through the sql-server"
        fi
    fi
    if (cd "$sync_dir" && run_bounded "$BACKUP_SYNC_TIMEOUT" dolt backup sync "${db}-backup" 2>"$sync_stderr"); then
        SYNCED=$((SYNCED + 1))
        sync_ok=1
        echo "backup: $db — synced ($sync_mode)"
        # Stamp ONLY on exit 0. Health reads this instead of any mtime on the
        # artifact plane, because the failure path can write those too.
        write_local_backup_sync_stamp "$db" "$BACKUP_ARTIFACT_DIR"
    else
        # ga-28huj class: the reason used to go to /dev/null, so "sync failed"
        # was unactionable. Carry the first stderr line into the failure label.
        # Guarded for `set -euo pipefail`: an EMPTY stderr file is the normal
        # case on a timeout kill, and an unguarded pipeline over it exits
        # non-zero and would abort the whole backup run.
        sync_reason=""
        if [ -s "$sync_stderr" ]; then
            sync_reason="$(tr -s '[:space:]' ' ' < "$sync_stderr" 2>/dev/null | head -1 | cut -c1-120 || true)"
        fi
        [ -n "$sync_reason" ] || sync_reason="no stderr; exceeded ${BACKUP_SYNC_TIMEOUT}s bound or exited non-zero"
        append_failed_db "$db($sync_mode sync failed: $sync_reason)"
    fi
    rm -f "$sync_stderr" 2>/dev/null || true
    if [ "$SNAPSHOT_ENABLED" -eq 1 ]; then
        discard_db_snapshot "$db_snapshot"
    fi
    # Reclaim chunks abandoned by earlier killed syncs — but ONLY behind a sync
    # that just exited 0, because that is the only state in which dolt itself
    # has just written the manifest we are about to trust. After a FAILED sync
    # the manifest is exactly as likely to be half-written, and a manifest
    # truncated at a pair boundary still parses cleanly while silently omitting
    # real table files, which we would then delete. Waiting for the next
    # successful run costs one cycle and reclaims the same bytes.
    if [ "$sync_ok" -eq 1 ]; then
        prune_backup_orphans "$db"
    fi
done

FAILED_COUNT=$FAILED

# --- Step 3: Rsync backup artifacts to offsite storage ---
#
# ga-l9smko. This leg used to report itself in ONE WORD inside a summary line
# while the RUN recorded success either way, so "nobody ever configured an
# off-box copy" and "the off-box copy completed" were byte-identical durable
# evidence — and the only recurring durability job this city runs was green BY
# CONSTRUCTION for as long as GC_BACKUP_OFFSITE_PATH stayed unset.
#
# Three changes, the first two unconditional:
#   * a CONFIGURED leg that did not copy now FAILS the run. It used to be
#     labelled "failed (non-fatal)" and exit 0. An off-box copy that did not
#     happen is not a non-fatal detail — it is the entire purpose of the leg —
#     and unlike an unconfigured target it is a real, dischargeable failure.
#   * the freshness stamp is written ONLY by an rsync that exits 0, so readers
#     can answer "how old is the off-box copy" instead of assuming it. An
#     unconfigured or waived run writes NO stamp, which is what stops any reader
#     from going green without a real copy behind it.
#   * an unconfigured leg is NAMED as such rather than "skipped", and fails the
#     run only under GC_BACKUP_OFFSITE_REQUIRED=1.
OFFSITE_STATUS="unconfigured — NO OFF-BOX COPY"
OFFSITE_FATAL=0
OFFSITE_DETAIL=""

if [ -n "$OFFSITE_PATH" ]; then
    if [ ! -d "$BACKUP_ARTIFACT_DIR" ]; then
        OFFSITE_STATUS="missing-artifacts"
        OFFSITE_FATAL=1
        OFFSITE_DETAIL="artifact dir $BACKUP_ARTIFACT_DIR does not exist, so nothing was copied off-box"
    elif same_path "$BACKUP_ARTIFACT_DIR" "$DOLT_DATA_DIR"; then
        OFFSITE_STATUS="invalid-source"
        OFFSITE_FATAL=1
        OFFSITE_DETAIL="artifact dir resolves to the live data dir ($DOLT_DATA_DIR); refusing to rsync live data off-box"
    elif run_bounded 300 rsync -a --delete "$BACKUP_ARTIFACT_DIR/" "$OFFSITE_PATH/" 2>/dev/null; then
        OFFSITE_STATUS="ok"
        write_offsite_backup_sync_stamp "$OFFSITE_PATH" "$BACKUP_ARTIFACT_DIR"
    else
        OFFSITE_STATUS="failed"
        OFFSITE_FATAL=1
        OFFSITE_DETAIL="rsync to $OFFSITE_PATH exited non-zero or exceeded its 300s bound"
    fi
else
    OFFSITE_WAIVER="$(offsite_waiver_reason)"
    if [ -n "$OFFSITE_WAIVER" ]; then
        # An operator risk acceptance, declared in the city and dated by its
        # file. It still writes NO stamp: a waiver records that nobody expects
        # an off-box copy, not that one exists.
        OFFSITE_STATUS="waived: $OFFSITE_WAIVER"
    elif [ "$OFFSITE_REQUIRED" -eq 1 ]; then
        OFFSITE_FATAL=1
        OFFSITE_DETAIL="GC_BACKUP_OFFSITE_PATH is unset and no waiver is declared in config/dolt/offsite-waived, while GC_BACKUP_OFFSITE_REQUIRED=1"
    fi
fi

# Age of the last REAL off-box copy, so the run output answers the question
# rather than leaving it to be assumed. No stamp reports "never", never a fresh
# copy — and "never" is the honest reading of an unconfigured leg.
OFFSITE_AGE="never"
OFFSITE_EPOCH="$(read_offsite_backup_sync_epoch)"
if [ -n "$OFFSITE_EPOCH" ]; then
    OFFSITE_AGE_SEC=$(( $(date +%s) - OFFSITE_EPOCH ))
    [ "$OFFSITE_AGE_SEC" -lt 0 ] && OFFSITE_AGE_SEC=0
    OFFSITE_AGE="${OFFSITE_AGE_SEC}s ago"
fi

# --- Step 4: Report ---

if [ "$FAILED_COUNT" -gt 0 ]; then
    dolt_escalate \
        "Dolt backup: $FAILED_COUNT/$TOTAL databases failed to sync [MEDIUM]" \
        "Failed databases:$FAILED_DBS" \
        2>/dev/null || true
fi

if [ "$OFFSITE_FATAL" -eq 1 ] && [ -n "$OFFSITE_PATH" ]; then
    # Only a CONFIGURED leg escalates. An unconfigured one is a standing
    # condition with an open decision behind it, and re-alarming a standing
    # condition every cycle is how a signal gets parked (ga-8bt85j).
    dolt_escalate \
        "Dolt backup: the off-box copy did not happen — $OFFSITE_STATUS [HIGH]" \
        "$OFFSITE_DETAIL. Last real off-box copy: $OFFSITE_AGE. The local .dolt-backup artifacts and the live store share one volume, so until this leg works there is no copy that survives a disk event." \
        2>/dev/null || true
fi

SUMMARY="backup — synced: $SYNCED/$TOTAL, offsite: $OFFSITE_STATUS, last off-box copy: $OFFSITE_AGE"
dolt_notify_done "$SUMMARY"
echo "backup: $SUMMARY"
if [ "$OFFSITE_FATAL" -eq 1 ]; then
    echo "backup: FAILING THE RUN — $OFFSITE_DETAIL" >&2
    exit 1
fi
