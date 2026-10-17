package beads

import "errors"

// The cache's per-row reconcile digest (ga-ckbnpj): what a backing answers so
// a reconcile can list only the rows that moved since its last pass. The
// native Dolt store implements it (native_dolt_store_reconcile_digest.go) and
// ProxiedStore forwards its read leaf. Nothing reads it yet: the delta
// reconcile that does arrives with the rest of its stack.

// errReconcileDigestUnsupported is a backing's "I cannot answer a digest",
// which sends the pass down the full list without recording a problem.
var errReconcileDigestUnsupported = errors.New("backing cannot answer a reconcile digest")

// cacheReconcileDigestStore is the optional backing capability the delta
// reconcile needs. The digest must change whenever any field the full-scan
// List returns for a row changes, and must list exactly the non-closed rows.
type cacheReconcileDigestStore interface {
	reconcileDigestForCache() (map[string]string, error)
}
