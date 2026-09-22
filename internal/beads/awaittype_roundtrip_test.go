package beads

import "testing"

// ga-knhu61: AwaitType must survive both native conversions, or a machinery
// gate set correctly at create would be silently stripped on the way to the
// store (create) or read back blank (list/get) and look like the seam default.
func TestNativeConversionsRoundTripAwaitType(t *testing.T) {
	b := Bead{Title: "gate", Type: "gate", AwaitType: AwaitBead}
	issue, err := nativeIssueFromBead(b)
	if err != nil {
		t.Fatalf("nativeIssueFromBead: %v", err)
	}
	if issue.AwaitType != AwaitBead {
		t.Fatalf("issue.AwaitType = %q, want %q", issue.AwaitType, AwaitBead)
	}
	back, err := beadFromNativeIssue(issue)
	if err != nil {
		t.Fatalf("beadFromNativeIssue: %v", err)
	}
	if back.AwaitType != AwaitBead {
		t.Fatalf("round-tripped AwaitType = %q, want %q", back.AwaitType, AwaitBead)
	}
}

func TestIsGateAwaitTypeVocabulary(t *testing.T) {
	for _, ok := range []string{AwaitHuman, AwaitTimer, AwaitMail, AwaitBead, AwaitGHRun, AwaitGHPR} {
		if !IsGateAwaitType(ok) {
			t.Fatalf("IsGateAwaitType(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "all-children", "any-children", "Human", "bead "} {
		if IsGateAwaitType(bad) {
			t.Fatalf("IsGateAwaitType(%q) = true, want false", bad)
		}
	}
}
