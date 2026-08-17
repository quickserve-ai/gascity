#!/bin/sh
# gc dolt health — Lightweight Dolt data-plane health report.
#
# Checks server status and latency, per-database commit counts and open
# beads, backup freshness, orphan databases, active compaction quarantine
# markers, and zombie Dolt processes.
#
# Environment: GC_CITY_PATH, GC_DOLT_PORT, GC_DOLT_HOST, GC_DOLT_USER,
#              GC_DOLT_PASSWORD, GC_DOLT_RIG_LIST_TIMEOUT_SECS
set -e

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

metadata_files() {
  printf '%s\n' "$GC_CITY_PATH/.beads/metadata.json"

  if command -v gc >/dev/null 2>&1; then
    # Bound the gc rig list call: if gc is itself in a bad state (the
    # failure mode this patrol is meant to detect) we must not block
    # here. Degrade to the fallback rig scan below. The bound (default in
    # runtime.sh, shared with the compact command) must absorb a
    # slow-but-healthy gc on a busy host (~16s observed) because the
    # fallback scan only sees the city directory and silently drops
    # external rig databases (gascity#2740).
    rig_paths=$(run_bounded "$GC_DOLT_RIG_LIST_TIMEOUT_SECS" gc rig list --json 2>/dev/null \
      | if command -v jq >/dev/null 2>&1; then
          jq -r '.rigs[].path' 2>/dev/null
        else
          grep '"path"' | sed 's/.*"path": *"//;s/".*//'
        fi) || true
    if [ -n "$rig_paths" ]; then
      printf '%s\n' "$rig_paths" | while IFS= read -r p; do
        [ -n "$p" ] && printf '%s\n' "$p/.beads/metadata.json"
      done
      return
    fi
  fi

  # Fallback: scan local rigs/ directory only. Cannot discover external rigs
  # when gc is unavailable — acceptable degradation.
  find "$GC_CITY_PATH/rigs" -path '*/.beads/metadata.json' 2>/dev/null || true
}

metadata_db() {
  meta="$1"
  if command -v jq >/dev/null 2>&1; then
    jq -r '.dolt_database // empty' "$meta" 2>/dev/null || true
    return
  fi
  grep -o '"dolt_database"[[:space:]]*:[[:space:]]*"[^"]*"' "$meta" 2>/dev/null | sed 's/.*: *"//;s/"$//' || true
}

json_output=false
data_dir="$DOLT_DATA_DIR"

while [ $# -gt 0 ]; do
  case "$1" in
    --json) json_output=true; shift ;;
    -h|--help)
      echo "Usage: gc dolt health [--json]"
      echo ""
      echo "Lightweight Dolt data-plane health report for patrol cycles."
      echo ""
      echo "Flags:"
      echo "  --json    Output as JSON (consumed by health patrol automation)"
      exit 0
      ;;
    *) echo "gc dolt health: unknown flag: $1" >&2; exit 1 ;;
  esac
done

# Note: run_bounded / TIMEOUT_BIN are provided by assets/scripts/runtime.sh.

# Determine host for probing.
host="${GC_DOLT_HOST:-127.0.0.1}"

# Check if server is running.
server_running=false
server_pid=0
server_latency=0
server_reachable=false

# Portable millisecond timestamp. BSD date(1) on macOS treats %N as a
# literal 'N' (exits 0, output like "1776740122N"), so the GNU-only
# || fallback never triggers. Feature-test the output instead.
now_ms() {
  _raw=$(date +%s%N 2>/dev/null)
  case "$_raw" in
    ''|*[!0-9]*) printf '%s000' "$(date +%s 2>/dev/null)" ;;
    *)        printf '%s' "$_raw" | cut -c1-13 ;;
  esac
}

# marker_epoch — convert an RFC3339 UTC timestamp (e.g. 2026-06-14T23:22:55Z)
# to epoch seconds, portably across GNU and BSD date(1). Empty output on a
# missing or unparseable timestamp so the caller can fall back to file mtime.
marker_epoch() {
  _ts="$1"
  case "$_ts" in
    ''|*[!0-9TZ:.+-]*) return 0 ;;
  esac
  # GNU date parses the RFC3339 string directly; BSD/macOS date needs an
  # explicit input format and the -j (do-not-set-clock) flag.
  # Use `if` rather than `&&` so a failed date(1) doesn't set a non-zero
  # exit status that would trigger `set -e` in the caller's subshell.
  if _e=$(date -u -d "$_ts" +%s 2>/dev/null); then printf '%s' "$_e"; return 0; fi
  if _e=$(date -u -j -f "%Y-%m-%dT%H:%M:%SZ" "$_ts" +%s 2>/dev/null); then printf '%s' "$_e"; return 0; fi
  return 0
}

# human_duration — format a whole-second count as a compact age string
# (e.g. 12d3h, 5h2m, 7m1s, 9s). Used for compaction quarantine marker age.
human_duration() {
  _s="$1"
  case "$_s" in ''|*[!0-9]*) printf '0s'; return ;; esac
  _d=$((_s / 86400)); _h=$(((_s % 86400) / 3600))
  _m=$(((_s % 3600) / 60)); _sec=$((_s % 60))
  if [ "$_d" -gt 0 ]; then printf '%dd%dh' "$_d" "$_h"
  elif [ "$_h" -gt 0 ]; then printf '%dh%dm' "$_h" "$_m"
  elif [ "$_m" -gt 0 ]; then printf '%dm%ds' "$_m" "$_sec"
  else printf '%ds' "$_sec"; fi
}

# Find dolt PID by port for local managed servers. External Dolt endpoints do
# not listen on 127.0.0.1, so do not let the local TCP precheck suppress the
# real SQL ping to GC_DOLT_HOST:GC_DOLT_PORT. is_local_dolt_host is provided by
# runtime.sh and shared with the status/logs commands.
should_probe_sql=false
is_external=false
if is_local_dolt_host "$host"; then
  pid=$(managed_runtime_listener_pid "$GC_DOLT_PORT" || true)
  if [ -n "$pid" ] || managed_runtime_tcp_reachable "$GC_DOLT_PORT"; then
    server_running=true
    [ -n "$pid" ] && server_pid="$pid"
    should_probe_sql=true
  fi
else
  # Configured external Dolt endpoint (non-local GC_DOLT_HOST). GC does not own
  # a local managed process here, so server.running / server.pid keep their
  # local-process defaults (false / 0). Reachability is decided by the SQL ping
  # below and reported honestly via server.reachable + server.external — a
  # reachable remote endpoint must not read as a downed local server
  # (gastownhall/gascity su-deol8).
  is_external=true
  should_probe_sql=true
fi

if [ "$should_probe_sql" = true ]; then
  # Measure query latency.
  start_ms=$(now_ms)
  conn_args="--host $host --port $GC_DOLT_PORT --user $GC_DOLT_USER --no-tls"
  # Always export DOLT_CLI_PASSWORD (even empty) so the client does not
  # prompt for a password on stdin. Without this, the SELECT 1 probe
  # silently fails with "Failed to parse credentials: operation not
  # supported by device" on sessions without a controlling TTY —
  # which then left the health report claiming "server: running" but
  # never reporting per-database detail.
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  # Bound the ping. A TCP-reachable but unresponsive server (stuck
  # goroutine, saturated pool, migration lock) would otherwise hang.
  if run_bounded 5 dolt $conn_args sql -q "SELECT 1" >/dev/null 2>&1; then
    server_reachable=true
    end_ms=$(now_ms)
    server_latency=$((end_ms - start_ms))
    [ "$server_latency" -lt 0 ] && server_latency=0
  fi
fi

# Cache metadata file paths once (avoids repeated gc calls and word-splitting).
_meta_cache=$(mktemp)
# Scratch file for the zombie scan's matched-server filter. The foreign-managed
# decision runs in a `... | while read` subshell (so $zombie_count can't be
# mutated through the pipe); the survivors are spooled here and read back in
# the parent shell.
_zombie_scan_out=$(mktemp)
metadata_files > "$_meta_cache"
trap 'rm -f "$_meta_cache" "$_zombie_scan_out"' EXIT

# Collect database info.
#
# NOTE: we must NOT invoke `dolt log` against the on-disk database
# directory while the sql-server holds it open. Historically this was
# done with `cd "$d" && dolt log --oneline | wc -l`; on an active DB
# the client contends with the server for Dolt's file locks and the
# client process blocks indefinitely, orphaning zombie `dolt log`
# processes and wedging the health CLI. Query the running server via
# SQL instead — it's the authoritative source, never deadlocks with
# itself, and is cheap (dolt_log is indexed by commit hash).

# db_name_is_safe NAME — accept NAME only when its first byte is alnum/underscore
# and every byte is in [A-Za-z0-9_-], before it is interpolated into a
# backtick-quoted SQL identifier. Dolt derives names from directory names
# (local) or returns them from SHOW DATABASES (external); either source could in
# principle carry characters (backticks, semicolons, leading dashes) that break
# out of the identifier and execute attacker-chosen SQL as the patrol user. Not
# an external-attack surface today — the catalog is server-controlled — but
# fragile enough under config drift that it is worth skipping rather than probing.
db_name_is_safe() {
  case "$1" in
    [A-Za-z0-9_]*) ;;
    *) return 1 ;;
  esac
  case "$1" in
    *[!A-Za-z0-9_-]*) return 1 ;;
  esac
  return 0
}

# db_commit_and_open_counts NAME — emit `NAME|commits|open_beads|remotes|remote_names`
# by querying the running server for NAME's commit count (dolt_log) and
# open-bead count (issues WHERE status='open'). Both counts come from SQL
# against the live server: it is authoritative, never deadlocks with an on-disk
# dolt client, and is cheap. 0 on timeout, error, or a database without the
# table (a non-beads DB) — the same fail-soft contract for every database so
# one bad DB never hangs the report. Under managed Dolt the beads live in the
# server's `issues` table, not an on-disk beads.jsonl (absent or stale), which
# the old file grep reported as open_beads=0 for every live database (#3200).
# Extract the first fully-numeric line rather than a fixed row so a future
# `USE`/warning banner cannot silently collapse the count to 0.
#
# The remote fields feed the origin-mirror verdict: the count stays EMPTY on
# probe failure, so mirror freshness fails closed instead of claiming no
# target, and the names list every configured remote.
db_commit_and_open_counts() {
  _name="$1"
  _commits_csv=$(run_bounded 5 dolt $conn_args sql --result-format csv \
    -q "USE \`$_name\`; SELECT COUNT(*) FROM dolt_log;" 2>/dev/null || true)
  _commits=$(printf '%s\n' "$_commits_csv" | grep -E '^[0-9]+$' | head -1)
  case "$_commits" in ''|*[!0-9]*) _commits=0 ;; esac
  _open_csv=$(run_bounded 5 dolt $conn_args sql --result-format csv \
    -q "USE \`$_name\`; SELECT COUNT(*) FROM issues WHERE status='open';" 2>/dev/null || true)
  _open_beads=$(printf '%s\n' "$_open_csv" | grep -E '^[0-9]+$' | head -1)
  case "$_open_beads" in ''|*[!0-9]*) _open_beads=0 ;; esac
  _remotes_csv=$(run_bounded 5 dolt $conn_args sql --result-format csv \
    -q "USE \`$_name\`; SELECT COUNT(*) FROM dolt_remotes;" 2>/dev/null || true)
  _remotes=$(printf '%s\n' "$_remotes_csv" | grep -E '^[0-9]+$' | head -1)
  case "$_remotes" in *[!0-9]*) _remotes= ;; esac
  # ga-3o5xrw: the COUNT alone cannot say WHICH remotes are configured, and the
  # mirror-freshness verdict below needs the names — it has to require a recent
  # stamp for EACH of them, not merely for "some remote". Field 4 keeps its
  # existing meaning (count; empty = the probe failed and remotes are
  # unknowable) so nothing downstream changes; field 5 adds the names.
  _remote_names=""
  if [ -n "$_remotes" ] && [ "$_remotes" != "0" ]; then
    _names_csv=$(run_bounded 5 dolt $conn_args sql --result-format csv \
      -q "USE \`$_name\`; SELECT name FROM dolt_remotes ORDER BY name;" 2>/dev/null || true)
    _remote_names=$(printf '%s\n' "$_names_csv" | sed '1d' | tr -d '"\r' \
      | grep -E '^[A-Za-z0-9_.-]+$' | paste -sd, -)
  fi
  printf '%s|%s|%s|%s|%s\n' "$_name" "$_commits" "$_open_beads" "$_remotes" "$_remote_names"
}

# external_database_names — list user databases on a configured external Dolt
# endpoint via SQL. The databases live on the remote server, so the on-disk
# data-dir scan used for managed Dolt reports none (databases=[]); SHOW DATABASES
# is the authoritative catalog for a remote endpoint (su-deol8). The CSV header
# and system databases are filtered; unsafe identifiers are skipped.
external_database_names() {
  _show_csv=$(run_bounded 5 dolt $conn_args sql --result-format csv \
    -q "SHOW DATABASES;" 2>/dev/null || true)
  printf '%s\n' "$_show_csv" | while IFS= read -r _raw; do
    _name=$(printf '%s' "$_raw" | tr -d '\r' | sed 's/^"//; s/"$//')
    [ -n "$_name" ] || continue
    [ "$_name" = "Database" ] && continue
    case "$(printf '%s' "$_name" | tr '[:upper:]' '[:lower:]')" in
      information_schema|mysql|dolt|dolt_cluster|performance_schema|sys|__gc_probe) continue ;;
    esac
    db_name_is_safe "$_name" || continue
    printf '%s\n' "$_name"
  done
}

db_info=""
if [ "$server_reachable" = true ]; then
  if [ "$is_external" = true ]; then
    # External endpoint: enumerate databases from the reachable remote server
    # via SQL, then count each. The on-disk scan below cannot see remote
    # databases, so it would report databases=[] despite healthy SQL (su-deol8).
    db_info=$(external_database_names | while IFS= read -r name; do
      [ -n "$name" ] || continue
      db_commit_and_open_counts "$name"
    done)
  elif [ -d "$data_dir" ]; then
    # Local managed Dolt: the on-disk data dir is authoritative for which
    # databases exist. Scan it, then count each via SQL against the server.
    for d in "$data_dir"/*/; do
      [ ! -d "$d/.dolt" ] && continue
      name="$(basename "$d")"
      case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
      db_name_is_safe "$name" || continue
      line=$(db_commit_and_open_counts "$name")
      db_info="$db_info$line
"
    done
  fi
fi

# Check backup freshness.
#
# Two unrelated artifacts have both been called "backups" in this report, and
# the JSON field names promise the one the old probe did not read. The `dolt_*`
# fields now measure the Dolt backup remotes under GC_BACKUP_ARTIFACT_DIR
# (default $GC_CITY_PATH/.dolt-backup), which is what the backup order writes
# and what an operator means when they ask whether the bead store is backed up.
# The migration snapshots that `gc dolt rollback` restores live under
# $GC_CITY_PATH/migration-backup-* and are still reported, now as `migration_*`.
# Reading one and labelling it the other is why a city could run 18 hours with
# no bead-store backup while this command printed nothing unusual.
#
# `dolt_measured` exists because the previous shape had no way to say "I did
# not look". It initialised freshness to "", age to 0 and stale to false, then
# skipped the block that would overwrite them whenever nothing was found, so a
# probe that measured nothing rendered as a confident dolt_stale:false — the
# one reading an operator must never get from a backup check. The flag is
# spelled affirmatively so its zero value is the cautious claim, and dolt_stale
# is null rather than false whenever nothing was measured.

# Format an age in seconds the way this report has always formatted it.
format_age() {
  fa_sec="$1"
  if [ "$fa_sec" -ge 3600 ]; then
    printf '%dh%dm' "$((fa_sec / 3600))" "$((fa_sec % 3600 / 60))"
  elif [ "$fa_sec" -ge 60 ]; then
    printf '%dm%ds' "$((fa_sec / 60))" "$((fa_sec % 60))"
  else
    printf '%ds' "$fa_sec"
  fi
}

path_mtime() {
  pm_value=$(stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null || echo 0)
  case "$pm_value" in
    ''|*[!0-9]*) pm_value=0 ;;
  esac
  printf '%s' "$pm_value"
}

# Stale threshold for Dolt backup remotes. Defaults to twice the 6h backup
# interval, matching mol-dog-doctor.sh so the two paths cannot disagree about
# the same database; GC_DOCTOR_BACKUP_STALE_S is honoured for the same reason.
backup_stale_after="${GC_HEALTH_BACKUP_STALE_S:-${GC_DOCTOR_BACKUP_STALE_S:-43200}}"
case "$backup_stale_after" in
  ''|*[!0-9]*) backup_stale_after=43200 ;;
esac

backup_artifact_dir="${GC_BACKUP_ARTIFACT_DIR:-$GC_CITY_PATH/.dolt-backup}"
backup_measured=false
backup_worst_seen=false
backup_freshness=""
backup_stale=null
backup_age_sec=0
backup_db_list=""
now=$(date +%s)

# A database is backup-eligible when the artifact directory holds a
# same-named subdirectory, which is where the backup order points every remote
# it configures (file://$BACKUP_ARTIFACT_DIR/<db>). This costs no dolt call, so
# it stays inside the patrol's fork budget. An eligible database whose
# directory holds no manifest yet is measured and reported stale: never
# having been backed up is a known-bad state, not an unknown one.
#
# The manifest's mtime is the age of the newest restorable backup. `dolt backup
# sync` writes chunk data first and adopts it by rewriting the manifest last,
# so a sync cut off in between leaves chunks newer than anything the manifest
# references, and the newest file of any kind would date a backup that does
# not exist.
if [ -d "$backup_artifact_dir" ]; then
  for bdir in "$backup_artifact_dir"/*/; do
    [ -d "$bdir" ] || continue
    bname="$(basename "$bdir")"
    case "$(printf '%s' "$bname" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
    case "$bname" in
      [A-Za-z0-9_]*)
        case "$bname" in *[!A-Za-z0-9_-]*) continue ;; esac
        ;;
      *) continue ;;
    esac
    backup_measured=true
    db_newest=0
    [ -f "${bdir}manifest" ] && db_newest=$(path_mtime "${bdir}manifest")
    if [ "$db_newest" -le 0 ]; then
      db_age=-1
      db_stale=true
      db_fresh=""
    else
      db_age=$((now - db_newest))
      [ "$db_age" -lt 0 ] && db_age=0
      db_fresh=$(format_age "$db_age")
      db_stale=false
      [ "$db_age" -gt "$backup_stale_after" ] && db_stale=true
    fi
    backup_db_list="$backup_db_list$bname|$db_age|$db_fresh|$db_stale
"
    # The aggregate reports the WORST eligible database, so a single stale
    # database can never be averaged away by a healthy sibling.
    if [ "$db_stale" = true ]; then
      backup_stale=true
    elif [ "$backup_stale" = null ]; then
      backup_stale=false
    fi
    # The age follows the same worst-first rule, which a plain `-gt` against a
    # zero seed gets wrong in two directions. A database that has never
    # produced a backup carries -1, and that is the worst state there is rather
    # than the smallest number, so it has to win outright or the aggregate
    # reports 0 for a city with no backup at all — the same confident zero from
    # an unmeasured probe that this block exists to stop emitting. A genuine
    # age of 0 also has to be able to seed the aggregate, or a database synced
    # in the second the check runs reports the empty freshness of one that was
    # never measured.
    if [ "$backup_worst_seen" != true ]; then
      backup_worst_seen=true
      backup_age_sec="$db_age"
      backup_freshness="$db_fresh"
    elif [ "$backup_age_sec" -ge 0 ]; then
      if [ "$db_age" -lt 0 ] || [ "$db_age" -gt "$backup_age_sec" ]; then
        backup_age_sec="$db_age"
        backup_freshness="$db_fresh"
      fi
    fi
  done
fi
if [ "$backup_measured" != true ]; then
  backup_age_sec=0
  backup_freshness=""
  backup_stale=null
fi

# Migration snapshots: what this block used to measure, under a name that says so.
migration_measured=false
migration_freshness=""
migration_stale=null
migration_age_sec=0
newest_backup=$(ls -1d "$GC_CITY_PATH"/migration-backup-* 2>/dev/null | sort -r | head -1 || true)
if [ -n "$newest_backup" ]; then
  migration_measured=true
  migration_mtime=$(path_mtime "$newest_backup")
  migration_age_sec=$((now - migration_mtime))
  [ "$migration_age_sec" -lt 0 ] && migration_age_sec=0
  migration_freshness=$(format_age "$migration_age_sec")
  migration_stale=false
  [ "$migration_age_sec" -gt 1800 ] && migration_stale=true
fi

# mirror_park_reason <db> <remote> — echo the reason a database/remote pair is
# deliberately excluded from the mirror durability verdict, or nothing if it
# is not.
#
# GC_DOLT_MIRROR_PARKED is a comma-separated list of `<db>/<remote>[=reason]`,
# e.g. "qcore/origin=gated on ga-qo9w". Parking is PER REMOTE, never per
# database: parking a whole database would also silence its healthy mirrors,
# which is the failure the park is supposed to make visible, not hide.
#
# WHY HEALTH KNOWS ABOUT PARKS AT ALL (ga-3o5xrw). qcore's dead mirror was
# deliberately parked on 2026-08-05, but the decision existed only as an env var
# inside an order file and a note on a bead — not in `gc dolt health`, which is
# where anyone actually looks. Three agents in one night each investigated that
# documented silence as a broken alarm, and each filed it as a monitoring defect
# before finding the park. A monitor must state what it is NOT covering and why,
# or correct silence costs more than a false alarm would have.
mirror_park_reason() {
  _mp_key="$1/$2"
  [ -z "${GC_DOLT_MIRROR_PARKED:-}" ] && return 0
  # Split on commas via $IFS word-splitting rather than a `| while read` loop:
  # the right-hand side of a pipeline is a subshell in POSIX sh, where `return`
  # is not portable and any state set inside is discarded on exit.
  _mp_saved_ifs="$IFS"
  IFS=','
  # Intentionally unquoted: this is the split.
  # shellcheck disable=SC2086
  set -- $GC_DOLT_MIRROR_PARKED
  IFS="$_mp_saved_ifs"
  for _mp_entry in "$@"; do
    _mp_entry=$(printf '%s' "$_mp_entry" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
    [ -z "$_mp_entry" ] && continue
    case "$_mp_entry" in
      "$_mp_key")
        printf 'parked\n'
        return 0
        ;;
      "$_mp_key="*)
        _mp_reason=${_mp_entry#"$_mp_key="}
        printf '%s\n' "${_mp_reason:-parked}"
        return 0
        ;;
    esac
  done
  return 0
}

# Check off-box mirror freshness (qc-lu207, ga-a8w1c).
#
# This plane is reported under backups.origin_mirrors and never through the
# dolt_* fields above: those date the LOCAL backup remotes under
# GC_BACKUP_ARTIFACT_DIR by their manifests, and a fresh local backup says
# nothing about whether any off-box remote has received the data, nor the
# reverse.
#
# Source of truth: remote-verification stamps written by sync/compact into
# $BACKUP_FRESHNESS_DIR (see runtime.sh). A stamp means the sync path recently
# proved the remote contains the local branch: either a push succeeded or a
# fetch-and-classify found local and remote up to date. Health deliberately
# does not perform network probes itself; they can hang exactly when the remote
# is down.
#
# The verdict is PER DATABASE-AND-REMOTE PAIR (ga-3o5xrw): a database is only
# as backed up as its least-verified non-parked remote, so one silently
# under-pushed mirror makes the whole database read stale rather than being
# averaged away by a healthy sibling. Every configured remote counts, including
# one `gc dolt sync` does not select by default (see its "Remote resolution"):
# name it in GC_DOLT_REMOTE_<DB> so sync pushes it, or park it here.
#
# Verdict rules — no data must NEVER read as healthy:
#   - db with a remote and a recent verification       -> ok
#   - db with a remote and an old verification         -> stale
#   - db with a remote and NO verification             -> unknown
#     (a fresh install reads "unknown" until sync first verifies its remote)
#   - db without a remote                              -> skipped (not a target)
#   - no db has a remote                               -> state "no-remotes",
#     stale=false: that is a MEASURED verdict (nothing is supposed to be
#     mirrored), unlike the historical fail-open where stale=false was just an
#     initializer that never got overwritten.
#   - server unreachable (remotes unknowable)          -> unknown, stale=true
mirror_stale_threshold="${GC_DOLT_BACKUP_STALE_SECS:-1800}"
case "$mirror_stale_threshold" in ''|*[!0-9]*) mirror_stale_threshold=1800 ;; esac
mirror_freshness=""
mirror_stale=true
mirror_age_sec=0
mirror_state="unknown"
mirror_detail=""
bf_any_remote_db=false
bf_any_bad=false
bf_any_unknown=false
bf_probe_failed=false
bf_oldest_epoch=0
if [ "$server_reachable" = true ] && [ -n "$db_info" ]; then
  # Parse the per-db lines collected above
  # (name|commits|open_beads|remote_count|remote_names).
  bf_lines=$(printf '%s' "$db_info")
  while IFS='|' read -r bf_name _bf_commits _bf_open bf_remotes bf_remote_names; do
    [ -z "$bf_name" ] && continue
    case "$bf_remotes" in
      '') bf_probe_failed=true; continue ;;   # probe failed: can't classify this db
      0) continue ;;                          # no remote: not a mirror target
    esac
    bf_any_remote_db=true
    # The count says there ARE remotes but the name probe came back empty, so
    # this database's remotes are unknowable. Never fall through to "nothing to
    # verify" — that is the false-green shape this whole check exists to avoid.
    if [ -z "$bf_remote_names" ]; then
      bf_probe_failed=true
      mirror_detail="$mirror_detail$bf_name|unknown|||remote list unreadable
"
      continue
    fi
    # ga-3o5xrw: verify EVERY configured remote, not "some remote". The stamp
    # used to be one file per database, so on a two-remote database a fresh
    # stamp could equally mean "the live mirror was pushed" or "the dead mirror
    # was picked and nothing was pushed at all" — health could not tell those
    # apart and read ok for both. Requiring a per-remote stamp makes a mirror
    # that is silently receiving nothing show up as its own stale line.
    for bf_remote in $(printf '%s\n' "$bf_remote_names" | tr ',' ' '); do
      [ -z "$bf_remote" ] && continue
      bf_park_reason=$(mirror_park_reason "$bf_name" "$bf_remote")
      if [ -n "$bf_park_reason" ]; then
        # A deliberately parked mirror is excluded from the durability verdict
        # but still PRINTED (ga-3o5xrw). Correct silence has to be legible as
        # deliberate: three agents each investigated this park as a broken
        # alarm in one night because the decision lived only in an order file
        # and a bead note, nowhere health was actually read.
        mirror_detail="$mirror_detail$bf_name@$bf_remote|parked|||$bf_park_reason
"
        continue
      fi
      bf_stamp="$BACKUP_FRESHNESS_DIR/$bf_name@$bf_remote"
      bf_state="unknown"
      bf_age=""
      bf_refspec=""
      # Legacy per-database stamp (pre-ga-3o5xrw layout). It records which
      # remote it was written for, so it is a valid stamp for exactly that
      # remote and nothing else. Honoring it avoids a spurious city-wide RED
      # window on the deploy that changes the layout; it ages out on its own
      # once every remote has been stamped under the new key.
      if [ ! -f "$bf_stamp" ] && [ -f "$BACKUP_FRESHNESS_DIR/$bf_name" ] &&
        [ "$(sed -n 's/^remote=//p' "$BACKUP_FRESHNESS_DIR/$bf_name" 2>/dev/null | head -1)" = "$bf_remote" ]; then
        bf_stamp="$BACKUP_FRESHNESS_DIR/$bf_name"
      fi
      if [ -f "$bf_stamp" ]; then
        bf_epoch=$(sed -n 's/^pushed_at_epoch=//p' "$bf_stamp" 2>/dev/null | head -1)
        bf_refspec=$(sed -n 's/^refspec=//p' "$bf_stamp" 2>/dev/null | head -1)
        case "$bf_epoch" in
          ''|*[!0-9]*) bf_state="unknown" ;;
          *)
            bf_age=$((now - bf_epoch))
            [ "$bf_age" -lt 0 ] && bf_age=0
            if [ "$bf_age" -le "$mirror_stale_threshold" ]; then
              bf_state="ok"
            else
              bf_state="stale"
            fi
            if [ "$bf_oldest_epoch" -eq 0 ] || [ "$bf_epoch" -lt "$bf_oldest_epoch" ]; then
              bf_oldest_epoch="$bf_epoch"
            fi
            ;;
        esac
      fi
      [ "$bf_state" = "unknown" ] && bf_any_unknown=true
      [ "$bf_state" = "ok" ] || bf_any_bad=true
      mirror_detail="$mirror_detail$bf_name@$bf_remote|$bf_state|$bf_age|$bf_refspec|
"
    done
  done <<BFEOF
$bf_lines
BFEOF
  if [ "$bf_any_remote_db" = false ] && [ "$bf_probe_failed" = false ]; then
    mirror_state="no-remotes"
    mirror_stale=false
  elif [ "$bf_probe_failed" = true ] || [ "$bf_any_unknown" = true ]; then
    mirror_state="unknown"
    mirror_stale=true
  elif [ "$bf_any_bad" = true ]; then
    mirror_state="stale"
    mirror_stale=true
  else
    mirror_state="ok"
    mirror_stale=false
  fi
fi
if [ "$bf_oldest_epoch" -gt 0 ] && [ "$bf_any_unknown" = false ] && [ "$bf_probe_failed" = false ]; then
  mirror_age_sec=$((now - bf_oldest_epoch))
  [ "$mirror_age_sec" -lt 0 ] && mirror_age_sec=0
  mirror_freshness=$(format_age "$mirror_age_sec")
fi

# Find orphan databases.
#
# Authoritative source: `gc dolt-cleanup` (HYPHEN — the Go-side command,
# dry-run by default, rig-protected). Its dry-run drop candidates
# (`dropped.names`) are the real orphans: every registered rig DB is excluded
# via city config, so a live rig DB is never listed. The previous
# metadata-only scan flagged every live rig DB as an orphan whenever a rig's
# metadata.json was sparse or unreachable (e.g. externally-pathed rigs) — a
# false positive automation could act on destructively (#3200). Reuse
# the cleanup authority; fall back to the metadata scan only when gc/jq are
# unavailable (gc itself may be the failure this patrol is detecting).
orphan_list=""
orphan_count=0
if [ -d "$data_dir" ]; then
  orphan_names=""
  cleanup_ok=false
  if command -v gc >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
    cleanup_json=$(run_bounded 10 gc dolt-cleanup --json 2>/dev/null) || true
    if [ -n "$cleanup_json" ] && printf '%s' "$cleanup_json" | jq -e '.dropped.names' >/dev/null 2>&1; then
      orphan_names=$(printf '%s' "$cleanup_json" | jq -r '.dropped.names[]? // empty' 2>/dev/null)
      cleanup_ok=true
    fi
  fi

  if [ "$cleanup_ok" != true ]; then
    # Fallback: approximate orphans from rig metadata (every DB whose name is
    # not referenced by a rig's metadata.json dolt_database). Less reliable
    # than the cleanup authority — used only when gc/jq are unavailable.
    referenced=""
    while IFS= read -r meta; do
      [ -f "$meta" ] || continue
      db=$(metadata_db "$meta")
      [ -n "$db" ] && referenced="$referenced $db "
    done < "$_meta_cache"
    for d in "$data_dir"/*/; do
      [ ! -d "$d/.dolt" ] && continue
      name="$(basename "$d")"
      case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
      case "$referenced" in *" $name "*) continue ;; esac
      orphan_names="$orphan_names$name
"
    done
  fi

  # Materialize the orphan list with on-disk sizes, from whichever source
  # produced the names. Only names that still exist as a Dolt database
  # directory are reported.
  for name in $orphan_names; do
    [ -n "$name" ] || continue
    d="$data_dir/$name"
    [ -d "$d/.dolt" ] || continue
    size_kb=$(du -sk "$d" 2>/dev/null | cut -f1)
    size_bytes=$(( ${size_kb:-0} * 1024 ))
    if [ "$size_bytes" -ge 1048576 ]; then
      size=$(awk "BEGIN {printf \"%.1f MB\", $size_bytes/1048576}")
    elif [ "$size_bytes" -ge 1024 ]; then
      size=$(awk "BEGIN {printf \"%.1f KB\", $size_bytes/1024}")
    else
      size="${size_bytes} B"
    fi
    orphan_list="$orphan_list$name|$size
"
    orphan_count=$((orphan_count + 1))
  done
fi

# Detect active compaction quarantine markers.
#
# `gc dolt compact` writes a per-database marker under
# $PACK_STATE_DIR/compact-quarantine/<db> when a post-flatten integrity probe
# trips (value-hash drift, row-count change, etc. — see commands/compact/run.sh).
# While a marker stands, auto-GC and scheduled compaction for that database are
# blocked indefinitely until an operator clears it, so the working set can grow
# unbounded and degrade the managed sql-server. Nothing else in this report
# surfaces the marker, so a quarantine can sit unnoticed for many days
# (gascity#3729). Scan filesystem-only — independent of server reachability,
# since a wedged server may itself be a downstream symptom of the un-GC'd
# bloat — and report each marker's db, reason, and age. The directory and
# one-file-per-db key=value body layout mirror compact/run.sh exactly.
quarantine_dir="$PACK_STATE_DIR/compact-quarantine"
quarantine_list=""
quarantine_count=0
if [ -d "$quarantine_dir" ]; then
  for marker in "$quarantine_dir"/*; do
    [ -f "$marker" ] || continue
    q_db=$(basename "$marker")
    # compact/run.sh writes transient files into this same directory:
    # `mktemp "$dir/$db.tmp.XXXXXX"` (write_compact_marker) and
    # `mktemp "$dir/$db.probe.XXXXXX"` (ensure_compact_marker_writable, run on
    # EVERY flatten). Neither is a marker; reading one yields a phantom entry
    # and a spurious exit 2.
    case "$q_db" in *.tmp.*|*.probe.*) continue ;; esac
    # Anchor each key to column 1 with index()==1 — the same reader idiom
    # compact/run.sh uses; the substr offset skips the "reason="/"created_at="
    # key (8 and 12 = key length + 1).
    q_reason=$(awk 'index($0, "reason=") == 1 { print substr($0, 8); exit }' "$marker" 2>/dev/null || true)
    q_created=$(awk 'index($0, "created_at=") == 1 { print substr($0, 12); exit }' "$marker" 2>/dev/null || true)
    [ -n "$q_reason" ] || q_reason="unknown"
    q_epoch=$(marker_epoch "$q_created")
    if [ -z "$q_epoch" ]; then
      q_epoch=$(stat -c %Y "$marker" 2>/dev/null || stat -f %m "$marker" 2>/dev/null || echo "")
    fi
    q_age_sec=0
    if [ -n "$q_epoch" ]; then
      q_now=$(date +%s)
      q_age_sec=$((q_now - q_epoch))
      [ "$q_age_sec" -lt 0 ] && q_age_sec=0
    fi
    quarantine_list="$quarantine_list$q_db|$q_reason|$q_age_sec
"
    quarantine_count=$((quarantine_count + 1))
  done
fi

# Check for zombie dolt processes.
# Use pgrep -x to match only processes named "dolt", then verify
# each is actually running sql-server via ps. This avoids false
# positives from processes that merely mention "dolt" in their args
# (e.g., Claude sessions whose prompt text contains "dolt sql-server").
#
# Rig-local Dolt servers (configured via dolt.port in config.yaml)
# are legitimate — exclude any PID listening on a known rig port.
#
# Foreign Dolt servers (managed by OTHER cities on the same host) are
# also legitimate. gc ALWAYS writes a dolt.pid next to a managed dolt
# config, so the sibling dolt.pid — located by parsing `--config <path>`
# from the process command line — is the authoritative ownership signal:
# present and self-referential means a healthy gc-managed instance.
# Externally-managed Dolt servers (launchd- or manually-started servers
# for unrelated apps, on their own datadir and port) also carry an
# explicit `--config` but have NO sibling dolt.pid; they are not town
# strays and must not be flagged, or health patrol automation could kill a
# healthy, unrelated server. Without these exclusions, every patrol in
# every city flags the others (and unrelated apps) as zombies on shared
# dev hosts. The `--config` parse happens inside the single bounded
# `ps -eo` + awk pass below (it already has the full args line in hand);
# only the sibling dolt.pid read is left to the shell loop, which
# iterates O(matched sql-servers) — never O(all pids/zombies) — so the
# bounded-fork invariant still holds.
#
# GC_HEALTH_SKIP_ZOMBIE_SCAN is a test-only escape hatch. Zombie
# enumeration spawns one `ps` per matching process, which on shared
# dev machines with many accumulated dolt processes dominates the
# runtime of the hang-mode test below. Setting it to "1" skips the
# scan so tests exercise just the bounded-probe behavior they care
# about without being hostage to ambient process state.
zombie_count=0
zombie_pids=""
if [ "${GC_HEALTH_SKIP_ZOMBIE_SCAN:-0}" != "1" ]; then
  # Collect PIDs of legitimate rig-local Dolt servers.
  rig_dolt_pids=""
  while IFS= read -r meta; do
    [ -f "$meta" ] || continue
    config_file="$(dirname "$meta")/config.yaml"
    [ -f "$config_file" ] || continue
    rig_port=$(grep '^dolt\.port:' "$config_file" 2>/dev/null | sed "s/^dolt\\.port:[[:space:]]*//; s/[[:space:]]*#.*$//; s/['\\\"]//g; s/[[:space:]]*$//" | head -1)
    case "$rig_port" in ''|*[!0-9]*) continue ;; esac
    [ "$rig_port" = "$GC_DOLT_PORT" ] && continue
    rig_pid=$(managed_runtime_listener_pid "$rig_port" || true)
    [ -n "$rig_pid" ] && rig_dolt_pids="$rig_dolt_pids $rig_pid "
  done < "$_meta_cache"

  # Enumerate the process table ONCE, not one `ps -p <pid> -o args=` fork per
  # `pgrep -x dolt` match. pgrep matches every dolt-named process including
  # Z-state zombies, so under a non-reaping PID 1 the old per-PID fork became
  # an O(zombies) `ps` storm re-paid on every 30s health tick (#2482). Collect
  # the candidate PIDs from pgrep, then classify them in a single `ps`+`awk`
  # pass: keep candidates that are dolt sql-server processes, skip Z-state
  # zombies (a defunct dolt never carries sql-server args anyway), and exclude
  # the managed city server and rig-local dolts. For each survivor the awk
  # pass also extracts the dolt `--config <path>` (or `--config=<path>`) from
  # the args line it already holds, and emits `pid<TAB>config_path` so the
  # shell loop below can do the foreign-managed check without re-forking ps.
  candidate_pids=" $(pgrep -x dolt 2>/dev/null | tr '\n' ' ' || true)"
  ps -eo pid=,stat=,args= 2>/dev/null | awk \
    -v server="$server_pid" -v rigs="$rig_dolt_pids" -v cands="$candidate_pids" '
    BEGIN {
      # Build an O(1) lookup set from the pgrep candidates once. The
      # per-row membership test below was an index() substring scan
      # re-paid for every process-table row, i.e. O(rows x candidate
      # string length); the reported incident had ~41k candidate PIDs
      # (#2618). Splitting into an associative set makes each lookup O(1).
      n = split(cands, a, " ")
      for (i = 1; i <= n; i++) if (a[i] != "") cand[a[i]] = 1
    }
    {
      pid = $1
      if (!(pid in cand)) next                   # not a pgrep -x dolt match
      if (pid == server) next                     # the managed city server
      if (index(rigs, " " pid " ") != 0) next     # a configured rig-local dolt
      if ($2 ~ /Z/) next                          # Z-state zombie: never a server
      if (index($0, "sql-server") == 0) next      # not a dolt sql-server
      # Extract the dolt --config path from the args fields (args start at
      # $3 after pid/stat). Accept both the space-separated `--config PATH`
      # and the `--config=PATH` spellings. Emitted alongside the pid so the
      # shell can read the sibling dolt.pid; empty when no --config is given.
      config = ""
      for (i = 3; i <= NF; i++) {
        if ($i == "--config" && (i + 1) <= NF) { config = $(i+1); break }
        if (index($i, "--config=") == 1) { config = substr($i, 10); break }
      }
      print pid "\t" config
    }' > "$_zombie_scan_out" 2>/dev/null || true

  # Iterate ONLY the matched sql-servers (O(matched servers)) the awk pass
  # emitted — not the full candidate/zombie set. This loop is where the
  # foreign-managed decision lives; keeping it bounded by the awk output is
  # what preserves the bounded-fork invariant. Reading from the scratch file
  # (not a pipe) keeps the loop in the parent shell so the zombie_count /
  # zombie_pids accumulation survives.
  _tab="$(printf '\t')"
  while IFS="$_tab" read -r p config_path; do
    [ -n "$p" ] || continue
    # Ownership check for processes launched with an explicit --config.
    # The sibling dolt.pid (gc writes one next to every managed config)
    # is authoritative — we key on its presence, not on whether the
    # config file itself is readable (it may live in another user's home
    # on a shared host):
    #   - present and claims this PID   -> healthy gc-managed Dolt instance
    #     (another city/rig on this host) -> not a zombie.
    #   - present but claims a DIFFERENT PID -> a gc-style config dir whose
    #     recorded server died or was replaced -> still a zombie.
    #   - absent -> the process is NOT gc-managed (e.g. a launchd-managed
    #     or manually-started server for an unrelated app on its own
    #     datadir/port) -> not a town stray; exclude it so automation
    #     does not kill a healthy, unrelated Dolt server.
    if [ -n "$config_path" ]; then
      foreign_pid_file="$(dirname "$config_path")/dolt.pid"
      if [ -f "$foreign_pid_file" ]; then
        recorded_pid=$(head -1 "$foreign_pid_file" 2>/dev/null | tr -d ' \t\r\n')
        [ "$recorded_pid" = "$p" ] && continue
      else
        continue
      fi
    fi
    zombie_count=$((zombie_count + 1))
    zombie_pids="$zombie_pids $p"
  done < "$_zombie_scan_out"
fi

# Output.
timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

if [ "$json_output" = true ]; then
  # Build JSON output. `server.reachable` reports whether the SQL
  # handshake actually succeeded (port listening AND server answering
  # SELECT 1). Consumers should key health off
  # `server.reachable`, not `server.running`, because a process can
  # hold the port while its goroutines are wedged.
  #
  # `server.external` distinguishes a configured remote endpoint from a local
  # managed server. For an external endpoint GC owns no local process, so
  # `server.running` / `server.pid` are local-process defaults (false / 0) and
  # MUST NOT be read as a downed server — a reachable remote endpoint is
  # healthy at `server.reachable=true, server.external=true` (su-deol8).
  cat <<JSONEOF
{
  "timestamp": "$timestamp",
  "server": {
    "running": $server_running,
    "reachable": $server_reachable,
    "external": $is_external,
    "pid": $server_pid,
    "port": $GC_DOLT_PORT,
    "latency_ms": $server_latency
  },
  "databases": [
JSONEOF
  first=true
  echo "$db_info" | while IFS='|' read -r name commits open_beads remotes; do
    [ -z "$name" ] && continue
    if [ "$first" = true ]; then first=false; else echo ","; fi
    printf '    {"name": "%s", "commits": %s, "open_beads": %s}' "$name" "$commits" "$open_beads"
  done
  cat <<JSONEOF

  ],
  "backups": {
    "dolt_measured": $backup_measured,
    "dolt_freshness": "$backup_freshness",
    "dolt_age_sec": $backup_age_sec,
    "dolt_stale": $backup_stale,
    "migration_measured": $migration_measured,
    "migration_freshness": "$migration_freshness",
    "migration_age_sec": $migration_age_sec,
    "migration_stale": $migration_stale,
    "dolt_databases": [
JSONEOF
  first=true
  echo "$backup_db_list" | while IFS='|' read -r b_name b_age b_fresh b_stale; do
    [ -z "$b_name" ] && continue
    if [ "$first" = true ]; then first=false; else echo ","; fi
    # age_sec is -1 for an eligible database that has never produced a backup
    # file; stale is true there, so no consumer reads -1 as a fresh age.
    printf '      {"name": "%s", "age_sec": %s, "freshness": "%s", "stale": %s}' \
      "$b_name" "$b_age" "$b_fresh" "$b_stale"
  done
  cat <<JSONEOF

    ],
    "origin_mirrors": {
      "freshness": "$mirror_freshness",
      "age_sec": $mirror_age_sec,
      "stale": $mirror_stale,
      "state": "$mirror_state",
      "databases": [
JSONEOF
  first=true
  echo "$mirror_detail" | while IFS='|' read -r bname bstate bage brefspec bnote; do
    [ -z "$bname" ] && continue
    if [ "$first" = true ]; then first=false; else echo ","; fi
    case "$bage" in ''|*[!0-9]*) bage=null ;; esac
    # The park reason comes from operator env and the refspec from a stamp
    # file; escape backslash then quote so neither can break the document.
    brefspec_esc=$(printf '%s' "$brefspec" | sed 's/\\/\\\\/g; s/"/\\"/g')
    bnote_esc=$(printf '%s' "$bnote" | sed 's/\\/\\\\/g; s/"/\\"/g')
    printf '        {"name": "%s", "state": "%s", "age_sec": %s, "refspec": "%s", "note": "%s"}' \
      "$bname" "$bstate" "$bage" "$brefspec_esc" "$bnote_esc"
  done
  cat <<JSONEOF

      ]
    }
  },
  "orphans": [
JSONEOF
  first=true
  echo "$orphan_list" | while IFS='|' read -r name size; do
    [ -z "$name" ] && continue
    if [ "$first" = true ]; then first=false; else echo ","; fi
    printf '    {"name": "%s", "size": "%s"}' "$name" "$size"
  done
  cat <<JSONEOF

  ],
  "quarantine": [
JSONEOF
  first=true
  echo "$quarantine_list" | while IFS='|' read -r q_db q_reason q_age_sec; do
    [ -z "$q_db" ] && continue
    if [ "$first" = true ]; then first=false; else echo ","; fi
    # db and reason both come from the filesystem; escape backslash then quote.
    q_db_esc=$(printf '%s' "$q_db" | sed 's/\\/\\\\/g; s/"/\\"/g')
    q_reason_esc=$(printf '%s' "$q_reason" | sed 's/\\/\\\\/g; s/"/\\"/g')
    printf '    {"db": "%s", "reason": "%s", "age_sec": %s}' "$q_db_esc" "$q_reason_esc" "$q_age_sec"
  done
  cat <<JSONEOF

  ],
  "processes": {
    "zombie_count": $zombie_count,
    "zombie_pids": [$(echo "$zombie_pids" | tr -s ' ' ',' | sed 's/^,//;s/,$//')]
  }
}
JSONEOF
  # JSON mode always exits 0 when the payload is well-formed. Health
  # state is signalled in-band via `server.reachable` (and the rest of
  # the document). Automation that parses the JSON must not fail before
  # stdout is parsed just because
  # the server is down; that's exactly the condition the patrol is
  # supposed to detect and react to. Callers that want exit-code
  # signalling should use the human-readable form.
  exit 0
fi

# Human-readable output. For a configured external endpoint GC owns no local
# process, so report reachability of the remote server rather than the
# local-process "not running" signal that would misread as a downed server
# (su-deol8).
if [ "$server_running" = true ]; then
  echo "Server: running (PID $server_pid, port $GC_DOLT_PORT, latency ${server_latency}ms)"
elif [ "$is_external" = true ] && [ "$server_reachable" = true ]; then
  echo "Server: external endpoint reachable ($host:$GC_DOLT_PORT, latency ${server_latency}ms)"
elif [ "$is_external" = true ]; then
  echo "Server: external endpoint unreachable ($host:$GC_DOLT_PORT)"
else
  echo "Server: not running"
fi

if [ -n "$db_info" ]; then
  echo ""
  echo "Databases:"
  echo "$db_info" | while IFS='|' read -r name commits open_beads remotes; do
    [ -z "$name" ] && continue
    echo "  $name: $commits commits, $open_beads open beads"
  done
fi

echo ""
if [ "$backup_measured" = true ]; then
  echo "Backups:"
  echo "$backup_db_list" | while IFS='|' read -r b_name b_age b_fresh b_stale; do
    [ -z "$b_name" ] && continue
    if [ "$b_age" -lt 0 ]; then
      echo "  $b_name: never backed up [STALE]"
    elif [ "$b_stale" = true ]; then
      echo "  $b_name: ${b_fresh} ago [STALE]"
    else
      echo "  $b_name: ${b_fresh} ago"
    fi
  done
else
  echo "Backups: not measured (no backup remotes under $backup_artifact_dir)"
fi
if [ "$migration_measured" = true ]; then
  migration_stale_note=""
  [ "$migration_stale" = true ] && migration_stale_note=" [STALE]"
  echo "Migration snapshots: ${migration_freshness} ago${migration_stale_note}"
fi

case "$mirror_state" in
  no-remotes)
    echo "Origin mirrors: no databases have a configured remote" ;;
  ok)
    echo "Origin mirrors: ok (last verified ${mirror_freshness} ago)" ;;
  *)
    stale=""
    [ "$mirror_stale" = true ] && [ "$mirror_state" != "stale" ] && stale=" [STALE]"
    if [ -n "$mirror_freshness" ]; then
      echo "Origin mirrors: ${mirror_state}${stale} (last verified ${mirror_freshness} ago)"
    else
      echo "Origin mirrors: ${mirror_state}${stale} (no successful verification recorded)"
    fi ;;
esac
if [ -n "$mirror_detail" ]; then
  echo "$mirror_detail" | while IFS='|' read -r bname bstate bage brefspec bnote; do
    [ -z "$bname" ] && continue
    if [ "$bstate" = parked ]; then
      # Printed, not omitted: a deliberately excluded mirror must be legible as
      # deliberate in the same output where the others read fresh or stale.
      echo "  $bname: parked — ${bnote:-no reason recorded} (not counted for durability)"
    elif [ -n "$bage" ]; then
      echo "  $bname: $bstate (verified ${bage}s ago, $brefspec)"
    elif [ -n "$bnote" ]; then
      echo "  $bname: $bstate ($bnote)"
    else
      echo "  $bname: $bstate (no successful verification recorded)"
    fi
  done
fi

if [ "$quarantine_count" -gt 0 ]; then
  echo ""
  echo "Compaction quarantine: $quarantine_count (auto-GC blocked)"
  echo "$quarantine_list" | while IFS='|' read -r q_db q_reason q_age_sec; do
    [ -z "$q_db" ] && continue
    echo "  $q_db: $q_reason (held $(human_duration "$q_age_sec"))"
  done
fi

if [ "$orphan_count" -gt 0 ]; then
  echo ""
  echo "Orphans: $orphan_count"
  echo "$orphan_list" | while IFS='|' read -r name size; do
    [ -z "$name" ] && continue
    echo "  $name ($size)"
  done
fi

if [ "$zombie_count" -gt 0 ]; then
  echo ""
  echo "Zombie processes: $zombie_count (PIDs:$zombie_pids)"
fi

# Exit status (human mode only): 0 when the data plane is healthy
# (server running AND answering SQL). Non-zero signals a CLI caller
# that something is wrong — server not running, or port in use by a
# process that isn't speaking MySQL. Stale backups, orphans, and
# zombies are informational and do not fail the exit code.
#
# A standing compaction quarantine is the exception: auto-GC for that
# database is blocked until an operator clears the marker, so it is a
# real (if non-fatal) data-plane degradation. Signal it with a distinct
# non-zero code (2) so CLI and CI callers can catch a blocked compaction
# without conflating it with an unreachable server (1).
#
# JSON mode is unconditionally exit 0 (see above) — programmatic
# consumers read `server.reachable` and the `quarantine` array from the
# payload instead.
if [ "$server_reachable" = true ]; then
  if [ "$quarantine_count" -gt 0 ]; then
    exit 2
  fi
  exit 0
fi
exit 1
