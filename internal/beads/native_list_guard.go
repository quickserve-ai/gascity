package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// nativeListGuardTimeout bounds the guard probe. A probe that outruns it
// counts as failed, and the listing reads unpushed.
const nativeListGuardTimeout = 10 * time.Second

// nativeListGuardRowLimit caps the rows one probe returns. The guard only needs
// to know that a row exists; the statuses it logs need a sample, not the set.
const nativeListGuardRowLimit = 100

// nativeListGuardHeartbeatEvery is how many guard runs pass between the Info
// lines that show the guard working (noteGuardHeartbeat).
const nativeListGuardHeartbeatEvery = 120

// errNativeListNoSQLHandle is a storage the guard can never probe. It does not
// clear, so its listings read unpushed and the store warns once.
var errNativeListNoSQLHandle = errors.New("no SQL handle")

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
// whose status is neither in notClosed nor closed, at most
// nativeListGuardRowLimit in all across the two. It covers every kind of row,
// which is stricter than the opted-in caller needs: a stray status on a row
// that caller would not have used still costs it the pushdown.
func nativeListGuardQuery(notClosed string) (string, []any) {
	var names []any
	for _, name := range strings.Split(notClosed, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	names = append(names, "closed")
	marks := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	query := "(SELECT id, status FROM issues WHERE status NOT IN (" + marks +
		")) UNION ALL (SELECT id, status FROM wisps WHERE status NOT IN (" + marks +
		")) LIMIT " + strconv.Itoa(nativeListGuardRowLimit)
	return query, append(append([]any{}, names...), names...)
}

// nativeListOutsideRows runs the guard query on the storage's own SQL handle.
// A storage with no SQL handle is an error, so its listing reads unpushed.
func nativeListOutsideRows(ctx context.Context, storage beadslib.Storage, notClosed string) ([]nativeListOutsideRow, error) {
	accessor, ok := storage.(interface{ UnderlyingDB() *sql.DB })
	if !ok {
		return nil, fmt.Errorf("storage %T: %w", storage, errNativeListNoSQLHandle)
	}
	db := accessor.UnderlyingDB()
	if db == nil {
		return nil, fmt.Errorf("storage %T returned a nil handle: %w", storage, errNativeListNoSQLHandle)
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
// back clears itself on the next listing. Its counts go on tally, so a retried
// attempt is not counted twice.
func (s *NativeDoltStore) guardNotClosed(ctx context.Context, storage beadslib.Storage, notClosed string, tally *nativeListTally) string {
	if notClosed == "" {
		return ""
	}
	probe := s.listOutsideProbe
	if probe == nil {
		probe = nativeListOutsideRows
	}
	tally.guardRuns++
	rows, err := probe(ctx, storage, notClosed)
	if err != nil {
		tally.guardFallbacks++
		if errors.Is(err, errNativeListNoSQLHandle) {
			if s.guardNoSQLWarned.CompareAndSwap(false, true) {
				s.listLogger().Warn("native bead store: this storage has no SQL handle for the not-closed guard; every opted-in listing on this store reads unpushed",
					"prefix", s.idPrefix, "err", err, "guard_stats", s.listCounters.guardText(*tally))
			}
			return ""
		}
		s.listLogger().Warn("native bead store: not-closed guard failed; reading this listing unpushed",
			"prefix", s.idPrefix, "err", err, "guard_stats", s.listCounters.guardText(*tally))
		return ""
	}
	if len(rows) == 0 {
		return notClosed
	}
	tally.guardRows += int64(len(rows))
	tally.guardFallbacks++
	s.listLogger().Warn("native bead store: rows sit outside the not-closed status set; reading this listing unpushed",
		"prefix", s.idPrefix, "count", len(rows), "count_capped", len(rows) >= nativeListGuardRowLimit,
		"statuses", nativeListOutsideStatuses(rows, 3), "guard_stats", s.listCounters.guardText(*tally))
	return ""
}

// noteGuardHeartbeat logs one Info line when guardRuns, the store's run count
// after a List's commit, is a multiple of nativeListGuardHeartbeatEvery. It is
// the counters' one production reader: without it a healthy guard says nothing.
// Each committed run yields a distinct count, so racing Lists log it once.
func (s *NativeDoltStore) noteGuardHeartbeat(guardRuns int64) {
	if guardRuns == 0 || guardRuns%nativeListGuardHeartbeatEvery != 0 {
		return
	}
	stats := s.listCounters.snapshot()
	s.listLogger().Info("native bead store: not-closed guard heartbeat",
		"prefix", s.idPrefix, "guard_runs", guardRuns, "guard_rows", stats.GuardRows, "guard_fallbacks", stats.GuardFallbacks)
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

// guardText renders the committed guard counts plus t, the attempt in progress.
func (c *nativeListCounters) guardText(t nativeListTally) string {
	return fmt.Sprintf("runs=%d rows=%d fallbacks=%d", c.guardRuns.Load()+t.guardRuns, c.guardRows.Load()+t.guardRows, c.guardFallbacks.Load()+t.guardFallbacks)
}
