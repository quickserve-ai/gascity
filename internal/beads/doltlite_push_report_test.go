//go:build gascity_native_beads

package beads

import "testing"

// A DoltLite read store cannot push the not-closed set, so it writes no report
// and the order gate keeps the index's answer for its listing (the mayor's
// ruling 2026-11-10T08:52:17Z, ga-tyf38i). Named TestDoltlite* so the Bazel
// target that builds this package with gascity_native_beads runs it.
func TestDoltlitePushedListingReportsNothing(t *testing.T) {
	store := newDoltliteStoreWithIssues(t, nil)
	got := pushReport(t, store)
	if got != PushNotClosedUnreported {
		t.Fatalf("report = %d, want unreported", got)
	}
	if got.MayHaveHidden() {
		t.Fatal("MayHaveHidden = true for a store that cannot push, want false")
	}
}
