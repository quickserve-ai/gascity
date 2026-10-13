package beads_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// fanoutRow is one bead the fake bd below can return.
type fanoutRow struct {
	ID        string
	Assignee  string
	Priority  int
	Created   string
	Ephemeral bool
}

// predicatedBd is a fake bd that answers the way a real bd with a server-side
// assignee predicate does: `bd list --assignee=X` and a `bd query` whose
// clauses include `assignee=X` (bare or quoted) return only X's rows, in bd's
// default (priority ASC, created_at DESC, id ASC) order. A read that carries no
// assignee predicate returns every row of its tier: that is the scan the
// multi-route tests forbid, and it is recorded so they can say so.
type predicatedBd struct {
	t    *testing.T
	rows []fanoutRow
	fail map[string]error

	mu    sync.Mutex
	calls []string
}

func (f *predicatedBd) runner(_, name string, args ...string) ([]byte, error) {
	full := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, full)
	f.mu.Unlock()

	var (
		assignee  string
		predicate bool
		ephemeral bool
	)
	switch {
	case len(args) > 0 && args[0] == "list":
		for _, arg := range args {
			if v, ok := strings.CutPrefix(arg, "--assignee="); ok {
				assignee, predicate = v, true
			}
		}
	case len(args) > 2 && args[0] == "query":
		ephemeral = true
		for _, clause := range strings.Split(args[2], " AND ") {
			if v, ok := strings.CutPrefix(clause, "assignee="); ok {
				assignee, predicate = unquoteBdQueryValue(f.t, v), true
			}
		}
	default:
		return nil, fmt.Errorf("unexpected command: %s", full)
	}
	if err := f.fail[assignee]; err != nil && predicate {
		return nil, err
	}

	var matched []fanoutRow
	for _, row := range f.rows {
		if row.Ephemeral != ephemeral {
			continue
		}
		if predicate && row.Assignee != assignee {
			continue
		}
		matched = append(matched, row)
	}
	slices.SortStableFunc(matched, func(a, b fanoutRow) int {
		if a.Priority != b.Priority {
			return a.Priority - b.Priority
		}
		if a.Created != b.Created {
			return strings.Compare(b.Created, a.Created)
		}
		return strings.Compare(a.ID, b.ID)
	})
	out := make([]map[string]any, 0, len(matched))
	for _, row := range matched {
		out = append(out, map[string]any{
			"id":         row.ID,
			"title":      "message",
			"status":     "open",
			"issue_type": "message",
			"assignee":   row.Assignee,
			"priority":   row.Priority,
			"created_at": row.Created,
			"ephemeral":  row.Ephemeral,
		})
	}
	return json.Marshal(out)
}

func (f *predicatedBd) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// unquoteBdQueryValue decodes a bd query DSL value the way the bd lexer does:
// a bare token as-is, or a double-quoted string with \" and \\ escapes.
func unquoteBdQueryValue(t *testing.T, v string) string {
	t.Helper()
	if !strings.HasPrefix(v, `"`) {
		return v
	}
	if len(v) < 2 || !strings.HasSuffix(v, `"`) {
		t.Fatalf("unterminated quoted bd query value %q", v)
	}
	var sb strings.Builder
	body := v[1 : len(v)-1]
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' && i+1 < len(body) {
			i++
		}
		sb.WriteByte(body[i])
	}
	return sb.String()
}

func fanoutIDs(got []beads.Bead) []string {
	ids := make([]string, 0, len(got))
	for _, b := range got {
		ids = append(ids, b.ID)
	}
	return ids
}

// A quote or backslash inside a value is escaped the way the bd lexer reads
// it, so the predicate names exactly the value asked for.
func TestBdStoreListWispsEscapesQuotedQueryValue(t *testing.T) {
	const odd = `ops/"night" \shift`
	fake := &predicatedBd{t: t, rows: []fanoutRow{
		{ID: "w1", Assignee: odd, Priority: 2, Created: "2026-09-25T08:00:00Z", Ephemeral: true},
		{ID: "w2", Assignee: "someone-else", Priority: 2, Created: "2026-09-25T09:00:00Z", Ephemeral: true},
	}}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(beads.ListQuery{Assignee: odd, TierMode: beads.TierWisps})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	query := firstCommandWithPrefix(fake.snapshot(), "bd query ")
	if want := `assignee="ops/\"night\" \\shift"`; !strings.Contains(query, want) {
		t.Fatalf("query cmd = %q, want escaped predicate %s", query, want)
	}
	if want := []string{"w1"}; !slices.Equal(fanoutIDs(got), want) {
		t.Fatalf("List ids = %v, want %v", fanoutIDs(got), want)
	}
}

// bd reads the value none/null as "no assignee", so such a value cannot be a
// predicate for a route literally named that; it stays a client-side filter.
func TestBdStoreListWispsKeepsNoneValueClientSide(t *testing.T) {
	fake := &predicatedBd{t: t}
	s := beads.NewBdStore("/city", fake.runner)

	if _, err := s.List(beads.ListQuery{Assignee: "None", TierMode: beads.TierWisps}); err != nil {
		t.Fatalf("List: %v", err)
	}
	query := firstCommandWithPrefix(fake.snapshot(), "bd query ")
	if strings.Contains(strings.ToLower(query), "assignee=") {
		t.Fatalf("query cmd = %q, a none/null value must not become a bd predicate", query)
	}
}
