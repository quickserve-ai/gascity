#!/bin/sh
# gc dolt health — Lightweight Dolt data-plane health report.
#
# Checks server status and latency, per-database commit counts and open
# beads, backup freshness, orphan databases, and zombie Dolt processes.
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

# Find dolt PID by port.
pid=$(managed_runtime_listener_pid "$GC_DOLT_PORT" || true)
if [ -n "$pid" ] || managed_runtime_tcp_reachable "$GC_DOLT_PORT"; then
  server_running=true
  [ -n "$pid" ] && server_pid="$pid"
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
db_info=""
if [ -d "$data_dir" ] && [ "$server_reachable" = true ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
    # Reject names with anything outside [A-Za-z0-9_-] before interpolating
    # into the SQL identifier. The first byte must still be alnum/underscore
    # to avoid option-shaped names. Dolt permits directory names that shell
    # basename happily returns (e.g. backticks, semicolons) but which
    # would break out of the identifier and execute attacker-chosen SQL
    # as the patrol user. Not an external-attack surface today — data
    # directories are server-controlled — but fragile enough under
    # config drift that it's worth skipping rather than probing.
    case "$name" in
      [A-Za-z0-9_]*)
        case "$name" in *[!A-Za-z0-9_-]*) continue ;; esac
        ;;
      *) continue ;;
    esac
    # Count commits via SQL (bounded). 0 on timeout or error — keep
    # going rather than hang the whole report. Extract the first
    # fully-numeric line rather than `sed -n '2p'`: future dolt builds
    # may emit a status row for `USE` or a warning banner, in which
    # case positional parsing silently collapses the count to 0 and the
    # "empty repo" fallback masks the parse miss. Numeric-line grep
    # gives a deterministic result or clearly-failed parse.
    commits_csv=$(run_bounded 5 dolt $conn_args sql --result-format csv \
      -q "USE \`$name\`; SELECT COUNT(*) FROM dolt_log;" 2>/dev/null || true)
    commits=$(printf '%s\n' "$commits_csv" | grep -E '^[0-9]+$' | head -1)
    # JSON consumers require a number; use 0 on failure.
    case "$commits" in
      ''|*[!0-9]*) commits=0 ;;
    esac
    # Count open beads from the running server (authoritative). Under managed
    # Dolt the beads live in the server's `issues` table, not an on-disk
    # beads.jsonl — that file is absent or stale, so the old file grep reported
    # open_beads=0 for every live database (#3200). 0 on timeout, error, or a
    # database without an `issues` table (a non-beads DB) — same fail-soft
    # contract as the commit count above.
    open_csv=$(run_bounded 5 dolt $conn_args sql --result-format csv \
      -q "USE \`$name\`; SELECT COUNT(*) FROM issues WHERE status='open';" 2>/dev/null || true)
    open_beads=$(printf '%s\n' "$open_csv" | grep -E '^[0-9]+$' | head -1)
    case "$open_beads" in
      ''|*[!0-9]*) open_beads=0 ;;
    esac
    # Count configured remotes (bounded, fail-soft to "" so a probe failure
    # is distinguishable from a measured 0). Feeds the backup-freshness
    # check below: only databases WITH a remote are backup targets.
    remotes_csv=$(run_bounded 5 dolt $conn_args sql --result-format csv \
      -q "USE \`$name\`; SELECT COUNT(*) FROM dolt_remotes;" 2>/dev/null || true)
    remotes=$(printf '%s\n' "$remotes_csv" | grep -E '^[0-9]+$' | head -1)
    # ga-3o5xrw: the COUNT alone cannot say WHICH remotes are configured, and
    # the mirror-freshness verdict below needs the names — it requires a recent
    # stamp for EACH of them, not merely for "some remote". Field 4 keeps its
    # existing meaning (count; empty = probe failed and remotes are unknowable)
    # so nothing downstream changes; field 5 adds the names.
    remote_names=""
    if [ -n "$remotes" ] && [ "$remotes" != "0" ]; then
      names_csv=$(run_bounded 5 dolt $conn_args sql --result-format csv \
        -q "USE \`$name\`; SELECT name FROM dolt_remotes ORDER BY name;" 2>/dev/null || true)
      remote_names=$(printf '%s\n' "$names_csv" | sed '1d' | tr -d '"\r' \
        | grep -E '^[A-Za-z0-9_.-]+$' | paste -sd, -)
    fi
    db_info="$db_info$name|$commits|$open_beads|$remotes|$remote_names
"
  done
fi

# mirror_park_reason <db> <remote> — echo the reason a database/remote pair is
# deliberately excluded from the durability verdict, or nothing if it is not.
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
# averaged away by a healthy sibling.
#
# Verdict rules — no data must NEVER read as healthy:
#   - db with a remote and a recent verification       -> ok
#   - db with a remote and an old verification         -> stale
#   - db with a remote and NO verification             -> unknown
#     (a fresh install reads "unknown" until sync first verifies its remote)
#   - db without a remote                              -> skipped (not a target)
#   - no db has a remote                               -> state "no-remotes",
#     dolt_stale=false: that is a MEASURED verdict (nothing is supposed to
#     be backed up), unlike the historical fail-open where stale=false was
#     just an initializer that never got overwritten.
#   - server unreachable (remotes unknowable)          -> unknown, stale=true
backup_stale_threshold="${GC_DOLT_BACKUP_STALE_SECS:-1800}"
case "$backup_stale_threshold" in ''|*[!0-9]*) backup_stale_threshold=1800 ;; esac
backup_freshness=""
backup_age_sec=0
backup_stale=true
backup_state="unknown"
backup_detail=""
bf_now=$(date +%s)
bf_any_remote_db=false
bf_any_bad=false
bf_any_unknown=false
bf_probe_failed=false
bf_oldest_epoch=0
if [ "$server_reachable" = true ] && [ -n "$db_info" ]; then
  # Parse the per-db lines collected above (name|commits|open_beads|remotes).
  bf_lines=$(printf '%s' "$db_info")
  while IFS='|' read -r bf_name _bf_commits _bf_open bf_remotes bf_remote_names; do
    [ -z "$bf_name" ] && continue
    case "$bf_remotes" in
      '') bf_probe_failed=true; continue ;;   # probe failed: can't classify this db
      0) continue ;;                          # no remote: not a backup target
    esac
    bf_any_remote_db=true
    # The count says there ARE remotes but the name probe came back empty, so
    # this database's remotes are unknowable. Never fall through to "nothing to
    # verify" — that is the false-green shape this whole check exists to avoid.
    if [ -z "$bf_remote_names" ]; then
      bf_probe_failed=true
      backup_detail="$backup_detail$bf_name|unknown|||remote list unreadable
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
        backup_detail="$backup_detail$bf_name@$bf_remote|parked|||$bf_park_reason
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
            bf_age=$((bf_now - bf_epoch))
            [ "$bf_age" -lt 0 ] && bf_age=0
            if [ "$bf_age" -le "$backup_stale_threshold" ]; then
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
      backup_detail="$backup_detail$bf_name@$bf_remote|$bf_state|$bf_age|$bf_refspec|
"
    done
  done <<BFEOF
$bf_lines
BFEOF
  if [ "$bf_any_remote_db" = false ] && [ "$bf_probe_failed" = false ]; then
    backup_state="no-remotes"
    backup_stale=false
  elif [ "$bf_probe_failed" = true ] || [ "$bf_any_unknown" = true ]; then
    backup_state="unknown"
    backup_stale=true
  elif [ "$bf_any_bad" = true ]; then
    backup_state="stale"
    backup_stale=true
  else
    backup_state="ok"
    backup_stale=false
  fi
fi
if [ "$bf_oldest_epoch" -gt 0 ] && [ "$bf_any_unknown" = false ] && [ "$bf_probe_failed" = false ]; then
  backup_age_sec=$((bf_now - bf_oldest_epoch))
  [ "$backup_age_sec" -lt 0 ] && backup_age_sec=0
  if [ "$backup_age_sec" -ge 3600 ]; then
    backup_freshness="$((backup_age_sec / 3600))h$((backup_age_sec % 3600 / 60))m"
  elif [ "$backup_age_sec" -ge 60 ]; then
    backup_freshness="$((backup_age_sec / 60))m$((backup_age_sec % 60))s"
  else
    backup_freshness="${backup_age_sec}s"
  fi
fi

# Local recovery artifacts are a separate backup plane from GitHub-origin
# mirrors. mol-dog-backup writes one <db>/manifest under .dolt-backup; mirror
# push stamps above say nothing about those local artifacts (ga-co5cx).
#
# Freshness on this plane comes from $LOCAL_BACKUP_FRESHNESS_DIR/<db>
# (synced_at_epoch, written only on a sync that exits 0 — see runtime.sh), NOT
# from any mtime under .dolt-backup. The manifest's presence still gates
# "is this database a local backup target at all", but it does not date it:
# a killed sync leaves chunks and an old manifest behind (ga-g3p5rm).
local_backup_dir="${GC_BACKUP_ARTIFACT_DIR:-$GC_CITY_PATH/.dolt-backup}"
local_backup_threshold="${GC_DOLT_LOCAL_BACKUP_STALE_SECS:-28800}"
case "$local_backup_threshold" in ''|*[!0-9]*) local_backup_threshold=28800 ;; esac
local_backup_state="unknown"
local_backup_stale=true
local_backup_age_sec=0
local_backup_freshness=""
local_backup_detail=""
local_backup_any=false
local_backup_bad=false
local_backup_oldest_epoch=0

local_db_names=""
if [ -n "$db_info" ]; then
  local_db_names=$(printf '%s\n' "$db_info" | cut -d'|' -f1)
elif [ -d "$local_backup_dir" ]; then
  local_db_names=$(for local_manifest in "$local_backup_dir"/*/manifest; do
    [ -f "$local_manifest" ] || continue
    basename "$(dirname "$local_manifest")"
  done)
fi

for local_db in $local_db_names; do
  local_manifest="$local_backup_dir/$local_db/manifest"
  local_stamp="$LOCAL_BACKUP_FRESHNESS_DIR/$local_db"
  local_state="unknown"
  local_age=""
  if [ -f "$local_manifest" ]; then
    local_backup_any=true
    # ga-g3p5rm: freshness comes from a stamp that ONLY a sync exiting 0 writes,
    # never from an mtime on the artifact plane. A failed sync writes chunk
    # files into .dolt-backup/<db>, which bumps that directory's mtime exactly
    # as a successful sync would — so the old reading self-healed in the wrong
    # direction and reported a 6.5-day-dead hq backup as ok. FAIL CLOSED: no
    # stamp means unknown, and unknown is not ok.
    local_epoch=$(sed -n 's/^synced_at_epoch=//p' "$local_stamp" 2>/dev/null | head -1)
    case "$local_epoch" in
      ''|*[!0-9]*) local_state="unknown" ;;
      *)
        local_age=$((bf_now - local_epoch))
        [ "$local_age" -lt 0 ] && local_age=0
        if [ "$local_age" -le "$local_backup_threshold" ]; then
          local_state="ok"
        else
          local_state="stale"
        fi
        if [ "$local_backup_oldest_epoch" -eq 0 ] || [ "$local_epoch" -lt "$local_backup_oldest_epoch" ]; then
          local_backup_oldest_epoch="$local_epoch"
        fi
        ;;
    esac
  fi
  [ "$local_state" = "ok" ] || local_backup_bad=true
  local_backup_detail="$local_backup_detail$local_db|$local_state|$local_age
"
done

if [ "$local_backup_any" = true ] && [ "$local_backup_bad" = false ]; then
  local_backup_state="ok"
  local_backup_stale=false
elif [ "$local_backup_any" = true ]; then
  local_backup_state="stale"
fi
if [ "$local_backup_oldest_epoch" -gt 0 ]; then
  local_backup_age_sec=$((bf_now - local_backup_oldest_epoch))
  [ "$local_backup_age_sec" -lt 0 ] && local_backup_age_sec=0
  if [ "$local_backup_age_sec" -ge 3600 ]; then
    local_backup_freshness="$((local_backup_age_sec / 3600))h$((local_backup_age_sec % 3600 / 60))m"
  elif [ "$local_backup_age_sec" -ge 60 ]; then
    local_backup_freshness="$((local_backup_age_sec / 60))m$((local_backup_age_sec % 60))s"
  else
    local_backup_freshness="${local_backup_age_sec}s"
  fi
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
  cat <<JSONEOF
{
  "timestamp": "$timestamp",
  "server": {
    "running": $server_running,
    "reachable": $server_reachable,
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
    "local": {
      "freshness": "$local_backup_freshness",
      "age_sec": $local_backup_age_sec,
      "stale": $local_backup_stale,
      "state": "$local_backup_state",
      "databases": [
JSONEOF
  first=true
  echo "$local_backup_detail" | while IFS='|' read -r bname bstate bage; do
    [ -z "$bname" ] && continue
    if [ "$first" = true ]; then first=false; else echo ","; fi
    case "$bage" in ''|*[!0-9]*) bage=null ;; esac
    printf '        {"name": "%s", "state": "%s", "age_sec": %s}' "$bname" "$bstate" "$bage"
  done
  cat <<JSONEOF

      ]
    },
    "origin_mirrors": {
      "freshness": "$backup_freshness",
      "age_sec": $backup_age_sec,
      "stale": $backup_stale,
      "state": "$backup_state",
      "databases": [
JSONEOF
  first=true
  echo "$backup_detail" | while IFS='|' read -r bname bstate bage brefspec bnote; do
    [ -z "$bname" ] && continue
    if [ "$first" = true ]; then first=false; else echo ","; fi
    case "$bage" in ''|*[!0-9]*) bage=null ;; esac
    printf '        {"name": "%s", "state": "%s", "age_sec": %s, "refspec": "%s", "note": "%s"}' \
      "$bname" "$bstate" "$bage" "$brefspec" "$bnote"
  done
  cat <<JSONEOF

      ]
    },
    "dolt_freshness": "$backup_freshness",
    "dolt_age_sec": $backup_age_sec,
    "dolt_stale": $backup_stale,
    "dolt_backup_state": "$backup_state",
    "dolt_backup_dbs": [
JSONEOF
  first=true
  echo "$backup_detail" | while IFS='|' read -r bname bstate bage brefspec bnote; do
    [ -z "$bname" ] && continue
    if [ "$first" = true ]; then first=false; else echo ","; fi
    case "$bage" in ''|*[!0-9]*) bage=null ;; esac
    printf '      {"name": "%s", "state": "%s", "age_sec": %s, "refspec": "%s", "note": "%s"}' \
      "$bname" "$bstate" "$bage" "$brefspec" "$bnote"
  done
  cat <<JSONEOF

    ]
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

# Human-readable output.
if [ "$server_running" = true ]; then
  echo "Server: running (PID $server_pid, port $GC_DOLT_PORT, latency ${server_latency}ms)"
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
case "$local_backup_state" in
  ok) echo "Local recovery backups: ok (oldest update ${local_backup_freshness} ago)" ;;
  *) echo "Local recovery backups: ${local_backup_state} [STALE]" ;;
esac
if [ -n "$local_backup_detail" ]; then
  echo "$local_backup_detail" | while IFS='|' read -r bname bstate bage; do
    [ -z "$bname" ] && continue
    if [ -n "$bage" ]; then
      echo "  $bname: $bstate (updated ${bage}s ago)"
    else
      echo "  $bname: $bstate (backup missing or unreadable)"
    fi
  done
fi

case "$backup_state" in
  no-remotes)
    echo "Origin mirrors: no databases have a configured remote" ;;
  ok)
    echo "Origin mirrors: ok (last verified ${backup_freshness} ago)" ;;
  *)
    stale=""
    [ "$backup_stale" = true ] && [ "$backup_state" != "stale" ] && stale=" [STALE]"
    if [ -n "$backup_freshness" ]; then
      echo "Origin mirrors: ${backup_state}${stale} (last verified ${backup_freshness} ago)"
    else
      echo "Origin mirrors: ${backup_state}${stale} (no successful verification recorded)"
    fi ;;
esac
if [ -n "$backup_detail" ]; then
  echo "$backup_detail" | while IFS='|' read -r bname bstate bage brefspec bnote; do
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
# JSON mode is unconditionally exit 0 (see above) — programmatic
# consumers read `server.reachable` from the payload instead.
if [ "$server_reachable" = true ]; then
  exit 0
fi
exit 1
