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
// The seed commit fires ONLY when INSERT IGNORE actually added the row, so a
// healthy database performs zero writes here and steady state mints no commits.
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
	// commit, which is strictly the less surprising failure.
	if n, raErr := res.RowsAffected(); raErr == nil && n > 0 {
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
			if err := drainCall(ctx, db, "CALL DOLT_COMMIT('-m', ?)", seedCommitMessage); err != nil {
				return fmt.Errorf("liveness: committing seeded dolt_ignore: %w", err)
			}
		}
	}
	if _, err := db.ExecContext(ctx, createTableStmt); err != nil {
		return fmt.Errorf("liveness: creating %s: %w", TableName, err)
	}
	assertTransactionCommitOff(ctx, db)
	return nil
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

// assertTransactionCommitOff verifies the invariant the whole design rests on:
// with @@GLOBAL.dolt_transaction_commit = 1 every SQL transaction against this
// server mints a Dolt commit, so liveness writes would commit ~250 times an hour
// exactly as before — the change would be silently inert. It warns rather than
// refusing: the table is still the right place for this data, and refusing the
// open would take liveness down over a server setting a human must change.
func assertTransactionCommitOff(ctx context.Context, db DB) {
	rows, err := db.QueryContext(ctx, "SELECT @@GLOBAL.dolt_transaction_commit")
	if err != nil {
		warnf("liveness: cannot read @@GLOBAL.dolt_transaction_commit (%v); cannot confirm that liveness writes mint no Dolt commits", err)
		return
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return
	}
	var raw any
	if err := rows.Scan(&raw); err != nil {
		return
	}
	if !isZeroish(raw) {
		warnf("liveness: @@GLOBAL.dolt_transaction_commit is %v, not 0 — EVERY liveness write will mint a Dolt commit and this change is inert until it is turned off", raw)
	}
}

// isZeroish reports whether a MySQL system-variable value reads as 0/off across
// the shapes a driver may hand back (int64, []byte, string).
func isZeroish(raw any) bool {
	switch v := raw.(type) {
	case nil:
		return true
	case int64:
		return v == 0
	case []byte:
		return isZeroText(string(v))
	case string:
		return isZeroText(v)
	default:
		return isZeroText(fmt.Sprint(v))
	}
}

func isZeroText(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "0" || s == "off" || s == "false" || s == ""
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
