package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/gastownhall/gascity/internal/liveness"
)

// This file covers the BINDING's dial-and-install path — specifically that it
// captures the liveness endpoint's clock offset. See livenessBinding.Now: the
// fence stamps a degraded write mints are minted exactly when the store is
// UNAVAILABLE, so an offset that was never recorded silently sends every one of
// them back to the raw local clock — the wrong timebase for the server-minted
// written_at values they have to fence.

// offsetLivenessStore reports a clock a fixed distance from the local one,
// standing in for a Dolt host whose clock differs from this machine's.
type offsetLivenessStore struct {
	*liveness.MemStore
	offset time.Duration
}

func (s *offsetLivenessStore) Now() time.Time { return time.Now().UTC().Add(s.offset) }

// TestLivenessDialRecordsTheClockOffsetSoNowSurvivesPoolRetirement is the
// deterministic half: a real dial through livenessBinding.Store(), then the pool
// is retired, and Now() must still report the ENDPOINT's clock. The production
// dial used to record nothing (only the test helpers did), so after a
// retirement every fence stamp fell back to the raw local clock.
func TestLivenessDialRecordsTheClockOffsetSoNowSurvivesPoolRetirement(t *testing.T) {
	const skew = 90 * time.Second
	orig := openScopeLivenessStoreFn
	t.Cleanup(func() { openScopeLivenessStoreFn = orig })
	openScopeLivenessStoreFn = func(string, string) (liveness.Store, error) {
		return &offsetLivenessStore{MemStore: liveness.NewMemStore(), offset: skew}, nil
	}

	b := &livenessBinding{scopeRoot: t.TempDir(), mode: liveness.ModeTable}
	if b.Store() == nil {
		t.Fatalf("the dial installed no store")
	}

	// The pool dies — the moment fence stamps actually matter.
	b.noteOpError(errors.New("invalid connection"))
	if b.Store() != nil {
		t.Fatalf("precondition: the pool was not retired")
	}

	drift := b.Now().Sub(time.Now().UTC().Add(skew))
	if drift < -2*time.Second || drift > 2*time.Second {
		t.Fatalf("Now() is %v off the endpoint's clock; the production dial recorded no offset and fell back to the raw local clock", drift)
	}
}

// TestLivenessDialAgainstARealDoltRecordsTheServerClock is the dolt-backed
// half: the store the binding installs is a genuine SQLStore over a live
// `dolt sql-server` — opened, schema-seeded and calibrated exactly as
// openScopeLivenessStore does once the scope's target has resolved. The dial
// must capture that store's measured offset, and Now() must keep reporting the
// SERVER's clock after the pool is retired.
func TestLivenessDialAgainstARealDoltRecordsTheServerClock(t *testing.T) {
	server := startLivenessDoltServer(t)
	ctx := context.Background()

	orig := openScopeLivenessStoreFn
	t.Cleanup(func() { openScopeLivenessStoreFn = orig })
	openScopeLivenessStoreFn = func(string, string) (liveness.Store, error) {
		db, err := sql.Open("mysql", server.dsn)
		if err != nil {
			return nil, err
		}
		if err := liveness.EnsureSchema(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
		store := liveness.NewSQLStore(db)
		if err := store.Calibrate(ctx); err != nil {
			_ = store.Close()
			return nil, err
		}
		return store, nil
	}

	b := &livenessBinding{scopeRoot: t.TempDir(), mode: liveness.ModeTable}
	store := b.Store()
	if store == nil {
		t.Fatalf("the dial installed no store")
	}
	// A live write proves the installed handle really is the real server's.
	if err := store.SetBatch(ctx, "gc-dial", map[string]string{"state": "active"}); err != nil {
		t.Fatalf("SetBatch through the dialed store: %v", err)
	}
	b.mu.Lock()
	recorded := b.haveClockOffset
	b.mu.Unlock()
	if !recorded {
		t.Fatalf("the production dial recorded no clock offset; after a pool retirement every fence stamp uses the raw local clock")
	}

	// Retire the pool, then compare Now() against the server's OWN clock read on
	// a fresh connection.
	b.noteOpError(errors.New("invalid connection"))
	if b.Store() != nil {
		t.Fatalf("precondition: the pool was not retired")
	}
	probe, err := sql.Open("mysql", server.dsn)
	if err != nil {
		t.Fatalf("open probe connection: %v", err)
	}
	defer func() { _ = probe.Close() }()
	var raw string
	if err := probe.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&raw); err != nil {
		t.Fatalf("read server clock: %v", err)
	}
	serverNow, err := time.Parse("2006-01-02 15:04:05.999999", raw)
	if err != nil {
		t.Fatalf("parse server clock %q: %v", raw, err)
	}
	if drift := b.Now().Sub(serverNow.UTC()); drift < -5*time.Second || drift > 5*time.Second {
		t.Fatalf("Now() is %v off the server clock after the pool was retired", drift)
	}
}

// livenessDoltServer is a throwaway `dolt sql-server` for the dial tests. It
// mirrors the harness in internal/liveness/sqlstore_dolt_test.go rather than
// borrowing cmd/gc's, whose helpers are gated behind skipSlowCmdGCTest — this
// case must actually RUN in the ordinary suite, because the defect it covers
// (a dial that records no clock offset) is invisible to every unit-level test.
type livenessDoltServer struct {
	dsn string
}

func startLivenessDoltServer(t *testing.T) *livenessDoltServer {
	t.Helper()
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt binary not in PATH; skipping the real-dial liveness test")
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
	for _, kv := range [][2]string{{"user.name", "liveness-dial-test"}, {"user.email", "liveness@test.local"}} {
		cfg := exec.Command(doltBin, "config", "--global", "--add", kv[0], kv[1])
		cfg.Env = env
		cfg.Dir = dataDir
		if out, err := cfg.CombinedOutput(); err != nil {
			t.Skipf("dolt config --global %s failed: %v\n%s", kv[0], err, out)
		}
	}

	cmd := exec.Command(doltBin, "sql-server", "--host", "127.0.0.1",
		"--port", strconv.Itoa(port), "--data-dir", dataDir)
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dolt sql-server: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	prefix := fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port)
	boot, err := sql.Open("mysql", prefix)
	if err != nil {
		t.Fatalf("open dolt connection: %v", err)
	}
	defer func() { _ = boot.Close() }()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if err := boot.Ping(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dolt sql-server did not become ready on port %d", port)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := boot.Exec("CREATE DATABASE livenessdial"); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	return &livenessDoltServer{dsn: prefix + "livenessdial"}
}
