package liveness

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// TableName is the non-versioned table holding session-liveness rows. It is
// registered in dolt_ignore at seed time (see EnsureSchema), so its rows live
// only in the working set: they never stage, never commit, and never replicate.
const TableName = "session_liveness"

// Snapshot is one bead's liveness state as the table holds it.
type Snapshot struct {
	// Values maps liveness key -> value. An empty-string value is a real,
	// meaningful entry: it means the key was CLEARED, and the overlay must
	// project that empty value rather than falling back to committed metadata.
	Values map[string]string
	// WrittenAt is max(written_at) across the bead's rows — the bead's "last
	// liveness write" clock. Zero when the bead has no rows.
	WrittenAt time.Time
}

// Store is the non-versioned liveness surface. Implementations must be safe for
// concurrent use by multiple goroutines AND multiple processes: the production
// implementation is backed by a shared Dolt table precisely because the previous
// candidate (a per-process JSON sidecar) could not offer the second guarantee.
type Store interface {
	// SetBatch upserts every key in kv for beadID inside ONE transaction.
	// An empty-string value writes a tombstone row (v=''), not a delete — see
	// the note on Snapshot.Values.
	SetBatch(ctx context.Context, beadID string, kv map[string]string) error
	// Get returns the snapshot for one bead. A bead with no rows yields a
	// zero Snapshot and a nil error.
	Get(ctx context.Context, beadID string) (Snapshot, error)
	// GetMany returns snapshots for the requested beads. Beads with no rows are
	// simply absent from the result map.
	GetMany(ctx context.Context, beadIDs []string) (map[string]Snapshot, error)
	// Close releases the store's resources.
	Close() error
}

// DB is the narrow database surface SQLStore needs. *sql.DB satisfies it; tests
// that want to drive the real SQL text without a server can substitute their own.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
	Close() error
}

// SQLStore is the production Store: a `session_liveness` table on the same
// managed Dolt database the bead store uses, registered in dolt_ignore so writes
// mint no Dolt commits.
//
// It holds its OWN connection pool rather than borrowing the beads library's:
// the store must work identically whether the bead store resolved to
// NativeDoltStore or to the exec/bd fallback, and the bd fallback has no
// in-process handle to borrow.
type SQLStore struct {
	db DB
	// getMaxIDs bounds the id list in one GetMany statement. Larger batches are
	// chunked. Dolt plans `bead_id IN (...)` as a primary-key prefix probe, so
	// the chunk size only bounds statement text, not work.
	getMaxIDs int
}

var _ Store = (*SQLStore)(nil)

// NewSQLStore wraps an already-open handle to the managed Dolt database. The
// caller owns schema seeding (EnsureSchema) and is responsible for having
// pointed db at the SAME database that holds the scope's `issues` table.
func NewSQLStore(db DB) *SQLStore {
	return &SQLStore{db: db, getMaxIDs: 400}
}

// Close closes the underlying handle.
func (s *SQLStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// SetBatch upserts kv for beadID in a single SQL transaction.
//
// It never calls DOLT_COMMIT: the table is dolt_ignore'd and
// @@GLOBAL.dolt_transaction_commit is 0 on the target servers, so a plain SQL
// transaction here mints no Dolt commit. That is the entire point of the change;
// a DOLT_COMMIT added here would silently restore the churn it removes.
func (s *SQLStore) SetBatch(ctx context.Context, beadID string, kv map[string]string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("liveness: store not open")
	}
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return fmt.Errorf("liveness: empty bead id")
	}
	if len(kv) == 0 {
		return nil
	}
	keysToWrite, err := sortedWritableKeys(kv)
	if err != nil {
		return err
	}
	now := time.Now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("liveness: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	const stmt = "INSERT INTO " + TableName + " (bead_id, k, v, written_at) VALUES (?, ?, ?, ?) " +
		"ON DUPLICATE KEY UPDATE v = VALUES(v), written_at = VALUES(written_at)"
	for _, k := range keysToWrite {
		if _, err := tx.ExecContext(ctx, stmt, beadID, k, kv[k], now); err != nil {
			return fmt.Errorf("liveness: upsert %s[%s]: %w", beadID, k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("liveness: commit: %w", err)
	}
	committed = true
	return nil
}

// sortedWritableKeys validates and orders the keys of a patch. Ordering is
// deterministic so concurrent writers touching overlapping key sets take row
// locks in the same order and cannot deadlock against each other.
func sortedWritableKeys(kv map[string]string) ([]string, error) {
	out := make([]string, 0, len(kv))
	for k := range kv {
		if k == WrittenAtKey {
			return nil, fmt.Errorf("liveness: %s is derived from the table's own timestamps and cannot be written", WrittenAtKey)
		}
		if strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("liveness: empty metadata key")
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// Get returns the liveness snapshot for one bead.
func (s *SQLStore) Get(ctx context.Context, beadID string) (Snapshot, error) {
	if s == nil || s.db == nil {
		return Snapshot{}, fmt.Errorf("liveness: store not open")
	}
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return Snapshot{}, nil
	}
	byBead, err := s.query(ctx, []string{beadID})
	if err != nil {
		return Snapshot{}, err
	}
	return byBead[beadID], nil
}

// GetMany returns liveness snapshots for the requested beads, chunking the id
// list so one oversized List cannot produce an unbounded statement.
func (s *SQLStore) GetMany(ctx context.Context, beadIDs []string) (map[string]Snapshot, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("liveness: store not open")
	}
	ids := dedupeIDs(beadIDs)
	out := make(map[string]Snapshot, len(ids))
	chunk := s.getMaxIDs
	if chunk <= 0 {
		chunk = 400
	}
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part, err := s.query(ctx, ids[start:end])
		if err != nil {
			return nil, err
		}
		for id, snap := range part {
			out[id] = snap
		}
	}
	return out, nil
}

func (s *SQLStore) query(ctx context.Context, ids []string) (map[string]Snapshot, error) {
	if len(ids) == 0 {
		return map[string]Snapshot{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	//nolint:gosec // G201: the only interpolation is a generated ?-placeholder list.
	q := "SELECT bead_id, k, v, written_at FROM " + TableName + " WHERE bead_id IN (" + placeholders + ")"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("liveness: read: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]Snapshot, len(ids))
	for rows.Next() {
		var (
			beadID, k, v string
			rawWritten   any
		)
		// written_at is scanned as any, not time.Time: whether the driver hands
		// back a time.Time or the raw bytes depends on the connection's
		// parseTime flag, and this store must not depend on how the caller
		// happened to build its DSN.
		if err := rows.Scan(&beadID, &k, &v, &rawWritten); err != nil {
			return nil, fmt.Errorf("liveness: scan: %w", err)
		}
		writtenAt := parseWrittenAt(rawWritten)
		snap, ok := out[beadID]
		if !ok {
			snap = Snapshot{Values: make(map[string]string, 8)}
		}
		snap.Values[k] = v
		if writtenAt.After(snap.WrittenAt) {
			snap.WrittenAt = writtenAt
		}
		out[beadID] = snap
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("liveness: read: %w", err)
	}
	return out, nil
}

// writtenAtLayouts are the textual DATETIME(6) shapes a driver returns when it
// is NOT parsing times itself, in the order Dolt/MySQL emit them.
var writtenAtLayouts = []string{
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	time.RFC3339Nano,
	time.RFC3339,
}

// parseWrittenAt normalizes whatever the driver produced for written_at into a
// UTC time. An unparseable value yields the zero time, which the caller treats
// as "no clock" — a missing freshness stamp degrades to Bead.UpdatedAt rather
// than failing the read.
func parseWrittenAt(raw any) time.Time {
	var text string
	switch v := raw.(type) {
	case nil:
		return time.Time{}
	case time.Time:
		return v.UTC()
	case []byte:
		text = string(v)
	case string:
		text = v
	default:
		return time.Time{}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}
	}
	for _, layout := range writtenAtLayouts {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func dedupeIDs(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// MemStore is an in-process Store for tests and for callers that want the
// splitter's shape without a database. It is NOT a production substitute: it
// gives no cross-process visibility, which is precisely the property the table
// exists to provide.
type MemStore struct {
	mu   sync.Mutex
	rows map[string]map[string]memRow
	// Now, when set, supplies the write timestamp (tests pin it).
	Now func() time.Time
}

type memRow struct {
	value     string
	writtenAt time.Time
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-process liveness store.
func NewMemStore() *MemStore {
	return &MemStore{rows: map[string]map[string]memRow{}}
}

// SetBatch applies the whole patch under one lock, matching the SQL store's
// single-transaction contract.
func (m *MemStore) SetBatch(_ context.Context, beadID string, kv map[string]string) error {
	if strings.TrimSpace(beadID) == "" {
		return fmt.Errorf("liveness: empty bead id")
	}
	if len(kv) == 0 {
		return nil
	}
	if _, err := sortedWritableKeys(kv); err != nil {
		return err
	}
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]map[string]memRow{}
	}
	bead := m.rows[beadID]
	if bead == nil {
		bead = map[string]memRow{}
		m.rows[beadID] = bead
	}
	for k, v := range kv {
		bead[k] = memRow{value: v, writtenAt: now}
	}
	return nil
}

// Get returns one bead's snapshot.
func (m *MemStore) Get(_ context.Context, beadID string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked(beadID), nil
}

// GetMany returns snapshots for the requested beads.
func (m *MemStore) GetMany(_ context.Context, beadIDs []string) (map[string]Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Snapshot, len(beadIDs))
	for _, id := range dedupeIDs(beadIDs) {
		if snap := m.snapshotLocked(id); snap.Values != nil {
			out[id] = snap
		}
	}
	return out, nil
}

func (m *MemStore) snapshotLocked(beadID string) Snapshot {
	bead := m.rows[beadID]
	if len(bead) == 0 {
		return Snapshot{}
	}
	snap := Snapshot{Values: make(map[string]string, len(bead))}
	for k, row := range bead {
		snap.Values[k] = row.value
		if row.writtenAt.After(snap.WrittenAt) {
			snap.WrittenAt = row.writtenAt
		}
	}
	return snap
}

// Close is a no-op.
func (m *MemStore) Close() error { return nil }

// Overlay merges a bead's liveness snapshot over a copy of its committed
// metadata and returns the merged map. Keys present in the snapshot WIN
// (including an empty-string clear); keys absent from it fall back to whatever
// the committed metadata holds — the natural carry-over that lets pre-existing
// session beads work with no migration step.
//
// When the snapshot has rows, WrittenAtKey is stamped with its clock so
// freshness consumers have a replacement for the now-quiet Bead.UpdatedAt.
// A zero-value snapshot returns meta unchanged (same map, not a copy) so the
// no-liveness case allocates nothing.
func Overlay(meta map[string]string, snap Snapshot) map[string]string {
	if len(snap.Values) == 0 {
		return meta
	}
	merged := make(map[string]string, len(meta)+len(snap.Values)+1)
	for k, v := range meta {
		merged[k] = v
	}
	for k, v := range snap.Values {
		merged[k] = v
	}
	if !snap.WrittenAt.IsZero() {
		merged[WrittenAtKey] = snap.WrittenAt.UTC().Format(time.RFC3339Nano)
	}
	return merged
}
