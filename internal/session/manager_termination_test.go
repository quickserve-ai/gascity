package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// recordingSink captures every termination the Manager funnels through the seam.
type recordingSink struct {
	got []runtime.Termination
}

func (r *recordingSink) RecordTermination(_ string, t runtime.Termination) error {
	r.got = append(r.got, t)
	return nil
}

// newRecordedManager returns only the manager and the sink: every caller
// discarded the provider and the store, and unparam is right that an unused
// return is a claim the helper does not make good on.
func newRecordedManager(t *testing.T) (*Manager, *recordingSink) {
	t.Helper()
	sink := &recordingSink{}
	return NewManagerWithOptions(beads.NewMemStore(), runtime.NewFake(), WithTerminationSinks(sink)), sink
}

func liveSession(t *testing.T, mgr *Manager, title string) Info {
	t.Helper()
	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper", Title: title, Command: "claude",
		WorkDir: "/tmp", Provider: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return info
}

// TestManagerTerminationsAreRecorded is the point of ga-ksac39 S1 for the
// Manager: every ending it causes must arrive at the sink with the RIGHT kind.
//
// IT ASSERTS THE CASE COUNT. `go test -run <regex>` prints "ok" when the regex
// matches nothing, and a subtest table that silently stops iterating looks
// identical to one that passed. The count assertion is what makes "not run"
// distinguishable from "passed" — the same discipline the fence's --self-test
// applies to itself.
func TestManagerTerminationsAreRecorded(t *testing.T) {
	cases := []struct {
		name string
		want runtime.TerminationKind
		act  func(t *testing.T, mgr *Manager, id string) error
	}{
		// Bare Kill records UNCLASSIFIED, not operator-kill. This assertion
		// was the other way round until the full cmd/gc suite proved it wrong:
		// the drain path reaches this same method, so a fixed operator-kind
		// here relabels every drain. See
		// TestKillIntentComesFromTheCallerNotTheManager.
		{"Kill", runtime.KindUnclassified, func(_ *testing.T, m *Manager, id string) error {
			return m.Kill(id)
		}},
		{"Suspend", runtime.KindOperatorSuspend, func(_ *testing.T, m *Manager, id string) error {
			return m.Suspend(id)
		}},
		{"Close", runtime.KindOperatorClose, func(_ *testing.T, m *Manager, id string) error {
			_, err := m.CloseDetailed(id)
			return err
		}},
	}

	ran := 0
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ran++
			mgr, sink := newRecordedManager(t)
			info := liveSession(t, mgr, tc.name)
			if err := tc.act(t, mgr, info.ID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(sink.got) != 1 {
				t.Fatalf("%s recorded %d terminations, want exactly 1: %+v",
					tc.name, len(sink.got), sink.got)
			}
			rec := sink.got[0]
			if rec.Kind != tc.want {
				t.Errorf("%s Kind = %q, want %q", tc.name, rec.Kind, tc.want)
			}
			if rec.SessionID != info.ID {
				t.Errorf("%s SessionID = %q, want %q", tc.name, rec.SessionID, info.ID)
			}
			if rec.At.IsZero() {
				t.Errorf("%s At is zero — the seam must stamp it", tc.name)
			}
			// RequestedAt is deliberately zero on the synchronous operator
			// paths: there is no request distinct from the action, and a
			// stamped "now" here would mint a Timer B of ~0ms that reads as a
			// measurement. Pin it so a later edit cannot fabricate one quietly.
			if !rec.RequestedAt.IsZero() {
				t.Errorf("%s RequestedAt = %v, want zero on a synchronous operator path",
					tc.name, rec.RequestedAt)
			}
			if !rec.Kind.Valid() {
				t.Errorf("%s Kind %q is outside the closed set", tc.name, rec.Kind)
			}
		})
	}
	if ran != len(cases) {
		t.Fatalf("harness ran %d of %d cases — a table that stops early is "+
			"indistinguishable from one that passed", ran, len(cases))
	}
}

// TestManagerStopsWithoutSinks pins StopRecorded rule 1 at the Manager boundary:
// a Manager with no sinks wired must still stop sessions. Recording is
// bookkeeping; the stop is the operation that matters, and force-exits cluster
// in exactly the windows where bookkeeping is broken.
func TestManagerStopsWithoutSinks(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp) // no WithTerminationSinks
	info := liveSession(t, mgr, "no-sinks")
	if err := mgr.Kill(info.ID); err != nil {
		t.Fatalf("Kill without sinks: %v", err)
	}
	if sp.IsRunning(info.SessionName) {
		t.Error("session still running after Kill with no sinks wired")
	}
}

// TestManagerStopSurvivesASickSink is the same rule against a sink that fails
// rather than one that is absent: the session must still be gone.
func TestManagerStopSurvivesASickSink(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp, WithTerminationSinks(panickingSink{}))
	info := liveSession(t, mgr, "sick-sink")
	err := mgr.Kill(info.ID)
	if sp.IsRunning(info.SessionName) {
		t.Fatal("session still running after Kill with a panicking sink — the " +
			"stop waited on bookkeeping")
	}
	// *** THIS ASSERTION WAS INVERTED ON 2026-09-19, and the inversion is the
	// fix, not a weakening. *** It used to require Kill to RETURN the sink's
	// error, reading rule 2's "reported, not swallowed" as "returned". That is
	// what made Suspend and CloseDetailed abandon their lifecycle transitions
	// when a sink failed, leaving a dead runtime behind a live bead — the
	// Codex #106 finding. It is also the wrong contract for Kill itself: the
	// reconciler's verifiedStop branches on this error, so a void event
	// recorder (which cannot acknowledge a write, and is the ordinary
	// configuration) would have made every successful kill look failed and
	// earn a retry.
	//
	// Rule 2 is satisfied by the LOG LINE in Manager.stopRecorded. The seam
	// still hands both errors back; the Manager chooses which question its
	// callers are asking. worker.RuntimeHandle.stopRecorded made the same
	// choice, for the same reason, one package over.
	if err != nil {
		t.Errorf("Kill returned %v — a sink panic is a bookkeeping failure and must not read as a failed stop", err)
	}
}

type panickingSink struct{}

func (panickingSink) RecordTermination(string, runtime.Termination) error {
	panic("sink is sick")
}

// TestManagerWritesTheRecordToTheBeadByDefault is the difference between
// "migrated" and "collecting". The Manager funneling through the seam records
// nothing unless a sink is attached; the authoritative bead sink is therefore a
// DEFAULT, and this pins that it stays one. A regression here is silent: every
// stop still works, the ratio just quietly loses its denominator.
func TestManagerWritesTheRecordToTheBeadByDefault(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp) // NO WithTerminationSinks
	info := liveSession(t, mgr, "default-sink")

	if err := mgr.Kill(info.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	// Unclassified, because this calls bare Kill and states no intent — what
	// matters for THIS test is that a record LANDED, not which kind it carries.
	if got := b.Metadata[TerminationKindKey]; got != string(runtime.KindUnclassified) {
		t.Errorf("%s = %q, want %q", TerminationKindKey, got, runtime.KindUnclassified)
	}
	if b.Metadata[TerminationAtKey] == "" {
		t.Errorf("%s is empty — the record landed without a timestamp", TerminationAtKey)
	}
	// RequestedAt is zero on this path, and the patch writes an EMPTY STRING
	// rather than omitting the key, so a re-terminated bead cannot show a stale
	// value from a previous ending beside a fresh kind.
	if _, present := b.Metadata[TerminationRequestedAtKey]; !present {
		t.Errorf("%s is absent; it must be present-and-empty, not missing",
			TerminationRequestedAtKey)
	}
}

// TestKillIntentComesFromTheCallerNotTheManager is the regression that the full
// cmd/gc suite caught and a targeted run would not have.
//
// Manager.Kill is a SHARED MECHANISM: the same method serves `gc session kill`
// and the reconciler's drain-timeout force stop. S1 first hardcoded
// operator-kill here, which silently relabelled every drain as an operator
// action — so drain-timeout and drain-handoff could never appear in the ratio,
// and its most important category would have read ZERO while looking healthy.
// A metric that is confidently wrong beats no metric only in the wrong
// direction.
func TestKillIntentComesFromTheCallerNotTheManager(t *testing.T) {
	t.Run("a caller that states intent gets it recorded", func(t *testing.T) {
		mgr, sink := newRecordedManager(t)
		info := liveSession(t, mgr, "drain")
		asked := time.Now().UTC().Add(-90 * time.Second)

		err := mgr.KillWithTermination(info.ID, runtime.Termination{
			Kind:        runtime.KindDrainTimeout,
			Actor:       "reconciler",
			Reason:      "pool-excess",
			RequestedAt: asked,
		})
		if err != nil {
			t.Fatalf("KillWithTermination: %v", err)
		}
		if len(sink.got) != 1 {
			t.Fatalf("recorded %d, want 1", len(sink.got))
		}
		rec := sink.got[0]
		if rec.Kind != runtime.KindDrainTimeout {
			t.Errorf("Kind = %q, want drain-timeout — a drain must not read as an operator kill", rec.Kind)
		}
		if rec.Actor != "reconciler" {
			t.Errorf("Actor = %q, want reconciler", rec.Actor)
		}
		// Timer B only exists because the caller passed the moment the drain
		// was ASKED. The drain kinds are the ones required to carry it.
		if !rec.Kind.MustCarryRequestedAt() {
			t.Error("drain-timeout must be in the must-carry-requested_at set")
		}
		if rec.RequestedAt.IsZero() {
			t.Fatal("RequestedAt lost — Timer B is unmeasurable")
		}
		if b := rec.At.Sub(rec.RequestedAt); b < 80*time.Second {
			t.Errorf("Timer B = %v, want ~90s", b)
		}
	})

	t.Run("a caller that states nothing gets unclassified, never a guess", func(t *testing.T) {
		mgr, sink := newRecordedManager(t)
		info := liveSession(t, mgr, "no-intent")
		if err := mgr.Kill(info.ID); err != nil {
			t.Fatalf("Kill: %v", err)
		}
		if len(sink.got) != 1 {
			t.Fatalf("recorded %d, want 1", len(sink.got))
		}
		if got := sink.got[0].Kind; got != runtime.KindUnclassified {
			t.Errorf("Kind = %q, want unclassified: the Manager cannot know who "+
				"called it, and a guess here corrupts the ratio silently", got)
		}
		if !sink.got[0].Kind.CountsInDenominator() {
			t.Error("unclassified must still count — it is the instrument's own health metric")
		}
	})
}

// TestBothSinksReceiveTheSameEnding pins katya's condition 2 at the Manager
// boundary: two sinks in TWO failure domains, not one sink with extra steps.
//
// The bead sink rides Dolt and fails during exactly the incidents that produce
// force-exits; the event sink is a local append that almost never does. This
// asserts an ending reaches BOTH, and — more importantly — that ONE SINK FAILING
// DOES NOT COST THE OTHER ITS RECORD, which is the entire reason there are two.
func TestBothSinksReceiveTheSameEnding(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	extra := &recordingSink{}
	mgr := NewManagerWithOptions(store, sp, WithTerminationSinks(extra))
	info := liveSession(t, mgr, "two-domains")

	if err := mgr.KillWithTermination(info.ID, runtime.Termination{
		Kind: runtime.KindOperatorKill, Reason: "two-sink check",
	}); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	// The explicitly supplied sink saw it...
	if len(extra.got) != 1 || extra.got[0].Kind != runtime.KindOperatorKill {
		t.Fatalf("supplied sink got %+v, want one operator-kill", extra.got)
	}
	// ...and so did the DEFAULT bead sink, which is not the same object.
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata[TerminationKindKey] != string(runtime.KindOperatorKill) {
		t.Errorf("bead sink did not record: %s = %q",
			TerminationKindKey, b.Metadata[TerminationKindKey])
	}
}

// TestASickSinkDoesNotCostTheOtherItsRecord is the failure-domain claim itself.
func TestASickSinkDoesNotCostTheOtherItsRecord(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	healthy := &recordingSink{}
	mgr := NewManagerWithOptions(store, sp, WithTerminationSinks(panickingSink{}, healthy))
	info := liveSession(t, mgr, "one-sick")

	_ = mgr.KillWithTermination(info.ID, runtime.Termination{Kind: runtime.KindOperatorKill})

	if len(healthy.got) != 1 {
		t.Fatalf("the healthy sink recorded %d — a sick peer took its record with it",
			len(healthy.got))
	}
	if sp.IsRunning(info.SessionName) {
		t.Error("and the stop must still have happened")
	}
}

// failingSink is a sink whose write always fails — the ALLOWED case. The whole
// five-second budget exists because this is expected during an incident.
type failingSink struct{ calls int }

func (f *failingSink) RecordTermination(string, runtime.Termination) error {
	f.calls++
	return errors.New("store is sick")
}

// TestASinkFailureDoesNotAbortTheLifecycleTransition is the Codex #106 finding.
//
// Suspend and CloseDetailed branch on the stop's result to decide whether to
// persist the session's new state. While Manager.stopRecorded returned the
// JOINED error, a failing sink made both of them return before writing that
// state — so a runtime that really had stopped was left behind a bead that still
// read live, and the reconciler would see it as recoverable. A recording problem
// turned into a stranded session.
func TestASinkFailureDoesNotAbortTheLifecycleTransition(t *testing.T) {
	// Each case asserts the transition IN ITS OWN TERMS: Suspend writes the
	// metadata state, CloseDetailed closes the bead's status. Asserting one
	// shape for both would have let the Close case pass for the wrong reason.
	for _, tc := range []struct {
		name      string
		act       func(*Manager, Info) error
		persisted func(beads.Bead) (string, string)
	}{
		{
			"Suspend", func(m *Manager, info Info) error { return m.Suspend(info.ID) },
			func(b beads.Bead) (string, string) { return b.Metadata["state"], string(StateSuspended) },
		},
		{"CloseDetailed", func(m *Manager, info Info) error {
			_, err := m.CloseDetailed(info.ID)
			return err
		}, func(b beads.Bead) (string, string) { return b.Status, "closed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &failingSink{}
			store := beads.NewMemStore()
			mgr := NewManagerWithOptions(store, runtime.NewFake(), WithTerminationSinks(sink))
			info := liveSession(t, mgr, "sink-failure")

			if err := tc.act(mgr, info); err != nil {
				t.Fatalf("%s returned %v — a sink failure is ALLOWED and must not read as a failed stop", tc.name, err)
			}
			if sink.calls == 0 {
				t.Fatal("the sink was never called, so this test proves nothing about sink failures")
			}
			b, err := store.Get(info.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got, want := tc.persisted(b); got != want {
				t.Errorf("persisted state = %q, want %q — the lifecycle transition was abandoned because BOOKKEEPING failed", got, want)
			}
		})
	}
}

// TestARealStopFailureStillFails — the other side, so the fix above cannot be
// "ignore every error". A provider that cannot stop must still be an error.
func TestARealStopFailureStillFails(t *testing.T) {
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(beads.NewMemStore(), sp)
	info := liveSession(t, mgr, "stop-failure")
	// Key the failure off the name the Fake actually saw, rather than
	// re-deriving the Manager's naming rule in the test and getting a pass from
	// a Stop that was never attempted.
	names, err := sp.ListRunning("")
	if err != nil || len(names) != 1 {
		t.Fatalf("ListRunning = %v, %v; want exactly one live fake session", names, err)
	}
	sp.StopErrors[names[0]] = errors.New("provider wedged")
	if err := mgr.Suspend(info.ID); err == nil {
		t.Error("a genuine stop failure must still surface: the fix separates the two questions, it does not silence one")
	}
}
