package beads

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	beadslib "github.com/steveyegge/beads"
)

var _ cacheReconcileDigestStore = (*NativeDoltStore)(nil)

// reconcileDigestForCache answers, for every non-closed row in both tiers, a
// string that changes whenever any field the cache's full-scan List would
// return for that row changes (ga-ckbnpj). The cache compares it with the
// previous pass's and re-reads only the rows whose digest moved.
//
// The row's own columns: content_hash (bd's hash over title, text fields,
// status, assignee, metadata and the other content columns), row_lock, and
// updated_at. updated_at alone is not enough: it is whole seconds, so two
// writes in one second share it, and the blocked-state writes pin it
// (issueops.markBlockedTemplate sets updated_at = updated_at), so is_blocked
// is carried explicitly too. The native List also joins five other tables onto
// each row, and a write to any of them leaves updated_at alone, so each gets a
// term of its own:
//
//   - labels: labels_json
//   - the row's own edges: dep_count, parent_id, deps_json, and each edge
//     target's status, content_hash and updated_at, closed targets included:
//     readiness reads a closed blocker's work outcome (its metadata), and a
//     closed row is in no digest of its own
//   - other rows' edges onto it, both tiers: rdep_count
//   - leases: lease_expires_at, heartbeat_at, granted_node
//   - comments: comment_count
//
// A set term is COUNT plus the BIT_XOR of each member's first 64 bits of SHA1.
// Not CRC32: CRC32 is affine, so over an even number of equal-length members
// its constant cancels, and a swap such as labels {aa,bb} -> {ab,ba} leaves
// BIT_XOR(CRC32) unchanged (2988704377 for both on Dolt, 2026-10-19). Its
// residual blind spot (a member swap whose 64-bit prefixes collide) is what the
// cache's periodic full scan is for.
//
// A storage that exports no SQL pool answers errReconcileDigestUnsupported,
// never an empty digest: an empty one would read as "every row vanished".
func (s *NativeDoltStore) reconcileDigestForCache() (map[string]string, error) {
	digest := make(map[string]string)
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		accessor, ok := storage.(interface{ UnderlyingDB() *sql.DB })
		if !ok {
			return fmt.Errorf("%w: storage %T exports no UnderlyingDB", errReconcileDigestUnsupported, storage)
		}
		db := accessor.UnderlyingDB()
		clear(digest)
		for _, tier := range reconcileDigestTiers {
			if err := readReconcileDigestTier(ctx, db, tier, digest); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return digest, nil
}

type reconcileDigestTier struct{ rows, labels, deps, comments string }

var reconcileDigestTiers = []reconcileDigestTier{
	{rows: "issues", labels: "labels", deps: "dependencies", comments: "comments"},
	{rows: "wisps", labels: "wisp_labels", deps: "wisp_dependencies", comments: "wisp_comments"},
}

const reconcileDigestTarget = "COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external)"

//nolint:gosec // G201: every table name comes from reconcileDigestTiers.
func reconcileDigestSQL(t reconcileDigestTier) string {
	return fmt.Sprintf(`
		SELECT r.id, CONCAT_WS('|', r.status, CAST(r.updated_at AS CHAR), CAST(r.is_blocked AS CHAR),
			COALESCE(r.content_hash, '-'), CAST(r.row_lock AS CHAR),
			COALESCE(l.h, '-'), COALESCE(d.h, '-'), COALESCE(rd.h, '-'), COALESCE(le.h, '-'), COALESCE(cm.h, '-'))
		FROM %[1]s r
		LEFT JOIN (SELECT issue_id, CONCAT(COUNT(*), ':', BIT_XOR(CAST(CONV(LEFT(SHA1(label), 16), 16, 10) AS UNSIGNED))) AS h
			FROM %[2]s GROUP BY issue_id) l ON l.issue_id = r.id
		LEFT JOIN (SELECT e.issue_id, CONCAT(COUNT(*), ':', BIT_XOR(CAST(CONV(LEFT(SHA1(CONCAT_WS('|',
				COALESCE(e.depends_on_issue_id, e.depends_on_wisp_id, e.depends_on_external), e.type,
				COALESCE(CAST(e.metadata AS CHAR), ''), COALESCE(e.thread_id, ''),
				COALESCE(ti.status, tw.status, ''), COALESCE(ti.content_hash, tw.content_hash, ''),
				COALESCE(CAST(ti.updated_at AS CHAR), CAST(tw.updated_at AS CHAR), ''))), 16), 16, 10) AS UNSIGNED))) AS h
			FROM %[3]s e
			LEFT JOIN issues ti ON ti.id = e.depends_on_issue_id
			LEFT JOIN wisps tw ON tw.id = e.depends_on_wisp_id
			GROUP BY e.issue_id) d ON d.issue_id = r.id
		LEFT JOIN (SELECT dep_id, CONCAT(COUNT(*), ':', BIT_XOR(CAST(CONV(LEFT(SHA1(CONCAT_WS('|', issue_id, type)), 16), 16, 10) AS UNSIGNED))) AS h
			FROM (SELECT %[5]s AS dep_id, issue_id, type FROM dependencies
				UNION ALL SELECT %[5]s AS dep_id, issue_id, type FROM wisp_dependencies) x
			GROUP BY dep_id) rd ON rd.dep_id = r.id
		LEFT JOIN (SELECT issue_id, CONCAT_WS('|', COALESCE(holder, ''), CAST(granted_at AS CHAR),
				CAST(lease_expires_at AS CHAR), CAST(heartbeat_at AS CHAR), COALESCE(granted_node, '')) AS h
			FROM leases) le ON le.issue_id = r.id
		LEFT JOIN (SELECT issue_id, CAST(COUNT(*) AS CHAR) AS h FROM %[4]s GROUP BY issue_id) cm ON cm.issue_id = r.id
		WHERE r.status <> 'closed'`, t.rows, t.labels, t.deps, t.comments, reconcileDigestTarget)
}

func readReconcileDigestTier(ctx context.Context, db *sql.DB, t reconcileDigestTier, into map[string]string) error {
	rows, err := db.QueryContext(ctx, reconcileDigestSQL(t))
	if err != nil {
		return fmt.Errorf("reconcile digest %s: %w", t.rows, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err below reports the scan
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			return fmt.Errorf("reconcile digest %s: %w", t.rows, err)
		}
		into[strings.TrimSpace(id)] = t.rows + "|" + h
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reconcile digest %s: %w", t.rows, err)
	}
	return nil
}
