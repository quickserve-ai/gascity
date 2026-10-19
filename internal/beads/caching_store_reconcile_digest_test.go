package beads

import (
	"crypto/sha1" //nolint:gosec // G505: mirrors the digest's SQL SHA1 fold; not a security use
	"encoding/binary"
	"hash/crc32"
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
			// each joined term must reach the returned digest, not just be read
			"COALESCE(l.h, '-')",
			"COALESCE(d.h, '-')",
			"COALESCE(rd.h, '-')",
			"COALESCE(le.h, '-')",
			"COALESCE(cm.h, '-')",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("%s digest SQL lacks %q", tier.rows, want)
			}
		}
	}
}

// TestReconcileDigestSetTermsFoldSHA1NotCRC32 pins the set terms' member fold.
// BIT_XOR(CRC32(m)) is blind to swaps: CRC32 is affine, so over an even number
// of equal-length members its constant cancels.
func TestReconcileDigestSetTermsFoldSHA1NotCRC32(t *testing.T) {
	for _, tier := range reconcileDigestTiers {
		sql := reconcileDigestSQL(tier)
		if strings.Contains(sql, "CRC32(") {
			t.Errorf("%s digest SQL folds a set term with CRC32", tier.rows)
		}
		if got := strings.Count(sql, "BIT_XOR(CAST(CONV(LEFT(SHA1("); got != 3 {
			t.Errorf("%s digest SQL has %d SHA1 set folds, want 3 (labels, deps, rdeps)", tier.rows, got)
		}
	}
	crcFold := func(ms ...string) (x uint32) {
		for _, m := range ms {
			x ^= crc32.ChecksumIEEE([]byte(m))
		}
		return x
	}
	shaFold := func(ms ...string) (x uint64) {
		for _, m := range ms {
			sum := sha1.Sum([]byte(m)) //nolint:gosec // G401: mirrors the SQL fold
			x ^= binary.BigEndian.Uint64(sum[:8])
		}
		return x
	}
	if crcFold("aa", "bb") != crcFold("ab", "ba") {
		t.Fatal("control: CRC32 no longer cancels on {aa,bb} vs {ab,ba}; the reason for SHA1 is stale")
	}
	if shaFold("aa", "bb") == shaFold("ab", "ba") {
		t.Error("SHA1 fold does not tell {aa,bb} from {ab,ba}")
	}
}
