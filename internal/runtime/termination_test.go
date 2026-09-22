package runtime

import (
	"errors"
	"strings"
	"sync"
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

// TestEventIDIsMintedAtTheSeamAndKeptWhenSupplied — the dedup key is
// (SessionID, EventID), so every record must carry one, and a caller that
// retries one ending must be able to pin it (katya, PR #106 finding 4).
func TestEventIDIsMintedAtTheSeamAndKeptWhenSupplied(t *testing.T) {
	sink := &recordingSink{}
	if err := StopRecorded(NewFake(), "seat", Termination{Kind: KindOperatorSuspend}, sink); err != nil {
		t.Fatal(err)
	}
	minted := sink.got[0].EventID
	if !isULID(minted) {
		t.Fatalf("seam-minted EventID %q is not a ULID", minted)
	}

	pinned := NewTerminationEventID(time.Unix(1700000000, 0))
	sink2 := &recordingSink{}
	for i := 0; i < 3; i++ {
		if err := StopRecorded(NewFake(), "seat", Termination{Kind: KindDrainTimeout, EventID: pinned}, sink2); err != nil {
			t.Fatal(err)
		}
	}
	for i, rec := range sink2.got {
		if rec.EventID != pinned {
			t.Errorf("attempt %d EventID = %q, want the caller's %q: a retried ending must dedup to one row", i, rec.EventID, pinned)
		}
	}
}

// TestRealTwinsGetDistinctEventIDs is the case the old attribute key lost.
// Suspend, resume, suspend: same session, same kind, zero RequestedAt both
// times. Those are two real endings, and they must not collapse to one row.
func TestRealTwinsGetDistinctEventIDs(t *testing.T) {
	sink := &recordingSink{}
	at := time.Unix(1700000000, 0).UTC()
	for i := 0; i < 2; i++ {
		rec := Termination{Kind: KindOperatorSuspend, SessionID: "ga-wisp-x", At: at}
		if err := StopRecorded(NewFake(), "seat", rec, sink); err != nil {
			t.Fatal(err)
		}
	}
	if sink.got[0].EventID == sink.got[1].EventID {
		t.Fatalf("two real endings with identical attributes share EventID %q: the dedup would count them as one", sink.got[0].EventID)
	}
	if !sink.got[0].RequestedAt.IsZero() || !sink.got[1].RequestedAt.IsZero() {
		t.Error("RequestedAt must stay zero (unknown) rather than be backfilled to serve a key")
	}
	// Minted in the same millisecond, the ids must still sort in mint order.
	if !(sink.got[0].EventID < sink.got[1].EventID) {
		t.Errorf("EventIDs %q, %q are not in mint order", sink.got[0].EventID, sink.got[1].EventID)
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
	for _, k := range []TerminationKind{
		KindOperatorKill, KindOperatorClose,
		KindOperatorSuspend, KindObservedDead, KindCityStop, KindInterruptRestart,
	} {
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

// TestAHangingSinkDoesNotStarveTheOther is the two-failure-domain property,
// checked rather than assumed.
//
// The two sinks exist BECAUSE they fail differently: the bead write rides Dolt
// and hangs during the incidents that produce force-exits, the event append is
// local and does not. The bead sink is wired first at every reconciler site, so
// while the pass ran sequentially a hung store consumed the whole budget before
// the event sink was attempted — and if the process died in that window both
// records were lost. The independence was a property of the design and not of
// the code (Codex, PR #106).
func TestAHangingSinkDoesNotStarveTheOther(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hanging := sinkFunc(func(string, Termination) error {
		<-release
		return nil
	})
	fast := &recordingTestSink{}

	p := NewFake()
	start := time.Now()
	// Hanging sink FIRST, exactly as the bead sink is wired in production.
	err := StopRecorded(p, "s1", Termination{Kind: KindOperatorKill}, hanging, fast)
	elapsed := time.Since(start)

	if len(fast.got) != 1 {
		t.Errorf("the second sink recorded %d terminations, want 1 — a hung first sink starved the domain that exists to survive it", len(fast.got))
	}
	if elapsed < TerminationSinkBudget {
		t.Errorf("returned after %s, before the %s budget — the hang was not actually exercised", elapsed, TerminationSinkBudget)
	}
	if err == nil || !errors.Is(err, ErrTerminationSinkTimeout) {
		t.Errorf("err = %v, want the timeout reported rather than swallowed", err)
	}
	if p.IsRunning("s1") {
		t.Error("the stop did not happen")
	}
}

type sinkFunc func(string, Termination) error

func (f sinkFunc) RecordTermination(name string, rec Termination) error { return f(name, rec) }

type recordingTestSink struct {
	mu  sync.Mutex
	got []Termination
}

func (r *recordingTestSink) RecordTermination(_ string, rec Termination) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, rec)
	return nil
}

func isULID(s string) bool {
	if len(s) != 26 || s[0] > '7' {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune(crockford, c) {
			return false
		}
	}
	return true
}

// TestNewTerminationEventIDEncodesTheTimestamp pins the encoding against a
// known vector: the ULID spec's own example timestamp 1469918176385 ms
// encodes to the prefix "01ARYZ6S41".
func TestNewTerminationEventIDEncodesTheTimestamp(t *testing.T) {
	id := NewTerminationEventID(time.UnixMilli(1469918176385))
	if !isULID(id) {
		t.Fatalf("%q is not a ULID", id)
	}
	if got := id[:10]; got != "01ARYZ6S41" {
		t.Errorf("timestamp prefix = %q, want 01ARYZ6S41 (the ULID spec vector)", got)
	}
	later := NewTerminationEventID(time.UnixMilli(1469918176386))
	if !(id < later) {
		t.Errorf("%q !< %q: ids must sort by mint time", id, later)
	}
}
