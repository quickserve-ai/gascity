package beads

import (
	"encoding/json"
	"testing"
)

// Reordered label/needs/dependency sets must NOT read as a change: the Dolt gcg
// rig store does not guarantee a stable element order across scans, so an
// order-sensitive comparison re-fired bead.updated every reconcile pass and
// flooded cache-reconcile (ga-ocypq2).
func TestBeadChangedIgnoresSetOrder(t *testing.T) {
	base := Bead{
		ID:     "gcg-wisp-x",
		Title:  "t",
		Status: "open",
		Type:   "task",
		Labels: []string{"a", "b", "c"},
		Needs:  []string{"n1", "n2"},
		Dependencies: []Dep{
			{IssueID: "x", DependsOnID: "d1", Type: "blocks"},
			{IssueID: "x", DependsOnID: "d2", Type: "tracks"},
		},
	}
	reordered := base
	reordered.Labels = []string{"c", "a", "b"}
	reordered.Needs = []string{"n2", "n1"}
	reordered.Dependencies = []Dep{
		{IssueID: "x", DependsOnID: "d2", Type: "tracks"},
		{IssueID: "x", DependsOnID: "d1", Type: "blocks"},
	}
	if beadChanged(base, reordered, false) {
		t.Error("beadChanged = true for a pure label/needs/dep reorder; want false")
	}
	if depsChanged(base.Dependencies, reordered.Dependencies) {
		t.Error("depsChanged = true for a pure dependency reorder; want false")
	}

	// A real content change must still be detected.
	labelAdded := base
	labelAdded.Labels = []string{"a", "b", "c", "d"}
	if !beadChanged(base, labelAdded, false) {
		t.Error("beadChanged = false when a label was added; want true")
	}
	depChanged := base
	depChanged.Dependencies = []Dep{
		{IssueID: "x", DependsOnID: "d1", Type: "blocks"},
		{IssueID: "x", DependsOnID: "d3", Type: "tracks"}, // d3 != d2
	}
	if !depsChanged(base.Dependencies, depChanged.Dependencies) {
		t.Error("depsChanged = false when a dependency target changed; want true")
	}
}

func TestCacheEventConflictsCurrentIgnoresLabelOrder(t *testing.T) {
	current := Bead{
		ID:     "gcg-wisp-x",
		Title:  "t",
		Status: "open",
		Type:   "task",
		Labels: []string{"a", "b", "c"},
	}
	patch := current
	patch.Labels = []string{"c", "a", "b"}

	fields := map[string]json.RawMessage{
		"labels": json.RawMessage(`["c","a","b"]`),
	}

	if cacheEventConflictsCurrent(current, patch, fields) {
		t.Fatal("cacheEventConflictsCurrent = true for a pure label reorder; want false")
	}
}

func TestStringSetEqual(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{}, nil, true},
		{[]string{"a", "b"}, []string{"b", "a"}, true},
		{[]string{"a", "a", "b"}, []string{"a", "b", "a"}, true},
		{[]string{"a", "a"}, []string{"a", "b"}, false}, // multiset, not just set
		{[]string{"a"}, []string{"a", "b"}, false},
	}
	for _, c := range cases {
		if got := stringSetEqual(c.a, c.b); got != c.want {
			t.Errorf("stringSetEqual(%v,%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestDepSetEqual(t *testing.T) {
	d1 := Dep{IssueID: "x", DependsOnID: "d1", Type: "blocks"}
	d2 := Dep{IssueID: "x", DependsOnID: "d2", Type: "tracks"}
	if !depSetEqual([]Dep{d1, d2}, []Dep{d2, d1}) {
		t.Error("depSetEqual = false for reordered equal sets; want true")
	}
	if depSetEqual([]Dep{d1, d1}, []Dep{d1, d2}) {
		t.Error("depSetEqual = true for different multisets; want false")
	}
}

// ga-knhu61: a bead.updated event that changes await_type must both flag the
// stale cache row (conflict) and land on the merged copy — the re-typing
// operation (`bd update --await-type`) is exactly this event.
func TestCacheEventCarriesAwaitType(t *testing.T) {
	current := Bead{ID: "gc-gate", Title: "g", Status: "open", Type: "gate", AwaitType: AwaitHuman}
	patch := current
	patch.AwaitType = AwaitBead
	fields := map[string]json.RawMessage{"await_type": json.RawMessage(`"bead"`)}

	if !cacheEventConflictsCurrent(current, patch, fields) {
		t.Fatal("cacheEventConflictsCurrent = false for an await_type change; want true")
	}
	merged := mergeCacheEventPatch(current, patch, fields)
	if merged.AwaitType != AwaitBead {
		t.Fatalf("merged AwaitType = %q, want %q", merged.AwaitType, AwaitBead)
	}
	if merged := mergeCacheEventPatch(current, patch, map[string]json.RawMessage{"title": json.RawMessage(`"g"`)}); merged.AwaitType != AwaitHuman {
		t.Fatalf("merge without await_type field mutated AwaitType to %q; want untouched %q", merged.AwaitType, AwaitHuman)
	}
}
