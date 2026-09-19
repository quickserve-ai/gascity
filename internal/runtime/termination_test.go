package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The provider under test is the package's own Fake, deliberately: a
// hand-rolled double here would drift from Provider as the interface grows, and
// a double that is KINDER than the real thing manufactures evidence. Fake
// records Calls and supports per-session StopErrors, which is everything these
// tests need.

// stoppedNames returns the names Fake.Stop was called with, in order.
func stoppedNames(f *Fake) []string {
	var out []string
	for _, c := range f.Calls {
		if c.Method == "Stop" {
			out = append(out, c.Name)
		}
	}
	return out
}

type recordingSink struct {
	got  []Termination
	name []string
	err  error
	boom bool
}

func (r *recordingSink) RecordTermination(name string, t Termination) error {
	if r.boom {
		panic("sink exploded")
	}
	r.name = append(r.name, name)
	r.got = append(r.got, t)
	return r.err
}

// TestStopRecordedAlwaysStops is the load-bearing contract: a seat must ALWAYS
// be able to stop. Force-exits cluster in exactly the windows where the store is
// sick, so a bookkeeping sink that fails, or panics, must not hold the stop.
func TestStopRecordedAlwaysStops(t *testing.T) {
	cases := []struct {
		name string
		sink *recordingSink
	}{
		{"healthy sink", &recordingSink{}},
		{"sink returns an error", &recordingSink{err: errors.New("dolt is down")}},
		{"sink PANICS", &recordingSink{boom: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewFake()
			err := StopRecorded(p, "seat-1", Termination{Kind: KindDrainTimeout, Actor: "controller"}, tc.sink)
			if len(stoppedNames(p)) != 1 || stoppedNames(p)[0] != "seat-1" {
				t.Fatalf("the stop did not happen: stopped=%v (err=%v)", stoppedNames(p), err)
			}
			if tc.sink.err != nil || tc.sink.boom {
				if err == nil {
					t.Error("a sink failure must be REPORTED, not swallowed")
				}
			}
		})
	}
}

// TestStopRecordedNilSinkStillStops — a caller with no sink wired yet (or a nil
// interface value) must still be able to stop.
func TestStopRecordedNilSinkStillStops(t *testing.T) {
	p := NewFake()
	if err := StopRecorded(p, "seat-2", Termination{Kind: KindCityStop}, nil); err != nil {
		t.Fatalf("StopRecorded with a nil sink: %v", err)
	}
	if len(stoppedNames(p)) != 1 {
		t.Fatalf("stop did not run with a nil sink: %v", stoppedNames(p))
	}
}

// TestStopRecordedSurfacesStopError — the stop's own failure must survive; a
// successful record must not mask it.
func TestStopRecordedSurfacesStopError(t *testing.T) {
	boom := errors.New("tmux refused")
	p := NewFake()
	p.StopErrors = map[string]error{"seat-3": boom}
	err := StopRecorded(p, "seat-3", Termination{Kind: KindHandoff}, &recordingSink{})
	if !errors.Is(err, boom) {
		t.Fatalf("stop error lost: %v", err)
	}
}

// TestStopRecordedRecordsEveryKindIncludingHandoff — the ratio needs a
// DENOMINATOR. If the good ending were the one path that skipped the record,
// every ratio computed from these records would read 0%.
func TestStopRecordedRecordsEveryKindIncludingHandoff(t *testing.T) {
	for _, k := range TerminationKinds() {
		sink := &recordingSink{}
		if err := StopRecorded(NewFake(), "seat", Termination{Kind: k}, sink); err != nil {
			t.Fatalf("kind %q: %v", k, err)
		}
		if len(sink.got) != 1 {
			t.Fatalf("kind %q wrote no record", k)
		}
		if sink.got[0].Kind != k {
			t.Errorf("kind %q was rewritten to %q", k, sink.got[0].Kind)
		}
	}
}

// TestUnknownKindIsCoercedToUnclassified — "I don't know" must be VISIBLE and
// counted, never silent, and never a reason to refuse a stop.
func TestUnknownKindIsCoercedToUnclassified(t *testing.T) {
	for _, bad := range []TerminationKind{"", "totally-made-up", "HANDOFF"} {
		sink := &recordingSink{}
		p := NewFake()
		if err := StopRecorded(p, "seat", Termination{Kind: bad}, sink); err != nil {
			t.Fatalf("kind %q: %v", bad, err)
		}
		if len(stoppedNames(p)) != 1 {
			t.Fatalf("kind %q: an unknown kind must not block the stop", bad)
		}
		if len(sink.got) != 1 || sink.got[0].Kind != KindUnclassified {
			t.Errorf("kind %q was not coerced to unclassified: %+v", bad, sink.got)
		}
	}
}

// TestAtIsStampedWhenAbsent — At is the seam's to set, so no caller has to
// remember a clock.
func TestAtIsStampedWhenAbsent(t *testing.T) {
	sink := &recordingSink{}
	if err := StopRecorded(NewFake(), "seat", Termination{Kind: KindHandoff}, sink); err != nil {
		t.Fatal(err)
	}
	if sink.got[0].At.IsZero() {
		t.Error("At was left zero; every record must carry when the ending happened")
	}
	// An explicitly supplied At is preserved.
	want := time.Unix(1700000000, 0).UTC()
	sink2 := &recordingSink{}
	if err := StopRecorded(NewFake(), "seat", Termination{Kind: KindHandoff, At: want}, sink2); err != nil {
		t.Fatal(err)
	}
	if !sink2.got[0].At.Equal(want) {
		t.Errorf("At = %v, want the caller's %v", sink2.got[0].At, want)
	}
}

// TestRequestedAtSurvives — Timer B = At - RequestedAt. The field exists from
// birth so callers are not touched twice (katya, condition 4).
func TestRequestedAtSurvives(t *testing.T) {
	req := time.Unix(1700000000, 0).UTC()
	at := req.Add(7 * time.Minute)
	sink := &recordingSink{}
	if err := StopRecorded(NewFake(), "seat", Termination{Kind: KindDrainHandoff, RequestedAt: req, At: at}, sink); err != nil {
		t.Fatal(err)
	}
	got := sink.got[0]
	if !got.RequestedAt.Equal(req) {
		t.Fatalf("RequestedAt = %v, want %v", got.RequestedAt, req)
	}
	if d := got.At.Sub(got.RequestedAt); d != 7*time.Minute {
		t.Errorf("Timer B = %v, want 7m", d)
	}
}

// TestRatioBuckets pins the two judgement calls so a later edit has to argue
// with a test rather than quietly change the KPI's meaning.
func TestRatioBuckets(t *testing.T) {
	if !KindHandoff.CountsInNumerator() || !KindDrainHandoff.CountsInNumerator() {
		t.Error("handoff and drain-handoff are the numerator")
	}
	if KindHandoffTarget.CountsInNumerator() {
		t.Error("handoff-target must NOT be in the headline numerator (katya R3): a third party wrote that note")
	}
	if KindObservedDead.CountsInDenominator() {
		t.Error("observed-dead must be OUT of the denominator: nothing could have been asked of a dead runtime")
	}
	if KindInterruptRestart.CountsInDenominator() {
		t.Error("interrupt-restart must be OUT of the denominator: the session restarts in place and the conversation continues, so it is not an ending")
	}
	// THE EXCLUSION LIST IS ENUMERATED, NOT INFERRED. Every kind outside it
	// must count, so adding a THIRD exclusion trips this test and has to be
	// argued for here. Writing the loop as "skip anything excluded" would make
	// the guard vacuous — it would pass for any future exclusion, silently,
	// which is the exact failure it exists to prevent.
	excluded := map[TerminationKind]bool{
		KindObservedDead:     true,
		KindInterruptRestart: true,
	}
	for _, k := range TerminationKinds() {
		if excluded[k] {
			continue
		}
		if !k.CountsInDenominator() {
			t.Errorf("kind %q silently left the denominator — that is how a ratio grows quiet holes", k)
		}
	}
	if !KindUnclassified.CountsInDenominator() {
		t.Error("unclassified MUST count in the denominator; excluding it would hide the instrument's own blind spot")
	}
}

// TestUnclassifiedBudgetIsPreRegistered guards the number itself. It is
// pre-registered before the baseline week precisely so it cannot be tuned once
// the numbers are in; changing it should require editing this test and saying
// why (katya, condition 3).
func TestUnclassifiedBudgetIsPreRegistered(t *testing.T) {
	if UnclassifiedBudget != 0.05 {
		t.Errorf("UnclassifiedBudget = %v, want 0.05 as pre-registered 2026-09-19 before the baseline week", UnclassifiedBudget)
	}
}

// TestSinkSeesSessionName — a record nobody can attribute to a seat is not a
// record. This is the field the ga-qbc7d2 nudge gap showed the cost of losing.
func TestSinkSeesSessionName(t *testing.T) {
	sink := &recordingSink{}
	if err := StopRecorded(NewFake(), "qcore/worker-7", Termination{Kind: KindOperatorKill}, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.name) != 1 || sink.name[0] != "qcore/worker-7" {
		t.Fatalf("sink got session name %v, want qcore/worker-7", sink.name)
	}
}

// TestMultipleSinksAllRunEvenWhenOneFails — two sinks exist BECAUSE they have
// different failure domains (bead/Dolt vs local event append). A design whose
// first sink's failure skipped the second would collapse them back into one.
func TestMultipleSinksAllRunEvenWhenOneFails(t *testing.T) {
	bead := &recordingSink{err: errors.New("dolt is down")}
	event := &recordingSink{}
	p := NewFake()
	err := StopRecorded(p, "seat", Termination{Kind: KindDrainTimeout}, bead, event)
	if len(event.got) != 1 {
		t.Fatal("the second sink was skipped when the first failed — the two failure domains are not independent")
	}
	if len(stoppedNames(p)) != 1 {
		t.Fatal("the stop did not happen")
	}
	if err == nil || !strings.Contains(err.Error(), "dolt is down") {
		t.Errorf("the failing sink was not reported: %v", err)
	}
}

// TestStopRecordedPreservesSinkErrorIdentity pins the fix for a latent bug: the
// first implementation joined sink errors as STRINGS, so every sentinel a sink
// defined became untestable through StopRecorded. The sentinels existed and
// could never fire, which is the worst shape for a guard — present, documented,
// and permanently false.
func TestStopRecordedPreservesSinkErrorIdentity(t *testing.T) {
	sentinel := errors.New("a sink sentinel")
	p := NewFake()
	err := StopRecorded(p, "seat", Termination{Kind: KindHandoff}, errSink{sentinel})
	if err == nil {
		t.Fatal("a sink failure must be reported")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("sink error identity lost through StopRecorded: %v", err)
	}
	if !errors.Is(err, ErrTerminationRecord) {
		t.Errorf("a record failure must be tagged with ErrTerminationRecord: %v", err)
	}
}

// TestStopRecordedDetailedKeepsTheTwoQuestionsApart — "is the session gone" and
// "was the ending written down" are different questions, and a caller usually
// only has a contract about the first.
func TestStopRecordedDetailedKeepsTheTwoQuestionsApart(t *testing.T) {
	sentinel := errors.New("bookkeeping is unconfirmable")
	p := NewFake()
	stopErr, recErr := StopRecordedDetailed(p, "seat", Termination{Kind: KindHandoff}, errSink{sentinel})
	if stopErr != nil {
		t.Errorf("stopErr = %v, want nil — the stop succeeded", stopErr)
	}
	if !errors.Is(recErr, sentinel) {
		t.Errorf("recErr lost the sink's identity: %v", recErr)
	}
	if len(stoppedNames(p)) != 1 {
		t.Error("the stop must still have happened")
	}

	// And when BOTH fail, both survive as themselves.
	boom := errors.New("tmux refused")
	p2 := NewFake()
	p2.StopErrors = map[string]error{"seat": boom}
	stopErr2, recErr2 := StopRecordedDetailed(p2, "seat", Termination{Kind: KindHandoff}, errSink{sentinel})
	if !errors.Is(stopErr2, boom) {
		t.Errorf("stop error lost: %v", stopErr2)
	}
	if !errors.Is(recErr2, sentinel) {
		t.Errorf("record error lost: %v", recErr2)
	}
}

type errSink struct{ err error }

func (e errSink) RecordTermination(string, Termination) error { return e.err }

// TestStopRecordedDoesNotWaitOnAHangingSink is the defect katya found in S1
// review: rule 1 said "the stop never waits on the record", and the code
// enforced that against sink FAILURE and sink PANIC but not against a sink that
// simply never returns.
//
// WHY IT MATTERED MORE THAN A FAILURE WOULD. The bead sink's write rides the
// Dolt pool, whose natural bound under load is the mysql driver's ~2-minute
// silent read timeout. `gc session kill` is an INCIDENT REMEDY — the command
// you reach for when a seat is wedged and the store is sick — so the unbounded
// version blocked the fix for up to two minutes at exactly the moment it was
// needed. A hang, not an error.
func TestStopRecordedDoesNotWaitOnAHangingSink(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	p := NewFake()
	start := time.Now()
	stopErr, recErr := StopRecordedDetailed(p, "wedged-seat",
		Termination{Kind: KindOperatorKill}, hangingSink{release})
	elapsed := time.Since(start)

	if len(stoppedNames(p)) != 1 {
		t.Fatal("the stop did not happen — a hanging sink held up an incident remedy")
	}
	if stopErr != nil {
		t.Errorf("stopErr = %v, want nil", stopErr)
	}
	if !errors.Is(recErr, ErrTerminationSinkTimeout) {
		t.Errorf("recErr = %v, want the timeout sentinel so a reader can tell "+
			"'the store was slow' from 'the record failed'", recErr)
	}
	// Generous upper bound: the assertion is "bounded", not "exactly 5s".
	if elapsed > TerminationSinkBudget+3*time.Second {
		t.Errorf("stop took %v, want bounded by TerminationSinkBudget (%v)",
			elapsed, TerminationSinkBudget)
	}
}

// TestStopRecordedStillWaitsForAPromptSink — the budget must not turn every
// sink into a race. A sink that returns quickly keeps today's exact semantics,
// including rule 2's reported failure.
func TestStopRecordedStillWaitsForAPromptSink(t *testing.T) {
	sentinel := errors.New("prompt failure")
	p := NewFake()
	stopErr, recErr := StopRecordedDetailed(p, "seat",
		Termination{Kind: KindOperatorKill}, errSink{sentinel})
	if stopErr != nil {
		t.Errorf("stopErr = %v, want nil", stopErr)
	}
	if !errors.Is(recErr, sentinel) {
		t.Errorf("a prompt sink's own error must survive: %v", recErr)
	}
	if errors.Is(recErr, ErrTerminationSinkTimeout) {
		t.Error("a prompt sink must not be reported as a timeout")
	}
}

// TestRequestedAtInvariantIsByKindNotBySentinel freezes the semantics katya
// asked to fix before the baseline week: zero means DIFFERENT things per kind,
// and which kinds must carry the field is checkable rather than conventional.
func TestRequestedAtInvariantIsByKindNotBySentinel(t *testing.T) {
	must := map[TerminationKind]bool{KindDrainHandoff: true, KindDrainTimeout: true}
	ran := 0
	for _, k := range TerminationKinds() {
		ran++
		if got := k.MustCarryRequestedAt(); got != must[k] {
			t.Errorf("%q MustCarryRequestedAt = %v, want %v", k, got, must[k])
		}
	}
	if ran != len(TerminationKinds()) {
		t.Fatalf("checked %d kinds of %d", ran, len(TerminationKinds()))
	}
	// The operator kinds this branch migrated must be in the "never" half, or
	// their deliberate zero would read as an instrument defect.
	for _, k := range []TerminationKind{KindOperatorKill, KindOperatorClose,
		KindOperatorSuspend, KindObservedDead, KindCityStop, KindInterruptRestart} {
		if k.MustCarryRequestedAt() {
			t.Errorf("%q must NOT require requested_at — its zero is by design", k)
		}
	}
}

type hangingSink struct{ release <-chan struct{} }

func (h hangingSink) RecordTermination(string, Termination) error {
	<-h.release
	return nil
}

// TestExcludedKindsAreStillReported — katya's condition 1. A bucket that leaves
// the headline ratio must not leave the report, or its exclusion becomes
// indistinguishable from the events never having happened.
func TestExcludedKindsAreStillReported(t *testing.T) {
	for _, k := range TerminationKinds() {
		if !k.CountsInDenominator() && !k.ReportedSeparately() {
			t.Errorf("kind %q is excluded from the denominator AND not reported "+
				"separately — it would vanish entirely", k)
		}
	}
	if !KindHandoffTarget.ReportedSeparately() {
		t.Error("handoff-target is the precedent for excluded-but-reported")
	}
}
