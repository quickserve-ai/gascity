package doctor

import "testing"

func TestExecCommandAbsTarget(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
		wantOK  bool
	}{
		{"plain absolute path", "/city/assets/scripts/patrol.py", "/city/assets/scripts/patrol.py", true},
		{"absolute path with args", "/city/assets/scripts/patrol.py --fast", "/city/assets/scripts/patrol.py", true},
		{"leading whitespace", "  /usr/local/bin/run", "/usr/local/bin/run", true},
		{"bare name via PATH", "python3 /city/script.py", "", false},
		{"empty", "", "", false},
		{"shell syntax", "/bin/sh -c 'echo hi'", "/bin/sh", true},
		{"quoted first token", "\"/path with space/run\"", "", false},
		{"variable expansion", "$HOME/bin/run", "", false},
		{"command substitution in token", "/bin/$(x)/run", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := execCommandAbsTarget(tt.command)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("execCommandAbsTarget(%q) = (%q, %v), want (%q, %v)",
					tt.command, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
