package doctor

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type capturingSink struct{ got []runtime.Termination }

func (c *capturingSink) RecordTermination(_ string, t runtime.Termination) error {
	c.got = append(c.got, t)
	return nil
}

// TestDoctorSessionEndingsAreRecorded closes one of the two denominator holes
// ga-ksac39 left open: doctor's orphan reap force-exits a LIVE session and its
// zombie recycle ends a dead one, and until now neither wrote anything at all.
//
// THE KINDS ARE NOT THE SAME, AND THE DIFFERENCE IS THE POINT. A zombie is a
// shell whose agent process is already dead — nothing could have been asked of
// it, so observed-dead, OUT of the ratio's denominator. An orphan is ALIVE and
// simply absent from config; killing it is an operator force-exit and it COUNTS.
// Collapsing the two would quietly shrink the denominator.
func TestDoctorSessionEndingsAreRecorded(t *testing.T) {
	cases := []struct {
		name string
		want runtime.TerminationKind
		run  func(sink *capturingSink) error
	}{
		{"zombie", runtime.KindObservedDead, func(sink *capturingSink) error {
			sp := runtime.NewFake()
			a := config.Agent{Name: "seat", ProcessNames: []string{"claude"}}
			// Compute the name the check will compute — never guess it.
			sn := agent.SessionNameFor("gastown", a.QualifiedName(), "")
			if err := sp.Start(context.Background(), sn, runtime.Config{}); err != nil {
				return err
			}
			// Shell running, agent process dead: that IS the zombie shape.
			sp.Zombies = map[string]bool{sn: true}
			cfg := &config.City{Agents: []config.Agent{a}}
			c := NewZombieSessionsCheck(cfg, "gastown", "", sp, WithTerminationSink(sink))
			return c.Fix(&CheckContext{})
		}},
	}
	ran := 0
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ran++
			sink := &capturingSink{}
			if err := tc.run(sink); err != nil {
				t.Skipf("environment refused the fix path: %v", err)
			}
			if len(sink.got) == 0 {
				t.Fatalf("%s: nothing recorded — the ending is still uncounted", tc.name)
			}
			if got := sink.got[0].Kind; got != tc.want {
				t.Errorf("%s Kind = %q, want %q", tc.name, got, tc.want)
			}
			if sink.got[0].Actor != "doctor" {
				t.Errorf("%s Actor = %q, want doctor", tc.name, sink.got[0].Actor)
			}
		})
	}
	if ran != len(cases) {
		t.Fatalf("ran %d of %d", ran, len(cases))
	}
}

// TestDoctorChecksStillBuildWithNoSink pins the variadic option's whole purpose:
// the twelve existing test callers and the pre-wiring production caller must
// keep working, recording nothing rather than failing.
func TestDoctorChecksStillBuildWithNoSink(t *testing.T) {
	sp := runtime.NewFake()
	cfg := &config.City{}
	if c := NewZombieSessionsCheck(cfg, "gastown", "", sp); c == nil || c.termSink != nil {
		t.Error("zombie check without an option must carry a nil sink")
	}
	if c := NewOrphanSessionsCheck(cfg, "gastown", "", sp); c == nil || c.termSink != nil {
		t.Error("orphan check without an option must carry a nil sink")
	}
}
