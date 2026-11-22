package beadmeta

import "testing"

// TestDispatchHoldLabelsMatchCanonicalHoldValues pins beadmeta as the single
// named home for the two canonical hold values documented in
// engdocs/contributors/hold-label-conventions.md (hold:mayor, hold:external)
// so internal/config and cmd/gc can share one definition instead of each
// re-spelling the label strings (ga-x9kptu / ga-5736js).
func TestDispatchHoldLabelsMatchCanonicalHoldValues(t *testing.T) {
	if HoldMayorLabel != "hold:mayor" {
		t.Fatalf("HoldMayorLabel = %q, want %q", HoldMayorLabel, "hold:mayor")
	}
	if HoldExternalLabel != "hold:external" {
		t.Fatalf("HoldExternalLabel = %q, want %q", HoldExternalLabel, "hold:external")
	}
	want := []string{HoldMayorLabel, HoldExternalLabel}
	if len(DispatchHoldLabels) != len(want) {
		t.Fatalf("DispatchHoldLabels = %#v, want %#v", DispatchHoldLabels, want)
	}
	for i, v := range want {
		if DispatchHoldLabels[i] != v {
			t.Fatalf("DispatchHoldLabels[%d] = %q, want %q", i, DispatchHoldLabels[i], v)
		}
	}
}

func TestHasDispatchHold(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"nil", nil, false},
		{"unrelated", []string{"mysql-cutover", "needs-mayor", "mpr-human-hold"}, false},
		{"other hold value", []string{"hold:polecat"}, false},
		{"hold external", []string{"x", HoldExternalLabel}, true},
		{"hold mayor", []string{HoldMayorLabel}, true},
		{"case and space folded", []string{"  Hold:External "}, true},
	}
	for _, tc := range cases {
		if got := HasDispatchHold(tc.labels); got != tc.want {
			t.Errorf("%s: HasDispatchHold(%q) = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
}

// TestHasOpenWorkPark pins the unclaimed-work park rule (ga-v28pab): the two
// canonical dispatch holds (case-folded), any "hold:" label and
// "freeze:deferred" (trimmed, exact, as the route query's jq compares them).
// Hold-ish labels without the "hold:" prefix are ordinary work.
func TestHasOpenWorkPark(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"nil", nil, false},
		{"unrelated", []string{"mysql-cutover", "needs-mayor", "mpr-human-hold", "holding-pattern", "hold"}, false},
		{"freeze label other than deferred", []string{"freeze:a"}, false},
		{"canonical hold", []string{HoldExternalLabel}, true},
		{"town human hold", []string{"hold:human"}, true},
		{"town decision hold", []string{"x", "hold:human-decision"}, true},
		{"clock hold", []string{"hold:clock"}, true},
		{"cert-wait hold", []string{"hold:cert-wait"}, true},
		{"freeze deferred", []string{"freeze:deferred"}, true},
		{"space trimmed", []string{"  hold:human ", "x"}, true},
		{"canonical hold case-folded", []string{" Hold:External"}, true},
		{"town hold is case-exact like the route query", []string{"Hold:Human"}, false},
		{"freeze deferred trimmed", []string{" freeze:deferred "}, true},
		{"freeze deferred is case-exact", []string{"Freeze:Deferred"}, false},
	}
	for _, tc := range cases {
		if got := HasOpenWorkPark(tc.labels); got != tc.want {
			t.Errorf("%s: HasOpenWorkPark(%q) = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
}
