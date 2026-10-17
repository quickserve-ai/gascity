package beads

import (
	"strings"
	"testing"
)

// TestReconcileDigestSQLCarriesEveryJoinedTerm pins the digest's surface
// against the native List's projection: each table the List joins onto a row
// must have a term, or a write there goes unseen until the next full list.
// The terms' behaviour against a server-mode Dolt is the integration
// follow-up; the embedded store exports no SQL pool and answers unsupported.
func TestReconcileDigestSQLCarriesEveryJoinedTerm(t *testing.T) {
	for _, tier := range reconcileDigestTiers {
		sql := reconcileDigestSQL(tier)
		for _, want := range []string{
			"FROM " + tier.rows + " r",
			"r.is_blocked",
			"r.updated_at",
			"r.content_hash",
			"r.row_lock",
			"FROM " + tier.labels + " GROUP BY issue_id",
			"FROM " + tier.deps + " e",
			"ti.content_hash",
			"FROM dependencies",
			"FROM wisp_dependencies",
			"FROM leases",
			"FROM " + tier.comments + " GROUP BY issue_id",
			"WHERE r.status <> 'closed'",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("%s digest SQL lacks %q", tier.rows, want)
			}
		}
	}
}
