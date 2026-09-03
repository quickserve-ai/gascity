package liveness

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// These tests exercise the REAL SQL path against a throwaway `dolt sql-server`,
// following the harness pattern in internal/beads/native_dolt_store_integration_test.go.
// They skip when the dolt binary is unavailable, so a machine without dolt still
// gets the full unit-level suite in liveness_test.go.

type doltTestServer struct {
	dsnPrefix string
	database  string
	// doltBin, port and dataDir let a test kill and relaunch the server on the
	// SAME address, which is how the restart-recovery case is exercised.
	doltBin string
	port    int
	dataDir string
	env     []string
	cmd     *exec.Cmd
}

func startTestDoltServer(t *testing.T) *doltTestServer {
	t.Helper()
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt binary not in PATH; skipping the real-SQL liveness tests")
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()

	dataDir := t.TempDir()
	env := append(os.Environ(), "DOLT_ROOT_PATH="+dataDir)
	// DOLT_COMMIT refuses to run without a committer identity, and EnsureSchema
	// commits the one-time dolt_ignore seed.
	for _, kv := range [][2]string{{"user.name", "liveness-test"}, {"user.email", "liveness@test.local"}} {
		cfg := exec.Command(doltBin, "config", "--global", "--add", kv[0], kv[1])
		cfg.Env = env
		cfg.Dir = dataDir
		if out, err := cfg.CombinedOutput(); err != nil {
			t.Skipf("dolt config --global %s failed: %v\n%s", kv[0], err, out)
		}
	}

	server := &doltTestServer{
		dsnPrefix: fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port),
		database:  "livenesstest",
		doltBin:   doltBin,
		port:      port,
		dataDir:   dataDir,
		env:       env,
	}
	server.launch(t)
	t.Cleanup(func() { server.kill() })

	boot, err := sql.Open("mysql", server.dsnPrefix)
	if err != nil {
		t.Fatalf("open dolt connection: %v", err)
	}
	if _, err := boot.Exec("CREATE DATABASE livenesstest"); err != nil {
		_ = boot.Close()
		t.Fatalf("create test database: %v", err)
	}
	_ = boot.Close()
	return server
}

// launch starts the server and blocks until it answers.
func (s *doltTestServer) launch(t *testing.T) {
	t.Helper()
	cmd := exec.Command(s.doltBin, "sql-server", "--host", "127.0.0.1",
		"--port", strconv.Itoa(s.port), "--data-dir", s.dataDir)
	cmd.Env = s.env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dolt sql-server: %v", err)
	}
	s.cmd = cmd

	probe, err := sql.Open("mysql", s.dsnPrefix)
	if err != nil {
		t.Fatalf("open dolt connection: %v", err)
	}
	defer func() { _ = probe.Close() }()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if err := probe.Ping(); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("dolt sql-server did not become ready on port %d", s.port)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// kill stops the server and waits for the port to be released.
func (s *doltTestServer) kill() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	_, _ = s.cmd.Process.Wait()
	s.cmd = nil
}

// connect opens a NEW connection pool to the test database. Separate calls are
// genuinely separate client connections — that is what makes the concurrency and
// restart-recovery cases meaningful.
//
// parseTime is deliberately OFF, matching the production dialer
// (cmd/gc managedDoltOpenDatabase): the driver then hands written_at back as raw
// bytes, and the store has to normalize it itself. connectParseTime covers the
// other half.
func (s *doltTestServer) connect(t *testing.T) *sql.DB {
	t.Helper()
	return s.open(t, s.dsnPrefix+s.database)
}

func (s *doltTestServer) connectParseTime(t *testing.T) *sql.DB {
	t.Helper()
	return s.open(t, s.dsnPrefix+s.database+"?parseTime=true")
}

func (s *doltTestServer) open(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", s.database, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func doltCommitCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM dolt_log").Scan(&n); err != nil {
		t.Fatalf("count dolt_log: %v", err)
	}
	return n
}

func TestSQLStoreAgainstDolt(t *testing.T) {
	server := startTestDoltServer(t)
	ctx := context.Background()
	db := server.connect(t)

	beforeSeed := doltCommitCount(t, db)
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	afterSeed := doltCommitCount(t, db)
	if afterSeed != beforeSeed+1 {
		t.Fatalf("dolt_log went %d -> %d, want exactly one seed commit", beforeSeed, afterSeed)
	}

	t.Run("the table is registered in dolt_ignore", func(t *testing.T) {
		var pattern string
		var ignored bool
		err := db.QueryRow("SELECT pattern, ignored FROM dolt_ignore WHERE pattern = ?", TableName).Scan(&pattern, &ignored)
		if err != nil {
			t.Fatalf("read dolt_ignore: %v", err)
		}
		if !ignored {
			t.Fatalf("dolt_ignore[%s].ignored = false, want true", TableName)
		}
	})

	t.Run("EnsureSchema is idempotent and mints no second commit", func(t *testing.T) {
		before := doltCommitCount(t, db)
		if err := EnsureSchema(ctx, db); err != nil {
			t.Fatalf("EnsureSchema (second call): %v", err)
		}
		if got := doltCommitCount(t, db); got != before {
			t.Fatalf("dolt_log went %d -> %d on a re-seed, want unchanged", before, got)
		}
	})

	store := NewSQLStore(db)

	t.Run("writes mint no Dolt commits", func(t *testing.T) {
		before := doltCommitCount(t, db)
		for i := 0; i < 25; i++ {
			err := store.SetBatch(ctx, "gc-churn", map[string]string{
				"state":            "active",
				"generation":       strconv.Itoa(i),
				"last_woke_at":     time.Now().UTC().Format(time.RFC3339),
				"awake_started_at": time.Now().UTC().Format(time.RFC3339),
			})
			if err != nil {
				t.Fatalf("SetBatch #%d: %v", i, err)
			}
		}
		if got := doltCommitCount(t, db); got != before {
			t.Fatalf("dolt_log went %d -> %d across 25 liveness batches, want unchanged — the whole point of the change", before, got)
		}
	})

	t.Run("round trip, tombstone, and GetMany", func(t *testing.T) {
		if err := store.SetBatch(ctx, "gc-a", map[string]string{"state": "active", "sleep_reason": "idle"}); err != nil {
			t.Fatalf("SetBatch gc-a: %v", err)
		}
		if err := store.SetBatch(ctx, "gc-b", map[string]string{"state": "asleep"}); err != nil {
			t.Fatalf("SetBatch gc-b: %v", err)
		}
		if err := store.SetBatch(ctx, "gc-a", map[string]string{"sleep_reason": ""}); err != nil {
			t.Fatalf("SetBatch clear: %v", err)
		}
		snap, err := store.Get(ctx, "gc-a")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if snap.Values["state"] != "active" {
			t.Errorf("state = %q, want active", snap.Values["state"])
		}
		if v, ok := snap.Values["sleep_reason"]; !ok || v != "" {
			t.Errorf("sleep_reason = (%q,%v), want a present empty tombstone", v, ok)
		}
		if snap.WrittenAt.IsZero() {
			t.Errorf("WrittenAt is zero, want the row clock")
		}
		many, err := store.GetMany(ctx, []string{"gc-a", "gc-b", "gc-nonexistent"})
		if err != nil {
			t.Fatalf("GetMany: %v", err)
		}
		if len(many) != 2 {
			t.Fatalf("GetMany returned %d snapshots, want 2", len(many))
		}
		if many["gc-b"].Values["state"] != "asleep" {
			t.Errorf("gc-b state = %q, want asleep", many["gc-b"].Values["state"])
		}
	})

	t.Run("survives discarding and reopening the store", func(t *testing.T) {
		writer := NewSQLStore(server.connect(t))
		if err := writer.SetBatch(ctx, "gc-restart", map[string]string{
			"state":          "asleep",
			"instance_token": "tok-42",
		}); err != nil {
			t.Fatalf("SetBatch: %v", err)
		}
		// Drop every handle the writer had, exactly as a process exit would.
		if err := writer.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		reopened := NewSQLStore(server.connect(t))
		snap, err := reopened.Get(ctx, "gc-restart")
		if err != nil {
			t.Fatalf("Get after reopen: %v", err)
		}
		if snap.Values["instance_token"] != "tok-42" || snap.Values["state"] != "asleep" {
			t.Fatalf("after reopen Values = %v, want the pre-restart values", snap.Values)
		}
	})

	t.Run("the write clock survives either parseTime setting", func(t *testing.T) {
		if err := store.SetBatch(ctx, "gc-clock", map[string]string{"state": "active"}); err != nil {
			t.Fatalf("SetBatch: %v", err)
		}
		plain, err := store.Get(ctx, "gc-clock")
		if err != nil {
			t.Fatalf("Get (parseTime off): %v", err)
		}
		if plain.WrittenAt.IsZero() {
			t.Fatalf("WrittenAt is zero with parseTime off — the production dialer sets no parseTime, so the store must normalize raw bytes itself")
		}
		parsed, err := NewSQLStore(server.connectParseTime(t)).Get(ctx, "gc-clock")
		if err != nil {
			t.Fatalf("Get (parseTime on): %v", err)
		}
		if !parsed.WrittenAt.Equal(plain.WrittenAt) {
			t.Fatalf("WrittenAt differs by parseTime setting: %v vs %v", plain.WrittenAt, parsed.WrittenAt)
		}
	})

	t.Run("the server clock calibration and the fence agree", func(t *testing.T) {
		// written_at is minted server-side, so a fence stamped from an
		// UNCALIBRATED local clock could fail to fence on a Dolt host whose clock
		// differs. Calibrate() is what makes the two comparable; this proves the
		// round trip on a real server.
		calibrated := NewSQLStore(server.connect(t))
		if err := calibrated.Calibrate(ctx); err != nil {
			t.Fatalf("Calibrate: %v", err)
		}
		before := calibrated.Now()
		if err := calibrated.SetBatch(ctx, "gc-fence", map[string]string{"state": "active"}); err != nil {
			t.Fatalf("SetBatch: %v", err)
		}
		snap, err := calibrated.Get(ctx, "gc-fence")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		written := snap.Times["state"]
		if written.IsZero() {
			t.Fatalf("no written_at recorded")
		}
		// A stamp taken BEFORE the write must not fence it out...
		if !written.After(before) {
			t.Errorf("written_at %v is not after the pre-write stamp %v; the fence would drop a fresh row", written, before)
		}
		// ...and one taken AFTER must fence it.
		after := calibrated.Now()
		if written.After(after) {
			t.Errorf("written_at %v is after a post-write stamp %v; the fence would keep a stale row", written, after)
		}
	})

	t.Run("concurrent writers on separate connections lose nothing per key", func(t *testing.T) {
		testSQLStoreConcurrentWritersAcrossSeparateConnections(ctx, t, server)
	})
}

// testSQLStoreConcurrentWritersAcrossSeparateConnections is the case that
// justifies choosing a table over the JSON sidecar: two writers with SEPARATE
// connections (standing in for two processes — the reconciler and a CLI) hammer
// the same bead. Each owns a disjoint key, and both keys must survive. The
// sidecar could not offer this: each process cached the whole JSON document and
// atomically rewrote it, so the loser's keys vanished.
func testSQLStoreConcurrentWritersAcrossSeparateConnections(ctx context.Context, t *testing.T, server *doltTestServer) {
	t.Helper()
	const rounds = 40
	writers := []struct {
		key   string
		store *SQLStore
	}{
		{key: "state", store: NewSQLStore(server.connect(t))},
		{key: "generation", store: NewSQLStore(server.connect(t))},
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(writers)*rounds)
	for _, w := range writers {
		wg.Add(1)
		go func(key string, store *SQLStore) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				val := fmt.Sprintf("%s-%d", key, i)
				if err := store.SetBatch(ctx, "gc-race", map[string]string{key: val}); err != nil {
					errs <- fmt.Errorf("%s round %d: %w", key, i, err)
					return
				}
			}
		}(w.key, w.store)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent SetBatch: %v", err)
	}

	snap, err := NewSQLStore(server.connect(t)).Get(ctx, "gc-race")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	wantState := fmt.Sprintf("state-%d", rounds-1)
	wantGeneration := fmt.Sprintf("generation-%d", rounds-1)
	if snap.Values["state"] != wantState {
		t.Errorf("state = %q, want the last write %q", snap.Values["state"], wantState)
	}
	if snap.Values["generation"] != wantGeneration {
		t.Errorf("generation = %q, want the last write %q — the other writer's key was lost", snap.Values["generation"], wantGeneration)
	}
}

// TestSQLStoreRecoversAcrossAServerRestart is the mandatory case for the
// review's blocker 3. The Dolt server is killed mid-test and relaunched on the
// same address; writes must resume.
//
// It proves the half a pooled *sql.DB can recover on its own — a server that
// comes back at the SAME address. The half it cannot recover, a managed-Dolt
// rebind onto a DIFFERENT port, is handled a layer up by retiring the pool so
// the endpoint is re-resolved (cmd/gc livenessBinding.noteOpError, covered by
// TestBindingRetiresThePoolOnAConnectionError). Both halves matter: without the
// second, a rebind left liveness committing for the life of the process.
func TestSQLStoreRecoversAcrossAServerRestart(t *testing.T) {
	server := startTestDoltServer(t)
	ctx := context.Background()
	db := server.connect(t)
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	store := NewSQLStore(db)
	if err := store.SetBatch(ctx, "gc-restart", map[string]string{"state": "active"}); err != nil {
		t.Fatalf("SetBatch before restart: %v", err)
	}

	server.kill()

	// While it is down, writes fail — and they fail with an error the binding
	// classifies as connection-class, which is what triggers the re-resolve.
	err := store.SetBatch(ctx, "gc-restart", map[string]string{"state": "asleep"})
	if err == nil {
		t.Fatalf("SetBatch succeeded with the server down")
	}
	if !IsConnectionError(err) {
		t.Fatalf("SetBatch error %v is not classified as a connection error; the pool would never be retired", err)
	}

	server.launch(t)

	// The pool re-dials the same address on its own. Retry briefly: the server
	// answers TCP a moment before it will accept queries on the database.
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = store.SetBatch(ctx, "gc-restart", map[string]string{"state": "asleep"})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SetBatch never recovered after the restart: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	snap, err := store.Get(ctx, "gc-restart")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if snap.Values["state"] != "asleep" {
		t.Fatalf("state = %q after recovery, want asleep", snap.Values["state"])
	}
}

// TestEnsureSchemaDefersTheSeedCommitWhenOtherTablesAreStaged covers review item
// E: DOLT_COMMIT commits the whole staged set, so seeding must not sweep another
// writer's staged work into a commit labeled as ours.
func TestEnsureSchemaDefersTheSeedCommitWhenOtherTablesAreStaged(t *testing.T) {
	server := startTestDoltServer(t)
	ctx := context.Background()
	db := server.connect(t)

	// Another writer's in-flight work, staged but not committed.
	if _, err := db.ExecContext(ctx, "CREATE TABLE someone_elses_work (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("create other table: %v", err)
	}
	if err := drainCall(ctx, db, "CALL DOLT_ADD('someone_elses_work')"); err != nil {
		t.Fatalf("stage other table: %v", err)
	}

	var warnings []string
	origWarn := warnf
	warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { warnf = origWarn })

	before := doltCommitCount(t, db)
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if got := doltCommitCount(t, db); got != before {
		t.Fatalf("dolt_log went %d -> %d; the seed committed another writer's staged table", before, got)
	}
	if len(warnings) == 0 {
		t.Fatalf("deferring the seed commit was silent; an operator needs to see it")
	}
	// The pattern is effective the moment it is inserted, commit or not.
	var pattern string
	if err := db.QueryRowContext(ctx, "SELECT pattern FROM dolt_ignore WHERE pattern = ?", TableName).Scan(&pattern); err != nil {
		t.Fatalf("the dolt_ignore pattern was not seeded: %v", err)
	}
}

// TestEnsureSchemaCommitsASeedItHadToDeferEarlier is the other half of the
// deferral: the INSERT IGNORE succeeds exactly ONCE per database, so if that one
// call had to defer the commit, nothing would ever carry the seed into history
// again — and a permanently dirty working set is the documented hub merge-wedge
// class (ga-7unsv0). A later call must notice the uncommitted dolt_ignore and
// commit it once the staging area is clear.
func TestEnsureSchemaCommitsASeedItHadToDeferEarlier(t *testing.T) {
	server := startTestDoltServer(t)
	ctx := context.Background()
	db := server.connect(t)

	origWarn := warnf
	warnf = func(string, ...any) {}
	t.Cleanup(func() { warnf = origWarn })

	// Another writer's staged work forces the first call to defer.
	if _, err := db.ExecContext(ctx, "CREATE TABLE someone_elses_work (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("create other table: %v", err)
	}
	if err := drainCall(ctx, db, "CALL DOLT_ADD('someone_elses_work')"); err != nil {
		t.Fatalf("stage other table: %v", err)
	}
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema (deferring): %v", err)
	}
	if !doltIgnoreDirty(t, db) {
		t.Fatalf("precondition: dolt_ignore is already clean, so the deferral never happened")
	}

	// The other writer commits its own work; the staging area is clear again.
	if err := drainCall(ctx, db, "CALL DOLT_COMMIT('-m', 'someone else')"); err != nil {
		t.Fatalf("other writer's commit: %v", err)
	}

	before := doltCommitCount(t, db)
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema (second call): %v", err)
	}
	if got := doltCommitCount(t, db); got != before+1 {
		t.Fatalf("dolt_log went %d -> %d, want the deferred seed committed exactly once", before, got)
	}
	if doltIgnoreDirty(t, db) {
		t.Fatalf("dolt_ignore is still uncommitted; a permanently dirty working set wedges hub merges (ga-7unsv0)")
	}
	// And a third call finds nothing to do.
	before = doltCommitCount(t, db)
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema (third call): %v", err)
	}
	if got := doltCommitCount(t, db); got != before {
		t.Fatalf("dolt_log went %d -> %d on a clean re-seed, want unchanged", before, got)
	}
}

// setGlobalTransactionCommit flips @@GLOBAL.dolt_transaction_commit on its own
// throwaway connection. A session copies the global when it CONNECTS, so the
// setter's own connection would never see the new value — every connection the
// test then opens does.
func setGlobalTransactionCommit(ctx context.Context, t *testing.T, server *doltTestServer, value int) {
	t.Helper()
	setter := server.connect(t)
	if _, err := setter.ExecContext(ctx, fmt.Sprintf("SET @@GLOBAL.dolt_transaction_commit = %d", value)); err != nil {
		t.Skipf("cannot set @@GLOBAL.dolt_transaction_commit on this dolt build: %v", err)
	}
	t.Cleanup(func() {
		_, _ = setter.ExecContext(context.Background(), "SET @@GLOBAL.dolt_transaction_commit = 0")
	})
}

// assertTransactionCommitIsOn proves the instrument before the measurement: a
// test that silently ran with the global OFF would measure nothing.
func assertTransactionCommitIsOn(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	var raw string
	if err := db.QueryRowContext(ctx, "SELECT @@SESSION.dolt_transaction_commit").Scan(&raw); err != nil {
		t.Fatalf("read @@SESSION.dolt_transaction_commit: %v", err)
	}
	if raw == "0" || strings.EqualFold(raw, "off") {
		t.Skipf("@@SESSION.dolt_transaction_commit = %q on this dolt build; the global did not propagate to a new session", raw)
	}
}

func doltIgnoreDirty(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM dolt_status WHERE table_name = 'dolt_ignore'").Scan(&n); err != nil {
		t.Fatalf("read dolt_status: %v", err)
	}
	return n > 0
}

// TestEnsureSchemaOnAFreshDBWithTransactionCommitOn is the case every
// gc-MANAGED server hits on its first dial: gc sets
// @@GLOBAL.dolt_transaction_commit = 1 (cmd/gc/dolt_transaction_commit.go), so
// the INSERT IGNORE auto-commits, DOLT_ADD stages nothing, and DOLT_COMMIT
// answers "nothing to commit". Treating that as an error failed the dial and
// degraded liveness for a 30-second retry window on a database that was in
// exactly the desired state.
func TestEnsureSchemaOnAFreshDBWithTransactionCommitOn(t *testing.T) {
	server := startTestDoltServer(t)
	ctx := context.Background()
	// The global seeds each session's copy AT CONNECT TIME, so the connection
	// that sets it never observes it. Everything under test dials afterwards.
	setGlobalTransactionCommit(ctx, t, server, 1)
	db := server.connect(t)
	assertTransactionCommitIsOn(ctx, t, db)

	var warnings []string
	origWarn := warnf
	warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { warnf = origWarn })

	before := doltCommitCount(t, db)
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema on a fresh db with dolt_transaction_commit on: %v", err)
	}
	// The seed is committed exactly once — by the INSERT's own auto-commit, not
	// by the DOLT_COMMIT below it. That auto-commit is what leaves DOLT_ADD with
	// nothing to stage and makes DOLT_COMMIT answer Error 1105 "nothing to
	// commit"; measured on dolt here, verbatim.
	if got := doltCommitCount(t, db); got != before+1 {
		t.Fatalf("dolt_log went %d -> %d, want exactly one seed commit", before, got)
	}
	if len(warnings) != 0 {
		t.Fatalf("EnsureSchema warned on a healthy fresh db: %v", warnings)
	}
	var ignored bool
	if err := db.QueryRowContext(ctx, "SELECT ignored FROM dolt_ignore WHERE pattern = ?", TableName).Scan(&ignored); err != nil {
		t.Fatalf("read dolt_ignore: %v", err)
	}
	if !ignored {
		t.Fatalf("dolt_ignore[%s].ignored = false", TableName)
	}
	if doltIgnoreDirty(t, db) {
		t.Fatalf("dolt_ignore is uncommitted after a clean seed; the working set must not stay dirty")
	}
	// Idempotent, and still silent.
	warnings = nil
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema (second call): %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("the second EnsureSchema warned: %v", warnings)
	}
}

// TestLivenessWritesMintNoCommitsEvenWithTransactionCommitOn is the MEASUREMENT
// that retired the old "@@GLOBAL.dolt_transaction_commit must be off" warning.
// The warning's claim was false, and an operator acting on it by turning the
// global off would re-open the stranded-writes class it exists to close
// (ga-7unsv0). A dolt_ignore'd table's rows are simply not part of any commit's
// tree; the control writes to a NON-ignored table on the same connection are
// what make that a measurement rather than an assumption.
func TestLivenessWritesMintNoCommitsEvenWithTransactionCommitOn(t *testing.T) {
	server := startTestDoltServer(t)
	ctx := context.Background()
	setGlobalTransactionCommit(ctx, t, server, 1)
	db := server.connect(t)
	assertTransactionCommitIsOn(ctx, t, db)

	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE control_versioned (id INT PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("create control table: %v", err)
	}

	store := NewSQLStore(db)
	before := doltCommitCount(t, db)
	for i := 0; i < 10; i++ {
		if err := store.SetBatch(ctx, "gc-measure", map[string]string{"generation": strconv.Itoa(i)}); err != nil {
			t.Fatalf("SetBatch #%d: %v", i, err)
		}
	}
	afterLiveness := doltCommitCount(t, db)
	if afterLiveness != before {
		t.Fatalf("10 liveness writes minted %d Dolt commits with the global ON, want 0", afterLiveness-before)
	}

	for i := 0; i < 10; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO control_versioned (id, v) VALUES (?, ?)", i, "x"); err != nil {
			t.Fatalf("control insert #%d: %v", i, err)
		}
	}
	afterControl := doltCommitCount(t, db)
	if afterControl == afterLiveness {
		t.Fatalf("the control writes to a NON-ignored table minted no commits either; the global is not actually on and this test measures nothing")
	}
}
