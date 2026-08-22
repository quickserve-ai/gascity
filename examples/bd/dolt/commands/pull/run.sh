#!/bin/sh
# gc dolt pull — Pull Dolt databases from their configured remotes.
#
# Uses the live Dolt SQL server when reachable so pull does not contend with
# active databases. Falls back to CLI mode only when no server is running.
# Pulls the configured remote's `main` branch in both SQL and CLI modes.
#
# Environment: GC_CITY_PATH, GC_DOLT_PORT, GC_DOLT_USER, GC_DOLT_PASSWORD
#   GC_DOLT_REMOTE_USER_<DB>_<REMOTE> (optional) — the username this database
#     presents to that remote for DOLT_PULL. Both halves uppercased with '-'
#     and '.' replaced by '_' (qcore + origin -> GC_DOLT_REMOTE_USER_QCORE_ORIGIN).
#     Unset means no identity is passed and the SQL is byte-identical to before
#     ga-p5bmfx. The PASSWORD never travels in the call — it comes from the dolt
#     SERVER process environment (DOLT_REMOTE_PASSWORD); argv is world-readable.
#
# Remote selection: a database with ONE remote pulls from it. A database with
# SEVERAL is an ERROR unless --remote names one. See list_remotes_sql below for
# why pull deliberately does NOT fan out the way sync does (ga-34kjld).
set -e

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

db_filter=""
remote_filter=""
data_dir="$DOLT_DATA_DIR"

while [ $# -gt 0 ]; do
  case "$1" in
    --db) db_filter="$2"; shift 2 ;;
    --remote) remote_filter="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: gc dolt pull [--db NAME] [--remote NAME]"
      echo ""
      echo "Pull Dolt databases from their configured remotes."
      echo ""
      echo "Flags:"
      echo "  --db NAME   Pull only the named database"
      echo "  --remote NAME   Pull from the named remote (required when a database has several)"
      exit 0
      ;;
    *) echo "gc dolt pull: unknown flag: $1" >&2; exit 1 ;;
  esac
done

case "$(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | tr '[:upper:]' '[:lower:]')" in
  information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe)
  echo "gc dolt pull: reserved Dolt database name: $(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//') (used internally by Dolt or gc)" >&2
  exit 1
  ;;
esac

is_running() {
  managed_runtime_tcp_reachable "$GC_DOLT_PORT"
}

valid_database_name() {
  case "$1" in
    [A-Za-z0-9_]*)
      case "$1" in *[!A-Za-z0-9_-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

valid_remote_name() {
  case "$1" in
    [A-Za-z0-9_.-]*)
      case "$1" in *[!A-Za-z0-9_.-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

# remote_identity_user <db> <remote> — emit the configured remote username for
# this (database, remote) pair, or nothing. Server-side DOLT_* procedures
# authenticate to the remote as the LOCAL SQL SESSION USER unless told
# otherwise; this pack connects as GC_DOLT_USER (root), so a remotesapi hub that
# knows a different account denies every pull (ga-tj1bgl). Scoping is per
# (DATABASE, REMOTE) with no global fallback — remote names are not unique
# across databases. Mirrors commands/sync/run.sh; the pack duplicates helpers
# across commands rather than sharing them (see valid_remote_name).
remote_identity_user() {
  riu_db="$1"
  riu_remote="$2"
  valid_database_name "$riu_db" || return 1
  valid_remote_name "$riu_remote" || return 1
  riu_key="$(printf '%s' "$riu_db" | tr 'a-z.-' 'A-Z__')_$(printf '%s' "$riu_remote" | tr 'a-z.-' 'A-Z__')"
  case "$riu_key" in
    *[!A-Z0-9_]*) return 0 ;;
  esac
  eval "printf '%s' \"\${GC_DOLT_REMOTE_USER_$riu_key:-}\""
}

# remote_identity_sql_args <db> <remote> — emit "'--user', '<user>', " or nothing.
# Fails loudly on an implausible username rather than splicing it into SQL.
# THE PASSWORD IS NEVER EMITTED HERE.
remote_identity_sql_args() {
  risa_user=$(remote_identity_user "$1" "$2") || return 1
  [ -n "$risa_user" ] || return 0
  case "$risa_user" in
    *[!A-Za-z0-9_.-]*)
      printf 'gc dolt pull: refusing remote user %s for %s/%s (allowed A-Za-z0-9_.-)\n' "$risa_user" "$1" "$2" >&2
      return 1
      ;;
  esac
  printf "'--user', '%s', " "$risa_user"
}

dolt_sql() {
  query="$1"
  host="${GC_DOLT_HOST:-127.0.0.1}"
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  run_bounded 120 dolt --host "$host" --port "$GC_DOLT_PORT" --user "$GC_DOLT_USER" --no-tls \
    sql --result-format csv -q "$query"
}

# list_remotes_sql <db> — emit EVERY configured remote as "name|url", one per
# line, ordered by name.
#
# ga-34kjld: this was `SELECT name, url FROM dolt_remotes LIMIT 1` with an awk
# that exited after the first row, so a database with several remotes pulled
# from whichever row Dolt happened to return first — arbitrary, and silently so.
#
# WHY PULL DOES NOT FAN OUT THE WAY SYNC DOES — the answer here is deliberately
# NOT ga-3o5xrw's. Pushing to every remote is additive: each remote receives our
# state independently and no remote's outcome changes another's, so "push to all
# of them" is simply more of the same operation. Pulling is a MERGE into the
# local branch. Pulling from N remotes would run N sequential merges, each able
# to conflict, with the outcome depending on order and a mid-sequence failure
# leaving a partially merged working set behind. That is a different operation
# from the one anyone asked for, and it is not something a patrol should decide
# on its own. So pull stays single-remote and makes the CHOICE explicit instead:
# exactly one remote is used silently, several is an error that names them, and
# --remote disambiguates.
list_remotes_sql() {
  db="$1"
  remote_csv=$(dolt_sql "USE \`$db\`; SELECT name, url FROM dolt_remotes ORDER BY name") || return 1
  printf '%s\n' "$remote_csv" | awk -F, 'NR > 1 && $1 != "" {print $1 "|" $2}'
}

# select_remote <db> <remotes-newline-separated> — emit the single "name|url" to
# pull from, or fail with a diagnostic. Honors --remote when set.
select_remote() {
  sr_db="$1"
  sr_all="$2"
  if [ -n "$remote_filter" ]; then
    sr_hit=$(printf '%s\n' "$sr_all" | awk -F'|' -v want="$remote_filter" '$1 == want {print; exit}')
    if [ -z "$sr_hit" ]; then
      echo "  $sr_db: ERROR: no remote named $remote_filter (have: $(printf '%s\n' "$sr_all" | cut -d'|' -f1 | tr '\n' ' '))" >&2
      return 1
    fi
    printf '%s' "$sr_hit"
    return 0
  fi
  sr_count=$(printf '%s\n' "$sr_all" | grep -c . || true)
  if [ "$sr_count" -gt 1 ]; then
    echo "  $sr_db: ERROR: $sr_count remotes configured ($(printf '%s\n' "$sr_all" | cut -d'|' -f1 | tr '\n' ' ')) — pull merges, so it will not choose for you; re-run with --remote NAME" >&2
    return 1
  fi
  printf '%s\n' "$sr_all" | sed -n '1p'
}

pull_database_sql() {
  name="$1"
  if ! valid_database_name "$name"; then
    echo "  $name: ERROR: invalid database name" >&2
    return 1
  fi

  remote_all=$(list_remotes_sql "$name") || {
    echo "  $name: ERROR: failed to query remotes" >&2
    return 1
  }
  if [ -z "$remote_all" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  remote_pair=$(select_remote "$name" "$remote_all") || return 1
  if [ -z "$remote_pair" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  remote_name=${remote_pair%%|*}
  remote_url=${remote_pair#*|}
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    return 1
  fi

  remote_ident=$(remote_identity_sql_args "$name" "$remote_name") || {
    echo "  $name: ERROR: invalid remote identity for remote $remote_name" >&2
    return 1
  }

  if dolt_sql "USE \`$name\`; CALL DOLT_PULL(${remote_ident}'$remote_name', 'main')" >/dev/null 2>&1; then
    echo "  $name: pulled from $remote_url"
    return 0
  fi

  echo "  $name: ERROR: pull failed" >&2
  return 1
}

pull_database_cli() {
  d="$1"
  name="$2"

  # ga-34kjld, CLI half. This previously grepped name and url INDEPENDENTLY and
  # took `head -1` of each, so with several remotes configured it could pair one
  # remote's name with a DIFFERENT remote's url and then report having pulled
  # from a url it never contacted. Names and urls are now read as PAIRS in file
  # order, and the same rule as the SQL path applies: one remote is used, several
  # is an error unless --remote names one.
  #
  # IDENTITY IS NOT AVAILABLE IN CLI MODE. GC_DOLT_REMOTE_USER_<DB>_<REMOTE> is
  # passed to the server-side DOLT_PULL procedure; the `dolt pull` CLI below
  # authenticates from dolt's own credential store instead. CLI mode is the
  # no-server fallback, so an authenticated hub pull must run with the server up.
  # Said out loud rather than left as a silent asymmetry.
  remote_name=""
  remote_url=""
  if [ -f "$d/.dolt/remotes.json" ]; then
    remotes_all=$(tr ',' '\n' < "$d/.dolt/remotes.json" 2>/dev/null \
      | awk '
          /"name":"/ { if (match($0, /"name":"[^"]*"/)) { n=substr($0, RSTART+8, RLENGTH-9) } }
          /"url":"/  { if (match($0, /"url":"[^"]*"/))  { u=substr($0, RSTART+7, RLENGTH-8); if (n != "") { print n "|" u; n="" } } }
        ' || true)
    if [ -n "$remotes_all" ]; then
      remote_pair_cli=$(select_remote "$name" "$remotes_all") || return 1
      remote_name=${remote_pair_cli%%|*}
      remote_url=${remote_pair_cli#*|}
    fi
  fi
  [ -z "$remote_name" ] && remote_name="origin"

  if [ -z "$remote_url" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    return 1
  fi

  if (cd "$d" && dolt pull "$remote_name" main 2>&1); then
    echo "  $name: pulled from $remote_url"
    return 0
  fi

  echo "  $name: ERROR: pull failed" >&2
  return 1
}

exit_code=0
server_running=false
is_running && server_running=true
if [ -d "$data_dir" ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
    [ -n "$db_filter" ] && [ "$name" != "$db_filter" ] && continue
    if [ -f "$d/.no-sync" ]; then
      echo "  $name: skipped (.no-sync)"
      continue
    fi

    if [ "$server_running" = true ]; then
      pull_database_sql "$name" || exit_code=1
    else
      pull_database_cli "$d" "$name" || exit_code=1
    fi
  done
fi

exit $exit_code
