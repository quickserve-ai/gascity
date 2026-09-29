#!/bin/sh
# _backup_stamp.sh — the local-backup freshness stamp (ga-g3p5rm), shared by
# runtime.sh (mol-dog-doctor.sh reads the stamp's age) and mol-dog-backup.sh
# (which writes it after a successful sync without sourcing runtime.sh).
# Needs PACK_STATE_DIR; with it unset every write degrades to a silent no-op.

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
LOCAL_BACKUP_FRESHNESS_DIR="${PACK_STATE_DIR:-}/local-backup-freshness"

# write_local_backup_sync_stamp <db> [artifact_dir]
# Best-effort, mirroring write_backup_push_stamp: a stamp failure must never
# fail an otherwise successful sync, so every step degrades to a silent no-op.
# tmp+mv keeps a concurrent health read from seeing a torn file.
write_local_backup_sync_stamp() {
  lbs_db="$1"
  [ -n "${PACK_STATE_DIR:-}" ] || return 0
  mkdir -p "$LOCAL_BACKUP_FRESHNESS_DIR" 2>/dev/null || return 0
  {
    printf 'synced_at_epoch=%s\n' "$(date +%s)"
    printf 'artifact_dir=%s\n' "${2:-}"
  } > "$LOCAL_BACKUP_FRESHNESS_DIR/$lbs_db.tmp" 2>/dev/null || { rm -f "$LOCAL_BACKUP_FRESHNESS_DIR/$lbs_db.tmp" 2>/dev/null; return 0; }
  mv -f "$LOCAL_BACKUP_FRESHNESS_DIR/$lbs_db.tmp" "$LOCAL_BACKUP_FRESHNESS_DIR/$lbs_db" 2>/dev/null || return 0
}
