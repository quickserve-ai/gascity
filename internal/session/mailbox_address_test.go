package session

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// The mailbox-address codec is the session-class read half consumed by the mail
// CLI/API. These tests pin it to the exact behavior of the cmd/gc functions it
// replaces (sessionMailboxAddress / sessionMailboxAddresses) and to the
// handler_extmsg alias/session_name precedence, so the wire output is
// byte-identical after routing those callers through the front door. They also
// assert the read methods emit ZERO bead writes (Phase 1 is read-only).

func TestMailboxAddressCodec(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "alias wins",
			bead: beads.Bead{ID: "sess-1", Metadata: map[string]string{"alias": "mayor", "session_name": "sn-1"}},
			want: "mayor",
		},
		{
			name: "id when no alias",
			bead: beads.Bead{ID: "sess-1", Metadata: map[string]string{"session_name": "sn-1"}},
			want: "sess-1",
		},
		{
			name: "session_name when no alias or id",
			bead: beads.Bead{Metadata: map[string]string{"session_name": "sn-1"}},
			want: "sn-1",
		},
		{
			name: "alias trimmed",
			bead: beads.Bead{ID: "sess-1", Metadata: map[string]string{"alias": "  mayor  "}},
			want: "mayor",
		},
		{
			name: "empty everything",
			bead: beads.Bead{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MailboxAddress(tt.bead); got != tt.want {
				t.Errorf("MailboxAddress = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMailboxAddressesCodec(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want []string
	}{
		{
			name: "alias + id + history deduped",
			bead: beads.Bead{
				ID: "sess-1",
				Metadata: map[string]string{
					"alias":         "mayor",
					"alias_history": "deacon,mayor,polecat",
					"session_name":  "sn-1",
				},
			},
			want: []string{"mayor", "sess-1", "deacon", "polecat"},
		},
		{
			name: "no addresses falls back to session_name",
			bead: beads.Bead{Metadata: map[string]string{"session_name": "sn-1"}},
			want: []string{"sn-1"},
		},
		{
			name: "id only",
			bead: beads.Bead{ID: "sess-1"},
			want: []string{"sess-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MailboxAddresses(tt.bead); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MailboxAddresses = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestMailboxAddressesIncludingRuntimeNameCodec pins the API-dup semantics that
// MailboxAddressesIncludingRuntimeName preserves: unlike MailboxAddresses (the
// CLI fork, tested above), it appends session_name UNCONDITIONALLY (last),
// keeping runtime session mailboxes reachable via API reads (bf576b04a).
func TestMailboxAddressesIncludingRuntimeNameCodec(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want []string
	}{
		{
			name: "session_name appended last even when other addresses resolve",
			bead: beads.Bead{
				ID: "sess-1",
				Metadata: map[string]string{
					"alias":         "mayor",
					"alias_history": "deacon,mayor,polecat",
					"session_name":  "sn-1",
				},
			},
			want: []string{"mayor", "sess-1", "deacon", "polecat", "sn-1"},
		},
		{
			name: "session_name deduped against primary",
			bead: beads.Bead{Metadata: map[string]string{"session_name": "sn-1"}},
			want: []string{"sn-1"},
		},
		{
			name: "id only",
			bead: beads.Bead{ID: "sess-1"},
			want: []string{"sess-1"},
		},
		{
			name: "empty everything",
			bead: beads.Bead{},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MailboxAddressesIncludingRuntimeName(tt.bead); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MailboxAddressesIncludingRuntimeName = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestExtmsgHandleSourceCodec(t *testing.T) {
	tests := []struct {
		name string
		bead beads.Bead
		want string
	}{
		{
			name: "alias wins (no id fallback)",
			bead: beads.Bead{ID: "sess-1", Metadata: map[string]string{"alias": "mayor", "session_name": "sn-1"}},
			want: "mayor",
		},
		{
			name: "session_name when no alias (id is ignored)",
			bead: beads.Bead{ID: "sess-1", Metadata: map[string]string{"session_name": "sn-1"}},
			want: "sn-1",
		},
		{
			name: "empty when neither alias nor session_name",
			bead: beads.Bead{ID: "sess-1"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtmsgHandleSource(tt.bead); got != tt.want {
				t.Errorf("ExtmsgHandleSource = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStoreMailboxAddressReadOnly proves the front-door read methods load
// the session bead and apply the codec without emitting any bead writes.
func TestStoreMailboxAddressReadOnly(t *testing.T) {
	b := sessionBeadFixture("sess-1", "open", map[string]string{
		"alias":         "mayor",
		"alias_history": "deacon",
		"session_name":  "sn-1",
	})
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	rec := beadstest.NewRecordingStore(mem)
	is := NewStore(beads.SessionStore{Store: rec})

	addr, err := is.MailboxAddress("sess-1")
	if err != nil {
		t.Fatalf("MailboxAddress: %v", err)
	}
	if addr != "mayor" {
		t.Errorf("MailboxAddress = %q, want mayor", addr)
	}

	addrs, err := is.MailboxAddresses("sess-1")
	if err != nil {
		t.Fatalf("MailboxAddresses: %v", err)
	}
	if want := []string{"mayor", "sess-1", "deacon"}; !reflect.DeepEqual(addrs, want) {
		t.Errorf("MailboxAddresses = %#v, want %#v", addrs, want)
	}

	if got := len(rec.Calls()); got != 0 {
		t.Errorf("read methods emitted %d bead writes, want 0", got)
	}
}

// TestStoreExtmsgHandleSource proves the extmsg handle read method loads
// the bead, applies the alias/session_name codec, signals not-found via ok, and
// emits no bead writes.
func TestStoreExtmsgHandleSource(t *testing.T) {
	b := sessionBeadFixture("sess-1", "open", map[string]string{"session_name": "sn-1"})
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	rec := beadstest.NewRecordingStore(mem)
	is := NewStore(beads.SessionStore{Store: rec})

	source, ok := is.ExtmsgHandleSource("sess-1")
	if !ok {
		t.Fatal("ExtmsgHandleSource ok = false, want true")
	}
	if source != "sn-1" {
		t.Errorf("source = %q, want sn-1", source)
	}

	if _, ok := is.ExtmsgHandleSource("absent"); ok {
		t.Error("ExtmsgHandleSource(absent) ok = true, want false")
	}

	if got := len(rec.Calls()); got != 0 {
		t.Errorf("ExtmsgHandleSource emitted %d bead writes, want 0", got)
	}
}

// TestStoreMailboxAddressNotFound proves a missing session bead surfaces
// the verbatim Get error (beads.ErrNotFound-wrapped), matching the raw mail
// path which returned store.Get's error directly.
func TestStoreMailboxAddressNotFound(t *testing.T) {
	mem := beads.NewMemStore()
	is := NewStore(beads.SessionStore{Store: mem})
	if _, err := is.MailboxAddress("nope"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("err = %v, want beads.ErrNotFound", err)
	}
}

func TestNonSeatSessionAnsweringToMailbox(t *testing.T) {
	// ga-isa3j4 review round 3. The scan decides whether mail for a squatted
	// named seat may be stored under its configured identity.
	spec := NamedSessionSpec{Identity: "myrig/worker", SessionName: "myrig--worker"}
	create := func(t *testing.T, store beads.Store, labels []string, md map[string]string) beads.Bead {
		t.Helper()
		b, err := store.Create(beads.Bead{Type: BeadType, Labels: labels, Metadata: md})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return b
	}

	t.Run("label-lost alias holder is found", func(t *testing.T) {
		store := beads.NewMemStore()
		holder := create(t, store, nil, map[string]string{"alias": spec.Identity, "session_name": "rogue", "state": "active"})
		got, ok, err := NonSeatSessionAnsweringToMailbox(store, spec, beads.Bead{})
		if err != nil || !ok || got.ID != holder.ID {
			t.Fatalf("got (%q, %v, %v), want the label-lost holder %q", got.ID, ok, err, holder.ID)
		}
	})

	t.Run("the seat's own archived bead is exempt", func(t *testing.T) {
		store := beads.NewMemStore()
		create(t, store, []string{LabelSession}, map[string]string{
			NamedSessionMetadataKey:      "true",
			NamedSessionIdentityMetadata: spec.Identity,
			"alias":                      spec.Identity,
			"session_name":               "old-runtime",
			"state":                      "archived",
		})
		create(t, store, []string{LabelSession}, map[string]string{"session_name": spec.SessionName, "template": "other", "state": "asleep"})
		if got, ok, err := NonSeatSessionAnsweringToMailbox(store, spec, beads.Bead{}); err != nil || ok {
			t.Fatalf("got (%q, %v, %v), want no answer: the archived bead IS the seat", got.ID, ok, err)
		}
	})

	t.Run("the reported conflict is always checked", func(t *testing.T) {
		store := beads.NewMemStore()
		// Not in the store at all: only the caller's lookup saw it.
		conflict := beads.Bead{ID: "gc-ghost", Type: BeadType, Metadata: map[string]string{"alias": spec.Identity}}
		got, ok, err := NonSeatSessionAnsweringToMailbox(store, spec, conflict)
		if err != nil || !ok || got.ID != conflict.ID {
			t.Fatalf("got (%q, %v, %v), want the reported conflict", got.ID, ok, err)
		}
	})

	t.Run("closed beads do not answer", func(t *testing.T) {
		store := beads.NewMemStore()
		b := create(t, store, []string{LabelSession}, map[string]string{"alias": spec.Identity, "session_name": "rogue"})
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got, ok, err := NonSeatSessionAnsweringToMailbox(store, spec, beads.Bead{}); err != nil || ok {
			t.Fatalf("got (%q, %v, %v), want no answer from a closed bead", got.ID, ok, err)
		}
	})
}
