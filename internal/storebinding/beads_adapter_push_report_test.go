package storebinding

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The storebinding graph adapter cannot push the not-closed set itself: it
// passes a listing to the store it wraps, so the report is that store's. Over
// a store that cannot push it stays unreported, which the order gate reads as
// not narrowed (the mayor's ruling 2026-11-10T08:52:17Z, ga-tyf38i); over bd it
// is bd's "not narrowed".
func TestBeadsGraphAdapterPassesThePushReportThrough(t *testing.T) {
	newBd := func() beads.Store {
		return beads.NewBdStore(t.TempDir(), func(string, string, ...string) ([]byte, error) {
			return []byte("[]"), nil
		})
	}
	for _, tc := range []struct {
		name  string
		inner func() beads.Store
		want  beads.PushNotClosedOutcome
	}{
		{"over mem", func() beads.Store { return beads.NewMemStore() }, beads.PushNotClosedUnreported},
		{"over bd", newBd, beads.PushNotClosedNotNarrowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph, err := NewBeadsGraphStore(tc.inner())
			if err != nil {
				t.Fatal(err)
			}
			var report beads.PushNotClosedOutcome
			if _, err := graph.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth, PushNotClosed: true, PushReport: &report}); err != nil {
				t.Fatalf("List: %v", err)
			}
			if report != tc.want {
				t.Fatalf("report = %d, want %d", report, tc.want)
			}
			if report.MayHaveHidden() {
				t.Fatal("MayHaveHidden = true, want false: neither inner store narrowed the listing")
			}
		})
	}
}
