package worker

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/terminationevents"
)

// TestRuntimeHandleEndingsEmitATerminationEvent covers the deepest holes the
// ga-ksac39 census found: the legacy runtime-only targets that bypass the
// Manager entirely and, before this, wrote nothing at all.
//
// THE EVENT IS THE WHOLE RECORD FOR THESE ENDINGS. A RuntimeHandle has no
// bead-backed identity by definition, so the authoritative bead sink has
// nothing to address; if this event is lost the ending is uncounted, with no
// second copy anywhere. That makes it worth pinning harder than a path with
// two sinks.
func TestRuntimeHandleEndingsEmitATerminationEvent(t *testing.T) {
	cases := []struct {
		name string
		want runtime.TerminationKind
		act  func(h *RuntimeHandle) error
	}{
		{"Kill", runtime.KindOperatorKill, func(h *RuntimeHandle) error { return h.Kill(context.Background()) }},
		{"Close", runtime.KindOperatorClose, func(h *RuntimeHandle) error { return h.Close(context.Background()) }},
		// Stop is UNCLASSIFIED on purpose: a generic stop through the
		// LifecycleHandle interface carries no statement of intent, and a
		// guessed kind would put a wrong value in the ratio.
		{"Stop", runtime.KindUnclassified, func(h *RuntimeHandle) error { return h.Stop(context.Background()) }},
	}

	ran := 0
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ran++
			rec := &recordingEventRecorder{}
			sp := runtime.NewFake()
			h, err := NewRuntimeHandle(RuntimeHandleConfig{
				Provider: sp, SessionName: "legacy-target", Recorder: rec,
			})
			if err != nil {
				t.Fatalf("NewRuntimeHandle: %v", err)
			}
			if err := tc.act(h); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			var got int
			for _, ev := range rec.events {
				if ev.Type != terminationevents.TerminationEventType {
					continue
				}
				got++
				if ev.Message != string(tc.want) {
					t.Errorf("%s event kind = %q, want %q", tc.name, ev.Message, tc.want)
				}
				if ev.Subject != "legacy-target" {
					t.Errorf("%s event subject = %q, want the session name", tc.name, ev.Subject)
				}
				// SessionID is EMPTY by design here — there is no bead. Pinned
				// so nobody later "fixes" it with a name lookup, which would
				// put a store read on the stop path.
				if ev.SessionID != "" {
					t.Errorf("%s event SessionID = %q, want empty for a bead-less handle",
						tc.name, ev.SessionID)
				}
			}
			if got != 1 {
				t.Fatalf("%s emitted %d termination events, want exactly 1 (of %d events)",
					tc.name, got, len(rec.events))
			}
		})
	}
	if ran != len(cases) {
		t.Fatalf("harness ran %d of %d cases", ran, len(cases))
	}
}

// TestRuntimeHandleStopsWhenTheRecorderIsAbsent pins StopRecorded rule 1 on the
// legacy path: a handle built with no recorder still stops the session.
func TestRuntimeHandleStopsWhenTheRecorderIsAbsent(t *testing.T) {
	sp := runtime.NewFake()
	h, err := NewRuntimeHandle(RuntimeHandleConfig{
		Provider: sp, SessionName: "no-recorder", // Recorder nil -> events.Discard
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	if err := h.Kill(context.Background()); err != nil {
		t.Fatalf("Kill with no recorder: %v", err)
	}
	var stops int
	for _, c := range sp.Calls {
		if c.Method == "Stop" && c.Name == "no-recorder" {
			stops++
		}
	}
	if stops != 1 {
		t.Errorf("provider saw %d Stop calls, want 1 — the stop must not depend on recording", stops)
	}
}
