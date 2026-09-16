package main

import (
	"strings"
	"testing"
)

// The allowlist arm of TestModuleGraphCarriesOnlyTheSanctionedFleetReplace,
// over synthetic manifests. The guard itself reads the real go.mod, which can
// only ever show one state at a time; these are the states it must keep
// catching after the carry narrowed it from "no replace" to "one replace".
func TestUnsanctionedReplaceFindings(t *testing.T) {
	const (
		fleetPin   = "replace github.com/steveyegge/beads => github.com/quickserve-ai/beads v1.3.0-rc.2-fleet.20260915.2\n"
		moduleHead = "module github.com/gastownhall/gascity\n\ngo 1.26\n\n"
	)
	for _, testCase := range []struct {
		name     string
		goMod    string
		findings int
		want     string
	}{
		{name: "no replace at all is clean", goMod: moduleHead},
		{name: "the fleet pin is permitted", goMod: moduleHead + fleetPin},
		{
			name:  "a serial-less fleet tag is permitted",
			goMod: moduleHead + "replace github.com/steveyegge/beads => github.com/quickserve-ai/beads v1.3.0-rc.2-fleet.20260915\n",
		},
		{
			name:     "a second redirect of another module is caught",
			goMod:    moduleHead + fleetPin + "replace github.com/example/other => github.com/fork/other v1.0.0\n",
			findings: 1,
			want:     "the only redirect this fork carries",
		},
		{
			name:     "redirecting the beads path somewhere else is caught",
			goMod:    moduleHead + "replace github.com/steveyegge/beads => github.com/someone-else/beads v1.3.0-rc.2-fleet.20260915.2\n",
			findings: 1,
			want:     "the only redirect this fork carries",
		},
		{
			name:     "a pseudo-version is caught",
			goMod:    moduleHead + "replace github.com/steveyegge/beads => github.com/quickserve-ai/beads v1.3.0-rc.2.0.20260915120000-5febfb486000\n",
			findings: 1,
			want:     "named by a released tag",
		},
		{
			name:     "a bare release version is caught",
			goMod:    moduleHead + "replace github.com/steveyegge/beads => github.com/quickserve-ai/beads v1.3.0\n",
			findings: 1,
			want:     "named by a released tag",
		},
		{
			name:     "a local path is caught",
			goMod:    moduleHead + "replace github.com/steveyegge/beads => ../beads\n",
			findings: 1,
			want:     "the only redirect this fork carries",
		},
		{
			name:     "a left-hand version is caught",
			goMod:    moduleHead + "replace github.com/steveyegge/beads v1.3.0-rc.2 => github.com/quickserve-ai/beads v1.3.0-rc.2-fleet.20260915.2\n",
			findings: 1,
			want:     "carries no left-hand version",
		},
		{
			name:     "a duplicate of the sanctioned redirect is caught",
			goMod:    moduleHead + fleetPin + fleetPin,
			findings: 1,
			want:     "a second",
		},
		{
			name:     "the block form is read like the single-line form",
			goMod:    moduleHead + "replace (\n\tgithub.com/steveyegge/beads => github.com/quickserve-ai/beads v1.3.0-rc.2-fleet.20260915.2\n\tgithub.com/example/other => github.com/fork/other v1.0.0\n)\n",
			findings: 1,
			want:     "the only redirect this fork carries",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directives, malformed := replaceDirectives(testCase.goMod)
			if len(malformed) > 0 {
				t.Fatalf("parser reported malformed lines %v for a manifest this test wrote", malformed)
			}
			findings := unsanctionedReplaceFindings(directives)
			if len(findings) != testCase.findings {
				t.Fatalf("got %d findings %v, want %d", len(findings), findings, testCase.findings)
			}
			if testCase.want == "" {
				return
			}
			if !namesFinding(findings, testCase.want) {
				t.Errorf("findings %v name none of %q", findings, testCase.want)
			}
		})
	}
}

// namesFinding reports whether any finding mentions want.
func namesFinding(findings []string, want string) bool {
	for _, finding := range findings {
		if strings.Contains(finding, want) {
			return true
		}
	}
	return false
}
