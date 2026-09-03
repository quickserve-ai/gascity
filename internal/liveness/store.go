package liveness

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-sql-driver/mysql"
)

// TableName is the non-versioned table holding session-liveness rows. It is
// registered in dolt_ignore at seed time (see EnsureSchema), so its rows live
// only in the working set: they never stage, never commit, and never replicate.
const TableName = "session_liveness"

// maxKeyLen matches the k column's VARCHAR(64). Every key in the moved set is
// well under it (the longest, continuation_reset_pending, is 26 bytes); the
// bound exists so a future caller cannot hand the server a key it would
// silently truncate into a collision. TestEveryMovedKeyFitsTheColumn pins it.
const maxKeyLen = 64

// Snapshot is one bead's liveness state as the table holds it.
type Snapshot struct {
	// Values maps liveness key -> value. An empty-string value is a real,
	// meaningful entry: it means the key was CLEARED, and the overlay must
	// project that empty value rather than falling back to committed metadata.
	Values map[string]string
	// Times carries each row's OWN written_at. The overlay fences PER KEY
	// against that key's own fence marker, so a bead-level max is not
	// sufficient: after a degraded write, one key may have been refreshed since
	// the fallback while its siblings are still pre-outage rows that must be
	// dropped.
	Times map[string]time.Time
	// WrittenAt is max(written_at) across the bead's rows — the bead's "last
	// liveness write" clock, and the value the overlay projects as WrittenAtKey.
	// Zero when the bead has no rows.
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
	// DeleteKeys REMOVES beadID's rows for the named keys. It is the one
	// operation that is a genuine delete rather than a tombstone, and the
	// difference is the point: with no row at all the overlay falls back to the
	// bead's committed metadata, which is precisely what a transactional write
	// just made authoritative. A tombstone would instead project an empty value
	// over it. Used only by the post-commit Tx sweep (cmd/gc beadPolicyStore.Tx).
	// Deleting a key that has no row is not an error.
	DeleteKeys(ctx context.Context, beadID string, keys []string) error
	// Get returns the snapshot for one bead. A bead with no rows yields a
	// zero Snapshot and a nil error.
	Get(ctx context.Context, beadID string) (Snapshot, error)
	// GetMany returns snapshots for the requested beads. Beads with no rows are
	// simply absent from the result map.
	GetMany(ctx context.Context, beadIDs []string) (map[string]Snapshot, error)
	// Now returns this store's best estimate of the SERVER's clock. Fallback
	// stamps are minted from it so a stamp written by a client whose clock
	// differs from the Dolt host's still compares correctly against the
	// server-minted written_at values it fences.
	Now() time.Time
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
	// clockOffsetNanos is (server clock - local clock), measured at open. Every
	// row's written_at is minted SERVER-side (UTC_TIMESTAMP(6)), so a fallback
	// stamp minted from an unadjusted local clock would compare against a
	// different timebase on any scope whose Dolt lives on another host — and a
	// client running behind the server would fail to fence exactly the stale
	// rows the stamp exists to fence. Calibrate turns local time into
	// server time so the two are comparable.
	clockOffsetNanos atomic.Int64
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

// Calibrate measures the server clock against the local one so Now() can report
// server time. It is best-effort: a failure leaves the offset at zero, which is
// exactly the local-clock behavior, and is correct for the overwhelmingly common
// case of a managed Dolt on the same host.
//
// The local reading BRACKETS the query. The server sampled its clock somewhere
// inside the round trip, so comparing it against a local reading taken only
// AFTER the Scan charges the whole round trip to the offset and reports the
// server as running behind by that much — a systematic backward bias, and the
// bias direction that makes the fence fail to fence. Midpoint is the standard
// correction and is exact when the two legs are symmetric.
func (s *SQLStore) Calibrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("liveness: store not open")
	}
	t0 := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx, "SELECT UTC_TIMESTAMP(6)")
	if err != nil {
		return fmt.Errorf("liveness: reading server clock: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("liveness: reading server clock: %w", err)
		}
		return fmt.Errorf("liveness: server clock returned no row")
	}
	var raw any
	if err := rows.Scan(&raw); err != nil {
		return fmt.Errorf("liveness: reading server clock: %w", err)
	}
	t1 := time.Now().UTC()
	server := parseWrittenAt(raw)
	if server.IsZero() {
		return fmt.Errorf("liveness: server clock unparseable")
	}
	local := t0.Add(t1.Sub(t0) / 2)
	s.clockOffsetNanos.Store(server.Sub(local).Nanoseconds())
	return nil
}

// Now returns local time shifted onto the server's clock.
func (s *SQLStore) Now() time.Time {
	if s == nil {
		return time.Now().UTC()
	}
	return time.Now().UTC().Add(time.Duration(s.clockOffsetNanos.Load()))
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
// It never calls DOLT_COMMIT. The table is dolt_ignore'd, and an ignored table's
// rows are not part of any commit's tree — so a plain SQL transaction here mints
// no Dolt commit EVEN THOUGH gc sets @@GLOBAL.dolt_transaction_commit = 1 on
// every managed server (cmd/gc/dolt_transaction_commit.go). Measured on a real
// dolt server: 10 SetBatch writes with the global ON minted 0 commits, while 10
// control writes to a non-ignored table on the same connection minted 10. That
// is the entire point of the change; a DOLT_COMMIT added here would silently
// restore the churn it removes.
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
	// written_at is minted SERVER-side. A client clock is not a safe timebase for
	// a value the fence compares against: two gc processes on different hosts
	// would otherwise write rows whose ordering does not reflect reality, and a
	// client running behind the Dolt host would defeat the fence entirely.
	const stmt = "INSERT INTO " + TableName + " (bead_id, k, v, written_at) VALUES (?, ?, ?, UTC_TIMESTAMP(6)) " +
		"ON DUPLICATE KEY UPDATE v = VALUES(v), written_at = UTC_TIMESTAMP(6)"
	for _, k := range keysToWrite {
		if _, err := tx.ExecContext(ctx, stmt, beadID, k, kv[k]); err != nil {
			return fmt.Errorf("liveness: upsert %s[%s]: %w", beadID, k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("liveness: commit: %w", err)
	}
	committed = true
	return nil
}

// DeleteKeys removes beadID's rows for keys in one statement.
//
// A real DELETE, not a tombstone: the caller is the post-commit Tx sweep, whose
// whole purpose is to make the overlay fall THROUGH to the committed metadata
// the transaction just wrote. Writing v=” instead would project an empty value
// over that metadata — the opposite of what the sweep is for.
func (s *SQLStore) DeleteKeys(ctx context.Context, beadID string, keys []string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("liveness: store not open")
	}
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return fmt.Errorf("liveness: empty bead id")
	}
	wanted := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			wanted = append(wanted, k)
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	// Same deterministic order as SetBatch, for the same reason: concurrent
	// writers touching overlapping keys take row locks in one order.
	sort.Strings(wanted)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(wanted)), ",")
	args := make([]any, 0, len(wanted)+1)
	args = append(args, beadID)
	for _, k := range wanted {
		args = append(args, k)
	}
	//nolint:gosec // G201: the only interpolation is a generated ?-placeholder list.
	stmt := "DELETE FROM " + TableName + " WHERE bead_id = ? AND k IN (" + placeholders + ")"
	if _, err := s.db.ExecContext(ctx, stmt, args...); err != nil {
		return fmt.Errorf("liveness: delete %s: %w", beadID, err)
	}
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
		// The k column is VARCHAR(64). Refuse an over-long key rather than let
		// the server truncate it: a truncated key would collide with whatever
		// shares its first 64 bytes and silently corrupt that bead's telemetry.
		if len(k) > maxKeyLen {
			return nil, fmt.Errorf("liveness: key %q is %d bytes; the column holds %d", k, len(k), maxKeyLen)
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
			snap = Snapshot{Values: make(map[string]string, 8), Times: make(map[string]time.Time, 8)}
		}
		snap.Values[k] = v
		snap.Times[k] = writtenAt
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

// IsConnectionError reports whether err is a transport-class failure — the
// connection is gone or was never established — as opposed to a statement the
// server rejected. Callers use it to decide whether to throw away a pooled
// handle and re-resolve the endpoint: a managed Dolt hard-kill/rebind moves the
// PORT, so database/sql's own reconnect (which re-dials the SAME address) can
// never recover it and the pool stays dead for the life of the process.
//
// Matching is by error identity where the driver offers one and by message
// shape otherwise; the driver does not export a single sentinel that covers
// every path (a dead server surfaces as driver.ErrBadConn, as
// mysql.ErrInvalidConn, or as a bare *net.OpError depending on when it dies).
// A false positive costs one re-resolve; a false negative costs the outage.
func IsConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, mysql.ErrInvalidConn) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"invalid connection",
		"bad connection",
		"broken pipe",
		"connection refused",
		"connection reset",
		"can't connect",
		"cannot connect",
		"no such host",
		"server has gone away",
		"use of closed network connection",
		"database is closed",
		"sql: database is closed",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
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
	// Clock, when set, supplies the write timestamp (tests pin it).
	Clock func() time.Time
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
	now := m.Now()
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

// DeleteKeys removes the named rows, mirroring the SQL store's genuine delete.
func (m *MemStore) DeleteKeys(_ context.Context, beadID string, keys []string) error {
	if strings.TrimSpace(beadID) == "" {
		return fmt.Errorf("liveness: empty bead id")
	}
	if len(keys) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	bead := m.rows[beadID]
	if bead == nil {
		return nil
	}
	for _, k := range keys {
		delete(bead, k)
	}
	if len(bead) == 0 {
		delete(m.rows, beadID)
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
	snap := Snapshot{
		Values: make(map[string]string, len(bead)),
		Times:  make(map[string]time.Time, len(bead)),
	}
	for k, row := range bead {
		snap.Values[k] = row.value
		snap.Times[k] = row.writtenAt
		if row.writtenAt.After(snap.WrittenAt) {
			snap.WrittenAt = row.writtenAt
		}
	}
	return snap
}

// Now reports the store's clock. An in-process store has no server to skew
// against, so this is simply the local clock unless a test pins Clock.
func (m *MemStore) Now() time.Time {
	if m != nil && m.Clock != nil {
		return m.Clock().UTC()
	}
	return time.Now().UTC()
}

// Close is a no-op.
func (m *MemStore) Close() error { return nil }

// Overlay merges a bead's liveness snapshot over a copy of its committed
// metadata and returns the merged map. Keys present in the snapshot WIN
// (including an empty-string clear); keys absent from it fall back to whatever
// the committed metadata holds — the natural carry-over that lets pre-existing
// session beads work with no migration step.
//
// THE FENCE. A row for key k only wins if it was written AFTER k's own fence
// marker (FenceKeyFor(k)). That marker is committed alongside any liveness value
// that had to go to versioned metadata — a degraded write, a transactional
// write, or every write in ModeMetadata — and it is what stops a pre-outage row
// from shadowing the post-outage committed value once the liveness pool
// recovers. Without it the overlay is unconditional across arbitrary time, and a
// recovered pool resurrects a stale instance_token / generation /
// pending_create_claim into the wake-fencing path.
//
// A row exactly AT its stamp is dropped — the stamp is minted at the moment the
// versioned write is composed, so anything not strictly newer lost the race to
// it. A key with NO marker is not fenced at all: nothing ever committed a newer
// value for it, so its row is the freshest thing anyone has, and fencing it
// would swap live telemetry for whatever ancient value the bead happens to
// carry.
//
// Because the markers are per key they also accumulate: the keys refreshed since
// a fallback keep winning while their pre-outage siblings are dropped, and a
// SECOND fallback over a different key set fences its own keys without
// un-fencing the first one's.
//
// When any row survives, WrittenAtKey is stamped with the surviving max so
// freshness consumers have a replacement for the now-quiet Bead.UpdatedAt.
// A snapshot with nothing to contribute returns meta unchanged (same map, not a
// copy) so the no-liveness case allocates nothing.
func Overlay(meta map[string]string, snap Snapshot) map[string]string {
	if len(snap.Values) == 0 {
		return meta
	}
	merged := make(map[string]string, len(meta)+len(snap.Values)+1)
	for k, v := range meta {
		merged[k] = v
	}
	newest := time.Time{}
	applied := false
	for k, v := range snap.Values {
		if fence := ParseFence(meta[FenceKeyFor(k)]); !fence.IsZero() {
			written, ok := snap.Times[k]
			// A row with no usable timestamp cannot prove it postdates the
			// fence, so a fenced key drops it. Fail closed: the committed
			// value is the one the fallback write just recorded.
			if !ok || written.IsZero() || !written.After(fence) {
				continue
			}
		}
		merged[k] = v
		applied = true
		if written, ok := snap.Times[k]; ok && written.After(newest) {
			newest = written
		}
	}
	if !applied {
		return meta
	}
	if newest.IsZero() {
		newest = snap.WrittenAt
	}
	if !newest.IsZero() {
		merged[WrittenAtKey] = newest.UTC().Format(StampFormat)
	}
	return merged
}
