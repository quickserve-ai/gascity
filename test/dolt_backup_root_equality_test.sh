#!/usr/bin/env bash
# Test: mol-dog-backup.sh counts a database synced only when the backup's
# manifest root equals the source snapshot's root (ga-rirak7).
#
# `dolt backup sync` (2.2.4, and upstream main) exits 0 when its read of the
# SOURCE root fails, so the script used to stamp a backup fresh on exit 0 alone
# even when the backup root never moved. Every case runs the real script with a
# fake `dolt` that exits 0 on `backup sync`; the cases differ only in what that
# fake leaves in the two manifests.
#
# Acceptance criteria:
#   a. Equal roots after sync -> "synced (snapshot)", stamp written, 1/1.
#   b. Roots differ (fake sync exits 0 without touching them) -> no stamp,
#      FAILED line naming both roots, counted failed (0/1 + escalation), and the
#      run exits exactly as a plain failed sync does (CONTROL: fake sync exit 1).
#   c. Backup manifest unreadable or malformed -> FAILED line, no stamp.
#   d. Source manifest unreadable -> FAILED line, no stamp, counted failed.
#   e. Header validation: no appendix, empty/non-integer version, empty lock,
#      wrong magic -> "malformed"; a CRLF ending still reads.
#   f. The remote's url, read whole from `dolt backup -v`, must be this job's
#      artifact directory <dir>/<db> (trailing slash, a `..` through a
#      symlinked parent, and a symlink at <db> all accepted); anything else
#      fails closed, a file remote elsewhere and a
#      path containing a space included, and so does a lookup that exits
#      non-zero whatever it printed.
#   g. A failed root suppresses the orphan prune (CONTROL: equal roots prune).
#   h. The offsite leg refuses a symlinked <db> by name before rsync runs
#      (CONTROL: a plain <db> is rsynced and reports ok).
#   Every failed case prints exactly ONE FAILED line.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
PACK="$REPO_ROOT/examples/bd/dolt"
BACKUP="$PACK/assets/scripts/mol-dog-backup.sh"
FAILED=0

pass() { printf '\033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }

if [ ! -f "$BACKUP" ]; then
    printf 'ERROR: %s not found\n' "$BACKUP" >&2
    exit 1
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

ROOT_A="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
ROOT_B="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
LOCK="0123456789abcdefghijklmnopqrstuv"
ZERO="00000000000000000000000000000000"

# manifest <root>: a Dolt manifest line, no trailing newline (as Dolt writes it).
manifest() { printf '5:__DOLT__:%s:%s:%s' "$LOCK" "$1" "$ZERO"; }

# Fakes. `dolt backup sync` exits FAKE_SYNC_EXIT; with FAKE_SYNC_WRITE_ROOT set
# it writes that root into BOTH manifests (a healthy sync), otherwise it leaves
# them untouched (the ga-rirak7 exit-0-without-moving case). `dolt backup -v`
# advertises the same file:// path the sync writes (FAKE_BACKUP_URL overrides)
# and exits FAKE_BACKUP_V_EXIT. `cp -c` stands in for an APFS clone so the
# snapshot path runs on any filesystem.
BIN="$WORK/bin"
mkdir -p "$BIN"
cat > "$BIN/dolt" <<'FAKE'
#!/usr/bin/env bash
db="$(basename "$PWD")"
url="${FAKE_BACKUP_URL:-file://$FAKE_BACKUP_DIR/$db}"
if [ "${1:-}" = "version" ]; then echo "dolt version 2.2.4"; exit 0; fi
if [ "${1:-}" = "backup" ] && { [ "$#" -eq 1 ] || [ "${2:-}" = "-v" ]; }; then
    echo "$db-backup $url "
    exit "${FAKE_BACKUP_V_EXIT:-0}"
fi
if [ "${1:-} ${2:-}" = "backup sync" ]; then
    echo "sync-cwd $PWD" >> "$FAKE_DOLT_LOG"
    if [ -n "${FAKE_SYNC_WRITE_ROOT:-}" ]; then
        printf '5:__DOLT__:l:%s:z' "$FAKE_SYNC_WRITE_ROOT" > .dolt/noms/manifest
        bak="${url#file://}/manifest"
        # Keep every other field and Dolt's no-trailing-newline shape.
        awk -F: -v OFS=: -v ORS= -v r="$FAKE_SYNC_WRITE_ROOT" \
            'NR > 1 { printf "\n" } NR == 1 { $4 = r } { print }' "$bak" > "$bak.tmp"
        mv "$bak.tmp" "$bak"
    fi
    exit "${FAKE_SYNC_EXIT:-0}"
fi
exit 0
FAKE
cat > "$BIN/gc" <<'FAKE'
#!/bin/sh
printf 'gc %s\n' "$*" >> "$FAKE_GC_LOG"
exit 0
FAKE
cat > "$BIN/flock" <<'FAKE'
#!/bin/sh
exit 0
FAKE
# `rsync` records its argv (one line) and copies nothing: the offsite cases
# assert whether the copy was ATTEMPTED and with what source, not its bytes.
cat > "$BIN/rsync" <<'FAKE'
#!/bin/sh
printf 'rsync %s\n' "$*" >> "$FAKE_RSYNC_LOG"
exit 0
FAKE
cat > "$BIN/cp" <<'FAKE'
#!/bin/sh
if [ "$1" = "-c" ]; then shift; fi
exec /bin/cp "$@"
FAKE
chmod +x "$BIN/dolt" "$BIN/gc" "$BIN/flock" "$BIN/cp" "$BIN/rsync"

# run_case <name> <source-root|-> <backup-manifest-content|-> [env...]
# Sets CASE_OUT, CASE_RC, CASE_DIR. "-" means: no manifest at all.
run_case() {
    local name="$1" src_root="$2" bak_content="$3"
    shift 3
    CASE_DIR="$WORK/$name"
    local city="$CASE_DIR/city" data="$CASE_DIR/city/dolt-data"
    mkdir -p "$data/prod/.dolt/noms" "$CASE_DIR/backups/prod"
    [ "$src_root" = "-" ] || manifest "$src_root" > "$data/prod/.dolt/noms/manifest"
    [ "$bak_content" = "-" ] || printf '%s' "$bak_content" > "$CASE_DIR/backups/prod/manifest"
    local f
    for f in ${SEED_TABLE_FILES:-}; do
        printf 'chunkdata' > "$CASE_DIR/backups/prod/$f.darc"
        touch -t 202001010000 "$CASE_DIR/backups/prod/$f.darc"
    done
    set +e
    CASE_OUT=$(env -i PATH="$BIN:/usr/bin:/bin:/usr/sbin:/sbin" HOME="$CASE_DIR" \
        GC_CITY_PATH="$city" GC_PACK_DIR="$PACK" GC_DOLT_DATA_DIR="$data" \
        GC_DOLT_PORT=3307 GC_DOLT_HOST=127.0.0.1 GC_DOLT_USER=root GC_DOLT_PASSWORD= \
        GC_BACKUP_DATABASES=prod GC_BACKUP_ARTIFACT_DIR="$CASE_DIR/backups" \
        GC_DOLT_BACKUP_PRUNE_ORPHANS=0 \
        FAKE_BACKUP_DIR="$CASE_DIR/backups" FAKE_DOLT_LOG="$CASE_DIR/dolt.log" \
        FAKE_GC_LOG="$CASE_DIR/gc.log" FAKE_RSYNC_LOG="$CASE_DIR/rsync.log" \
        "$@" bash "$BACKUP" 2>&1)
    CASE_RC=$?
    set -e
}

stamp_path() { printf '%s\n' "$CASE_DIR/city/.gc/runtime/packs/dolt/local-backup-freshness/prod"; }

expect_contains() { # <label> <needle>
    if printf '%s' "$CASE_OUT" | grep -qF -- "$2"; then pass "$1"; else fail "$1"; printf '%s\n' "$CASE_OUT" | sed 's/^/    | /'; fi
}
expect_absent() { # <label> <needle>
    if printf '%s' "$CASE_OUT" | grep -qF -- "$2"; then fail "$1"; printf '%s\n' "$CASE_OUT" | sed 's/^/    | /'; else pass "$1"; fi
}
expect_no_stamp() {
    if [ -e "$(stamp_path)" ]; then fail "$1"; else pass "$1"; fi
}
expect_escalated() {
    if grep -qF "Dolt backup: 1/1 databases failed to sync" "$CASE_DIR/gc.log" 2>/dev/null; then pass "$1"; else fail "$1"; fi
}
expect_one_failed_line() {
    local n
    n=$(printf '%s\n' "$CASE_OUT" | grep -cF -- "— FAILED:" || true)
    if [ "$n" -eq 1 ]; then pass "$1"; else fail "$1 (got $n)"; printf '%s\n' "$CASE_OUT" | sed 's/^/    | /'; fi
}
expect_ran_from_snapshot() {
    if grep -qF "backup-snapshot/prod" "$CASE_DIR/dolt.log" 2>/dev/null; then pass "$1"; else fail "$1"; fi
}

echo "--- control: a sync that exits 1 (the existing failed-database path) ---"
run_case control "$ROOT_A" "$(manifest "$ROOT_B")" FAKE_SYNC_EXIT=1
CONTROL_RC=$CASE_RC
expect_contains "control: counted 0/1" "synced: 0/1"
expect_no_stamp "control: no stamp"
expect_escalated "control: escalates 1/1 failed"

echo "--- a: equal roots after sync ---"
run_case equal "$ROOT_A" "$(manifest "$ROOT_B")" FAKE_SYNC_WRITE_ROOT="$ROOT_B"
expect_ran_from_snapshot "a: sync ran from the snapshot clone"
expect_contains "a: reports synced (snapshot)" "backup: prod — synced (snapshot)"
expect_contains "a: counted 1/1" "synced: 1/1"
expect_absent "a: no FAILED line" "FAILED:"
if [ -s "$(stamp_path)" ] && grep -q '^synced_at_epoch=[0-9]' "$(stamp_path)"; then
    pass "a: freshness stamp written"
else
    fail "a: freshness stamp written"
fi
[ "$CASE_RC" -eq 0 ] && pass "a: exit 0" || fail "a: exit 0 (got $CASE_RC)"

echo "--- b: sync exits 0 but the backup root did not move ---"
run_case mismatch "$ROOT_A" "$(manifest "$ROOT_B")"
expect_ran_from_snapshot "b: sync ran from the snapshot clone"
expect_contains "b: FAILED line names both roots" \
    "backup: prod — FAILED: root mismatch after sync, source $ROOT_A backup $ROOT_B"
expect_absent "b: not reported synced" "synced (snapshot)"
expect_contains "b: counted 0/1" "synced: 0/1"
expect_no_stamp "b: no stamp"
expect_escalated "b: escalates 1/1 failed (failed count non-zero)"
expect_one_failed_line "b: exactly one FAILED line"
[ "$CASE_RC" -eq "$CONTROL_RC" ] && pass "b: exits as the failed-database control does ($CONTROL_RC)" \
    || fail "b: exits as the failed-database control does (got $CASE_RC, control $CONTROL_RC)"

echo "--- c: backup manifest unreadable or malformed ---"
run_case bak-missing "$ROOT_A" -
expect_contains "c1: missing backup manifest -> FAILED line" \
    "backup: prod — FAILED: backup manifest unreadable after sync"
expect_absent "c1: not reported synced" "synced (snapshot)"
expect_no_stamp "c1: no stamp"
expect_escalated "c1: counted failed"
expect_one_failed_line "c1: exactly one FAILED line"
run_case bak-garbage "$ROOT_A" "not a manifest"
expect_contains "c2: garbage backup manifest -> FAILED malformed" \
    "backup: prod — FAILED: backup manifest malformed after sync"
expect_no_stamp "c2: no stamp"
run_case bak-badroot "$ROOT_A" "5:__DOLT__:$LOCK:../../etc:$ZERO"
expect_contains "c3: malformed root field -> FAILED malformed" \
    "backup: prod — FAILED: backup manifest malformed after sync"
expect_no_stamp "c3: no stamp"

echo "--- e: manifest header validation (version, magic, lock, appendix) ---"
for hdr in "no-appendix|5:__DOLT__:$LOCK:$ROOT_A" \
           "empty-version|:__DOLT__:$LOCK:$ROOT_A:$ZERO" \
           "alpha-version|v5:__DOLT__:$LOCK:$ROOT_A:$ZERO" \
           "empty-lock|5:__DOLT__::$ROOT_A:$ZERO" \
           "bad-magic|5:__NOMS__:$LOCK:$ROOT_A:$ZERO"; do
    label="${hdr%%|*}"
    run_case "hdr-$label" "$ROOT_A" "${hdr#*|}"
    expect_contains "e: $label backup manifest -> FAILED malformed" \
        "backup: prod — FAILED: backup manifest malformed after sync"
    expect_no_stamp "e: $label -> no stamp"
done
run_case hdr-crlf "$ROOT_A" "$(manifest "$ROOT_A")"$'\r\n' FAKE_SYNC_WRITE_ROOT="$ROOT_A"
expect_contains "e: CRLF-terminated manifest still reads (tolerance kept)" "backup: prod — synced (snapshot)"

echo "--- d: source manifest unreadable ---"
run_case src-missing - "$(manifest "$ROOT_B")"
expect_contains "d: missing source manifest -> FAILED line" \
    "backup: prod — FAILED: source manifest unreadable after sync"
expect_absent "d: not reported synced" "synced (snapshot)"
expect_no_stamp "d: no stamp"
expect_escalated "d: counted failed"
expect_one_failed_line "d: exactly one FAILED line"

echo "--- f: the root check is bound to this job's artifact directory ---"
ART_DIR="$WORK/remote-s3/backups/prod"
run_case remote-s3 "$ROOT_A" "$(manifest "$ROOT_A")" FAKE_BACKUP_URL="aws://bucket/prod"
expect_contains "f1: non-file remote -> FAILED, not verifiable" \
    "backup: prod — FAILED: backup remote aws://bucket/prod is not this job's artifact directory $ART_DIR; root not verifiable"
expect_no_stamp "f1: no stamp"
expect_one_failed_line "f1: exactly one FAILED line"
# A file remote elsewhere is a misconfiguration for this job (it stamps, prunes
# and copies off-box the artifact dir), even when the root there DID move.
mkdir -p "$WORK/elsewhere/prod"
manifest "$ROOT_B" > "$WORK/elsewhere/prod/manifest"
ART_DIR="$WORK/remote-elsewhere/backups/prod"
run_case remote-elsewhere "$ROOT_A" "$(manifest "$ROOT_A")" \
    FAKE_BACKUP_URL="file://$WORK/elsewhere/prod" FAKE_SYNC_WRITE_ROOT="$ROOT_B"
expect_contains "f2: file remote elsewhere -> fails closed" \
    "backup: prod — FAILED: backup remote file://$WORK/elsewhere/prod is not this job's artifact directory $ART_DIR; root not verifiable"
expect_absent "f2: not reported synced" "synced (snapshot)"
expect_absent "f2: the elsewhere manifest is not compared" "root mismatch"
expect_no_stamp "f2: no stamp"
expect_escalated "f2: counted failed"
expect_one_failed_line "f2: exactly one FAILED line"
# The url is read WHOLE: a path with a space is seen in full and, being
# elsewhere, fails closed with the full url printed (not cut at the space).
mkdir -p "$WORK/space dir/prod"
manifest "$ROOT_B" > "$WORK/space dir/prod/manifest"
ART_DIR="$WORK/remote-space/backups/prod"
run_case remote-space "$ROOT_A" "$(manifest "$ROOT_A")" \
    FAKE_BACKUP_URL="file://$WORK/space dir/prod" FAKE_SYNC_WRITE_ROOT="$ROOT_B"
expect_contains "f3: url with a space -> full url printed, fails closed" \
    "backup: prod — FAILED: backup remote file://$WORK/space dir/prod is not this job's artifact directory $ART_DIR; root not verifiable"
expect_no_stamp "f3: no stamp"
expect_one_failed_line "f3: exactly one FAILED line"
# The artifact dir itself, spelled with a trailing slash, is the same place.
run_case remote-slash "$ROOT_A" "$(manifest "$ROOT_B")" \
    FAKE_BACKUP_URL="file://$WORK/remote-slash/backups/prod/" FAKE_SYNC_WRITE_ROOT="$ROOT_B"
expect_contains "f4: artifact dir with a trailing slash -> synced" "backup: prod — synced (snapshot)"
if [ -s "$(stamp_path)" ]; then pass "f4: stamp written"; else fail "f4: stamp written"; fi
# A lookup that exits non-zero fails closed even when its stdout carries a
# plausible line naming the right directory.
ART_DIR="$WORK/lookup-fails/backups/prod"
run_case lookup-fails "$ROOT_A" "$(manifest "$ROOT_B")" \
    FAKE_BACKUP_V_EXIT=1 FAKE_SYNC_WRITE_ROOT="$ROOT_B"
expect_contains "f5: lookup exits 1 with a plausible line -> unresolved, FAILED" \
    "backup: prod — FAILED: backup remote (unresolved) is not this job's artifact directory $ART_DIR; root not verifiable"
expect_absent "f5: not reported synced" "synced (snapshot)"
expect_no_stamp "f5: no stamp"
expect_one_failed_line "f5: exactly one FAILED line"
# `..` is resolved physically: with link -> f6phys/other, the artifact dir
# link/../backups IS f6phys/backups (where the remote points), not the
# spelling's sibling $CASE_DIR/backups. A logical cd compared the latter.
mkdir -p "$WORK/f6phys/other" "$WORK/f6phys/backups/prod" "$WORK/remote-dotdot"
ln -s "$WORK/f6phys/other" "$WORK/remote-dotdot/link"
manifest "$ROOT_A" > "$WORK/f6phys/backups/prod/manifest"
run_case remote-dotdot "$ROOT_A" "$(manifest "$ROOT_A")" \
    GC_BACKUP_ARTIFACT_DIR="$WORK/remote-dotdot/link/../backups" \
    FAKE_BACKUP_URL="file://$WORK/f6phys/backups/prod" FAKE_SYNC_WRITE_ROOT="$ROOT_B"
expect_contains "f6: artifact dir spelled link/../backups resolves physically -> synced" "backup: prod — synced (snapshot)"
expect_absent "f6: no false FAILED" "— FAILED:"
# A symlink AT <db> that points where the remote points is the same place:
# the whole <dir>/<db> path is canonicalised, not the parent plus the name.
mkdir -p "$WORK/f7real/prod" "$WORK/remote-dblink/backups"
ln -s "$WORK/f7real/prod" "$WORK/remote-dblink/backups/prod"
run_case remote-dblink "$ROOT_A" "$(manifest "$ROOT_B")" FAKE_SYNC_WRITE_ROOT="$ROOT_B"
expect_contains "f7: symlink at <db> -> synced" "backup: prod — synced (snapshot)"
expect_absent "f7: no false FAILED" "— FAILED:"
if [ -s "$(stamp_path)" ]; then pass "f7: stamp written"; else fail "f7: stamp written"; fi

echo "--- h: the offsite leg refuses a symlinked <db> by name instead of copying a link ---"
# The root check accepts a symlink at <db>; rsync -a would ship it as a link.
# A remote-looking target skips the volume guards, so the chain reaches the
# symlink check. CONTROL first: no symlink -> the (fake) rsync is called on
# the artifact dir and the leg reports ok.
run_case offsite-control "$ROOT_A" "$(manifest "$ROOT_B")" FAKE_SYNC_WRITE_ROOT="$ROOT_B" \
    GC_BACKUP_OFFSITE_PATH="backup@other-host:/vault"
expect_contains "h: CONTROL plain <db> -> synced" "backup: prod — synced (snapshot)"
expect_contains "h: CONTROL -> offsite ok" "offsite: ok"
if grep -qF -- "--delete $CASE_DIR/backups/ backup@other-host:/vault/" "$CASE_DIR/rsync.log" 2>/dev/null; then
    pass "h: CONTROL -> rsync called on the artifact dir"
else
    fail "h: CONTROL -> rsync called on the artifact dir"; cat "$CASE_DIR/rsync.log" 2>/dev/null | sed 's/^/    | /'
fi
mkdir -p "$WORK/hreal/prod" "$WORK/offsite-dblink/backups"
ln -s "$WORK/hreal/prod" "$WORK/offsite-dblink/backups/prod"
run_case offsite-dblink "$ROOT_A" "$(manifest "$ROOT_B")" FAKE_SYNC_WRITE_ROOT="$ROOT_B" \
    GC_BACKUP_OFFSITE_PATH="backup@other-host:/vault"
expect_contains "h: symlink at <db> -> the root check still syncs" "backup: prod — synced (snapshot)"
expect_contains "h: symlink at <db> -> offsite refused by status" "offsite: symlinked-artifact"
expect_contains "h: symlink at <db> -> the detail names the entry" "holds symlinked entries (prod)"
expect_contains "h: symlink at <db> -> the run fails" "backup: FAILING THE RUN"
if [ "$CASE_RC" -ne 0 ]; then pass "h: symlink at <db> -> exit non-zero"; else fail "h: symlink at <db> -> exit non-zero (got 0)"; fi
if [ ! -e "$CASE_DIR/rsync.log" ]; then
    pass "h: symlink at <db> -> rsync NOT called"
else
    fail "h: symlink at <db> -> rsync NOT called"; sed 's/^/    | /' "$CASE_DIR/rsync.log"
fi
if grep -qF "the off-box copy did not happen — symlinked-artifact" "$CASE_DIR/gc.log" 2>/dev/null; then
    pass "h: symlink at <db> -> escalated as a HIGH offsite failure"
else
    fail "h: symlink at <db> -> escalated as a HIGH offsite failure"
fi

echo "--- g: a failed root suppresses the orphan prune ---"
KEEP_HASH="kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
ORPHAN_HASH="oooooooooooooooooooooooooooooooo"
SEED_TABLE_FILES="$KEEP_HASH $ORPHAN_HASH"
run_case prune-control "$ROOT_A" "$(manifest "$ROOT_B"):$KEEP_HASH:2" \
    GC_DOLT_BACKUP_PRUNE_ORPHANS=1 FAKE_SYNC_WRITE_ROOT="$ROOT_B"
if [ ! -e "$CASE_DIR/backups/prod/$ORPHAN_HASH.darc" ] && [ -e "$CASE_DIR/backups/prod/$KEEP_HASH.darc" ]; then
    pass "g: CONTROL equal roots -> the orphan is pruned, the referenced file kept"
else
    fail "g: CONTROL equal roots -> the orphan is pruned, the referenced file kept"
    printf '%s\n' "$CASE_OUT" | sed 's/^/    | /'
fi
run_case prune-mismatch "$ROOT_A" "$(manifest "$ROOT_B"):$KEEP_HASH:2" GC_DOLT_BACKUP_PRUNE_ORPHANS=1
expect_contains "g: mismatch -> FAILED line" "backup: prod — FAILED: root mismatch after sync"
if [ -e "$CASE_DIR/backups/prod/$ORPHAN_HASH.darc" ]; then
    pass "g: mismatch -> the orphan SURVIVES (prune suppressed)"
else
    fail "g: mismatch -> the orphan SURVIVES (prune suppressed)"
fi
expect_absent "g: mismatch -> no prune line" "pruned"
SEED_TABLE_FILES=""

echo
if [ "$FAILED" -ne 0 ]; then
    echo "SOME TESTS FAILED"
    exit 1
fi
echo "ALL TESTS PASSED"
