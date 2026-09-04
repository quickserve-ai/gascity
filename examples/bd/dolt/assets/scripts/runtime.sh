#!/bin/sh

: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"

CITY_RUNTIME_DIR="${GC_CITY_RUNTIME_DIR:-$GC_CITY_PATH/.gc/runtime}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-$CITY_RUNTIME_DIR/packs/dolt}"
LEGACY_GC_DIR="$GC_CITY_PATH/.gc"

if [ -d "$PACK_STATE_DIR" ] || [ ! -d "$LEGACY_GC_DIR/dolt-data" ]; then
  DOLT_STATE_DIR="$PACK_STATE_DIR"
else
  DOLT_STATE_DIR="$LEGACY_GC_DIR"
fi

# Data lives under .beads/dolt (gc-beads-bd canonical path). Honor
# GC_DOLT_DATA_DIR first so shell pack commands target the same managed data
# directory as the Go lifecycle and doctor code.
DOLT_BEADS_DATA_DIR="${GC_DOLT_DATA_DIR:-$GC_CITY_PATH/.beads/dolt}"
if [ -n "${GC_DOLT_DATA_DIR:-}" ]; then
  DOLT_DATA_DIR="$GC_DOLT_DATA_DIR"
elif [ -d "$DOLT_BEADS_DATA_DIR" ]; then
  DOLT_DATA_DIR="$DOLT_BEADS_DATA_DIR"
else
  DOLT_DATA_DIR="$DOLT_STATE_DIR/dolt-data"
fi

DOLT_LOG_FILE="${GC_DOLT_LOG_FILE:-$DOLT_STATE_DIR/dolt.log}"
DOLT_PID_FILE="${GC_DOLT_PID_FILE:-$DOLT_STATE_DIR/dolt.pid}"
if [ -n "${GC_DOLT_STATE_FILE:-}" ]; then
  DOLT_STATE_FILE="$GC_DOLT_STATE_FILE"
else
  DOLT_STATE_FILE="$DOLT_STATE_DIR/dolt-state.json"
fi
DOLT_PROVIDER_STATE_FILE="$DOLT_STATE_DIR/dolt-provider-state.json"

# Backup remote-verification stamps (qc-lu207, ga-a8w1c). One file per
# database-and-remote PAIR under $PACK_STATE_DIR/backup-freshness/<db>@<remote>,
# written after a push succeeds OR a fetch-and-classify proves local and remote
# are up to date. Health reads these stamps without making its own network
# round trip.
#
# SEMANTICS — a stamp means "the sync path recently proved THIS remote contains
# the local branch", either because the push returned 0 or because a successful
# fetch found zero commits ahead and behind. This local proxy avoids remote
# probes in health itself, which must stay cheap and bounded when the remote is
# down.
#
# WHY THE KEY CARRIES THE REMOTE (ga-3o5xrw). The stamp used to be one file per
# DATABASE, which structurally cannot represent a database with N remotes: the
# last writer won, so a fresh stamp could mean "the live mirror was pushed" OR
# "the dead mirror was picked and nothing was pushed at all". qcore has two
# configured remotes and read fresh in exactly that second case. One stamp per
# pair makes the question health asks ("was EVERY configured remote verified
# recently?") expressible; a single per-db stamp could only ever answer "was
# SOMETHING verified recently?", which is not a durability claim.
#
# `@` is a safe separator: database names are constrained by valid_database_name
# and remote names by valid_remote_name, and neither admits '@' or '/'.
BACKUP_FRESHNESS_DIR="$PACK_STATE_DIR/backup-freshness"

# backup_stamp_path <db> <remote> — the stamp file for one database/remote pair.
backup_stamp_path() {
  printf '%s/%s@%s\n' "$BACKUP_FRESHNESS_DIR" "$1" "$2"
}

# write_backup_push_stamp <db> <remote> <local_branch> <remote_branch>
# Best-effort: a stamp failure must never fail a successful push or remote
# verification, so every step degrades to a silent no-op. tmp+mv keeps a
# concurrent health read from seeing a torn file.
write_backup_push_stamp() {
  bfs_db="$1"
  bfs_remote="$2"
  bfs_path=$(backup_stamp_path "$bfs_db" "$bfs_remote")
  mkdir -p "$BACKUP_FRESHNESS_DIR" 2>/dev/null || return 0
  {
    printf 'pushed_at_epoch=%s\n' "$(date +%s)"
    printf 'remote=%s\n' "$bfs_remote"
    printf 'refspec=%s:%s\n' "$3" "$4"
  } > "$bfs_path.tmp" 2>/dev/null || { rm -f "$bfs_path.tmp" 2>/dev/null; return 0; }
  mv -f "$bfs_path.tmp" "$bfs_path" 2>/dev/null || return 0
}

# Local backup-artifact success stamps (ga-g3p5rm). One file per database under
# $PACK_STATE_DIR/local-backup-freshness/<db>, written ONLY after
# `dolt backup sync` returns 0.
#
# WHY THIS EXISTS — health used to derive local freshness from the mtime of
# .dolt-backup/<db>. A directory's mtime bumps whenever an entry is created
# inside it, so a sync that wrote chunk files and then died bumped it exactly as
# a completed sync would. The signal measured was "something appeared in this
# directory recently"; the signal reported was "this database has a recent
# backup". Those agree in every case except the one the check exists to catch.
# Proven on hq: sync EXIT=124 with stderr "context canceled" advanced the
# directory mtime ~2h52m, and health called it ok while the newest VALID backup
# was 6.5 days old. Worse, it self-healed in the wrong direction — the more
# often the backup dog ran and failed, the fresher health claimed the backup was.
#
# SEMANTICS — a stamp means "dolt backup sync exited 0 for this database at this
# time". Absence means UNKNOWN, never ok: readers must fail closed rather than
# falling back to any mtime, because every mtime on this plane is writable by
# the failure path.
LOCAL_BACKUP_FRESHNESS_DIR="$PACK_STATE_DIR/local-backup-freshness"

# write_local_backup_sync_stamp <db> [artifact_dir]
# Best-effort, mirroring write_backup_push_stamp: a stamp failure must never
# fail an otherwise successful sync, so every step degrades to a silent no-op.
# tmp+mv keeps a concurrent health read from seeing a torn file.
write_local_backup_sync_stamp() {
  lbs_db="$1"
  mkdir -p "$LOCAL_BACKUP_FRESHNESS_DIR" 2>/dev/null || return 0
  {
    printf 'synced_at_epoch=%s\n' "$(date +%s)"
    printf 'artifact_dir=%s\n' "${2:-}"
  } > "$LOCAL_BACKUP_FRESHNESS_DIR/$lbs_db.tmp" 2>/dev/null || { rm -f "$LOCAL_BACKUP_FRESHNESS_DIR/$lbs_db.tmp" 2>/dev/null; return 0; }
  mv -f "$LOCAL_BACKUP_FRESHNESS_DIR/$lbs_db.tmp" "$LOCAL_BACKUP_FRESHNESS_DIR/$lbs_db" 2>/dev/null || return 0
}

# --- OFF-BOX (offsite) backup plane, ga-l9smko -------------------------------
#
# The local plane above proves a database was synced to .dolt-backup. It says
# NOTHING about whether that artifact directory was then copied OFF THE BOX, and
# for a city whose live store and its .dolt-backup sit on the same 93%-full
# volume, off-box is the only copy that survives the failure people actually
# hit. One disk event takes both (ga-jtjcdy).
#
# THE DEFECT THIS EXISTS TO REMOVE. mol-dog-backup's offsite leg was reported
# only as a word inside a summary line ("offsite: skipped"), while the RUN
# recorded success either way. So "GC_BACKUP_OFFSITE_PATH was never configured
# and nothing was copied" and "the off-box copy completed" produced byte-
# identical durable evidence, and the city's backup posture was green BY
# CONSTRUCTION for as long as the variable stayed unset.
#
# SEMANTICS, deliberately the same fail-closed shape as the local plane: a stamp
# means "rsync to the configured off-box path exited 0 at this time". Absence
# means UNKNOWN, never ok. Nothing but a real copy may write it — in particular
# an mtime under the offsite path must never be read as freshness, because the
# failure path writes there too. That is exactly how ga-g3p5rm's local plane
# reported a 6.5-day-dead backup as ok, and it self-healed in the wrong
# direction: the more often the dog ran and failed, the fresher it looked.
OFFSITE_BACKUP_FRESHNESS_FILE="$PACK_STATE_DIR/offsite-backup-freshness"

# write_offsite_backup_sync_stamp <offsite_path> [artifact_dir]
# Best-effort in exactly the sense the local writer is: a stamp failure must
# never fail an otherwise successful copy. tmp+mv keeps a concurrent health read
# from seeing a torn file.
write_offsite_backup_sync_stamp() {
  obs_path="${1:-}"
  mkdir -p "$(dirname "$OFFSITE_BACKUP_FRESHNESS_FILE")" 2>/dev/null || return 0
  {
    printf 'synced_at_epoch=%s\n' "$(date +%s)"
    printf 'offsite_path=%s\n' "$obs_path"
    printf 'artifact_dir=%s\n' "${2:-}"
  } > "$OFFSITE_BACKUP_FRESHNESS_FILE.tmp" 2>/dev/null || { rm -f "$OFFSITE_BACKUP_FRESHNESS_FILE.tmp" 2>/dev/null; return 0; }
  mv -f "$OFFSITE_BACKUP_FRESHNESS_FILE.tmp" "$OFFSITE_BACKUP_FRESHNESS_FILE" 2>/dev/null || return 0
}

# read_offsite_backup_sync_epoch — echo the epoch of the last REAL off-box copy,
# or nothing. Callers must treat "nothing" as unknown, never as ok.
read_offsite_backup_sync_epoch() {
  [ -f "$OFFSITE_BACKUP_FRESHNESS_FILE" ] || return 0
  robs_epoch=$(sed -n 's/^synced_at_epoch=//p' "$OFFSITE_BACKUP_FRESHNESS_FILE" 2>/dev/null | head -1)
  case "$robs_epoch" in
    ''|*[!0-9]*) return 0 ;;
  esac
  printf '%s' "$robs_epoch"
}

# offsite_waiver_reason — echo the reason this city has DELIBERATELY accepted
# having no off-box copy, or nothing when no such declaration exists.
#
# ONE SOURCE ON PURPOSE. The mirror-park declaration next door takes both a file
# and an env override, and its own comment records the hazard that created: the
# env copy silently OUTRANKS an edited file, so the declaration a reader edits
# can be the one that does not apply. A waiver is a durable operator risk
# acceptance, not a per-invocation flag, so it lives in exactly one place a
# person can read, diff and date:
#
#   $GC_CITY_PATH/config/dolt/offsite-waived
#
# First non-blank, non-comment line is the reason. Same file shape as
# config/dolt/mirrors-parked, so operators learn one convention rather than two.
offsite_waiver_reason() {
  owr_file="${GC_BACKUP_OFFSITE_WAIVER_FILE:-$GC_CITY_PATH/config/dolt/offsite-waived}"
  [ -f "$owr_file" ] || return 0
  while IFS= read -r owr_line || [ -n "$owr_line" ]; do
    # Strip a trailing CR before the emptiness test, or a CRLF file's blank and
    # comment lines both survive it and a reason arrives with a raw CR glued on.
    owr_line="${owr_line%$(printf '\r')}"
    case "$owr_line" in
      ''|\#*) continue ;;
    esac
    # SANITISE AT THE SOURCE. This string is operator-typed prose that reaches a
    # JSON document, a shell summary and a mail body. Reduce it here to
    # printable ASCII-safe text rather than trusting every downstream consumer
    # to escape it: a stray quote in a waiver reason took `gc dolt health --json`
    # from valid to unparseable, and its 30s consumer then reported the DOLT
    # SERVER as unreachable — an alarm that blames the wrong subsystem, fired by
    # the very remedy this mechanism tells operators to declare.
    # Escaping at emit time as well (json_escape_string) is belt and braces; this
    # is the belt.
    printf '%s' "$owr_line" | tr -d '\000-\037' | tr '"\\' "''"
    return 0
  done < "$owr_file"
  return 0
}

# json_escape_string — emit an arbitrary string safe for a JSON string literal.
# Escapes backslash and double quote and drops control characters. Used by any
# pack script that interpolates operator-supplied text into a JSON report.
json_escape_string() {
  printf '%s' "${1:-}" \
    | tr -d '\000-\037' \
    | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

# --- The offsite DECLARATION record ------------------------------------------
#
# WHY THIS EXISTS. GC_BACKUP_OFFSITE_PATH is an [[orders.overrides]] env value on
# mol-dog-backup. `gc dolt health` runs from a DIFFERENT order and inherits none
# of it, so health cannot see whether a target is declared at all — it would
# report a configured-but-broken city as "unconfigured" and point the operator at
# the wrong fix. Two processes needing one fact is exactly the hazard the
# mirrors-parked note above records.
#
# So the order that OWNS the fact publishes it, every run, success or failure.
# This is configuration state, not an action log: it says what the job was told
# to do and what it observed this cycle, and it is re-derived from scratch on
# every run rather than accumulated.
OFFSITE_BACKUP_DECLARATION_FILE="$PACK_STATE_DIR/offsite-backup-declaration"

# write_offsite_backup_declaration <declared_path> <outcome>
write_offsite_backup_declaration() {
  obd_path="${1:-}"
  obd_outcome="${2:-}"
  mkdir -p "$(dirname "$OFFSITE_BACKUP_DECLARATION_FILE")" 2>/dev/null || return 0
  {
    printf 'checked_at_epoch=%s\n' "$(date +%s)"
    printf 'declared_path=%s\n' "$obd_path"
    printf 'last_outcome=%s\n' "$obd_outcome"
  } > "$OFFSITE_BACKUP_DECLARATION_FILE.tmp" 2>/dev/null || { rm -f "$OFFSITE_BACKUP_DECLARATION_FILE.tmp" 2>/dev/null; return 0; }
  mv -f "$OFFSITE_BACKUP_DECLARATION_FILE.tmp" "$OFFSITE_BACKUP_DECLARATION_FILE" 2>/dev/null || return 0
}

# read_offsite_backup_declared_path / _checked_epoch — echo the field or nothing.
read_offsite_backup_declared_path() {
  [ -f "$OFFSITE_BACKUP_DECLARATION_FILE" ] || return 0
  sed -n 's/^declared_path=//p' "$OFFSITE_BACKUP_DECLARATION_FILE" 2>/dev/null | head -1
}

read_offsite_backup_checked_epoch() {
  [ -f "$OFFSITE_BACKUP_DECLARATION_FILE" ] || return 0
  robc_epoch=$(sed -n 's/^checked_at_epoch=//p' "$OFFSITE_BACKUP_DECLARATION_FILE" 2>/dev/null | head -1)
  case "$robc_epoch" in
    ''|*[!0-9]*) return 0 ;;
  esac
  printf '%s' "$robc_epoch"
}

# read_offsite_backup_stamp_path — the path the last REAL copy went to. Health
# compares it against the declared path: a stamp written to a target that is no
# longer configured dates a copy that is no longer the copy we would rely on.
read_offsite_backup_stamp_path() {
  [ -f "$OFFSITE_BACKUP_FRESHNESS_FILE" ] || return 0
  sed -n 's/^offsite_path=//p' "$OFFSITE_BACKUP_FRESHNESS_FILE" 2>/dev/null | head -1
}

# clear_offsite_backup_sync_stamp — invalidate the freshness plane.
#
# Called whenever a copy to the CONFIGURED target fails. The offsite rsync runs
# with --delete, so a run killed partway leaves the destination mutilated: the
# previous stamp then dates a tree that no longer exists in the form it
# described. Fail closed by removing it — "unknown" is recoverable on the next
# successful run, a confidently wrong "ok" is not.
clear_offsite_backup_sync_stamp() {
  rm -f "$OFFSITE_BACKUP_FRESHNESS_FILE" 2>/dev/null || true
  return 0
}

# paths_on_same_volume <a> <b> — 0 when both exist and share a device.
#
# The whole premise of an off-box copy is that one disk event cannot take both
# copies. Nothing verified that: `rsync -a --delete X/ X/` exits 0, so pointing
# the target at a sibling directory on the same 93%-full volume produced a
# stamped, "ok" off-box plane that was not off anything.
paths_on_same_volume() {
  posv_a="${1:-}"
  posv_b="${2:-}"
  [ -n "$posv_a" ] && [ -n "$posv_b" ] || return 1
  [ -e "$posv_a" ] && [ -e "$posv_b" ] || return 1
  posv_da=$(stat -f '%d' "$posv_a" 2>/dev/null || stat -c '%d' "$posv_a" 2>/dev/null || printf '')
  posv_db=$(stat -f '%d' "$posv_b" 2>/dev/null || stat -c '%d' "$posv_b" 2>/dev/null || printf '')
  [ -n "$posv_da" ] && [ -n "$posv_db" ] || return 1
  [ "$posv_da" = "$posv_db" ]
}

# offsite_target_is_remote <path> — 0 when the target is an rsync REMOTE spec
# (user@host:/path or host:/path), where a local device comparison is
# meaningless and must not be attempted.
offsite_target_is_remote() {
  otir_path="${1:-}"
  case "$otir_path" in
    rsync://*|*::*) return 0 ;;
    /*) return 1 ;;
    *:*) return 0 ;;
    *) return 1 ;;
  esac
}

GC_BEADS_BD_SCRIPT="$GC_CITY_PATH/.gc/scripts/gc-beads-bd.sh"

# Shared by health (which excludes a parked pair from the durability verdict
# but still prints it) and by sync (which does not touch a parked remote at
# all, so one abandoned mirror cannot hold the patrol permanently red).
# mirror_park_reason <db> <remote> — echo the reason a database/remote pair is
# deliberately excluded from the durability verdict, or nothing if it is not.
#
# THE DECLARATION HAS TWO SOURCES, in this precedence order:
#
#   1. GC_DOLT_MIRROR_PARKED — a comma-separated list of `<db>/<remote>[=reason]`,
#      e.g. "qcore/origin=gated on ga-qo9w". An explicit override for one
#      invocation or one order. A reason here may not contain a comma.
#   2. $GC_CITY_PATH/config/dolt/mirrors-parked — the city's declaration file,
#      one entry per line, `#` comments and blank lines ignored, reasons free to
#      contain commas. This is the source of truth; the env var exists to
#      override it.
#
# Parking is PER REMOTE, never per database: parking a whole database would also
# silence its healthy mirrors, which is the failure the park is supposed to make
# visible, not hide.
#
# WHY HEALTH KNOWS ABOUT PARKS AT ALL (ga-3o5xrw). qcore's dead mirror was
# deliberately parked on 2026-08-05, but the decision existed only as an env var
# inside an order file and a note on a bead — not in `gc dolt health`, which is
# where anyone actually looks. Three agents in one night each investigated that
# documented silence as a broken alarm, and each filed it as a monitoring defect
# before finding the park. A monitor must state what it is NOT covering and why,
# or correct silence costs more than a false alarm would have.
#
# WHY THE FILE EXISTS (ga-p2xtt4). Fixing the above by teaching health to read an
# env var did not finish the job: [order.env] reaches the two patrols and nothing
# else, so a person or agent typing `gc dolt health` still saw the parked mirror
# as "unknown" and the aggregate as STALE — the same false alarm, from the same
# decision, on the read path that everyone actually uses. A declaration that only
# some callers can see is not a declaration. It is also why the file, not the env
# var, is the source of truth: gc has no city-wide pack env (ga-7fbot5), so an
# env-only park has to be copied into every caller and silently rots out of step.
MIRROR_PARK_FILE="$GC_CITY_PATH/config/dolt/mirrors-parked"

# _mirror_park_match_entry <db/remote key> <entry> — echo the reason and succeed
# when this one declaration entry parks the key, else fail. Both sources share it
# so the env form and the file form cannot drift in how they match.
_mirror_park_match_entry() {
  _mpm_key="$1"
  _mpm_entry=$(printf '%s' "$2" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
  case "$_mpm_entry" in
    ''|'#'*) return 1 ;;
    "$_mpm_key")
      printf 'parked\n'
      return 0
      ;;
    "$_mpm_key="*)
      _mpm_reason=${_mpm_entry#"$_mpm_key="}
      printf '%s\n' "${_mpm_reason:-parked}"
      return 0
      ;;
  esac
  return 1
}

mirror_park_reason() {
  _mp_key="$1/$2"
  if [ -n "${GC_DOLT_MIRROR_PARKED:-}" ]; then
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
      _mirror_park_match_entry "$_mp_key" "$_mp_entry" && return 0
    done
    return 0
  fi
  [ -r "$MIRROR_PARK_FILE" ] || return 0
  # A redirect, not a pipe: `while ... done < file` runs in this shell, so the
  # `return` below actually returns from mirror_park_reason.
  while IFS= read -r _mp_line || [ -n "$_mp_line" ]; do
    _mirror_park_match_entry "$_mp_key" "$_mp_line" && return 0
  done < "$MIRROR_PARK_FILE"
  return 0
}


# is_local_dolt_host returns 0 (true) when the argument names the local managed
# Dolt server — loopback, the unspecified address, or an unset/empty host — and
# 1 (false) for a configured external endpoint. The health, status, and logs
# commands share it so they agree on whether GC owns a local managed process or
# is merely pointed at a remote server it cannot inspect on-disk. Mirrors the
# gc-beads-bd `is_remote` classification (gastownhall/gascity su-deol8).
is_local_dolt_host() {
  case "$1" in
    ""|127.0.0.1|0.0.0.0|localhost|::1|"[::1]") return 0 ;;
    *) return 1 ;;
  esac
}

read_runtime_state_flag() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  value=$(sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([^,}[:space:]]*\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true)
  case "$value" in
    true|false)
      printf '%s\n' "$value"
      ;;
  esac
)

read_runtime_state_number() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([0-9][0-9]*\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

read_runtime_state_string() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

canonical_path() (
  path="$1"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import os
import sys

print(os.path.realpath(sys.argv[1]))
PY
    return $?
  fi
  if command -v readlink >/dev/null 2>&1; then
    readlink -f "$path" 2>/dev/null && return 0
  fi
  printf '%s\n' "$path"
)

same_path() (
  left="$1"
  right="$2"
  [ "$left" = "$right" ] && return 0
  [ "$(canonical_path "$left")" = "$(canonical_path "$right")" ]
)

pid_is_running() (
  pid="$1"

  case "$pid" in
    ''|*[!0-9]*)
      return 1
      ;;
  esac

  if kill -0 "$pid" 2>/dev/null; then
    return 0
  fi

  if command -v ps >/dev/null 2>&1; then
    ps_pid=$(ps -p "$pid" -o pid= 2>/dev/null | tr -d '[:space:]')
    [ "$ps_pid" = "$pid" ] && return 0
  fi

  return 1
)

managed_runtime_listener_pid() (
  port="$1"

  case "$port" in
    ''|*[!0-9]*)
      return 0
      ;;
  esac

  if ! command -v lsof >/dev/null 2>&1; then
    return 0
  fi

  lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null \
    | while IFS= read -r holder_pid; do
        case "$holder_pid" in
          ''|*[!0-9]*)
            continue
            ;;
        esac
        if pid_is_running "$holder_pid"; then
          printf '%s\n' "$holder_pid"
          break
        fi
      done
)

managed_runtime_tcp_reachable() (
  port="$1"

  case "$port" in
    ''|*[!0-9]*)
      return 1
      ;;
  esac

  if command -v nc >/dev/null 2>&1; then
    nc -z 127.0.0.1 "$port" >/dev/null 2>&1
    return $?
  fi

  if command -v python3 >/dev/null 2>&1; then
    python3 - "$port" <<'PY' >/dev/null 2>&1
import socket
import sys

sock = socket.socket()
sock.settimeout(0.25)
try:
    sock.connect(("127.0.0.1", int(sys.argv[1])))
except OSError:
    raise SystemExit(1)
finally:
    sock.close()
PY
    return $?
  fi

  return 1
)

managed_runtime_port() (
  state_file="$1"
  expected_data_dir="$2"

  [ -f "$state_file" ] || return 0

  running=$(read_runtime_state_flag "$state_file" running)
  pid=$(read_runtime_state_number "$state_file" pid)
  port=$(read_runtime_state_number "$state_file" port)
  data_dir=$(read_runtime_state_string "$state_file" data_dir)

  [ "$running" = "true" ] || return 0
  [ -n "$pid" ] || return 0
  [ -n "$port" ] || return 0
  if ! same_path "$data_dir" "$expected_data_dir"; then
    printf 'dolt runtime: managed state data_dir=%s does not match expected data_dir=%s\n' \
      "$data_dir" "$expected_data_dir" >&2
    return 0
  fi
  pid_is_running "$pid" || return 0

  holder_pid=$(managed_runtime_listener_pid "$port" || true)
  if [ -n "$holder_pid" ]; then
    [ "$holder_pid" = "$pid" ] || return 0
    printf '%s\n' "$port"
    return 0
  fi

  if ! managed_runtime_tcp_reachable "$port"; then
    return 0
  fi

  printf '%s\n' "$port"
)

# Resolve GC_DOLT_PORT. The shared helper prefers validated live managed
# runtime state over stale inherited env, then falls back to GC_DOLT_PORT as an
# operator seed, and exits 78 if neither yields a port.
. "${GC_PACK_DIR:-${PACK_DIR:-${GC_SYSTEM_PACKS_DIR:-$GC_CITY_PATH/.gc/system/packs}/dolt}}/assets/scripts/port_resolve.sh"
GC_DOLT_PORT=$(resolve_dolt_port_or_die "$DOLT_STATE_FILE" "$DOLT_PROVIDER_STATE_FILE" "$DOLT_DATA_DIR" "$GC_CITY_PATH") || exit $?

# Resolve a bounded-execution helper. Prefer gtimeout (coreutils on
# macOS), fall back to timeout (coreutils on Linux), then to running
# the command directly if neither is installed. Running unbounded is
# still better than letting a wedged dolt client hang the caller, but
# patrol callers need a hard upper bound wherever possible.
if command -v gtimeout >/dev/null 2>&1; then
  TIMEOUT_BIN="gtimeout"
elif command -v timeout >/dev/null 2>&1; then
  TIMEOUT_BIN="timeout"
else
  TIMEOUT_BIN=""
fi

_run_bounded_warned_no_timeout=""

# Wall-clock bound (seconds) for `gc rig list --json` rig discovery, shared
# by the compact and health commands and tunable via
# GC_DOLT_RIG_LIST_TIMEOUT_SECS. The bound must absorb a slow-but-healthy gc
# on a busy host (~16s observed): discovery callers degrade to a city-only
# filesystem scan on timeout, which silently drops external rig databases
# (gascity#2740).
GC_DOLT_RIG_LIST_TIMEOUT_SECS="${GC_DOLT_RIG_LIST_TIMEOUT_SECS:-30}"

# run_bounded SECS CMD...  — Run CMD with a wall-clock timeout. Exits
# 124 on timeout (coreutils convention). Uses --kill-after=2 so an
# uncooperative child that ignores SIGTERM (e.g. a dolt client stuck
# in kernel socket wait) is escalated to SIGKILL rather than leaking
# zombies — which is the failure mode the bounded helper exists to
# prevent. If no bounded execution mechanism is available, fail closed rather
# than running a potentially wedged Dolt client unbounded.
run_bounded() {
  _t="$1"; shift
  if [ -n "$TIMEOUT_BIN" ]; then
    "$TIMEOUT_BIN" --kill-after=2 "$_t" "$@"
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$_t" "$@" <<'PY'
import subprocess
import sys

limit = float(sys.argv[1])
cmd = sys.argv[2:]
try:
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=limit)
except subprocess.TimeoutExpired as exc:
    sys.stdout.write(exc.stdout or "")
    sys.stderr.write(exc.stderr or "")
    sys.exit(124)
sys.stdout.write(proc.stdout)
sys.stderr.write(proc.stderr)
sys.exit(proc.returncode)
PY
  else
    printf 'dolt runtime: timeout/gtimeout/python3 not found; cannot run bounded command\n' >&2
    return 124
  fi
}
