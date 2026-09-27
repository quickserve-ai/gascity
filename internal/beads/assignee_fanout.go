package beads

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// A plural-Assignees ListQuery ("assigned to any of these routes") is the
// shape of every mail inbox read: one seat resolves to several route
// spellings (short, pack-qualified, city-qualified). The bd CLI and the
// native beads IssueFilter both take exactly ONE assignee predicate, and bd's
// query DSL evaluates `assignee=a OR assignee=b` as an in-memory predicate over
// every row the other clauses select. So a plural query cannot be one
// predicated read on those backings. Answering it as one UNpredicated read
// hydrated every open message in the store to return one seat's handful
// (pl-adj: 12,798 rows for 13 messages, p90 24 s on the hub).
//
// These backings instead answer it as one predicated read per route, merged
// and deduplicated. A bead has one assignee, so the per-route reads partition
// the answer; merging them in the backing's own default order reproduces the
// order a single read would have returned.

// assigneeFanOutRoutes returns the distinct, trimmed routes of a plural
// Assignees query that a single-assignee backing must read one at a time, or
// nil when the query is not plural (no Assignees, a singular Assignee, or one
// route, which the ordinary path already predicates). The result can hold one
// route or none after blanks and duplicates compact away; a query whose routes
// are all blank matches nothing.
func assigneeFanOutRoutes(query ListQuery) []string {
	if query.Assignee != "" || len(query.Assignees) < 2 {
		return nil
	}
	return compactStrings(query.Assignees)
}

// listPerAssignee answers a plural-Assignees query with one read per route,
// each carrying that route as its singular Assignee and every other filter of
// query unchanged.
//
// The reads run one after another, in the caller's goroutine. A seat has a
// handful of route spellings and each read is small and predicated, so the
// sum stays small; and the bd trace attributes a read to its caller by
// walking the calling goroutine's stack (captureBDTraceCallers), which a read
// on a fresh goroutine would lose.
//
// Each read keeps query.Limit only under SortDefault, the one order the
// backings cut their limit in. Under any other sort a backing's per-route
// top-N need not hold the merged top-N (the backing limits in priority order,
// not created order), so those reads come back whole — they are predicated,
// so whole is one route's rows — and the merge cuts.
//
// Results are deduplicated by ID, ordered by query.Sort (the backing's own
// default order, backingDefaultLess, when unsorted), and cut to query.Limit.
// A route whose read fails is never dropped silently: rows that did arrive
// come back under a *PartialResultError naming every failure, or the joined
// failures alone when nothing arrived.
func listPerAssignee(op string, query ListQuery, routes []string, read func(ListQuery) ([]Bead, error)) ([]Bead, error) {
	results := make([][]Bead, len(routes))
	errs := make([]error, len(routes))
	for i, route := range routes {
		routeQuery := query
		routeQuery.Assignees = nil
		routeQuery.Assignee = route
		if query.Sort != SortDefault {
			routeQuery.Limit = 0
		}
		results[i], errs[i] = read(routeQuery)
	}

	merged := make([]Bead, 0)
	seen := make(map[string]bool)
	var failures []error
	for i, rows := range results {
		if errs[i] != nil {
			failures = append(failures, fmt.Errorf("assignee %q: %w", routes[i], errs[i]))
		}
		for _, b := range rows {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			merged = append(merged, b)
		}
	}
	if query.Sort == SortDefault {
		sort.SliceStable(merged, func(i, j int) bool { return backingDefaultLess(merged[i], merged[j]) })
	} else {
		sortBeadsForQuery(merged, query.Sort)
	}
	if query.Limit > 0 && len(merged) > query.Limit {
		merged = merged[:query.Limit]
	}
	if len(failures) == 0 {
		return merged, nil
	}
	err := errors.Join(failures...)
	if len(merged) == 0 {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return merged, &PartialResultError{Op: op, Err: err}
}

// backingDefaultLess is the Go mirror of the order bd list, bd query and the
// native beads search return rows in when no sort is requested: priority ASC
// (an unset priority reads as the backing's default, 2), then created_at DESC,
// then id ASC. Merging per-route reads with it reproduces the order of one
// read over all routes.
func backingDefaultLess(a, b Bead) bool {
	pa, pb := readySortPriority(a), readySortPriority(b)
	if pa != pb {
		return pa < pb
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID < b.ID
}

// compactStrings returns values trimmed, with blanks and repeats removed,
// first occurrence first.
func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
