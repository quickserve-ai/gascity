package mail

import "testing"

func TestForeignTownPrefix(t *testing.T) {
	rigs := []string{"qcore", "astro", "core"}
	tests := []struct {
		name       string
		recipient  string
		city       string
		wantPrefix string
		wantOK     bool
	}{
		{"another town's seat", "gastown/woodhouse", "qlandia", "gastown", true},
		{"another town's rig-qualified seat", "gastown/qcore/ray", "qlandia", "gastown", true},
		{"this city's own name", "qlandia/mayor", "qlandia", "", false},
		{"a local rig", "qcore/ray", "qlandia", "", false},
		{"bare name", "woodhouse", "qlandia", "", false},
		{"controller form", "controller/", "qlandia", "", false},
		{"controller qualified", "controller/x", "qlandia", "", false},
		{"leading slash", "/woodhouse", "qlandia", "", false},
		{"trailing slash only", "gastown/", "qlandia", "", false},
		{"surrounding space", "  gastown/woodhouse ", "qlandia", "gastown", true},
		{"template target form", "template:myrig/worker", "qlandia", "", false},
		{"a local agent dir that differs from its rig", "core/ray", "qlandia", "", false},
		{"unknown city name still classifies", "gastown/woodhouse", "", "gastown", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPrefix, gotOK := ForeignTownPrefix(tt.recipient, tt.city, rigs)
			if gotPrefix != tt.wantPrefix || gotOK != tt.wantOK {
				t.Errorf("ForeignTownPrefix(%q, %q) = (%q, %v), want (%q, %v)",
					tt.recipient, tt.city, gotPrefix, gotOK, tt.wantPrefix, tt.wantOK)
			}
		})
	}
}
