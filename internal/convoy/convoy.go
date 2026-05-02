package convoy

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// ConvoyDeps bundles dependencies for convoy operations.
type ConvoyDeps struct {
	Cfg       *config.City
	GetStore  func(rig string) (beads.Store, error)
	FindStore func(beadID string) (beads.Store, error)
	Recorder  events.Recorder
}

// ConvoyCreateInput holds the parameters for creating a convoy.
type ConvoyCreateInput struct {
	Title  string
	Items  []string
	Fields ConvoyFields
	Labels []string
}

// ConvoyCreateResult holds the result of creating a convoy.
type ConvoyCreateResult struct {
	Convoy      beads.Bead
	LinkedCount int
}

// ConvoyProgressResult holds the progress of a convoy.
type ConvoyProgressResult struct {
	ConvoyID string
	Total    int
	Closed   int
	Complete bool
}

// RejectNonLeafChild reports an error when bead is an epic or a container
// type, both of which break sling expansion if placed as a direct child of a
// convoy. The shared rule keeps Pattern A (convoy → epic → tasks) out of every
// code path that links items into a convoy: CLI ('gc convoy create',
// 'gc convoy add'), the HTTP API, and the convoy package itself. See gc-867q
// and the prep-convoy skill for the failure mode this prevents.
func RejectNonLeafChild(bead beads.Bead) error {
	if bead.Type == "epic" {
		return fmt.Errorf("bead %s is an epic; epics must wrap convoys, not the other way around (Pattern B). Create the convoy first, then run 'gc bd update <convoy-id> --parent %s' to put the convoy under the epic", bead.ID, bead.ID)
	}
	if beads.IsContainerType(bead.Type) {
		return fmt.Errorf("bead %s is a %s; convoys cannot contain other %s beads", bead.ID, bead.Type, bead.Type)
	}
	return nil
}

// ConvoyCreate creates a convoy bead, applies metadata, links child items,
// and emits a ConvoyCreated event.
func ConvoyCreate(deps ConvoyDeps, store beads.Store, input ConvoyCreateInput) (ConvoyCreateResult, error) {
	// Pre-validate every item's type before creating the convoy. Without
	// this, a single epic in the items list would leave a half-built
	// convoy bead behind once the linking loop hit the offending item.
	for _, itemID := range input.Items {
		item, err := store.Get(itemID)
		if err != nil {
			return ConvoyCreateResult{}, fmt.Errorf("getting item %s: %w", itemID, err)
		}
		if rejectErr := RejectNonLeafChild(item); rejectErr != nil {
			return ConvoyCreateResult{}, rejectErr
		}
	}

	b := beads.Bead{
		Title:  input.Title,
		Type:   "convoy",
		Labels: input.Labels,
	}
	ApplyConvoyFields(&b, input.Fields)

	convoy, err := store.Create(b)
	if err != nil {
		return ConvoyCreateResult{}, fmt.Errorf("creating convoy: %w", err)
	}

	linked := 0
	for _, itemID := range input.Items {
		pid := convoy.ID
		if err := store.Update(itemID, beads.UpdateOpts{ParentID: &pid}); err != nil {
			return ConvoyCreateResult{Convoy: convoy, LinkedCount: linked},
				fmt.Errorf("linking item %s: %w", itemID, err)
		}
		linked++
	}

	if deps.Recorder != nil {
		deps.Recorder.Record(events.Event{
			Type:    events.ConvoyCreated,
			Subject: convoy.ID,
		})
	}

	return ConvoyCreateResult{Convoy: convoy, LinkedCount: linked}, nil
}

// ConvoyProgress returns the completion progress of a convoy.
func ConvoyProgress(_ ConvoyDeps, store beads.Store, id string) (ConvoyProgressResult, error) {
	b, err := store.Get(id)
	if err != nil {
		return ConvoyProgressResult{}, fmt.Errorf("getting convoy %s: %w", id, err)
	}
	if b.Type != "convoy" {
		return ConvoyProgressResult{}, fmt.Errorf("bead %s is not a convoy (type: %s)", id, b.Type)
	}

	children, err := store.List(beads.ListQuery{
		ParentID:      id,
		IncludeClosed: true,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		return ConvoyProgressResult{}, fmt.Errorf("listing children of %s: %w", id, err)
	}

	total := len(children)
	closed := 0
	for _, c := range children {
		if c.Status == "closed" {
			closed++
		}
	}

	return ConvoyProgressResult{
		ConvoyID: id,
		Total:    total,
		Closed:   closed,
		Complete: total > 0 && closed == total,
	}, nil
}

// ConvoyAddItems links beads to an existing convoy.
func ConvoyAddItems(_ ConvoyDeps, store beads.Store, convoyID string, items []string) error {
	b, err := store.Get(convoyID)
	if err != nil {
		return fmt.Errorf("getting convoy %s: %w", convoyID, err)
	}
	if b.Type != "convoy" {
		return fmt.Errorf("bead %s is not a convoy (type: %s)", convoyID, b.Type)
	}

	// Pre-validate every item's type before mutating any state, mirroring
	// ConvoyCreate. A mid-loop rejection would otherwise leave earlier
	// items already reparented.
	for _, itemID := range items {
		item, err := store.Get(itemID)
		if err != nil {
			return fmt.Errorf("getting item %s: %w", itemID, err)
		}
		if rejectErr := RejectNonLeafChild(item); rejectErr != nil {
			return rejectErr
		}
	}

	for _, itemID := range items {
		pid := convoyID
		if err := store.Update(itemID, beads.UpdateOpts{ParentID: &pid}); err != nil {
			return fmt.Errorf("linking item %s to convoy %s: %w", itemID, convoyID, err)
		}
	}
	return nil
}

// ConvoyClose closes a convoy bead and emits a ConvoyClosed event.
func ConvoyClose(deps ConvoyDeps, store beads.Store, id string) error {
	if _, err := store.Get(id); err != nil {
		return fmt.Errorf("getting convoy %s: %w", id, err)
	}

	if err := store.Close(id); err != nil {
		return fmt.Errorf("closing convoy %s: %w", id, err)
	}

	if deps.Recorder != nil {
		deps.Recorder.Record(events.Event{
			Type:    events.ConvoyClosed,
			Subject: id,
		})
	}

	return nil
}
