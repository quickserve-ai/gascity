package beadmeta

import (
	"slices"
	"testing"
)

func TestIsCreateInheritanceExcludedLabel(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  bool
	}{
		// The three state/hold families qc-p9m8oa9 excludes.
		{"hold:cert-wait", true},
		{"hold:mayor", true},
		{"hold:slack-reply", true},
		{"cert:action", true},
		{"cert:landed", true},
		{"needs-summon", true},
		{" hold:cert-wait ", true},
		// Dimensional labels cascade by design and must stay inheritable.
		{"town:cherub", false},
		{"init:qc-88kjhl", false},
		{"ready:needs-grooming", false},
		{"domain:voice", false},
		// Near misses are not members of any family.
		{"hold", false},
		{"cert", false},
		{"needs-summon:later", false},
		{"not-needs-summon", false},
		{"threshold:x", false},
		{"", false},
	} {
		if got := IsCreateInheritanceExcludedLabel(tc.label); got != tc.want {
			t.Errorf("IsCreateInheritanceExcludedLabel(%q) = %v, want %v", tc.label, got, tc.want)
		}
	}
}

func TestInheritedStateLabelsToStrip(t *testing.T) {
	parent := []string{"town:x", "hold:cert-wait", "ready:y", "cert:action", "needs-summon", "hold:cert-wait"}
	for _, tc := range []struct {
		name     string
		inherit  []string
		explicit []string
		want     []string
	}{
		{"strips every state family, once, in parent order", parent, nil, []string{"hold:cert-wait", "cert:action", "needs-summon"}},
		{"an explicitly passed state label is kept", parent, []string{"hold:cert-wait"}, []string{"cert:action", "needs-summon"}},
		{"explicit match ignores bd's whitespace trim", parent, []string{" needs-summon "}, []string{"hold:cert-wait", "cert:action"}},
		{"all explicit means nothing to strip", parent, []string{"needs-summon", "cert:action", "hold:cert-wait"}, nil},
		{"no state labels on the parent", []string{"town:x", "ready:y"}, nil, nil},
		{"no parent labels", nil, []string{"hold:cert-wait"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := InheritedStateLabelsToStrip(tc.inherit, tc.explicit)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("InheritedStateLabelsToStrip(%q, %q) = %q, want %q", tc.inherit, tc.explicit, got, tc.want)
			}
		})
	}
}
