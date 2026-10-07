//go:build integration || dolt_integration

package gastown_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReaperWorkflowRootCleanupRealDoltSemantics(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}

	cityDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "dolt")
	for _, db := range []string{"citydb", "rigdb"} {
		dbDir := filepath.Join(dataDir, db)
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dbDir, err)
		}
		runDoltForMaintenanceTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
		runDoltSQLForMaintenanceTest(t, doltPath, dbDir, maintenanceReaperSchemaSQL())
	}

	runDoltSQLForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "citydb"), maintenanceReaperCitySeedSQL())
	runDoltForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "citydb"), "add", ".")
	runDoltForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "citydb"), "commit", "-m", "seed city workflow roots")

	runDoltSQLForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "rigdb"), maintenanceReaperRigSeedSQL())
	runDoltForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "rigdb"), "add", ".")
	runDoltForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "rigdb"), "commit", "-m", "seed rig workflow roots")

	port := startDoltServerForMaintenanceTest(t, doltPath, dataDir)
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "citydb")
	writeCityBeadsMetadata(t, cityDir, "citydb")
	rigDir := filepath.Join(cityDir, "rigs", "rig-with-db-alias")
	writeCityBeadsMetadata(t, rigDir, "rigdb")
	writeSiteRigBinding(t, cityDir, "rig-with-db-alias", rigDir)

	binDir := t.TempDir()
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	if err := os.Symlink(doltPath, filepath.Join(binDir, "dolt")); err != nil {
		t.Fatalf("Symlink(dolt): %v", err)
	}
	writeRealDoltBdDouble(t, filepath.Join(binDir, "bd"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
case "$1 $2" in
  "session prune")
    printf '{"count":0}\n'
    ;;
esac
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":        bdLog,
		"GC_CITY":            cityDir,
		"GC_CITY_PATH":       cityDir,
		"GC_DOLT_HOST":       "127.0.0.1",
		"GC_DOLT_PORT":       fmt.Sprintf("%d", port),
		"GC_DOLT_USER":       "root",
		"GC_DOLT_PASSWORD":   "",
		"FAKE_RIG_LIST_JSON": `{"rigs":[{"name":"rig-with-db-alias","hq":false}]}`,
		"FAKE_SCOPE_DBS":     "city=citydb rig:rig-with-db-alias=rigdb",
		"PATH":               binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	runScript(t, coreScriptPath("reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if !strings.Contains(string(bdData), " issue-close --reason stale inactive workflow root auto-closed by reaper") {
		t.Fatalf("reaper did not close city workflow issue root through bd close:\n%s", bdData)
	}

	cityWispStatuses := queryMaintenanceStatusByID(t, doltPath, port, "citydb", "wisps")
	requireMaintenanceStatuses(t, cityWispStatuses, map[string]string{
		"wisp-close":               "closed",
		"wisp-city-store-root":     "closed",
		"wisp-cross-store-root":    "open",
		"wisp-held":                "blocked",
		"wisp-non-root-workflow":   "open",
		"wisp-recent-root":         "open",
		"wisp-nested-root":         "open",
		"wisp-subroot":             "closed",
		"wisp-live-grandchild":     "in_progress",
		"wisp-recent-closed-child": "closed",
	})

	cityIssueStatuses := queryMaintenanceStatusByID(t, doltPath, port, "citydb", "issues")
	requireMaintenanceStatuses(t, cityIssueStatuses, map[string]string{
		"issue-city-store-root":   "closed",
		"issue-close":             "closed",
		"issue-cross-store-root":  "open",
		"issue-held":              "blocked",
		"issue-dep-root":          "open",
		"issue-dep-live":          "in_progress",
		"issue-non-root-workflow": "open",
	})

	rigWispStatuses := queryMaintenanceStatusByID(t, doltPath, port, "rigdb", "wisps")
	requireMaintenanceStatuses(t, rigWispStatuses, map[string]string{
		"rig-wisp-close":            "closed",
		"rig-wisp-store-root":       "closed",
		"rig-wisp-other-store-root": "open",
	})

	rigIssueStatuses := queryMaintenanceStatusByID(t, doltPath, port, "rigdb", "issues")
	requireMaintenanceStatuses(t, rigIssueStatuses, map[string]string{
		"rig-issue-preserve": "open",
	})
}

// writeRealDoltBdDouble installs a bd double that applies the verbs the
// reaper sends through `gc bd` (close, update --set-metadata) to the scope's
// database on the real dolt sql-server, so the test observes the end state of
// the reaper's real-Dolt selections. The scope's database comes from
// FAKE_SCOPE_DBS via the fake gc route (GC_FAKE_SCOPE). Every call is logged to
// BD_CALL_LOG.
func writeRealDoltBdDouble(t *testing.T, path string) {
	t.Helper()
	writeExecutable(t, path, `#!/bin/sh
set -e
printf '%s\n' "$*" >> "$BD_CALL_LOG"
db=""
for pair in ${FAKE_SCOPE_DBS:-}; do
  case "$pair" in
    "${GC_FAKE_SCOPE:-city}="*) db="${pair#*=}" ;;
  esac
done
run_sql() {
  DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" dolt --host "$GC_DOLT_HOST" --port "$GC_DOLT_PORT" --user "$GC_DOLT_USER" --no-tls --use-db "$db" sql -q "$1"
}
verb="$1"
shift
ids=""
sets=""
while [ $# -gt 0 ]; do
  case "$1" in
    --reason) shift ;;
    --force|--json) ;;
    --set-metadata)
      key="${2%%=*}"
      value="${2#*=}"
      sets="$sets, '\$.\"$key\"', '$value'"
      shift
      ;;
    -*) ;;
    *) ids="$ids $1" ;;
  esac
  shift
done
case "$verb" in
  prune)
    printf '{"pruned_count":0}\n'
    ;;
  purge)
    printf '{"purged_count":0}\n'
    ;;
  backup)
    printf '{"backup":{},"dolt":{"configured":false}}\n'
    ;;
  close|update)
    # Report every applied id in bd's --json array shape.
    sep=""
    printf '['
    for id in $ids; do
      if [ "$verb" = close ]; then
        run_sql "UPDATE issues SET status='closed', closed_at=NOW() WHERE id='$id'; UPDATE wisps SET status='closed', closed_at=NOW() WHERE id='$id'; CALL DOLT_COMMIT('-Am', 'test bd close')" >/dev/null 2>&1 || true
      else
        run_sql "UPDATE issues SET metadata = JSON_SET(COALESCE(metadata, JSON_OBJECT())$sets) WHERE id='$id'; UPDATE wisps SET metadata = JSON_SET(COALESCE(metadata, JSON_OBJECT())$sets) WHERE id='$id'; CALL DOLT_COMMIT('-Am', 'test bd update')" >/dev/null 2>&1 || true
      fi
      printf '%s{"id":"%s"}' "$sep" "$id"
      sep=","
    done
    printf ']\n'
    ;;
esac
exit 0
`)
}

// maintenanceReaperSchemaSQL mirrors the bd columns the reaper touches. Like
// bd, updated_at is ON UPDATE CURRENT_TIMESTAMP, which Dolt fills from the
// server-local clock whenever an UPDATE leaves the column unset.
func maintenanceReaperSchemaSQL() string {
	return `
CREATE TABLE wisps (
  id VARCHAR(64) PRIMARY KEY,
  title VARCHAR(255),
  status VARCHAR(32),
  issue_type VARCHAR(32),
  priority BIGINT,
  created_at DATETIME(6),
  updated_at DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  closed_at DATETIME(6),
  assignee VARCHAR(255),
  description LONGTEXT,
  metadata JSON
);
CREATE TABLE issues (
  id VARCHAR(64) PRIMARY KEY,
  title VARCHAR(255),
  status VARCHAR(32),
  issue_type VARCHAR(32),
  priority BIGINT,
  created_at DATETIME(6),
  updated_at DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  closed_at DATETIME(6),
  assignee VARCHAR(255),
  description LONGTEXT,
  metadata JSON
);
CREATE TABLE dependencies (
  issue_id VARCHAR(64),
  depends_on_issue_id VARCHAR(64),
  depends_on_wisp_id VARCHAR(64),
  depends_on_external VARCHAR(64),
  type VARCHAR(32)
);
CREATE TABLE wisp_dependencies (
  issue_id VARCHAR(64),
  depends_on_issue_id VARCHAR(64),
  depends_on_wisp_id VARCHAR(64),
  depends_on_external VARCHAR(64),
  type VARCHAR(32)
);
CREATE TABLE labels (
  issue_id VARCHAR(64),
  label VARCHAR(255)
);
CREATE TABLE wisp_labels (
  issue_id VARCHAR(64),
  label VARCHAR(255)
);
`
}

func maintenanceReaperCitySeedSQL() string {
	return `
INSERT INTO wisps (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('wisp-close', 'closeable root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('wisp-city-store-root', 'closeable city-store root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"city:test-city"}'),
  ('wisp-cross-store-root', 'cross-store root preserved', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"rig:other"}'),
  ('wisp-held', 'held root', 'blocked', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('wisp-non-root-workflow', 'non-root topology bead preserved', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_bead_id":"wisp-nested-root"}'),
  ('wisp-recent-root', 'recent descendant root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('wisp-recent-closed-child', 'recent closed child', 'closed', 'task', 2, '2026-01-01 00:00:00', NOW(), '', '{"gc.root_bead_id":"wisp-recent-root"}'),
  ('wisp-nested-root', 'nested root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('wisp-subroot', 'nested subroot', 'closed', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.root_bead_id":"wisp-nested-root"}'),
  ('wisp-live-grandchild', 'live nested child', 'in_progress', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{}');
INSERT INTO wisp_dependencies (issue_id, depends_on_wisp_id, type) VALUES
  ('wisp-subroot', 'wisp-nested-root', 'tracks'),
  ('wisp-live-grandchild', 'wisp-subroot', 'tracks');
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('issue-close', 'closeable city issue root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('issue-city-store-root', 'closeable city-store issue root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"city:test-city"}'),
  ('issue-cross-store-root', 'cross-store issue root preserved', 'open', 'task', 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"rig:other"}'),
  ('issue-held', 'held city issue root', 'blocked', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('issue-non-root-workflow', 'non-root issue topology bead preserved', 'open', 'task', 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_bead_id":"issue-dep-root"}'),
  ('issue-dep-root', 'dependency-protected issue root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('issue-dep-live', 'live issue dependency child', 'in_progress', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{}');
INSERT INTO dependencies (issue_id, depends_on_issue_id, type) VALUES
  ('issue-dep-live', 'issue-dep-root', 'blocks');
`
}

func maintenanceReaperRigSeedSQL() string {
	return `
INSERT INTO wisps (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('rig-wisp-close', 'closeable non-city wisp root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('rig-wisp-store-root', 'closeable rig-store wisp root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"rig:rig-with-db-alias"}'),
  ('rig-wisp-other-store-root', 'other rig-store root preserved', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"rig:other"}');
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('rig-issue-preserve', 'non-city issue root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}');
`
}

func runDoltForMaintenanceTest(t *testing.T, doltPath, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt %s failed in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func runDoltSQLForMaintenanceTest(t *testing.T, doltPath, dir, query string) string {
	t.Helper()
	return runDoltForMaintenanceTest(t, doltPath, dir, "sql", "-q", query)
}

func startDoltServerForMaintenanceTest(t *testing.T, doltPath, dataDir string, extraEnv ...string) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("Close listener: %v", err)
	}

	logPath := filepath.Join(dataDir, "sql-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("Create(%s): %v", logPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, doltPath, "sql-server",
		"-H", "127.0.0.1",
		"-P", fmt.Sprintf("%d", port),
		"--data-dir", dataDir,
		"--loglevel", "warning",
	)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("Start dolt sql-server: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-waitCh:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
		}
		_ = logFile.Close()
	})
	return port
}

func waitForDoltServerForMaintenanceTest(t *testing.T, doltPath string, port int, db string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastOut []byte
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, doltPath,
			"--host", "127.0.0.1",
			"--port", fmt.Sprintf("%d", port),
			"--user", "root",
			"--no-tls",
			"--use-db", db,
			"sql", "-q", "SELECT 1",
		)
		cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
		lastOut, lastErr = cmd.CombinedOutput()
		cancel()
		if lastErr == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("dolt sql-server did not become ready on port %d: %v\n%s", port, lastErr, lastOut)
}

func queryMaintenanceStatusByID(t *testing.T, doltPath string, port int, db string, table string) map[string]string {
	t.Helper()
	lines := doltServerCSVQuery(t, doltPath, port, db, fmt.Sprintf("SELECT id,status FROM %s ORDER BY id", table))
	out := strings.Join(lines, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "id,status" {
		t.Fatalf("unexpected status output for %s.%s:\n%s", db, table, out)
	}
	statuses := make(map[string]string)
	for _, line := range lines[1:] {
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			t.Fatalf("unexpected status row for %s.%s: %q\nfull output:\n%s", db, table, line, out)
		}
		statuses[fields[0]] = fields[1]
	}
	return statuses
}

// doltServerCSVQuery runs query against db on the local dolt sql-server at
// port and returns the trimmed CSV output split into lines.
func doltServerCSVQuery(t *testing.T, doltPath string, port int, db, query string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--user", "root",
		"--no-tls",
		"--use-db", db,
		"sql", "-r", "csv", "-q", query,
	)
	cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("query %s (%s): %v\n%s", db, query, err, out)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func requireMaintenanceStatuses(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	for id, wantStatus := range want {
		if got[id] != wantStatus {
			t.Fatalf("status[%s] = %q, want %q\nall statuses: %#v", id, got[id], wantStatus, got)
		}
	}
}

// TestReaperStaleIssueCloseSkipsDurableExtmsgRecordsRealDolt runs the real
// reaper.sh Step 5 stale-issue query against a real Dolt sql-server. Durable
// extmsg protocol records (group roots, participants, bindings, memberships,
// transcript state and entries) must survive the stale sweep while an ordinary
// stale task is still closed, and the query itself must not fail (#6380).
//
// A hyphenated rig database (my-rig) pins that the correlated label probe
// binds to each database's own labels table: decoy labels in the other
// database must not change which rows are stale candidates, and the query
// must not fail for rig databases either.
func TestReaperStaleIssueCloseSkipsDurableExtmsgRecordsRealDolt(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}

	durableLabels := map[string]string{
		"ext-group":            "gc:extmsg-group",
		"ext-participant":      "gc:extmsg-participant",
		"ext-binding":          "gc:extmsg-binding",
		"ext-membership":       "gc:extmsg-membership",
		"ext-transcript-state": "gc:extmsg-transcript-state",
		"ext-transcript":       "gc:extmsg-transcript",
	}

	var seed strings.Builder
	seed.WriteString("INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES\n")
	seed.WriteString("  ('ord-stale', 'ordinary stale task', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}')")
	for id := range durableLabels {
		fmt.Fprintf(&seed, ",\n  ('%s', 'durable extmsg record', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}')", id)
	}
	seed.WriteString(";\nINSERT INTO labels (issue_id, label) VALUES\n  ('ord-stale', 'unrelated-label')")
	for id, label := range durableLabels {
		fmt.Fprintf(&seed, ",\n  ('%s', '%s')", id, label)
	}
	// Decoy: a durable label for the rig's ordinary row lives only in citydb.
	seed.WriteString(",\n  ('rig-ord', 'gc:extmsg-group');\n")
	// A live task blocked by a wisp: its dependency row has a NULL
	// depends_on_issue_id. Before the NOT IN guard, that NULL turned the whole
	// active-dependency exclusion list to NULL and no stale issue was closed.
	seed.WriteString("INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES\n" +
		"  ('live-on-wisp', 'live task blocked by a wisp', 'open', 'task', 2, NOW(), NOW(), '', '{}');\n" +
		"INSERT INTO dependencies (issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type) VALUES\n" +
		"  ('live-on-wisp', NULL, 'some-wisp', NULL, 'blocks');\n")

	// my-rig: rig-ext and rig-ext2 are durable via my-rig's own labels; rig-ord
	// is ordinary there. The decoy label for the city's ord-stale lives only in
	// my-rig. The asymmetric counts (1 ordinary vs 2 durable) make a probe
	// against the wrong database's labels table visible in the candidate count.
	rigSeed := `
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('rig-ord', 'ordinary stale rig task', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}'),
  ('rig-ext', 'durable extmsg rig record', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}'),
  ('rig-ext2', 'durable extmsg rig record', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}');
INSERT INTO labels (issue_id, label) VALUES
  ('rig-ext', 'gc:extmsg-group'),
  ('rig-ext2', 'gc:extmsg-binding'),
  ('ord-stale', 'gc:extmsg-group');
`

	cityDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "dolt")
	for db, dbSeed := range map[string]string{"citydb": seed.String(), "my-rig": rigSeed} {
		dbDir := filepath.Join(dataDir, db)
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dbDir, err)
		}
		runDoltForMaintenanceTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
		runDoltSQLForMaintenanceTest(t, doltPath, dbDir, maintenanceReaperSchemaSQL())
		runDoltSQLForMaintenanceTest(t, doltPath, dbDir, dbSeed)
		runDoltForMaintenanceTest(t, doltPath, dbDir, "add", ".")
		runDoltForMaintenanceTest(t, doltPath, dbDir, "commit", "-m", "seed stale extmsg records")
	}

	port := startDoltServerForMaintenanceTest(t, doltPath, dataDir)
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "citydb")
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "my-rig")
	writeCityBeadsMetadata(t, cityDir, "citydb")
	rigDir := filepath.Join(cityDir, "rigs", "my-rig")
	writeCityBeadsMetadata(t, rigDir, "my-rig")
	writeSiteRigBinding(t, cityDir, "my-rig", rigDir)

	binDir := t.TempDir()
	logDir := t.TempDir()
	bdLog := filepath.Join(logDir, "bd.log")
	escalateLog := filepath.Join(logDir, "escalate.log")
	if err := os.Symlink(doltPath, filepath.Join(binDir, "dolt")); err != nil {
		t.Fatalf("Symlink(dolt): %v", err)
	}
	writeRealDoltBdDouble(t, filepath.Join(binDir, "bd"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
case "$1 $2" in
  "session prune")
    printf '{"count":0}\n'
    ;;
esac
exit 0
`)
	escalateScript := filepath.Join(binDir, "escalate.sh")
	writeExecutable(t, escalateScript, `#!/bin/sh
printf '%s\n' "$*" >> "$ESCALATE_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":        bdLog,
		"ESCALATE_CALL_LOG":  escalateLog,
		"GC_ESCALATE_SCRIPT": escalateScript,
		"GC_CITY":            cityDir,
		"GC_CITY_PATH":       cityDir,
		"GC_DOLT_HOST":       "127.0.0.1",
		"GC_DOLT_PORT":       fmt.Sprintf("%d", port),
		"GC_DOLT_USER":       "root",
		"GC_DOLT_PASSWORD":   "",
		"FAKE_RIG_LIST_JSON": `{"rigs":[{"name":"my-rig","hq":false}]}`,
		"FAKE_SCOPE_DBS":     "city=citydb rig:my-rig=my-rig",
		"PATH":               binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	reaperOut, err := runScriptResult(t, coreScriptPath("reaper.sh"), env)
	if err != nil {
		t.Fatalf("reaper.sh failed: %v\n%s", err, reaperOut)
	}

	escalateData, err := os.ReadFile(escalateLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(escalate log): %v", err)
	}
	if strings.Contains(string(escalateData), "stale issue query failed") {
		t.Fatalf("reaper stale issue query failed on real Dolt:\n%s", escalateData)
	}
	// Rig rows are never closed by the city reaper; they are counted as skipped
	// candidates. Exactly one (rig-ord) must be a candidate: rig-ext/rig-ext2
	// are durable via my-rig's labels, and the citydb decoy must not hide
	// rig-ord. Probing citydb's labels instead would count 2; no filter, 3.
	if !strings.Contains(string(reaperOut), "skipped_non_city_issues:1,") {
		t.Fatalf("reaper did not report exactly one my-rig stale candidate:\n%s\nescalations:\n%s", reaperOut, escalateData)
	}

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v\nescalations:\n%s", err, escalateData)
	}
	if !strings.Contains(string(bdData), "close ord-stale --reason stale:auto-closed by reaper") {
		t.Fatalf("reaper did not close the ordinary stale issue:\nbd calls:\n%s\nescalations:\n%s", bdData, escalateData)
	}
	for id := range durableLabels {
		if strings.Contains(string(bdData), "close "+id+" ") {
			t.Fatalf("reaper closed durable extmsg record %s:\n%s", id, bdData)
		}
	}

	want := map[string]string{"ord-stale": "closed"}
	for id := range durableLabels {
		want[id] = "open"
	}
	requireMaintenanceStatuses(t, queryMaintenanceStatusByID(t, doltPath, port, "citydb", "issues"), want)
	requireMaintenanceStatuses(t, queryMaintenanceStatusByID(t, doltPath, port, "my-rig", "issues"), map[string]string{
		"rig-ord":  "open",
		"rig-ext":  "open",
		"rig-ext2": "open",
	})
}

func TestReaperAgeGatesUseUTCUnderNonUTCServerRealDolt(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}

	seed := `
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('past-age', 'updated beyond the stale age', 'open', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 61 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 49 HOUR), '', '{}'),
  ('within-age', 'updated inside the stale age', 'open', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 61 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 47 HOUR), '', '{}');
`
	cityDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "dolt")
	dbDir := filepath.Join(dataDir, "citydb")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dbDir, err)
	}
	runDoltForMaintenanceTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
	runDoltSQLForMaintenanceTest(t, doltPath, dbDir, maintenanceReaperSchemaSQL())
	runDoltSQLForMaintenanceTest(t, doltPath, dbDir, seed)
	runDoltForMaintenanceTest(t, doltPath, dbDir, "add", ".")
	runDoltForMaintenanceTest(t, doltPath, dbDir, "commit", "-m", "seed age gate rows")

	port := startDoltServerForMaintenanceTest(t, doltPath, dataDir, "TZ=America/Los_Angeles")
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "citydb")
	if off := queryDoltServerUTCOffsetHours(t, doltPath, port, "citydb"); off != -7 && off != -8 {
		t.Fatalf("dolt sql-server ignored TZ=America/Los_Angeles; the test cannot distinguish NOW() from UTC_TIMESTAMP() (offset %d hours)", off)
	}
	writeCityBeadsMetadata(t, cityDir, "citydb")

	binDir := t.TempDir()
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	if err := os.Symlink(doltPath, filepath.Join(binDir, "dolt")); err != nil {
		t.Fatalf("Symlink(dolt): %v", err)
	}
	writeRealDoltBdDouble(t, filepath.Join(binDir, "bd"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
case "$1 $2" in
  "session prune")
    printf '{"count":0}\n'
    ;;
esac
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":               bdLog,
		"GC_CITY":                   cityDir,
		"GC_CITY_PATH":              cityDir,
		"GC_DOLT_HOST":              "127.0.0.1",
		"GC_DOLT_PORT":              fmt.Sprintf("%d", port),
		"GC_DOLT_USER":              "root",
		"GC_DOLT_PASSWORD":          "",
		"GC_REAPER_STALE_ISSUE_AGE": "48h",
		"FAKE_RIG_LIST_JSON":        `{"rigs":[]}`,
		"FAKE_SCOPE_DBS":            "city=citydb",
		"PATH":                      binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	reaperOut, err := runScriptResult(t, coreScriptPath("reaper.sh"), env)
	if err != nil {
		t.Fatalf("reaper.sh failed: %v\n%s", err, reaperOut)
	}

	requireMaintenanceStatuses(t, queryMaintenanceStatusByID(t, doltPath, port, "citydb", "issues"), map[string]string{
		"past-age":   "closed",
		"within-age": "open",
	})
}

func queryDoltServerUTCOffsetHours(t *testing.T, doltPath string, port int, db string) int {
	t.Helper()
	lines := doltServerCSVQuery(t, doltPath, port, db, "SELECT TIMESTAMPDIFF(HOUR, UTC_TIMESTAMP(), NOW()) AS off")
	if len(lines) != 2 || strings.TrimSpace(lines[0]) != "off" {
		t.Fatalf("unexpected UTC offset output for %s:\n%s", db, strings.Join(lines, "\n"))
	}
	off, err := strconv.Atoi(strings.TrimSpace(lines[1]))
	if err != nil {
		t.Fatalf("parse UTC offset %q: %v", lines[1], err)
	}
	return off
}

// TestReaperStaleIssueAgeUsesUTCRealDolt pins step 5's age cutoff and the
// wisp closed_at write to UTC. bd stores UTC in its timestamp columns, and the
// server here runs at Asia/Kolkata (UTC+05:30), east of UTC, where comparing
// those columns against the server-local clock reaps 5.5h early.
func TestReaperStaleIssueAgeUsesUTCRealDolt(t *testing.T) {
	f := newReaperStaleIssueFixture(t, "Asia/Kolkata", `
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('utc-young', 'updated 718h ago in UTC', 'open', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 718 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 718 HOUR), '', '{}'),
  ('utc-old', 'updated 722h ago in UTC', 'open', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 722 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 722 HOUR), '', '{}'),
  ('utc-closed-parent', 'closed parent of an open wisp', 'closed', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), '', '{}');
INSERT INTO wisps (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('utc-orphan-wisp', 'open wisp under a closed parent', 'open', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 48 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 48 HOUR), '', '{}');
INSERT INTO wisp_dependencies (issue_id, depends_on_issue_id, type) VALUES
  ('utc-orphan-wisp', 'utc-closed-parent', 'parent-child');
`)

	// Precondition: the server's local clock really is 330 minutes ahead of
	// UTC. Without it the control below proves nothing.
	if got := f.column(t, "SELECT CAST(ROUND(TIMESTAMPDIFF(SECOND, UTC_TIMESTAMP(), NOW()) / 60) AS SIGNED)"); len(got) != 1 || got[0] != "330" {
		t.Fatalf("server local clock offset = %q minutes, want [330]; the TZ override did not reach the server", got)
	}

	// Control: the local-clock predicate step 5 used to run selects the bead
	// updated 718h ago, two hours short of the 720h threshold, and also the
	// bead updated 722h ago.
	control := f.column(t, "SELECT id FROM issues WHERE status = 'open' AND updated_at < DATE_SUB(NOW(), INTERVAL 720 HOUR) ORDER BY id")
	if strings.Join(control, ",") != "utc-old,utc-young" {
		t.Fatalf("control: local-clock predicate selected %q, want [utc-old utc-young]", control)
	}

	f.runReaper(t, nil)

	calls := f.bdCalls(t)
	if !strings.Contains(calls, "close utc-old --reason stale:auto-closed by reaper") {
		t.Fatalf("reaper did not close the bead updated 722h ago:\n%s", calls)
	}
	if strings.Contains(calls, "close utc-young") {
		t.Fatalf("reaper closed the bead updated 718h ago; step 5 is comparing against the server-local clock:\n%s", calls)
	}
	requireMaintenanceStatuses(t, queryMaintenanceStatusByID(t, f.doltPath, f.port, "citydb", "issues"), map[string]string{
		"utc-old":   "closed",
		"utc-young": "open",
	})

	// Step 1 closed the orphan wisp through `gc bd close` (upstream #6751):
	// bd owns the close timestamps, so the reaper writes no closed_at or
	// updated_at of its own any more; the beads UTC row covers bd's side.
	requireMaintenanceStatuses(t, queryMaintenanceStatusByID(t, f.doltPath, f.port, "citydb", "wisps"), map[string]string{
		"utc-orphan-wisp": "closed",
	})
}

// TestReaperStaleIssueSkipsOperatorDirectiveRealDolt pins step 5's exemption
// for beads labelled operator-directive. An unlabelled twin closes in the same
// pass, the positive control that step 5 still reaps. Removing the label then
// lets the next pass close the formerly labelled bead, which shows the label
// was the only thing keeping it open. Throughout, a stale bead and its live
// dependent stay open: on Dolt 2.2.4, spelling the label exemption as a second
// NOT IN drops the NOT from the dependency guard and closes exactly those two.
func TestReaperStaleIssueSkipsOperatorDirectiveRealDolt(t *testing.T) {
	f := newReaperStaleIssueFixture(t, "", `
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('labelled-directive', 'rarely triggered operator directive', 'open', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), '', '{}'),
  ('unlabelled-twin', 'same shape, no label', 'open', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), '', '{}'),
  ('dep-guarded', 'stale, but a live bead depends on it', 'open', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), '', '{}'),
  ('dep-live-child', 'stale, and depends on an open bead', 'in_progress', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), 'some-agent', '{}');
INSERT INTO dependencies (issue_id, depends_on_issue_id, type) VALUES
  ('dep-live-child', 'dep-guarded', 'blocks');
INSERT INTO labels (issue_id, label) VALUES
  ('labelled-directive', 'operator-directive');
`)

	f.runReaper(t, nil)

	calls := f.bdCalls(t)
	if !strings.Contains(calls, "close unlabelled-twin --reason stale:auto-closed by reaper") {
		t.Fatalf("positive control: reaper did not close the unlabelled stale bead:\n%s", calls)
	}
	if strings.Contains(calls, "close labelled-directive") {
		t.Fatalf("reaper closed a bead labelled operator-directive:\n%s", calls)
	}
	requireMaintenanceStatuses(t, queryMaintenanceStatusByID(t, f.doltPath, f.port, "citydb", "issues"), map[string]string{
		"labelled-directive": "open",
		"unlabelled-twin":    "closed",
		"dep-guarded":        "open",
		"dep-live-child":     "in_progress",
	})

	// Control: without the label, the same bead is selected and closed.
	f.column(t, "DELETE FROM labels WHERE issue_id = 'labelled-directive'")
	f.runReaper(t, nil)

	calls = f.bdCalls(t)
	if !strings.Contains(calls, "close labelled-directive --reason stale:auto-closed by reaper") {
		t.Fatalf("control: with its label removed, the bead was still not closed; something other than the label is exempting it:\n%s", calls)
	}
	requireMaintenanceStatuses(t, queryMaintenanceStatusByID(t, f.doltPath, f.port, "citydb", "issues"), map[string]string{
		"labelled-directive": "closed",
		"dep-guarded":        "open",
		"dep-live-child":     "in_progress",
	})
	if strings.Contains(calls, "close dep-") {
		t.Fatalf("reaper closed a dependency-guarded bead:\n%s", calls)
	}
}

// TestReaperDryRunListsStaleIssueClosesRealDolt pins what a dry run shows for
// step 5. In one run the labelled bead is absent (exempt), each unlabelled
// stale bead is listed with its close mode, the summary counts them, and
// nothing is closed.
func TestReaperDryRunListsStaleIssueClosesRealDolt(t *testing.T) {
	f := newReaperStaleIssueFixture(t, "", `
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('dry-labelled', 'rarely triggered operator directive', 'open', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), '', '{}'),
  ('dry-unassigned', 'stale and unassigned', 'open', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), '', '{}'),
  ('dry-assigned', 'stale and still assigned', 'in_progress', 'task', 2, DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), DATE_SUB(UTC_TIMESTAMP(), INTERVAL 800 HOUR), 'some-agent', '{}');
INSERT INTO labels (issue_id, label) VALUES
  ('dry-labelled', 'operator-directive');
`)

	out := f.runReaper(t, map[string]string{"GC_REAPER_DRY_RUN": "1"})

	lines := strings.Split(out, "\n")
	for _, want := range []string{
		"reaper: would-close-stale citydb dry-unassigned bare",
		"reaper: would-close-stale citydb dry-assigned force",
	} {
		if !slices.Contains(lines, want) {
			t.Fatalf("dry run did not list %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "dry-labelled") {
		t.Fatalf("dry run listed the bead labelled operator-directive:\n%s", out)
	}
	for _, want := range []string{"would_close_stale:2", "closed:0", "(dry run)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run summary missing %q:\n%s", want, out)
		}
	}

	if calls := f.bdCalls(t); strings.Contains(calls, "close") {
		t.Fatalf("dry run closed beads:\n%s", calls)
	}
	requireMaintenanceStatuses(t, queryMaintenanceStatusByID(t, f.doltPath, f.port, "citydb", "issues"), map[string]string{
		"dry-labelled":   "open",
		"dry-unassigned": "open",
		"dry-assigned":   "in_progress",
	})
}

// reaperStaleIssueFixture is one city bead store (citydb) on a live dolt
// sql-server, with stub bd and gc binaries on PATH. The bd stub records every
// call and applies closes to Dolt; the gc stub routes "gc bd" to it.
type reaperStaleIssueFixture struct {
	doltPath string
	port     int
	bdLog    string
	env      map[string]string
}

// newReaperStaleIssueFixture seeds citydb with the reaper schema plus seedSQL
// and serves it. A non-empty serverTZ runs the server under that TZ.
func newReaperStaleIssueFixture(t *testing.T, serverTZ, seedSQL string) *reaperStaleIssueFixture {
	t.Helper()
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}

	cityDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "dolt")
	dbDir := filepath.Join(dataDir, "citydb")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dbDir, err)
	}
	runDoltForMaintenanceTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
	runDoltSQLForMaintenanceTest(t, doltPath, dbDir, maintenanceReaperSchemaSQL())
	runDoltSQLForMaintenanceTest(t, doltPath, dbDir, seedSQL)
	runDoltForMaintenanceTest(t, doltPath, dbDir, "add", ".")
	runDoltForMaintenanceTest(t, doltPath, dbDir, "commit", "-m", "seed stale issues")

	var serverEnv []string
	if serverTZ != "" {
		serverEnv = append(serverEnv, "TZ="+serverTZ)
	}
	port := startDoltServerForMaintenanceTest(t, doltPath, dataDir, serverEnv...)
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "citydb")
	f := &reaperStaleIssueFixture{doltPath: doltPath, port: port}
	if serverTZ != "" {
		// Dolt 2.1 serves NOW() from the process TZ; Dolt >= 2.2 boots with
		// @@global.time_zone = '+00:00' and ignores it until told otherwise.
		f.column(t, "SET @@global.time_zone = 'SYSTEM'")
	}
	writeCityBeadsMetadata(t, cityDir, "citydb")

	binDir := t.TempDir()
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	if err := os.Symlink(doltPath, filepath.Join(binDir, "dolt")); err != nil {
		t.Fatalf("Symlink(dolt): %v", err)
	}
	// Step 1 closes orphan wisps through `gc bd close` too (upstream #6751),
	// so the double must apply a close to the wisps table as well as issues.
	writeRealDoltBdDouble(t, filepath.Join(binDir, "bd"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
case "$1 $2" in
  "session prune")
    printf '{"count":0}\n'
    ;;
esac
exit 0
`)

	f.bdLog = bdLog
	f.env = map[string]string{
		"BD_CALL_LOG":      bdLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     fmt.Sprintf("%d", port),
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"FAKE_SCOPE_DBS":   "city=citydb",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	return f
}

// runReaper runs reaper.sh against the fixture with extraEnv layered on top
// and returns its combined output.
func (f *reaperStaleIssueFixture) runReaper(t *testing.T, extraEnv map[string]string) string {
	t.Helper()
	env := maps.Clone(f.env)
	maps.Copy(env, extraEnv)
	out, err := runScriptResult(t, coreScriptPath("reaper.sh"), env)
	if err != nil {
		t.Fatalf("reaper.sh failed: %v\n%s", err, out)
	}
	return string(out)
}

// bdCalls returns every bd invocation recorded so far, one per line.
func (f *reaperStaleIssueFixture) bdCalls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.bdLog)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	return string(data)
}

// column runs query against citydb on the fixture's server and returns the
// first column of every result row.
func (f *reaperStaleIssueFixture) column(t *testing.T, query string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.doltPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", f.port),
		"--user", "root",
		"--no-tls",
		"--use-db", "citydb",
		"sql", "-r", "csv", "-q", query,
	)
	cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("query %q: %v\n%s", query, err, out)
	}
	var rows []string
	for i, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if i == 0 || line == "" {
			continue
		}
		first, _, _ := strings.Cut(strings.TrimSpace(line), ",")
		rows = append(rows, first)
	}
	return rows
}
