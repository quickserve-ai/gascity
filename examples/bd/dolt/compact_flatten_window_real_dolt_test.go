//go:build integration || dolt_integration

package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// flattenWindowShim wraps the real dolt binary so a test can land a write at
// an exact point around the flatten transaction, which no sleep-based race
// can do reliably. The shim matches the flatten call by its commit message.
//
//   - GC_TEST_AFTER_FLATTEN_SQL runs once, right after the flatten call
//     returns and before run.sh probes anything else: the window a routine bd
//     write hit on a production hq (two "bd: update" commits 1-2s after the
//     flatten, 2026-09-27).
//   - GC_TEST_BEFORE_FLATTEN_SQL runs once, right before the flatten call, as
//     an uncommitted working-set write, so the flatten's -Am folds a content
//     change into the flatten commit itself.
//
// The shim directory also carries a gc stub: a quarantine sends mail, and the
// real gc on PATH must never be reached from a test.
func flattenWindowShim(t *testing.T, realDolt string) string {
	t.Helper()
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "dolt"), `#!/bin/sh
real=`+realDolt+`
run_side_sql() {
  [ -n "$1" ] || return 0
  [ ! -f "$2" ] || return 0
  side_rc=0
  "$real" --host 127.0.0.1 --port "$GC_DOLT_PORT" --user root --no-tls \
    --use-db beads sql -q "$1" >"$2.out" 2>&1 || side_rc=$?
  printf '%s\n' "$side_rc" > "$2"
}
case "$*" in
  *"DOLT_RESET('--soft'"*"DOLT_COMMIT('-Am', 'compaction: flatten history')"*)
    run_side_sql "${GC_TEST_BEFORE_FLATTEN_SQL:-}" "$GC_TEST_SHIM_DIR/before.done"
    rc=0
    "$real" "$@" || rc=$?
    if [ "$rc" -eq 0 ]; then
      run_side_sql "${GC_TEST_AFTER_FLATTEN_SQL:-}" "$GC_TEST_SHIM_DIR/after.done"
    fi
    exit "$rc"
    ;;
esac
exec "$real" "$@"
`)
	writeExecutable(t, filepath.Join(dir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_TEST_SHIM_DIR/gc.log"
exit 0
`)
	return dir
}

// setupFlattenWindowDB seeds a local, unshared beads database with enough
// history to flatten and starts a real sql-server on it.
func setupFlattenWindowDB(t *testing.T, doltPath string) (cityPath, dataDir string, port int) {
	t.Helper()
	cityPath = t.TempDir()
	dataDir = filepath.Join(cityPath, ".beads", "dolt")
	dbDir := filepath.Join(dataDir, "beads")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	runDoltForCompactTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
	runDoltForCompactTest(t, doltPath, dbDir, "sql", "-q",
		"CREATE TABLE beads (id int primary key, name varchar(20)); INSERT INTO beads VALUES (1, 'first'), (2, 'second');")
	runDoltForCompactTest(t, doltPath, dbDir, "commit", "-Am", "seed beads")
	runDoltForCompactTest(t, doltPath, dbDir, "sql", "-q", "INSERT INTO beads VALUES (3, 'third');")
	runDoltForCompactTest(t, doltPath, dbDir, "commit", "-Am", "seed third bead")
	if err := os.WriteFile(filepath.Join(dbDir, ".compact-full-history"), nil, 0o644); err != nil {
		t.Fatalf("write .compact-full-history: %v", err)
	}
	var pid int
	port, pid = startRealDoltServerForCompactTest(t, doltPath, dataDir)
	writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, pid)
	waitForDoltServerQueryForCompactTest(t, doltPath, port)
	return cityPath, dataDir, port
}

// assertShimSideSQLRan fails the test unless the shim ran its side write and
// that write succeeded; a skipped or failed write would let either test pass
// without exercising the window it names.
func assertShimSideSQLRan(t *testing.T, shimDir, name, out string) {
	t.Helper()
	rc, err := os.ReadFile(filepath.Join(shimDir, name))
	if err != nil {
		t.Fatalf("shim never ran its %s write; the test exercised nothing:\n%s", name, out)
	}
	if strings.TrimSpace(string(rc)) != "0" {
		side, _ := os.ReadFile(filepath.Join(shimDir, name+".out"))
		t.Fatalf("shim %s write failed rc=%s:\n%s", name, strings.TrimSpace(string(rc)), side)
	}
}

func runFlattenWindowCompact(t *testing.T, realDolt, shimDir, cityPath, dataDir string, port int, extraEnv ...string) (string, error) {
	t.Helper()
	env := append([]string{
		"GC_TEST_SHIM_DIR=" + shimDir,
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=1",
		"GC_DOLT_COMPACT_CALL_TIMEOUT_SECS=20",
	}, extraEnv...)
	return runCompactScriptForRealDoltTest(t, filepath.Join(shimDir, "dolt"), repoRoot(t), cityPath, dataDir, port, nil, env...)
}

// A routine same-count UPDATE that commits right after the flatten must not
// quarantine the database. The flatten preserved every row; only a later
// writer changed one. Verification has to read the flatten commit itself,
// not whatever the live tables hold by the time each probe runs.
func TestCompactScriptRealDoltWriterAfterFlattenDoesNotQuarantine(t *testing.T) {
	realDolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}
	shimDir := flattenWindowShim(t, realDolt)
	cityPath, dataDir, port := setupFlattenWindowDB(t, realDolt)

	out, err := runFlattenWindowCompact(t, realDolt, shimDir, cityPath, dataDir, port,
		"GC_TEST_AFTER_FLATTEN_SQL=UPDATE beads SET name = 'updated' WHERE id = 1; CALL DOLT_COMMIT('-am', 'bd: update first');",
	)
	assertShimSideSQLRan(t, shimDir, "after.done", out)
	marker := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, statErr := os.Stat(marker); statErr == nil {
		data, _ := os.ReadFile(marker)
		t.Fatalf("a writer that committed after the flatten quarantined the database:\nmarker:\n%s\noutput:\n%s", data, out)
	}
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	// The flatten happened and the writer's commit survives on top of it.
	log := doltServerQueryForCompactTest(t, realDolt, port, "SELECT message FROM dolt_log ORDER BY commit_order DESC")
	joined := strings.Join(log, "\n")
	if !strings.Contains(joined, compactFlattenMessageForTest) || !strings.Contains(joined, "bd: update first") {
		t.Fatalf("dolt_log should hold the flatten and the writer's commit:\n%s", joined)
	}
	if rows := doltServerQueryForCompactTest(t, realDolt, port, "SELECT name FROM beads WHERE id = 1"); len(rows) != 1 || rows[0] != "updated" {
		t.Fatalf("writer's update lost: %q", rows)
	}
	// Verification passes; the writer is then seen past the flatten's own
	// commit, so full GC waits for a quiet run instead of starting now.
	if !strings.Contains(out, "deferring full GC until the next quiet run") {
		t.Fatalf("a writer past the flatten commit should defer full GC to the next run:\n%s", out)
	}
	pendingGC := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if _, statErr := os.Stat(pendingGC); statErr != nil {
		t.Fatalf("deferred full GC should leave a pending-GC retry marker: %v\n%s", statErr, out)
	}
}

// hq's steady state: a standing uncommitted cell on a checked table before
// the run starts (production hq carries one in config). Preflight reads it
// live, the flatten's -Am commits it, and the pinned per-table reads match.
// Only the database hash moves against the committed root, and the
// committed-root drift proof admits it as absorbed working-set state: the run
// defers full GC, does not quarantine, and the dirty value lands in the
// flatten commit.
func TestCompactScriptRealDoltStandingDirtyCellIsAbsorbedNotQuarantined(t *testing.T) {
	realDolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}
	shimDir := flattenWindowShim(t, realDolt)
	cityPath, dataDir, port := setupFlattenWindowDB(t, realDolt)
	doltServerQueryForCompactTest(t, realDolt, port, "UPDATE beads SET name = 'dirty' WHERE id = 2")
	if rows := doltServerQueryForCompactTest(t, realDolt, port, "SELECT COUNT(*) FROM dolt_status"); len(rows) != 1 || rows[0] == "0" {
		t.Fatalf("setup should leave the working set dirty, dolt_status rows = %q", rows)
	}

	out, err := runFlattenWindowCompact(t, realDolt, shimDir, cityPath, dataDir, port)
	marker := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if _, statErr := os.Stat(marker); statErr == nil {
		data, _ := os.ReadFile(marker)
		t.Fatalf("a standing dirty cell quarantined the database:\nmarker:\n%s\noutput:\n%s", data, out)
	}
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "absorbed working-set state committed by the flatten") {
		t.Fatalf("the dirty cell should be admitted as absorbed working-set state:\n%s", out)
	}
	if rows := doltServerQueryForCompactTest(t, realDolt, port, "SELECT name FROM beads AS OF 'HEAD' WHERE id = 2"); len(rows) != 1 || rows[0] != "dirty" {
		t.Fatalf("the flatten commit should carry the dirty value, got %q\n%s", rows, out)
	}
}

// Controls: a content change folded into the flatten commit itself (an
// uncommitted working-set write, after preflight, that the flatten's -Am
// commits) is real drift between the preflight snapshot and the flatten. It
// must quarantine and must not run full GC, alone or with a routine writer
// committing right after the flatten: a commit after the flatten cannot
// explain what is inside it, so it must not be read as writer-race evidence.
func TestCompactScriptRealDoltChangeInsideFlattenStillQuarantines(t *testing.T) {
	realDolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}
	const laterWriter = "GC_TEST_AFTER_FLATTEN_SQL=UPDATE beads SET name = 'updated' WHERE id = 1; CALL DOLT_COMMIT('-am', 'bd: update first');"
	cases := []struct {
		name       string
		inFlatten  string
		laterWrite bool
		wantReason string
	}{
		{
			name:       "same-count change",
			inFlatten:  "UPDATE beads SET name = 'lost' WHERE id = 2;",
			wantReason: "post-flatten table value hash changed without row-count increase",
		},
		{
			name:       "same-count change with a later writer",
			inFlatten:  "UPDATE beads SET name = 'lost' WHERE id = 2;",
			laterWrite: true,
			wantReason: "post-flatten table value hash changed without row-count increase",
		},
		{
			name:       "row loss with a later writer",
			inFlatten:  "DELETE FROM beads WHERE id = 3;",
			laterWrite: true,
			wantReason: "post-flatten row count decreased",
		},
		{
			name:       "row gain masking a change with a later writer",
			inFlatten:  "INSERT INTO beads VALUES (4, 'fourth'); UPDATE beads SET name = 'lost' WHERE id = 2;",
			laterWrite: true,
			wantReason: "post-flatten table value hash changed with row-count increase",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shimDir := flattenWindowShim(t, realDolt)
			cityPath, dataDir, port := setupFlattenWindowDB(t, realDolt)
			env := []string{"GC_TEST_BEFORE_FLATTEN_SQL=" + tc.inFlatten}
			if tc.laterWrite {
				env = append(env, laterWriter)
			}
			out, err := runFlattenWindowCompact(t, realDolt, shimDir, cityPath, dataDir, port, env...)
			assertShimSideSQLRan(t, shimDir, "before.done", out)
			if tc.laterWrite {
				assertShimSideSQLRan(t, shimDir, "after.done", out)
			}
			if err == nil {
				t.Fatalf("compact succeeded although the flatten commit changed rows:\n%s", out)
			}
			marker := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
			if reason := compactMarkerValue(t, marker, "reason"); reason != tc.wantReason {
				t.Fatalf("quarantine reason = %q, want %q\n%s", reason, tc.wantReason, out)
			}
			if strings.Contains(out, "running DOLT_GC --full") {
				t.Fatalf("full GC must not run after an in-flatten change:\n%s", out)
			}
		})
	}
}

const compactFlattenMessageForTest = "compaction: flatten history"
