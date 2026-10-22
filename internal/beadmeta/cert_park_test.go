package beadmeta

import "testing"

// TestCertParkSuppressesAssignedWake pins the park/resume vocabulary the
// cert-landing-patrol writes (ga-5zosxs) and the one predicate the wake path
// reads it through (ga-mzovhi). The resume rows are the load-bearing half: a
// parked bead that has been flipped must wake its owner again, or the fix turns
// wasted respawns into a silent stall.
func TestCertParkSuppressesAssignedWake(t *testing.T) {
	if CertWaitHoldLabel != "hold:cert-wait" || CertLandedLabel != "cert:landed" || CertActionLabel != "cert:action" {
		t.Fatalf("cert park vocabulary drifted from the patrol's spelling: %q %q %q", CertWaitHoldLabel, CertLandedLabel, CertActionLabel)
	}
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{name: "parked", labels: []string{"ready:ready", "hold:cert-wait", "town:cherub"}, want: true},
		{name: "parked with surrounding whitespace", labels: []string{" hold:cert-wait "}, want: true},
		{name: "patrol flip landed", labels: []string{"cert:landed"}, want: false},
		{name: "patrol flip action", labels: []string{"cert:action", "town:cherub"}, want: false},
		{name: "resume label outranks a hold not yet removed", labels: []string{"hold:cert-wait", "cert:landed"}, want: false},
		{name: "action outranks a hold listed after it", labels: []string{"cert:action", "hold:cert-wait"}, want: false},
		{name: "unparked", labels: []string{"ready:ready"}, want: false},
		{name: "no labels", labels: nil, want: false},
		{name: "a different hold class is not a cert park", labels: []string{"hold:mayor"}, want: false},
		{name: "the cert exclusion marker alone is not a park", labels: []string{"hold:cert-excluded"}, want: false},
		{name: "case differs from the patrol's spelling", labels: []string{"HOLD:CERT-WAIT"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CertParkSuppressesAssignedWake(tc.labels); got != tc.want {
				t.Fatalf("CertParkSuppressesAssignedWake(%q) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}
