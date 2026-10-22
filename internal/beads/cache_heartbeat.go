package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// ReconcileHeartbeat is the DURABLE liveness record for one CachingStore's
// reconcile loop. It exists because the reconciler's only operator-visible
// signal was a log line ("beads cache: reconciled rig=..."), and nothing
// watches for the ABSENCE of a log line.
//
// Why absence matters: the reconciler is the only path by which a bead created
// through a different store instance or a different gc process enters the
// controller's cache, and CachingStore.List answers IncludeClosed=false purely
// from cache without ever touching the backing store. A reconciler that stops
// completing therefore makes the controller structurally blind to externally
// created beads in that scope, while every other health signal — including the
// X-GC-Cache-Age-S header, whose LastFreshAt is bumped by every local write —
// keeps reading healthy. On 2026-08-13 the city ("ga") cache stopped
// reconciling for 3h31m with no alarm; a sibling rig kept ticking the whole
// time.
//
// The record is written by the store itself, at arm time and after every
// COMPLETED reconcile, so its own staleness is the alarm: an armed reconciler
// that stops completing stops advancing LastReconcileAt while ArmedAt and PID
// still say "this scope is supposed to be reconciling".
type ReconcileHeartbeat struct {
	// Scope is the doctor-visible scope label: "city", or a rig name.
	Scope string `json:"scope"`
	// Prefix is the bead ID prefix the store owns ("ga", "qc"). It is the
	// same token the supervisor log prints as rig=<prefix>, so an operator
	// can grep the log for the scope this record names.
	Prefix string `json:"prefix,omitempty"`
	// PID is the process that armed the reconciler. A record whose PID is
	// no longer alive is a leftover from a previous supervisor and must be
	// treated as unknown, never as an alarm.
	PID int `json:"pid"`
	// ArmedAt is when StartReconciler ran for this store. Non-zero in every
	// well-formed record; it is the floor for the staleness clock so a
	// freshly armed store is never alarmed before its first cycle is due.
	ArmedAt time.Time `json:"armed_at"`
	// LastReconcileAt is when the most recent reconcile COMPLETED its merge.
	// Zero means the store has been armed but has not yet finished a cycle.
	LastReconcileAt time.Time `json:"last_reconcile_at"`
	// IntervalMs is the store's current adaptive reconcile cadence. The
	// staleness window is a multiple of this, so a LARGE-cadence store is
	// not judged against a SMALL-cadence store's expectations. Always
	// positive in a well-formed record; a non-positive value means unknown.
	IntervalMs int64 `json:"interval_ms"`
	// State mirrors CacheStats.State ("live", "degraded", ...).
	State string `json:"state,omitempty"`
	// UpdatedAt is when this record was written.
	UpdatedAt time.Time `json:"updated_at"`
}

// reconcileHeartbeatDirName is the runtime subdirectory holding one heartbeat
// file per reconciling store scope.
const reconcileHeartbeatDirName = "beads-cache"

// ReconcileHeartbeatDir returns the directory holding a city's per-scope
// reconcile heartbeat records.
func ReconcileHeartbeatDir(cityRoot string) string {
	return citylayout.RuntimePath(cityRoot, "runtime", reconcileHeartbeatDirName)
}

// ReconcileHeartbeatScopeFileName encodes a scope label as a single filename.
// Rig-qualified names are flattened with the same "/" -> "--" mapping the rest
// of the runtime tree uses, so a scope can never escape the heartbeat dir.
func ReconcileHeartbeatScopeFileName(scope string) string {
	safe := strings.ReplaceAll(strings.TrimSpace(scope), "/", "--")
	safe = strings.ReplaceAll(safe, string(filepath.Separator), "--")
	return safe + ".json"
}

// ReconcileHeartbeatPath returns the heartbeat file for one scope.
func ReconcileHeartbeatPath(cityRoot, scope string) string {
	return filepath.Join(ReconcileHeartbeatDir(cityRoot), ReconcileHeartbeatScopeFileName(scope))
}

// WriteReconcileHeartbeat publishes hb for its scope under cityRoot. The write
// is atomic (temp file + rename) so a reader never observes a half-written
// record. Callers treat failures as best-effort: a store must keep reconciling
// even when its liveness record cannot be published.
func WriteReconcileHeartbeat(cityRoot string, hb ReconcileHeartbeat) error {
	scope := strings.TrimSpace(hb.Scope)
	if strings.TrimSpace(cityRoot) == "" || scope == "" {
		return errors.New("write reconcile heartbeat: city root and scope are required")
	}
	dir := ReconcileHeartbeatDir(cityRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("write reconcile heartbeat: %w", err)
	}
	data, err := json.Marshal(hb)
	if err != nil {
		return fmt.Errorf("write reconcile heartbeat: %w", err)
	}
	final := filepath.Join(dir, ReconcileHeartbeatScopeFileName(scope))
	tmp, err := os.CreateTemp(dir, ".heartbeat-*.tmp")
	if err != nil {
		return fmt.Errorf("write reconcile heartbeat: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()        //nolint:errcheck // best-effort cleanup
		os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("write reconcile heartbeat: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("write reconcile heartbeat: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("write reconcile heartbeat: %w", err)
	}
	return nil
}

// ReadReconcileHeartbeat loads one scope's heartbeat record. The returned bool
// reports whether a record was found; a missing file is NOT an error, because
// "this scope publishes no heartbeat" is the normal state for every store that
// is not supposed to be reconciling.
func ReadReconcileHeartbeat(cityRoot, scope string) (ReconcileHeartbeat, bool, error) {
	var hb ReconcileHeartbeat
	data, err := os.ReadFile(ReconcileHeartbeatPath(cityRoot, scope))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return hb, false, nil
		}
		return hb, false, fmt.Errorf("read reconcile heartbeat %s: %w", scope, err)
	}
	if err := json.Unmarshal(data, &hb); err != nil {
		return ReconcileHeartbeat{}, false, fmt.Errorf("read reconcile heartbeat %s: %w", scope, err)
	}
	return hb, true, nil
}
