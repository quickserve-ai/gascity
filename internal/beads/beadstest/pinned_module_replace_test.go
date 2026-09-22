package beadstest

import "testing"

// TestParseBeadsReplacementFollowsTheReplace pins the resolver to the module the
// build links: a go.mod replace on the beads module names the path and version
// the module cache unpacks; no replace, or a replace of another module, leaves
// the require in force; a directory replace reports an empty version.
func TestParseBeadsReplacementFollowsTheReplace(t *testing.T) {
	cases := []struct {
		name         string
		gomod        string
		wantPath     string
		wantVersion  string
		wantReplaced bool
	}{
		{"none", "module x\n\nrequire github.com/steveyegge/beads v1.3.0\n", "", "", false},
		{"other module", "replace github.com/other/mod => github.com/fork/mod v1.0.0\n", "", "", false},
		{"fleet tag", "// the fleet build\nreplace github.com/steveyegge/beads => github.com/quickserve-ai/beads v1.3.0-fleet.20260923.1 // pinned\n", "github.com/quickserve-ai/beads", "v1.3.0-fleet.20260923.1", true},
		{"versioned lhs", "replace github.com/steveyegge/beads v1.3.0 => github.com/quickserve-ai/beads v1.3.0-fleet.1\n", "github.com/quickserve-ai/beads", "v1.3.0-fleet.1", true},
		{"directory", "replace github.com/steveyegge/beads => ../beads\n", "../beads", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, v, ok := parseBeadsReplacement(tc.gomod)
			if ok != tc.wantReplaced || p != tc.wantPath || v != tc.wantVersion {
				t.Fatalf("parseBeadsReplacement = (%q, %q, %v), want (%q, %q, %v)", p, v, ok, tc.wantPath, tc.wantVersion, tc.wantReplaced)
			}
		})
	}
}
