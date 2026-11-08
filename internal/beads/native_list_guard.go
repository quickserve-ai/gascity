package beads

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// nativeListGuardTimeout bounds the guard probe. A probe that outruns it
// counts as failed, and the listing reads unpushed.
const nativeListGuardTimeout = 15 * time.Second

// nativeListOutsideRow is one row whose status the pushed not-closed set names
// nowhere: a pushed read would hide it, because the set is an allow-list.
type nativeListOutsideRow struct {
	ID     string
	Status string
}

// nativeListOutsideProbe finds the rows outside notClosed. It is a seam for
// tests; a nil probe on the store means nativeListOutsideRows.
type nativeListOutsideProbe func(ctx context.Context, storage beadslib.Storage, notClosed string) ([]nativeListOutsideRow, error)

// nativeListGuardQuery is the probe's SQL and arguments: rows of either table
// whose status is neither in notClosed nor closed. It covers every kind of
// row, which is stricter than the opted-in caller needs: a stray status on a
// row that caller would not have used still costs it the pushdown.
func nativeListGuardQuery(notClosed string) (string, []any) {
	var names []any
	for _, name := range strings.Split(notClosed, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	names = append(names, "closed")
	marks := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	query := "SELECT id, status FROM issues WHERE status NOT IN (" + marks +
		") UNION ALL SELECT id, status FROM wisps WHERE status NOT IN (" + marks + ")"
	return query, append(append([]any{}, names...), names...)
}

// nativeListOutsideRows runs the guard query on the storage's own SQL handle.
// A storage with no SQL handle is an error, so its listing reads unpushed.
func nativeListOutsideRows(ctx context.Context, storage beadslib.Storage, notClosed string) ([]nativeListOutsideRow, error) {
	accessor, ok := storage.(interface{ UnderlyingDB() *sql.DB })
	if !ok {
		return nil, fmt.Errorf("storage %T has no SQL handle", storage)
	}
	db := accessor.UnderlyingDB()
	if db == nil {
		return nil, fmt.Errorf("storage %T has a nil SQL handle", storage)
	}
	ctx, cancel := context.WithTimeout(ctx, nativeListGuardTimeout)
	defer cancel()
	query, args := nativeListGuardQuery(notClosed)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only rows; Err below reports a failed read
	var out []nativeListOutsideRow
	for rows.Next() {
		var row nativeListOutsideRow
		if err := rows.Scan(&row.ID, &row.Status); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// guardNotClosed decides whether an opted-in listing may push notClosed, and
// returns the set to push: notClosed when no row sits outside it, "" otherwise.
// Any row outside the set, or a probe that fails or times out, sends this one
// listing down the full unpushed read. Nothing latches, so a status renamed
// back clears itself on the next listing.
func (s *NativeDoltStore) guardNotClosed(ctx context.Context, storage beadslib.Storage, notClosed string) string {
	if notClosed == "" {
		return ""
	}
	probe := s.listOutsideProbe
	if probe == nil {
		probe = nativeListOutsideRows
	}
	s.listCounters.guardRuns.Add(1)
	rows, err := probe(ctx, storage, notClosed)
	if err != nil {
		s.listCounters.guardFallbacks.Add(1)
		s.listLogger().Warn("native bead store: not-closed guard failed; reading this listing unpushed",
			"prefix", s.idPrefix, "err", err, "guard_stats", s.listCounters.guardText())
		return ""
	}
	if len(rows) == 0 {
		return notClosed
	}
	s.listCounters.guardRows.Add(int64(len(rows)))
	s.listCounters.guardFallbacks.Add(1)
	s.listLogger().Warn("native bead store: rows sit outside the not-closed status set; reading this listing unpushed",
		"prefix", s.idPrefix, "count", len(rows), "statuses", nativeListOutsideStatuses(rows, 3),
		"guard_stats", s.listCounters.guardText())
	return ""
}

// nativeListOutsideStatuses names up to limit distinct statuses, in first-seen order.
func nativeListOutsideStatuses(rows []nativeListOutsideRow, limit int) string {
	var names []string
	seen := make(map[string]bool)
	for _, row := range rows {
		if seen[row.Status] {
			continue
		}
		seen[row.Status] = true
		names = append(names, row.Status)
		if len(names) == limit {
			break
		}
	}
	return strings.Join(names, ",")
}

func (c *nativeListCounters) guardText() string {
	return fmt.Sprintf("runs=%d rows=%d fallbacks=%d", c.guardRuns.Load(), c.guardRows.Load(), c.guardFallbacks.Load())
}
