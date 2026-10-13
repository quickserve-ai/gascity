package beads_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// mailShapedQuery is the query beadmail's messageCandidatesAll issues for an
// inbox read: open messages across both storage tiers, for every route
// spelling of one seat.
func mailShapedQuery(routes ...string) beads.ListQuery {
	return beads.ListQuery{
		Type:      "message",
		Status:    "open",
		TierMode:  beads.TierBoth,
		Live:      true,
		Assignees: routes,
	}
}

// A multi-route read reaches bd once per route with that route as the
// predicate, on BOTH tiers, and never as an unpredicated scan. This is the
// inbox read of a seat with three route spellings (pl-adj: 12,798 rows
// hydrated to return 13 messages on the hub).
func TestBdStoreListAssigneesMultipleReadsEachRoutePredicated(t *testing.T) {
	fake := &predicatedBd{t: t, rows: []fanoutRow{
		{ID: "msg-a", Assignee: "platform/gastown.rictus", Priority: 2, Created: "2026-09-25T10:00:00Z", Ephemeral: true},
		{ID: "msg-b", Assignee: "gastown.rictus", Priority: 2, Created: "2026-09-25T11:00:00Z", Ephemeral: true},
		{ID: "msg-c", Assignee: "rictus", Priority: 2, Created: "2026-09-25T09:00:00Z"},
		{ID: "msg-other", Assignee: "platform/gastown.nux", Priority: 2, Created: "2026-09-25T12:00:00Z", Ephemeral: true},
		{ID: "msg-other-durable", Assignee: "mayor", Priority: 2, Created: "2026-09-25T12:00:00Z"},
	}}
	routes := []string{"platform/gastown.rictus", "gastown.rictus", "rictus"}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(mailShapedQuery(routes...))
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	calls := fake.snapshot()
	var lists, queries []string
	for _, call := range calls {
		switch {
		case strings.HasPrefix(call, "bd list "):
			lists = append(lists, call)
			if !strings.Contains(call, "--assignee=") {
				t.Errorf("bd list without an assignee predicate (a scan): %s", call)
			}
		case strings.HasPrefix(call, "bd query "):
			queries = append(queries, call)
			if !strings.Contains(call, "assignee=") {
				t.Errorf("bd query without an assignee predicate (a scan): %s", call)
			}
		}
	}
	if len(lists) != len(routes) || len(queries) != len(routes) {
		t.Fatalf("got %d bd list and %d bd query calls, want %d of each (one per route per tier): %v",
			len(lists), len(queries), len(routes), calls)
	}
	for _, route := range routes {
		if firstCommandContaining(lists, "--assignee="+route+" ") == "" {
			t.Errorf("no bd list predicated on route %q: %v", route, lists)
		}
	}
	if firstCommandContaining(queries, `assignee="platform/gastown.rictus"`) == "" {
		t.Errorf("slash route must reach bd query as a quoted predicate: %v", queries)
	}

	// Durable leg first, then the wisp leg in bd's default order (created DESC
	// within one priority) — the same order one unpredicated read produced.
	if want := []string{"msg-c", "msg-b", "msg-a"}; !slices.Equal(fanoutIDs(got), want) {
		t.Fatalf("List ids = %v, want %v", fanoutIDs(got), want)
	}
}

// Merging per-route reads reproduces bd's single-read default order across
// routes: priority ASC, then created_at DESC, then id ASC.
func TestBdStoreListAssigneesMultipleKeepsBdDefaultOrderAndLimit(t *testing.T) {
	fake := &predicatedBd{t: t, rows: []fanoutRow{
		{ID: "r1-old", Assignee: "route-1", Priority: 2, Created: "2026-09-25T08:00:00Z"},
		{ID: "r2-new", Assignee: "route-2", Priority: 2, Created: "2026-09-25T10:00:00Z"},
		{ID: "r2-urgent", Assignee: "route-2", Priority: 0, Created: "2026-09-25T07:00:00Z"},
		{ID: "r1-tie-b", Assignee: "route-1", Priority: 2, Created: "2026-09-25T09:00:00Z"},
		{ID: "r2-tie-a", Assignee: "route-2", Priority: 2, Created: "2026-09-25T09:00:00Z"},
	}}
	s := beads.NewBdStore("/city", fake.runner)

	all, err := s.List(beads.ListQuery{Type: "message", Assignees: []string{"route-1", "route-2"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"r2-urgent", "r2-new", "r1-tie-b", "r2-tie-a", "r1-old"}
	if !slices.Equal(fanoutIDs(all), want) {
		t.Fatalf("List ids = %v, want bd default order %v", fanoutIDs(all), want)
	}

	top, err := s.List(beads.ListQuery{Type: "message", Assignees: []string{"route-1", "route-2"}, Limit: 3})
	if err != nil {
		t.Fatalf("List limit 3: %v", err)
	}
	if !slices.Equal(fanoutIDs(top), want[:3]) {
		t.Fatalf("List limit 3 ids = %v, want %v", fanoutIDs(top), want[:3])
	}
}

// Repeated or blank route spellings read each distinct route once.
func TestBdStoreListAssigneesDuplicateRoutesReadOnce(t *testing.T) {
	fake := &predicatedBd{t: t, rows: []fanoutRow{
		{ID: "a1", Assignee: "route-a", Priority: 2, Created: "2026-09-25T08:00:00Z"},
		{ID: "b1", Assignee: "route-b", Priority: 2, Created: "2026-09-25T09:00:00Z"},
	}}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(beads.ListQuery{Type: "message", Assignees: []string{"route-a", " route-a ", "", "route-b"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if calls := fake.snapshot(); len(calls) != 2 {
		t.Fatalf("got %d bd calls, want 2 (one per distinct route): %v", len(calls), calls)
	}
	if want := []string{"b1", "a1"}; !slices.Equal(fanoutIDs(got), want) {
		t.Fatalf("List ids = %v, want %v", fanoutIDs(got), want)
	}
}

// A route whose read fails must not vanish from the answer silently: the
// caller gets an error, never a shorter inbox presented as complete.
func TestBdStoreListAssigneesRouteFailureIsReported(t *testing.T) {
	fake := &predicatedBd{
		t: t,
		rows: []fanoutRow{
			{ID: "a1", Assignee: "route-a", Priority: 2, Created: "2026-09-25T08:00:00Z"},
			{ID: "b1", Assignee: "route-b", Priority: 2, Created: "2026-09-25T09:00:00Z"},
		},
		fail: map[string]error{"route-b": errors.New("dolt: connection refused")},
	}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(beads.ListQuery{Type: "message", Assignees: []string{"route-a", "route-b"}})
	if err == nil {
		t.Fatalf("List = %v, nil error; want the route-b failure reported", fanoutIDs(got))
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("List error = %v, want the route-b cause", err)
	}
	var partial *beads.PartialResultError
	if !errors.As(err, &partial) {
		t.Fatalf("List error = %T %v, want *PartialResultError carrying the rows that did arrive", err, err)
	}
	if want := []string{"a1"}; !slices.Equal(fanoutIDs(got), want) {
		t.Fatalf("partial ids = %v, want %v", fanoutIDs(got), want)
	}
}

// Control: a route-less read (the "all mail" listing) is still one scan.
func TestBdStoreListWithoutAssigneesStillScans(t *testing.T) {
	fake := &predicatedBd{t: t, rows: []fanoutRow{
		{ID: "a1", Assignee: "route-a", Priority: 2, Created: "2026-09-25T08:00:00Z"},
		{ID: "b1", Assignee: "route-b", Priority: 2, Created: "2026-09-25T09:00:00Z"},
	}}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(beads.ListQuery{Type: "message", AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	calls := fake.snapshot()
	if len(calls) != 1 || strings.Contains(calls[0], "--assignee=") {
		t.Fatalf("calls = %v, want one unpredicated bd list", calls)
	}
	if len(got) != 2 {
		t.Fatalf("List ids = %v, want both rows", fanoutIDs(got))
	}
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

// Every bd read a list issues records how many rows bd returned and how many
// the query kept, in the GC_BD_TRACE_JSON trace, so the cost of an inbox read
// is measured rather than assumed.
func TestBdStoreListTracesRowsPerRead(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace.jsonl")
	t.Setenv("GC_BD_TRACE_JSON", trace)
	fake := &predicatedBd{t: t, rows: []fanoutRow{
		{ID: "a1", Assignee: "route-a", Priority: 2, Created: "2026-09-25T08:00:00Z", Ephemeral: true},
		{ID: "a2", Assignee: "route-a", Priority: 2, Created: "2026-09-25T08:30:00Z", Ephemeral: true},
		{ID: "b1", Assignee: "route-b", Priority: 2, Created: "2026-09-25T09:00:00Z", Ephemeral: true},
	}}
	s := beads.NewBdStore("/city", fake.runner)

	if _, err := s.List(mailShapedQuery("route-a", "route-b")); err != nil {
		t.Fatalf("List: %v", err)
	}

	f, err := os.Open(trace)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer f.Close() //nolint:errcheck // test cleanup
	type rowsRecord struct {
		Source string   `json:"source"`
		Args   []string `json:"args"`
		Rows   *int     `json:"rows"`
		Kept   *int     `json:"kept"`
	}
	var records []rowsRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec rowsRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("trace line %q: %v", sc.Text(), err)
		}
		if rec.Source == beads.TraceSourceBDListRows {
			records = append(records, rec)
		}
	}
	if len(records) != 4 {
		t.Fatalf("got %d %s records, want 4 (two tiers x two routes): %+v", len(records), beads.TraceSourceBDListRows, records)
	}
	total := 0
	for _, rec := range records {
		if rec.Rows == nil || rec.Kept == nil {
			t.Fatalf("record %+v lacks rows/kept", rec)
		}
		if len(rec.Args) == 0 {
			t.Fatalf("record %+v lacks the bd args that name the read", rec)
		}
		total += *rec.Rows
	}
	if total != 3 {
		t.Fatalf("rows returned across reads = %d, want 3 (each route's own messages, nothing else)", total)
	}
}

func firstCommandContaining(calls []string, needle string) string {
	for _, call := range calls {
		if strings.Contains(call+" ", needle) {
			return call
		}
	}
	return ""
}
