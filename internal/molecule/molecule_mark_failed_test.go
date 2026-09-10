package molecule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// rootActivationFailStore makes the sequential graph-workflow path fail on the
// root's own activation and then refuses the root's failure stamp once, which is
// the correlated same-row failure seen in the field: the write that sent
// Instantiate into markFailed and markFailed's first write are on the same
// row, so a transient fault on it hits both. stampFailures > 1 keeps refusing
// the root's stamp for that many calls (use a large value for "always").
//
// The wrapper embeds the beads.Store interface, so no graph-apply handle is
// promoted from the memory store beneath it and Instantiate takes the
// sequential, fenced path whatever the process-wide flag says; the tests need
// no flag setup.
type rootActivationFailStore struct {
	beads.Store
	rootID           string
	activationFailed bool
	stampFailures    int
	stampRefused     int
	// stamps records the bead each SetMetadataBatch call targeted, in order.
	stamps []string
}

func (s *rootActivationFailStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(b)
	if err == nil && s.rootID == "" {
		s.rootID = created.ID
	}
	return created, err
}

func (s *rootActivationFailStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.rootID && !s.activationFailed {
		if v, ok := opts.Metadata[InstantiatingMetadataKey]; ok && v == "" {
			s.activationFailed = true
			return errors.New("injected activation failure on the root")
		}
	}
	return s.Store.Update(id, opts)
}

func (s *rootActivationFailStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.stamps = append(s.stamps, id)
	if id == s.rootID && s.stampRefused < s.stampFailures {
		s.stampRefused++
		return errors.New("injected stamp failure on the root")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func fencedWorkflowRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{
				ID:       "wf",
				Title:    "Workflow",
				Type:     "task",
				IsRoot:   true,
				Assignee: "controller",
				Metadata: map[string]string{
					"gc.kind":      "workflow",
					"gc.routed_to": "gascity/control-dispatcher",
				},
			},
			{
				ID:       "wf.body",
				Title:    "Body",
				Type:     "task",
				Assignee: "worker",
				Metadata: map[string]string{
					"gc.kind":      "scope",
					"gc.routed_to": "gascity/worker",
				},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.body", DependsOnID: "wf", Type: "parent-child"},
		},
	}
}

func assertMarkedFailedAndUnfenced(t *testing.T, base beads.Store, id string) {
	t.Helper()
	full, err := base.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if full.Metadata["molecule_failed"] != "true" {
		t.Errorf("bead %s molecule_failed = %q, want true", id, full.Metadata["molecule_failed"])
	}
	if full.Metadata[InstantiatingMetadataKey] != "" {
		t.Errorf("bead %s instantiating metadata = %q, want cleared (a fenced bead is never served)", id, full.Metadata[InstantiatingMetadataKey])
	}
}

// TestInstantiateFailureStampsTheRootWhoseActivationFailed pins that a mint
// which fails on the root's own activation still marks the root molecule_failed
// and drops its fence when the stamp's first attempt fails the way the
// activation did. Before the second pass, that one transient refusal left
// exactly the root fenced and unmarked while every other member was stamped —
// the 7-of-8 and 6-of-7 shapes in the field — and the fence hid it from
// dispatch, voiding and reporting alike.
func TestInstantiateFailureStampsTheRootWhoseActivationFailed(t *testing.T) {
	base := beads.NewMemStore()
	store := &rootActivationFailStore{Store: base, stampFailures: 1}

	_, instErr := Instantiate(context.Background(), store, fencedWorkflowRecipe(), Options{})
	if instErr == nil {
		t.Fatal("expected the injected activation failure")
	}
	if !store.activationFailed {
		t.Fatal("the root's activation was never attempted; the interleaving was not exercised")
	}
	if store.stampRefused != 1 {
		t.Fatalf("root stamps refused = %d, want 1", store.stampRefused)
	}
	if len(store.stamps) != 3 || store.stamps[0] != store.rootID || store.stamps[1] == store.rootID || store.stamps[2] != store.rootID {
		t.Fatalf("stamp sequence = %v, want [root, member, root]: the refused root stamp is retried after every other bead, not on the spot", store.stamps)
	}

	all, err := base.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("open beads = %d, want 2", len(all))
	}
	assertMarkedFailedAndUnfenced(t, base, store.rootID)
	for _, b := range all {
		assertMarkedFailedAndUnfenced(t, base, b.ID)
	}
	if strings.Contains(instErr.Error(), "not confirmed") {
		t.Fatalf("error reports an unconfirmed stamp although every stamp landed: %v", instErr)
	}
}

// TestInstantiateFailureNamesTheBeadsItCouldNotMark pins the loud half: when a
// bead's stamp keeps failing, the returned error names the bead and the fault,
// so the caller's log says which bead may be left fenced instead of hiding it
// behind the original failure.
func TestInstantiateFailureNamesTheBeadsItCouldNotMark(t *testing.T) {
	base := beads.NewMemStore()
	store := &rootActivationFailStore{Store: base, stampFailures: 1 << 20}

	_, instErr := Instantiate(context.Background(), store, fencedWorkflowRecipe(), Options{})
	if instErr == nil {
		t.Fatal("expected the injected activation failure")
	}
	if !strings.Contains(instErr.Error(), "activating graph step") {
		t.Fatalf("error lost the original failure: %v", instErr)
	}
	if !strings.Contains(instErr.Error(), "failure stamp not confirmed") || !strings.Contains(instErr.Error(), store.rootID) || !strings.Contains(instErr.Error(), "injected stamp failure") {
		t.Fatalf("error = %v, want it to name the unconfirmed stamp's bead (%s) and its fault", instErr, store.rootID)
	}

	all, err := base.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	for _, b := range all {
		if b.ID == store.rootID {
			continue
		}
		assertMarkedFailedAndUnfenced(t, base, b.ID)
	}
}

// stampFailStore refuses SetMetadataBatch a configured number of times per id.
type stampFailStore struct {
	beads.Store
	failures map[string]int
	refused  map[string]int
	// calls records the bead each SetMetadataBatch call targeted, in order.
	calls []string
}

func (s *stampFailStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.calls = append(s.calls, id)
	if s.refused == nil {
		s.refused = map[string]int{}
	}
	if s.refused[id] < s.failures[id] {
		s.refused[id]++
		return fmt.Errorf("injected stamp failure on %s", id)
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// TestMarkFailedReportingRetriesOnceAndJoinsEveryFailure pins the reporting
// variant: a stamp that fails once is retried after the others and not
// reported; every stamp that still fails is reported, not only the first.
func TestMarkFailedReportingRetriesOnceAndJoinsEveryFailure(t *testing.T) {
	base := beads.NewMemStore()
	var ids []string
	for _, title := range []string{"once", "always-1", "always-2"} {
		b, err := base.Create(beads.Bead{Title: title})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, b.ID)
	}
	store := &stampFailStore{Store: base, failures: map[string]int{
		ids[0]: 1,
		ids[1]: 1 << 20,
		ids[2]: 1 << 20,
	}}

	err := markFailedReporting(store, ids)
	if err == nil {
		t.Fatal("expected the two permanent stamp failures to be reported")
	}
	for _, id := range ids[1:] {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error = %v, want it to name %s", err, id)
		}
	}
	if strings.Contains(err.Error(), ids[0]) {
		t.Errorf("error = %v, names %s although its retry landed", err, ids[0])
	}
	assertMarkedFailedAndUnfenced(t, base, ids[0])
	if store.refused[ids[0]] != 1 || store.refused[ids[1]] != 2 || store.refused[ids[2]] != 2 {
		t.Fatalf("refusals = %v, want the failed stamp retried exactly once after the others", store.refused)
	}
	want := []string{ids[0], ids[1], ids[2], ids[0], ids[1], ids[2]}
	if strings.Join(store.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("stamp sequence = %v, want %v: every bead once, then the failed ones again in order", store.calls, want)
	}
}
