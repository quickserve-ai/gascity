package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// indeterminateReadStore persists every Create but cannot complete the
// read-back of anything it created: the shape of a Dolt too slow to answer the
// verification lookup, where the note most likely DID land.
type indeterminateReadStore struct {
	beads.Store
	created map[string]bool
}

func (s *indeterminateReadStore) Create(b beads.Bead) (beads.Bead, error) {
	out, err := s.Store.Create(b)
	if err == nil {
		if s.created == nil {
			s.created = map[string]bool{}
		}
		s.created[out.ID] = true
	}
	return out, err
}

func (s *indeterminateReadStore) Get(id string) (beads.Bead, error) {
	if s.created[id] {
		return beads.Bead{}, beads.ErrVerifyIndeterminate
	}
	return s.Store.Get(id)
}

// ga-0ejdbv review finding 2: an UNCONFIRMED handoff note used to print
// "creating mail: ..." and abort like a plain failure, inviting a re-run that
// sends a second note. It must still stop before the restart, but name the ID
// to check and say what a re-run does.
func TestCreateHandoffMail_UnconfirmedNoteNamesTheIDAndDoesNotInviteABlindRetry(t *testing.T) {
	base := beads.NewMemStore()
	msgStore := &indeterminateReadStore{Store: base}
	var stderr bytes.Buffer
	_, ok := createHandoffMail(msgStore, base, events.Discard, "woodhouse", "woodhouse", []string{"HANDOFF", "note body"}, "HANDOFF", nil, &stderr)
	if ok {
		t.Fatalf("createHandoffMail succeeded on an unconfirmed note; the restart must not proceed")
	}
	var id string
	for created := range msgStore.created {
		id = created
	}
	if id == "" {
		t.Fatalf("no note bead was created")
	}
	out := stderr.String()
	for _, want := range []string{"UNCONFIRMED", id, "NOT restarted", "GC_NO_API=1 gc mail peek " + id, "second copy", `plain "not found"`, `"absence unproven" = the lookup did not finish`} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "creating mail:") {
		t.Errorf("stderr reports an unconfirmed note as a plain failure:\n%s", out)
	}
}

// ga-0ejdbv round 3, finding 2: the controller's config-drift handoff restarts
// the session whatever the note's fate, so it must not print the CLI's
// "NOT restarted ... re-run gc handoff" advice.
func TestConfigDriftHandoffUnconfirmedDoesNotClaimTheRestartStopped(t *testing.T) {
	base := beads.NewMemStore()
	msgStore := &indeterminateReadStore{Store: base}
	var stderr bytes.Buffer
	sendConfigDriftHandoffMailWithStores(msgStore, base, events.Discard, "woodhouse", "drift note", &stderr)
	out := stderr.String()
	if !strings.Contains(out, "config-drift handoff") || !strings.Contains(out, "UNCONFIRMED") || !strings.Contains(out, "GC_NO_API=1 gc mail peek ") {
		t.Fatalf("stderr does not report the unconfirmed drift note with its check:\n%s", out)
	}
	for _, bad := range []string{"NOT restarted", "re-run gc handoff", "gc handoff:"} {
		if strings.Contains(out, bad) {
			t.Fatalf("stderr carries CLI-only advice %q on the controller path:\n%s", bad, out)
		}
	}
}
