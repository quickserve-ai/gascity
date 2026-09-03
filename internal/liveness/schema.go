package liveness

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// createTableStmt is the liveness table DDL. bead_id + k is the primary key, so
// an upsert is a single-row primary-key operation and two writers racing on the
// same key resolve last-write-wins at the ROW level — nothing is lost at the key
// level, which is exactly the multi-process property the retired JSON-sidecar
// design could not offer.
const createTableStmt = "CREATE TABLE IF NOT EXISTS " + TableName + " (" +
	"bead_id VARCHAR(255) NOT NULL, " +
	"k VARCHAR(64) NOT NULL, " +
	"v TEXT NOT NULL, " +
	"written_at DATETIME(6) NOT NULL, " +
	"PRIMARY KEY (bead_id, k))"

// seedCommitMessage labels the one-time dolt_ignore seed commit.
const seedCommitMessage = "gc: seed dolt_ignore " + TableName

// EnsureSchema idempotently registers the liveness table in dolt_ignore and
// creates it. Safe to call on every store open / controller start.
//
// ORDER IS LOAD-BEARING. The dolt_ignore pattern is registered (and, if it was
// genuinely new, committed) BEFORE the CREATE TABLE, so the table has never
// existed for even one moment as a committable table. Creating first and
// ignoring second leaves a window in which any concurrent DOLT_COMMIT -a stages
// the new table into history — after which dolt_ignore no longer suppresses it
// (an already-committed table stays committed) and every liveness write is back
// to minting the commits this whole change exists to remove.
//
// The seed commit is attempted only when there is something to commit: either
// the INSERT IGNORE just added the row, or a previous call added it and had to
// DEFER the commit. That second case is why the deferral is re-checked rather
// than trusted to "the next call will insert again" — the insert succeeds
// exactly ONCE per database, so a single deferral would otherwise leave the seed
// uncommitted forever, and a permanently dirty working set is the documented hub
// merge-wedge class (ga-7unsv0). A healthy steady state finds nothing to do here
// and mints no commits.
//
// ON @@GLOBAL.dolt_transaction_commit. gc sets it to 1 on every managed server
// (cmd/gc/dolt_transaction_commit.go) and this code must NOT ask an operator to
// turn it off: doing so re-opens the stranded-writes class that setting exists
// to close. It costs liveness nothing — MEASURED on a real dolt server with the
// global ON, 10 SetBatch writes to this dolt_ignore'd table minted 0 Dolt
// commits while 10 control writes to a non-ignored table on the same connection
// minted 10. An ignored table's rows are simply not part of any commit's tree.
// The one visible effect is here: the INSERT above auto-commits, so the DOLT_ADD
// below stages nothing and DOLT_COMMIT answers "nothing to commit" — success,
// not failure, and treated as such.
func EnsureSchema(ctx context.Context, db DB) error {
	if db == nil {
		return fmt.Errorf("liveness: nil database handle")
	}
	res, err := db.ExecContext(ctx, "INSERT IGNORE INTO dolt_ignore VALUES (?, true)", TableName)
	if err != nil {
		return fmt.Errorf("liveness: seeding dolt_ignore %q: %w", TableName, err)
	}
	// A RowsAffected error degrades to "not changed": the seed then rides along
	// in the working set as an uncommitted diff instead of getting its own
	// commit, which is strictly the less surprising failure. The dirty check
	// below catches it on the next call anyway.
	inserted := false
	if n, raErr := res.RowsAffected(); raErr == nil && n > 0 {
		inserted = true
	}
	needsCommit := inserted
	if !needsCommit {
		// The row already existed. It may still be sitting UNCOMMITTED from an
		// earlier call that deferred — the insert will never fire again, so this
		// is the only thing that can ever carry that seed into history.
		dirty, checkErr := doltIgnoreIsUncommitted(ctx, db)
		if checkErr != nil {
			warnf("liveness: cannot inspect dolt_status (%v); cannot confirm the dolt_ignore seed is committed", checkErr)
		}
		needsCommit = dirty
	}
	if needsCommit {
		// DOLT_COMMIT commits the whole STAGED set, not just what we added. If
		// another writer already has tables staged, committing here would sweep
		// their in-flight work into a commit labeled as ours. Leave the seed as
		// a working-set diff instead and let the next call (or that writer's own
		// commit) carry it — the pattern is already effective the moment it is
		// inserted, so deferring the commit costs nothing but a dirty
		// dolt_ignore until then.
		staged, checkErr := otherStagedTables(ctx, db)
		switch {
		case checkErr != nil:
			// Cannot prove the staging area is clean: do not commit. Same
			// reasoning, failing closed.
			warnf("liveness: cannot inspect dolt_status (%v); leaving the dolt_ignore seed uncommitted in the working set", checkErr)
		case len(staged) > 0:
			warnf("liveness: %d other table(s) staged (%s); leaving the dolt_ignore seed uncommitted rather than committing another writer's work",
				len(staged), strings.Join(staged, ", "))
		default:
			if err := drainCall(ctx, db, "CALL DOLT_ADD('dolt_ignore')"); err != nil {
				return fmt.Errorf("liveness: staging seeded dolt_ignore: %w", err)
			}
			if err := drainCall(ctx, db, "CALL DOLT_COMMIT('-m', ?)", seedCommitMessage); err != nil && !isNothingToCommit(err) {
				return fmt.Errorf("liveness: committing seeded dolt_ignore: %w", err)
			}
		}
	}
	if _, err := db.ExecContext(ctx, createTableStmt); err != nil {
		return fmt.Errorf("liveness: creating %s: %w", TableName, err)
	}
	return nil
}

// isNothingToCommit reports whether a DOLT_COMMIT failed only because the
// staging area was already empty.
//
// This is the NORMAL outcome on a gc-managed server: with
// @@GLOBAL.dolt_transaction_commit = 1 the INSERT IGNORE above auto-commits, so
// by the time DOLT_ADD runs there is nothing left to stage and DOLT_COMMIT
// returns Error 1105 "nothing to commit". Treating that as a failure took the
// whole dial down on a fresh database and degraded liveness for a 30-second
// retry window, over a database that was in exactly the desired state.
func isNothingToCommit(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "nothing to commit")
}

// doltIgnoreIsUncommitted reports whether dolt_ignore itself has changes in the
// working set or the index — the shape a DEFERRED seed leaves behind.
func doltIgnoreIsUncommitted(ctx context.Context, db DB) (bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT COUNT(*) FROM dolt_status WHERE table_name = 'dolt_ignore'")
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return false, rows.Err()
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		return false, err
	}
	return n > 0, rows.Err()
}

// warnf is the package's diagnostic sink. It is a var so tests can capture it.
var warnf = func(format string, args ...any) {
	log.Printf(format, args...)
}

// otherStagedTables returns the staged tables OTHER than dolt_ignore itself.
// dolt_status reports (table_name, staged, status); a row with staged=1 is in
// the index and would ride along with any DOLT_COMMIT we issue.
func otherStagedTables(ctx context.Context, db DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT table_name FROM dolt_status WHERE staged = TRUE")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if name == "dolt_ignore" {
			continue
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// drainCall runs a Dolt stored procedure and consumes its result set. Dolt's
// CALL statements return rows; leaving them unread wedges the connection for the
// next statement on it.
func drainCall(ctx context.Context, db DB, stmt string, args ...any) error {
	rows, err := db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() { //nolint:revive // draining the procedure's result set is the point
	}
	return rows.Err()
}
