package liveness

import (
	"context"
	"fmt"
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
		if err := drainCall(ctx, db, "CALL DOLT_ADD('dolt_ignore')"); err != nil {
			return fmt.Errorf("liveness: staging seeded dolt_ignore: %w", err)
		}
		if err := drainCall(ctx, db, "CALL DOLT_COMMIT('-m', ?)", seedCommitMessage); err != nil {
			return fmt.Errorf("liveness: committing seeded dolt_ignore: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, createTableStmt); err != nil {
		return fmt.Errorf("liveness: creating %s: %w", TableName, err)
	}
	return nil
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
