package config

import (
	"strings"
	"testing"
	"time"
)

// The default deadline is derived from the default client budget, and equals
// the 25s the server used before the key existed (ga-x49mfh, pl-lzd).
func TestMailReadDeadlineDefaultIsDerivedFromClientTimeout(t *testing.T) {
	m := MailConfig{}
	if got := m.EffectiveReadTimeout(); got != DefaultMailReadTimeout {
		t.Fatalf("EffectiveReadTimeout() = %v, want default %v", got, DefaultMailReadTimeout)
	}
	if got, want := m.ReadDeadline(), DefaultMailReadTimeout-MailReadDeadlineMargin; got != want {
		t.Fatalf("ReadDeadline() = %v, want %v (client timeout minus margin)", got, want)
	}
	if got := m.ReadDeadline(); got != 25*time.Second {
		t.Fatalf("ReadDeadline() = %v, want the documented 25s default", got)
	}
}

func TestMailReadTimeoutKeyOverridesDerivedDeadline(t *testing.T) {
	m := MailConfig{ReadTimeout: "50s"}
	if got := m.EffectiveReadTimeout(); got != 50*time.Second {
		t.Fatalf("EffectiveReadTimeout() = %v, want 50s", got)
	}
	if got := m.ReadDeadline(); got != 45*time.Second {
		t.Fatalf("ReadDeadline() = %v, want 45s", got)
	}
	if m.ReadDeadline() >= m.EffectiveReadTimeout() {
		t.Fatalf("ReadDeadline() %v must stay below the client timeout %v", m.ReadDeadline(), m.EffectiveReadTimeout())
	}
}

func TestValidateMailReadTimeout(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr []string
	}{
		{name: "unset", raw: ""},
		{name: "default value", raw: "30s"},
		{name: "at the ceiling", raw: MailReadTimeoutCeiling.String()},
		{name: "just above the margin", raw: "6s"},
		{name: "unparseable", raw: "30 seconds", wantErr: []string{`read_timeout = "30 seconds"`, "not a valid duration"}},
		{name: "at the margin", raw: "5s", wantErr: []string{`read_timeout = "5s"`, "5s"}},
		{name: "negative", raw: "-1s", wantErr: []string{`read_timeout = "-1s"`}},
		{name: "above the client ceiling", raw: "61s", wantErr: []string{`read_timeout = "61s"`, "1m0s"}},
		{name: "far above the client ceiling", raw: "2m", wantErr: []string{`read_timeout = "2m"`, "1m0s"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &City{Mail: MailConfig{ReadTimeout: tt.raw}}
			err := ValidateMailReadTimeout(cfg, "city.toml")
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ValidateMailReadTimeout(%q) = %v, want nil", tt.raw, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateMailReadTimeout(%q) = nil, want an error", tt.raw)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err.Error(), want)
				}
			}
		})
	}
}

func TestValidateMailReadTimeoutNilConfig(t *testing.T) {
	if err := ValidateMailReadTimeout(nil, "city.toml"); err != nil {
		t.Fatalf("ValidateMailReadTimeout(nil) = %v, want nil", err)
	}
}
