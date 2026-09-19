package session

import (
	"context"
	"testing"

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

func newRecordedManager(t *testing.T) (*Manager, *runtime.Fake, beads.Store, *recordingSink) {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sink := &recordingSink{}
	return NewManagerWithOptions(store, sp, WithTerminationSinks(sink)), sp, store, sink
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
		{"Kill", runtime.KindOperatorKill, func(_ *testing.T, m *Manager, id string) error {
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
			mgr, _, _, sink := newRecordedManager(t)
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
	if err == nil {
		t.Error("sink panic was swallowed; StopRecorded rule 2 says it is reported")
	}
}

type panickingSink struct{}

func (panickingSink) RecordTermination(string, runtime.Termination) error {
	panic("sink is sick")
}
