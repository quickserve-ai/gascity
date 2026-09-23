package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestTerminationPatchCarriesEveryField(t *testing.T) {
	req := time.Unix(1700000000, 0).UTC()
	at := req.Add(4 * time.Minute)
	p := TerminationPatch(runtime.Termination{
		Kind: runtime.KindDrainHandoff, Actor: "controller", Reason: "config-drift",
		At: at, RequestedAt: req, EventID: "01JABCDEFGHJKMNPQRSTVWXYZ0",
	})
	want := map[string]string{
		TerminationKindKey:        "drain-handoff",
		TerminationActorKey:       "controller",
		TerminationReasonKey:      "config-drift",
		TerminationAtKey:          at.Format(time.RFC3339),
		TerminationRequestedAtKey: req.Format(time.RFC3339),
		TerminationEventIDKey:     "01JABCDEFGHJKMNPQRSTVWXYZ0",
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %q, want %q", k, p[k], v)
		}
	}
}

// TestTerminationPatchClearsStaleOptionalFields — a session bead that ends
// twice must not carry the PREVIOUS ending's actor or reason beside the new
// kind. A half-updated record reads as a coherent one, which is worse than an
// obviously missing field.
func TestTerminationPatchClearsStaleOptionalFields(t *testing.T) {
	p := TerminationPatch(runtime.Termination{Kind: runtime.KindObservedDead})
	for _, k := range []string{TerminationActorKey, TerminationReasonKey, TerminationRequestedAtKey, TerminationEventIDKey} {
		v, present := p[k]
		if !present {
			t.Errorf("%s is absent; it must be written as empty so it OVERWRITES a previous ending's value", k)
		}
		if v != "" {
			t.Errorf("%s = %q, want empty", k, v)
		}
	}
	// The mandatory fields are still populated.
	if p[TerminationKindKey] != "observed-dead" || p[TerminationAtKey] == "" {
		t.Errorf("mandatory fields missing: %+v", p)
	}
}

func TestTerminationPatchStampsAtWhenAbsent(t *testing.T) {
	p := TerminationPatch(runtime.Termination{Kind: runtime.KindHandoff})
	if p[TerminationAtKey] == "" {
		t.Fatal("At must always be written: a record without a time cannot be bucketed into a week")
	}
	if _, err := time.Parse(time.RFC3339, p[TerminationAtKey]); err != nil {
		t.Errorf("At is not RFC3339: %q (%v)", p[TerminationAtKey], err)
	}
}

// TestBeadSinkRefusesWithoutASessionID — the sink must NOT fall back to
// resolving the name, because that would put a store read on the stop path.
// Refusing loudly is correct; a silent no-op would lose the record.
func TestBeadSinkRefusesWithoutASessionID(t *testing.T) {
	s := &BeadTerminationSink{store: &Store{}}
	err := s.RecordTermination("qcore/worker", runtime.Termination{Kind: runtime.KindOperatorKill})
	if !errors.Is(err, ErrNoSessionID) {
		t.Fatalf("want ErrNoSessionID, got %v", err)
	}
}

func TestBeadSinkNilIsSafe(t *testing.T) {
	if s := NewBeadTerminationSink(nil); s != nil {
		t.Fatal("a nil store must yield a nil sink")
	}
	var s *BeadTerminationSink
	if err := s.RecordTermination("seat", runtime.Termination{Kind: runtime.KindHandoff}); err != nil {
		t.Errorf("a nil sink must be a silent no-op, got %v", err)
	}
}

// TestTerminationKeysAreNotLivenessKeys guards the placement decision. Liveness
// heartbeats were moved to the dolt-ignored session_liveness table because they
// mint a Dolt commit per write. Termination records want the OPPOSITE: they are
// rare, and their value is precisely that dolt_history_issues keeps them — that
// history is the ratio's source of record. If someone later relocates these keys
// for "consistency with liveness", the ratio loses its history silently.
func TestTerminationKeysAreNotLivenessKeys(t *testing.T) {
	for _, k := range []string{
		TerminationKindKey, TerminationActorKey, TerminationReasonKey,
		TerminationAtKey, TerminationRequestedAtKey,
	} {
		if !strings.HasPrefix(k, "termination.") {
			t.Errorf("key %q left the termination. namespace", k)
		}
		if strings.Contains(k, "liveness") {
			t.Errorf("key %q looks like a liveness key; these must stay on the VERSIONED bead", k)
		}
	}
}

// An intent without a readable stamp is RETIRED, not live: the pinned guard
// retires an intent by clearing only its stamp, by compare-and-set on the
// stamp (Codex #106 r12), so it must read as absent.
func TestReadTerminationIntentRejectsAMissingStamp(t *testing.T) {
	if _, _, ok := ReadTerminationIntent("handoff", ""); ok {
		t.Fatal("ReadTerminationIntent(handoff, \"\") ok=true, want false: an unstamped intent is retired")
	}
	if _, _, ok := ReadTerminationIntent("handoff", "not-a-time"); ok {
		t.Fatal("ReadTerminationIntent(handoff, junk) ok=true, want false")
	}
	if _, _, ok := ReadTerminationIntent("handoff", "2026-09-22T17:00:00.123456789Z"); !ok {
		t.Fatal("a nanosecond stamp must read as valid")
	}
}
