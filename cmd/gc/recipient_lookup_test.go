package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

// ga-y0hv2w: a configured named seat resolves without listing every open
// session bead (slow under host load); other recipients still list, and the
// answers are unchanged.

// sessionListingCounter counts the whole-table session listing: both of its
// reads, by session type and by the session label, with no metadata filter.
// Update and the metadata setters fail and are counted.
type sessionListingCounter struct {
	beads.Store
	sessionListCalls int
	writes           int
}

func (s *sessionListingCounter) List(query beads.ListQuery) ([]beads.Bead, error) {
	if len(query.Metadata) == 0 && (query.Label == session.LabelSession || query.Type == session.BeadType) {
		s.sessionListCalls++
	}
	return s.Store.List(query)
}

func (s *sessionListingCounter) refuse() error {
	s.writes++
	return errors.New("recipient resolution must not write")
}

func (s *sessionListingCounter) Update(string, beads.UpdateOpts) error            { return s.refuse() }
func (s *sessionListingCounter) SetMetadata(string, string, string) error         { return s.refuse() }
func (s *sessionListingCounter) SetMetadataBatch(string, map[string]string) error { return s.refuse() }

// oneSeatCity is a city with one configured named seat of that name.
func oneSeatCity(name string) *config.City {
	return &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: name, StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Template: name, Mode: "always"}},
	}
}

func newCountedSessionStore(t *testing.T) (beads.Store, *sessionListingCounter) {
	t.Helper()
	t.Setenv("GC_SESSION", "fake")
	base := beads.NewMemStore()
	return base, &sessionListingCounter{Store: base}
}

// seatBead creates an open session bead for a configured identity; edit, if
// set, changes it before it is created.
func seatBead(t *testing.T, store beads.Store, identity string, edit func(*beads.Bead)) beads.Bead {
	t.Helper()
	b := beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":                     identity,
			"session_name":              strings.ReplaceAll(identity, "/", "--"),
			"template":                  targetBasename(identity),
			"configured_named_session":  "true",
			"configured_named_identity": identity,
			"configured_named_mode":     "always",
		},
	}
	if edit != nil {
		edit(&b)
	}
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create(%s): %v", identity, err)
	}
	return created
}

func resolveRecipient(t *testing.T, cfg *config.City, store beads.Store, name string) (string, error) {
	t.Helper()
	return resolveMailRecipientIdentityCached(t.TempDir(), cfg, store, name, &mailIdentitySessionCache{})
}

func TestResolveMailRecipientIdentity_ConfiguredSeatSkipsTheSessionListing(t *testing.T) {
	base, store := newCountedSessionStore(t)
	seatBead(t, base, "mayor", nil)

	if address, err := resolveRecipient(t, oneSeatCity("mayor"), store, "mayor"); err != nil || address != "mayor" {
		t.Fatalf("resolve(mayor) = %q, %v; want mayor", address, err)
	}
	if store.sessionListCalls != 0 || store.writes != 0 {
		t.Fatalf("listing calls = %d, writes = %d; want 0 and 0", store.sessionListCalls, store.writes)
	}
}

func TestResolveMailRecipientIdentity_UnconfiguredNameStillListsSessions(t *testing.T) {
	base, store := newCountedSessionStore(t)
	seatBead(t, base, "frontend/rig-worker", nil)

	address, err := resolveRecipient(t, &config.City{Workspace: config.Workspace{Name: "test-city"}}, store, "rig-worker")
	if err != nil || address != "frontend/rig-worker" {
		t.Fatalf("resolve(rig-worker) = %q, %v; want frontend/rig-worker", address, err)
	}
	if store.sessionListCalls == 0 {
		t.Fatalf("broad session List calls = 0, want the listing for a name that is not configured")
	}
}

// A city seat and a rig seat share the leaf "barry". From outside the rig the
// config resolves "barry" to the city seat alone, but the listing matches
// leaves across rigs and refuses; the refusal is kept.
func TestResolveMailRecipientIdentity_SharedLeafStillRefusesOutsideTheRig(t *testing.T) {
	base, store := newCountedSessionStore(t)
	t.Setenv("GC_DIR", t.TempDir())
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "qcore", Path: t.TempDir()}},
		Agents: []config.Agent{
			{Name: "barry", StartCommand: "true"},
			{Name: "barry", Dir: "qcore", StartCommand: "true"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "barry", Mode: "always"},
			{Template: "barry", Dir: "qcore", Mode: "always"},
		},
	}
	seatBead(t, base, "barry", nil)
	seatBead(t, base, "qcore/barry", nil)
	if got := currentRigContext(cfg); got != "" {
		t.Fatalf("currentRigContext = %q, want none (the test must run outside the rig)", got)
	}

	if _, err := resolveRecipient(t, cfg, store, "barry"); !errors.Is(err, session.ErrAmbiguous) {
		t.Fatalf("resolve(barry) = %v, want ErrAmbiguous", err)
	}
	if store.sessionListCalls == 0 {
		t.Fatalf("broad session List calls = 0, want the listing (a shared leaf keeps it)")
	}
}

// The wake resolves from the recipient string the send stored; the configured
// seat's string is the listing's, and the wake callback receives it.
func TestMailSendNotifyPassesTheConfiguredSeatToTheWake(t *testing.T) {
	base, store := newCountedSessionStore(t)
	seatBead(t, base, "mayor", nil)

	canonical, err := resolveRecipient(t, oneSeatCity("mayor"), store, "mayor")
	if err != nil {
		t.Fatalf("resolve(mayor): %v", err)
	}
	var nudged string
	nf := func(recipient, _ string) error {
		nudged = recipient
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := doMailSend(beadmail.New(beads.NewMemStore()), events.Discard, map[string]bool{"human": true, canonical: true},
		"human", []string{canonical, "wake up"}, nf, &stdout, &stderr)
	if code != 0 || nudged != "mayor" || store.sessionListCalls != 0 {
		t.Fatalf("doMailSend = %d (stderr %q), nudged %q, listing calls %d; want 0, mayor, 0", code, stderr.String(), nudged, store.sessionListCalls)
	}
}

// Each case is a shape the listing answered; the fast path answers the same
// (or, for a wisp duplicate the old listing could miss, refuses), skips the
// listing where it answers (fast), and writes nothing.
func TestResolveMailRecipientIdentity_KeepsTheListingsAnswers(t *testing.T) {
	inProgress, empty := "in_progress", ""
	wisp := func(b *beads.Bead) { b.Ephemeral = true; b.Metadata["alias"] = "backup" }
	for _, tc := range []struct {
		name, seat string
		setup      func(t *testing.T, base beads.Store) []string // returns ids the error must name
		want       string
		wantErr    error
		fast       bool
	}{
		{name: "duplicates with different addresses refuse (ruling, case B)", seat: "mayor", wantErr: session.ErrAmbiguous,
			setup: func(t *testing.T, base beads.Store) []string {
				a := seatBead(t, base, "mayor", nil)
				b := seatBead(t, base, "mayor", func(b *beads.Bead) { delete(b.Metadata, "alias"); b.Metadata["session_name"] = "mayor-2" })
				return []string{a.ID, b.ID}
			}},
		{name: "a duplicate in the wisp tier refuses on an unwrapped store", seat: "mayor", wantErr: session.ErrAmbiguous,
			setup: func(t *testing.T, base beads.Store) []string {
				return []string{seatBead(t, base, "mayor", nil).ID, seatBead(t, base, "mayor", wisp).ID}
			}},
		{name: "an in_progress bead does not make the seat ambiguous", seat: "mayor", want: "mayor", fast: true,
			setup: func(t *testing.T, base beads.Store) []string {
				seatBead(t, base, "mayor", nil)
				busy := seatBead(t, base, "mayor", func(b *beads.Bead) { b.Metadata["alias"] = "backup" })
				if err := base.Update(busy.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
					t.Fatalf("Update: %v", err)
				}
				return nil
			}},
		{name: "an alias-only bead is left to the existing path", seat: "controller", wantErr: session.ErrSessionNotFound,
			setup: func(t *testing.T, base beads.Store) []string {
				seatBead(t, base, "controller", func(b *beads.Bead) {
					b.Metadata = map[string]string{"alias": "controller", "template": "controller"}
				})
				return nil
			}},
		{name: "a lone wisp the default read misses does not answer", seat: "controller", wantErr: session.ErrSessionNotFound,
			setup: func(t *testing.T, base beads.Store) []string {
				seatBead(t, base, "controller", func(b *beads.Bead) { b.Ephemeral = true })
				return nil
			}},
		{name: "an empty-type bead resolves without a repair", seat: "mayor", want: "mayor", fast: true,
			setup: func(t *testing.T, base beads.Store) []string {
				b := seatBead(t, base, "mayor", nil)
				if err := base.Update(b.ID, beads.UpdateOpts{Type: &empty}); err != nil {
					t.Fatalf("Update(type): %v", err)
				}
				if got, err := base.Get(b.ID); err != nil || got.Type != "" {
					t.Fatalf("precondition: type = %q, %v; want empty", got.Type, err)
				}
				return nil
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, store := newCountedSessionStore(t)
			ids := tc.setup(t, base)
			address, err := resolveRecipient(t, oneSeatCity(tc.seat), store, tc.seat)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("resolve(%s) = %q, %v; want %v", tc.seat, address, err, tc.wantErr)
				}
				for _, id := range ids {
					if !strings.Contains(err.Error(), id) {
						t.Fatalf("error = %q, want it to name bead %s", err, id)
					}
				}
			} else if err != nil || address != tc.want {
				t.Fatalf("resolve(%s) = %q, %v; want %q", tc.seat, address, err, tc.want)
			}
			if store.writes != 0 || (tc.fast && store.sessionListCalls != 0) {
				t.Fatalf("writes = %d, listing calls = %d; want no writes and, on the fast path, no listing", store.writes, store.sessionListCalls)
			}
		})
	}
}

// scriptedIdentityReads answers the fast path's two identity reads from a
// script, so a bead can appear between them.
type scriptedIdentityReads struct {
	beads.Store
	reads [][]beads.Bead
}

func (s *scriptedIdentityReads) List(query beads.ListQuery) ([]beads.Bead, error) {
	if len(query.Metadata) == 0 || len(s.reads) == 0 {
		return s.Store.List(query)
	}
	next := s.reads[0]
	s.reads = s.reads[1:]
	return next, nil
}

// Same-address duplicates deliver, as the listing did, and are reported on
// stderr with their ids (the mayor's ruling), whichever read saw them; a bead
// appearing between the reads neither panics nor answers alone (round 3).
func TestResolveMailRecipientIdentity_SameAddressDuplicatesAndBeadsBetweenReads(t *testing.T) {
	base, _ := newCountedSessionStore(t)
	one, two := seatBead(t, base, "mayor", nil), seatBead(t, base, "mayor", nil)
	var warned bytes.Buffer
	saved := mailRecipientWarnings
	mailRecipientWarnings = &warned
	t.Cleanup(func() { mailRecipientWarnings = saved })

	late := &scriptedIdentityReads{Store: base, reads: [][]beads.Bead{nil, {one}}}
	if address, ok, err := resolveConfiguredMailRecipientByLookup(t.TempDir(), oneSeatCity("mayor"), late, "mayor"); ok || err != nil {
		t.Fatalf("first read empty, second not: got %q, %v, %v; want a decline", address, ok, err)
	}
	dup := &scriptedIdentityReads{Store: base, reads: [][]beads.Bead{{one}, {one, two}}}
	if address, ok, err := resolveConfiguredMailRecipientByLookup(t.TempDir(), oneSeatCity("mayor"), dup, "mayor"); !ok || err != nil || address != "mayor" {
		t.Fatalf("second read duplicate: got %q, %v, %v; want mayor", address, ok, err)
	}
	if !strings.Contains(warned.String(), two.ID) {
		t.Fatalf("warning = %q, want the second read's bead %s", warned.String(), two.ID)
	}
	warned.Reset()
	if address, err := resolveRecipient(t, oneSeatCity("mayor"), base, "mayor"); err != nil || address != "mayor" ||
		!strings.Contains(warned.String(), one.ID) || !strings.Contains(warned.String(), two.ID) {
		t.Fatalf("both reads see both: got %q, %v, warning %q; want mayor and both ids", address, err, warned.String())
	}
}
