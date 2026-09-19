package runtime

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryRatioBearingKindHasAProducer is the guard for the CONFIDENT ZERO.
//
// The Codex review of PR #106 found that KindHandoff, KindDrainHandoff and
// KindCityStop were declared, documented, bucketed and tested — and constructed
// by NO production code at all. The handoff ratio would therefore have read 0%
// forever, on a table that looked fully populated, and nothing already in the
// suite could see it: every record present, every kind valid, the unclassified
// budget untouched, the fence at zero. A kind nobody writes is invisible to
// every check that inspects what was written.
//
// So this test does not inspect behavior. It greps the tree for a construction
// of each kind outside tests and this package's own declarations. That is a
// crude instrument and deliberately so: the property it defends is "somebody,
// somewhere, can produce this bucket", and the cheapest honest way to check that
// is to look.
func TestEveryRatioBearingKindHasAProducer(t *testing.T) {
	root := repoRootFromRuntimePackage(t)
	// KindUnclassified is produced by a fallback rather than by a named caller,
	// and KindObservedDead/KindInterruptRestart are out of the ratio — but they
	// are checked too, because a bucket that cannot be produced is a lie
	// wherever it is reported.
	for _, kind := range TerminationKinds() {
		t.Run(string(kind), func(t *testing.T) {
			n := countProductionConstructions(t, root, kind)
			if pendingProducers[kind] {
				// An expected-absent kind must be absent. If it acquires a
				// producer, this test FAILS and the exemption has to be removed
				// deliberately — otherwise a pre-registered gap quietly becomes
				// permanent furniture.
				if n != 0 {
					t.Errorf("%s now HAS a producer (%d sites) — remove it from pendingProducers and say which slice shipped it", kindConst(kind), n)
				}
				return
			}
			if n == 0 {
				t.Errorf("no production code constructs %s — the bucket exists, is documented and is "+
					"bucketed, and will read ZERO forever on a table that looks healthy", kindConst(kind))
			}
		})
	}
}

// pendingProducers are the kinds whose producer arrives with a LATER slice. It
// is a pre-registered list, not an amnesty: each entry names the slice, and the
// test fails in BOTH directions so the list cannot rot in either.
//
// KindDrainHandoff — "the controller asked and the seat handed off in time" —
// cannot exist before S3 (consent), because nothing asks yet. That is the same
// fact drain-silent records from the other side, and when S3 ships the two move
// together: drain-silent toward zero, drain-handoff away from it.
var pendingProducers = map[TerminationKind]bool{
	KindDrainHandoff: true,
}

// kindConst maps a kind's wire value back to its Go identifier, which is what a
// producer search has to look for.
func kindConst(k TerminationKind) string {
	var b strings.Builder
	b.WriteString("Kind")
	for _, part := range strings.Split(string(k), "-") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

func countProductionConstructions(t *testing.T, root string, kind TerminationKind) int {
	t.Helper()
	// Match `runtime.KindX` or a bare `KindX` used as a value, not the const
	// declaration itself (which is `KindX TerminationKind = "..."`).
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(kindConst(kind)) + `([^A-Za-z0-9_]|$)`)
	decl := regexp.MustCompile(regexp.QuoteMeta(kindConst(kind)) + `\s+TerminationKind\s*=`)
	count := 0
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil //nolint:nilerr // an unreadable subtree is not this test's business
			}
			rel, _ := filepath.Rel(root, path)
			// The declarations and the classification helpers live here; they
			// are not producers.
			if rel == filepath.Join("internal", "runtime", "termination.go") ||
				rel == filepath.Join("internal", "runtime", "termination_fallback.go") {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil //nolint:nilerr
			}
			for _, line := range strings.Split(string(body), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") || decl.MatchString(trimmed) {
					continue
				}
				if re.MatchString(trimmed) {
					count++
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	return count
}

// repoRootFromRuntimePackage walks up to the module root, and FAILS rather than
// guessing: a producer search that silently scanned nothing would report every
// kind as missing, or — worse, after someone "fixed" that — as present.
func repoRootFromRuntimePackage(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			if _, statErr := os.Stat(filepath.Join(dir, "cmd", "gc")); statErr == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find the repo root from the runtime package; a producer search over nothing proves nothing")
	return ""
}

// TestTheProducerSearchIsNotBlind is the known-positive control. A search that
// can only ever answer "found" is as useless as one that always answers
// "missing", and this test's whole value is the negative it can report.
func TestTheProducerSearchIsNotBlind(t *testing.T) {
	root := repoRootFromRuntimePackage(t)
	if n := countProductionConstructions(t, root, TerminationKind("definitely-not-a-kind")); n != 0 {
		t.Errorf("the search found %d producers of a kind that does not exist", n)
	}
	if n := countProductionConstructions(t, root, KindOperatorKill); n == 0 {
		t.Error("the search found no producer of operator-kill, which is definitely produced — it is scanning nothing")
	}
}
