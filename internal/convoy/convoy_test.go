package convoy

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func testConvoyDeps(store beads.Store) ConvoyDeps {
	return ConvoyDeps{
		Cfg: &config.City{},
		GetStore: func(_ string) (beads.Store, error) {
			return store, nil
		},
		FindStore: func(_ string) (beads.Store, error) {
			return store, nil
		},
		Recorder: events.NewFake(),
	}
}

func TestConvoyCreateOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	result, err := ConvoyCreate(deps, store, ConvoyCreateInput{
		Title: "my convoy",
		Fields: ConvoyFields{
			Owner:  "mayor",
			Target: "main",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Convoy.Title != "my convoy" {
		t.Errorf("title = %q, want %q", result.Convoy.Title, "my convoy")
	}
	if result.Convoy.Type != "convoy" {
		t.Errorf("type = %q, want convoy", result.Convoy.Type)
	}
	// Verify metadata was applied.
	got, err := store.Get(result.Convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["convoy.owner"] != "mayor" {
		t.Errorf("owner = %q, want mayor", got.Metadata["convoy.owner"])
	}

	// Verify event was emitted.
	fake := deps.Recorder.(*events.Fake)
	if len(fake.Events) == 0 {
		t.Error("expected ConvoyCreated event to be emitted")
	}
}

func TestConvoyCreateWithItemsOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	// Create child beads first.
	b1, _ := store.Create(beads.Bead{Title: "task 1"})
	b2, _ := store.Create(beads.Bead{Title: "task 2"})

	result, err := ConvoyCreate(deps, store, ConvoyCreateInput{
		Title: "linked convoy",
		Items: []string{b1.ID, b2.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LinkedCount != 2 {
		t.Errorf("linked = %d, want 2", result.LinkedCount)
	}

	// Verify children are linked.
	child1, _ := store.Get(b1.ID)
	if child1.ParentID != result.Convoy.ID {
		t.Errorf("child1 parent = %q, want %q", child1.ParentID, result.Convoy.ID)
	}
}

func TestConvoyProgressOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	b1, _ := store.Create(beads.Bead{Title: "task 1", ParentID: convoy.ID})
	if _, err := store.Create(beads.Bead{Title: "task 2", ParentID: convoy.ID}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b1.ID); err != nil {
		t.Fatal(err)
	}

	progress, err := ConvoyProgress(deps, store, convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Total != 2 {
		t.Errorf("total = %d, want 2", progress.Total)
	}
	if progress.Closed != 1 {
		t.Errorf("closed = %d, want 1", progress.Closed)
	}
	if progress.Complete {
		t.Error("expected not complete")
	}
}

func TestConvoyProgressCompleteOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	b1, _ := store.Create(beads.Bead{Title: "task 1", ParentID: convoy.ID})
	if err := store.Close(b1.ID); err != nil {
		t.Fatal(err)
	}

	progress, err := ConvoyProgress(deps, store, convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !progress.Complete {
		t.Error("expected complete")
	}
}

func TestConvoyAddItemsOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})
	b1, _ := store.Create(beads.Bead{Title: "task 1"})

	err := ConvoyAddItems(deps, store, convoy.ID, []string{b1.ID})
	if err != nil {
		t.Fatal(err)
	}

	child, _ := store.Get(b1.ID)
	if child.ParentID != convoy.ID {
		t.Errorf("parent = %q, want %q", child.ParentID, convoy.ID)
	}
}

// TestRejectNonLeafChildAcceptsLeafTypes asserts that ordinary work bead types
// (task, bug, chore, etc.) are accepted as direct children of a convoy.
func TestRejectNonLeafChildAcceptsLeafTypes(t *testing.T) {
	for _, typ := range []string{"task", "bug", "chore", "feature", ""} {
		t.Run("type="+typ, func(t *testing.T) {
			if err := RejectNonLeafChild(beads.Bead{ID: "gc-1", Type: typ}); err != nil {
				t.Errorf("RejectNonLeafChild(type=%q) = %v, want nil", typ, err)
			}
		})
	}
}

// TestRejectNonLeafChildRejectsEpic asserts that an epic cannot be a direct
// child of a convoy. The error must mention 'epic' and 'Pattern B' so the
// user is pointed to the correct fix. See gc-867q.
func TestRejectNonLeafChildRejectsEpic(t *testing.T) {
	err := RejectNonLeafChild(beads.Bead{ID: "qc-pwnu", Type: "epic"})
	if err == nil {
		t.Fatal("RejectNonLeafChild(epic) = nil, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "epic") {
		t.Errorf("err = %q, want mention of 'epic'", msg)
	}
	if !strings.Contains(msg, "Pattern B") {
		t.Errorf("err = %q, want guidance referencing 'Pattern B'", msg)
	}
	if !strings.Contains(msg, "qc-pwnu") {
		t.Errorf("err = %q, want bead ID", msg)
	}
}

// TestRejectNonLeafChildRejectsConvoy asserts that a convoy cannot be nested
// inside another convoy.
func TestRejectNonLeafChildRejectsConvoy(t *testing.T) {
	err := RejectNonLeafChild(beads.Bead{ID: "qc-rz96", Type: "convoy"})
	if err == nil {
		t.Fatal("RejectNonLeafChild(convoy) = nil, want error")
	}
	if !strings.Contains(err.Error(), "convoy") {
		t.Errorf("err = %q, want mention of 'convoy'", err.Error())
	}
}

// TestConvoyCreateRejectsEpicItem asserts that ConvoyCreate refuses an epic in
// the items list and does NOT create a convoy bead. Without this guard,
// 'gc convoy create <name> <epic-id>' would leave a half-built convoy that
// later breaks sling expansion. See gc-867q.
func TestConvoyCreateRejectsEpicItem(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)
	epic, _ := store.Create(beads.Bead{Title: "narrative", Type: "epic"})

	result, err := ConvoyCreate(deps, store, ConvoyCreateInput{
		Title: "deploy",
		Items: []string{epic.ID},
	})
	if err == nil {
		t.Fatal("ConvoyCreate with epic item = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "Pattern B") {
		t.Errorf("err = %q, want guidance referencing 'Pattern B'", err.Error())
	}
	if result.Convoy.ID != "" {
		t.Errorf("convoy was created (ID=%q) despite epic rejection — want no convoy bead", result.Convoy.ID)
	}

	// Epic must not have been reparented.
	got, _ := store.Get(epic.ID)
	if got.ParentID != "" {
		t.Errorf("epic ParentID = %q, want empty", got.ParentID)
	}
}

// TestConvoyAddItemsRejectsEpicItem asserts that ConvoyAddItems refuses an epic
// in the items list and does NOT reparent any earlier-listed item. See gc-867q.
func TestConvoyAddItemsRejectsEpicItem(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)
	convoy, _ := store.Create(beads.Bead{Title: "deploy", Type: "convoy"})
	task, _ := store.Create(beads.Bead{Title: "real task"})
	epic, _ := store.Create(beads.Bead{Title: "narrative", Type: "epic"})

	err := ConvoyAddItems(deps, store, convoy.ID, []string{task.ID, epic.ID})
	if err == nil {
		t.Fatal("ConvoyAddItems with epic = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "Pattern B") {
		t.Errorf("err = %q, want guidance referencing 'Pattern B'", err.Error())
	}

	// Task must not have been reparented (validation is all-or-nothing).
	gotTask, _ := store.Get(task.ID)
	if gotTask.ParentID != "" {
		t.Errorf("task ParentID = %q, want empty (no partial application)", gotTask.ParentID)
	}
	gotEpic, _ := store.Get(epic.ID)
	if gotEpic.ParentID != "" {
		t.Errorf("epic ParentID = %q, want empty", gotEpic.ParentID)
	}
}

func TestConvoyCloseOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	convoy, _ := store.Create(beads.Bead{Title: "test", Type: "convoy"})

	err := ConvoyClose(deps, store, convoy.ID)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := store.Get(convoy.ID)
	if got.Status != "closed" {
		t.Errorf("status = %q, want closed", got.Status)
	}

	// Verify event was emitted.
	fake := deps.Recorder.(*events.Fake)
	found := false
	for _, e := range fake.Events {
		if e.Type == events.ConvoyClosed {
			found = true
		}
	}
	if !found {
		t.Error("expected ConvoyClosed event to be emitted")
	}
}

func TestConvoyCloseNotFoundOps(t *testing.T) {
	store := beads.NewMemStore()
	deps := testConvoyDeps(store)

	err := ConvoyClose(deps, store, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent convoy")
	}
}
