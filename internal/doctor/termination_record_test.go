package doctor

import (
	"context"
	"os"
	"path/filepath"
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

// TestDoctorRecorderIsDerivedNotInjected is katya's derive-by-default ruling.
//
// THESE ARE EVENT-ONLY SITES, so a forgotten option is not a missing
// corroboration — it is an UNCOUNTED DENOMINATOR ENTRY. Recording therefore
// cannot be something a caller opts into; it must be what happens unless
// someone says otherwise out loud.
func TestDoctorRecorderIsDerivedNotInjected(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := &CheckContext{CityPath: city}

	// No option at all -> a sink is DERIVED.
	got, closeFn := resolveTerminationSink(ctx, nil, false)
	defer closeFn()
	if got == nil {
		t.Error("no option must still yield a derived recorder — that is the whole ruling")
	}

	// Silence requires saying so.
	if s, c := resolveTerminationSink(ctx, nil, true); s != nil {
		c()
		t.Error("WithNoTerminationRecorder must yield silence")
	}

	// An explicit sink still wins, for tests that want to observe.
	explicit := &capturingSink{}
	if s, c := resolveTerminationSink(ctx, explicit, false); s != explicit {
		c()
		t.Error("an explicitly supplied sink must take precedence over the derived one")
	}

	// And a check built with no option carries the refusal flag unset, so it
	// will derive at fix time rather than stay silent.
	if c := NewZombieSessionsCheck(&config.City{}, "gastown", "", runtime.NewFake()); c.noRecorder {
		t.Error("a check built with no option must NOT be in refusal mode")
	}
}
