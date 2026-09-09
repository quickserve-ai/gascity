package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestRecoverUnroutedWorkRoutesLeavesAFencedGraphStepAlone pins that route
// recovery never reads a graph-first mint's instantiation fence as a lost
// route.
//
// A fenced step has no gc.kind, an empty gc.routed_to (withheld into
// gc.deferred_routed_to until activation) and a bare gc.run_target, which is
// exactly the carried-route shape the sweep repairs — so it restored the BARE
// pool name onto the step. The bare name matches no seat, and the restore is a
// metadata read-merge-write that can land over the activation and put the
// fence back (westeros qcore, 2026-09-09 07:45:43Z: 15 review roots lost in
// 10 days, one entry step each). This branch carries the pre-lane sweep, so
// the regression drives recoverUnroutedWorkRoutes directly.
func TestRecoverUnroutedWorkRoutesLeavesAFencedGraphStepAlone(t *testing.T) {
	cases := map[string]map[string]string{
		"fenced: instantiating, deferred route and type": {
			beadmeta.InstantiatingMetadataKey:    "true",
			beadmeta.DeferredRoutedToMetadataKey: "gascity/gastown.polecat",
			beadmeta.DeferredTypeMetadataKey:     "task",
		},
		"deferred route only (a failed mint blanks instantiating first)": {
			beadmeta.DeferredRoutedToMetadataKey: "gascity/gastown.polecat",
		},
		"instantiating only": {
			beadmeta.InstantiatingMetadataKey: "true",
		},
	}
	for name, fence := range cases {
		t.Run(name, func(t *testing.T) {
			meta := map[string]string{beadmeta.RunTargetMetadataKey: "gastown.polecat"}
			for k, v := range fence {
				meta[k] = v
			}
			store := beads.NewMemStoreFrom(0, []beads.Bead{
				{ID: "RW-fenced", Title: "Resolve PR identity", Type: "gate", Status: "open", Metadata: meta},
			}, nil)
			cr := &CityRuntime{
				cityName:            "city",
				standaloneCityStore: store,
				stderr:              io.Discard,
			}

			cr.recoverUnroutedWorkRoutes()

			if got := mustRoutedTo(t, store, "RW-fenced"); got != "" {
				t.Fatalf("gc.routed_to = %q on a fenced step, want empty: the route is withheld in %s, not lost", got, beadmeta.DeferredRoutedToMetadataKey)
			}
		})
	}
}
