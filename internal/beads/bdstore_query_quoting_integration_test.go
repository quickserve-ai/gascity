//go:build integration

package beads_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The wisp-leg bd query quotes a non-bare filter value (appendBdQueryClause).
// Every other test of that quoting runs a fake bd, so this one runs the real
// bd on PATH (the pinned beads v1.3.1, whose lexer reads quoted strings): a
// route and a label containing a slash must reach bd as predicates bd reads,
// so the rows trace shows bd returning exactly the rows List keeps. A value
// bd misread would fail the query or come back unpredicated, and rows would
// exceed kept.
func TestBdStoreListWispsQuotedPredicatesAgainstRealBd(t *testing.T) {
	store, _ := newConditionalIntegrationBdStore(t)
	for _, b := range []beads.Bead{
		{Title: "a labeled 1", Type: "task", Ephemeral: true, Assignee: "rig/a", Labels: []string{"team/ops"}},
		{Title: "a labeled 2", Type: "task", Ephemeral: true, Assignee: "rig/a", Labels: []string{"team/ops"}},
		{Title: "a unlabeled", Type: "task", Ephemeral: true, Assignee: "rig/a"},
		{Title: "b labeled", Type: "task", Ephemeral: true, Assignee: "rig/b", Labels: []string{"team/ops"}},
	} {
		if _, err := store.Create(b); err != nil {
			t.Fatalf("Create %q: %v", b.Title, err)
		}
	}
	trace := filepath.Join(t.TempDir(), "trace.jsonl")
	t.Setenv("GC_BD_TRACE_JSON", trace)

	got, err := store.List(beads.ListQuery{Assignee: "rig/a", Label: "team/ops", TierMode: beads.TierWisps})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List = %d rows %v, want the two rig/a rows labeled team/ops", len(got), got)
	}
	var query *listRowsRecord
	for _, rec := range readListRowsRecords(t, trace, beads.TraceSourceBDListRows) {
		if rec.Args[0] == "query" {
			query = &rec
		}
	}
	if query == nil {
		t.Fatal("no rows record for the bd query read")
	}
	clauses := strings.Join(query.Args, " ")
	for _, want := range []string{`assignee="rig/a"`, `label="team/ops"`} {
		if !strings.Contains(clauses, want) {
			t.Fatalf("bd query args = %q, want predicate %s", clauses, want)
		}
	}
	if *query.Rows != *query.Kept || *query.Kept != len(got) {
		t.Fatalf("bd query rows/decoded/kept = %d/%d/%d, List kept %d: bd did not apply the quoted predicates",
			*query.Rows, *query.Decoded, *query.Kept, len(got))
	}
}
