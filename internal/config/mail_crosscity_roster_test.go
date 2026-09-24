package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadParsesMailCrossCityTownsAndRosterRoot(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "westeros"

[mail.crosscity]
city = "westeros"
cities = ["qlandia", "gastown"]
roster_root = "/roster"

[mail.crosscity.towns]
qlandia = "alex"
gastown = "cherub"
`)
	cfg, err := Load(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cc := cfg.Mail.CrossCity
	if cc == nil {
		t.Fatal("Mail.CrossCity = nil, want parsed section")
	}
	if cc.RosterRoot != "/roster" {
		t.Errorf("RosterRoot = %q, want /roster", cc.RosterRoot)
	}
	if cc.Towns["qlandia"] != "alex" || cc.Towns["gastown"] != "cherub" {
		t.Errorf("Towns = %v, want qlandia->alex gastown->cherub", cc.Towns)
	}
	towns := cfg.MailCrossCityTowns()
	if towns["qlandia"] != "alex" {
		t.Errorf("MailCrossCityTowns = %v", towns)
	}
	var none *City
	if got := none.MailCrossCityTowns(); got != nil {
		t.Errorf("nil city MailCrossCityTowns = %v, want nil", got)
	}
}

func TestValidateMailCrossCityTowns(t *testing.T) {
	base := func() *City {
		return &City{Mail: MailConfig{CrossCity: &MailCrossCityConfig{
			City:   "westeros",
			Cities: []string{"qlandia"},
		}}}
	}
	tests := []struct {
		name    string
		towns   map[string]string
		wantErr string
	}{
		{name: "mapped peer", towns: map[string]string{"qlandia": "alex"}},
		{name: "unlisted city", towns: map[string]string{"gastown": "cherub"}, wantErr: "not a listed peer city"},
		{name: "local city", towns: map[string]string{"westeros": "westeros"}, wantErr: "not a listed peer city"},
		{name: "empty town", towns: map[string]string{"qlandia": " "}, wantErr: "town"},
		{name: "slash in town", towns: map[string]string{"qlandia": "a/b"}, wantErr: "invalid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			cfg.Mail.CrossCity.Towns = tc.towns
			err := ValidateMailCrossCity(cfg, "/city")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateMailCrossCity = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateMailCrossCity = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// roster_root wins outright: the roster root is the directory that contains
// cities/, and no pin is known for a hand-pointed root.
func TestMailCrossCityRosterSourceExplicitRoot(t *testing.T) {
	cfg := &City{Mail: MailConfig{CrossCity: &MailCrossCityConfig{
		City: "westeros", Cities: []string{"qlandia"}, RosterRoot: "/roster",
	}}}
	root, pin, err := cfg.MailCrossCityRosterSource("/city")
	if err != nil {
		t.Fatalf("MailCrossCityRosterSource: %v", err)
	}
	if root != "/roster" || pin != RosterPinUnknown {
		t.Errorf("= (%q, %q), want (/roster, %q)", root, pin, RosterPinUnknown)
	}
}

// With no roster_root, the roster is the pack-cache clone of the imported
// repository that ships cities/, at the commit packs.lock pins for it; the
// pin printed in a refusal is that commit.
func TestMailCrossCityRosterSourceFromPackCache(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	cityRoot := t.TempDir()
	const (
		source  = "https://github.com/example-org/bridge/tree/main/packs/ops"
		source2 = "https://github.com/example-org/bridge/tree/main/packs/seat"
		other   = "https://github.com/example-org/tools/tree/main/packs/x"
		commit  = "4e05dae7bf6e1f5be09ab2846c858f3b40762fd5"
		otherC  = "1111111111111111111111111111111111111111"
	)
	lock := "[packs.\"" + source + "\"]\ncommit = \"" + commit + "\"\n\n" +
		"[packs.\"" + source2 + "\"]\ncommit = \"" + commit + "\"\n\n" +
		"[packs.\"" + other + "\"]\ncommit = \"" + otherC + "\"\n"
	if err := os.WriteFile(filepath.Join(cityRoot, "packs.lock"), []byte(lock), 0o644); err != nil {
		t.Fatalf("WriteFile(packs.lock): %v", err)
	}
	bridgeClone := GlobalRepoCachePath(gcHome, source, commit)
	if err := os.MkdirAll(filepath.Join(bridgeClone, "cities", "alex"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// The other repo is cached too but ships no cities/ directory.
	if err := os.MkdirAll(filepath.Join(GlobalRepoCachePath(gcHome, other, otherC), "packs"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cfg := &City{
		Imports: map[string]Import{
			"ops":  {Source: source, Version: "sha:" + commit},
			"seat": {Source: source2, Version: "sha:" + commit},
			"x":    {Source: other, Version: "sha:" + otherC},
		},
		Mail: MailConfig{CrossCity: &MailCrossCityConfig{City: "westeros", Cities: []string{"qlandia"}}},
	}
	root, pin, err := cfg.MailCrossCityRosterSource(cityRoot)
	if err != nil {
		t.Fatalf("MailCrossCityRosterSource: %v", err)
	}
	if root != bridgeClone {
		t.Errorf("root = %q, want the bridge clone %q", root, bridgeClone)
	}
	if pin != commit {
		t.Errorf("pin = %q, want %q", pin, commit)
	}
}

// No roster_root and no imported repository shipping cities/ means no list:
// the send keeps today's behavior, and that is reported as an empty source,
// not an error.
func TestMailCrossCityRosterSourceAbsent(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityRoot := t.TempDir()
	cfg := &City{Mail: MailConfig{CrossCity: &MailCrossCityConfig{City: "westeros", Cities: []string{"qlandia"}}}}
	root, pin, err := cfg.MailCrossCityRosterSource(cityRoot)
	if err != nil || root != "" || pin != "" {
		t.Errorf("= (%q, %q, %v), want (\"\", \"\", nil)", root, pin, err)
	}
	var none *City
	if root, _, err := none.MailCrossCityRosterSource(cityRoot); err != nil || root != "" {
		t.Errorf("nil city = (%q, %v)", root, err)
	}
}

// Two imported repositories both shipping cities/ is ambiguous and refused,
// never resolved by map order.
func TestMailCrossCityRosterSourceAmbiguous(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	cityRoot := t.TempDir()
	const (
		a  = "https://github.com/example-org/bridge-a/tree/main/packs/ops"
		b  = "https://github.com/example-org/bridge-b/tree/main/packs/ops"
		ca = "4e05dae7bf6e1f5be09ab2846c858f3b40762fd5"
		cb = "2222222222222222222222222222222222222222"
	)
	lock := "[packs.\"" + a + "\"]\ncommit = \"" + ca + "\"\n\n[packs.\"" + b + "\"]\ncommit = \"" + cb + "\"\n"
	if err := os.WriteFile(filepath.Join(cityRoot, "packs.lock"), []byte(lock), 0o644); err != nil {
		t.Fatalf("WriteFile(packs.lock): %v", err)
	}
	for _, sc := range [][2]string{{a, ca}, {b, cb}} {
		if err := os.MkdirAll(filepath.Join(GlobalRepoCachePath(gcHome, sc[0], sc[1]), "cities"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	cfg := &City{
		Imports: map[string]Import{"a": {Source: a}, "b": {Source: b}},
		Mail:    MailConfig{CrossCity: &MailCrossCityConfig{City: "westeros", Cities: []string{"qlandia"}}},
	}
	if _, _, err := cfg.MailCrossCityRosterSource(cityRoot); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("MailCrossCityRosterSource = %v, want an ambiguity error", err)
	}
}

// A cache clone whose cities/ dir cannot be stat'ed for any reason other
// than absence is a discovery FAILURE, never "no source": a mapped town
// must then refuse, and a second readable repo must not be chosen instead.
func TestMailCrossCityRosterSourceFailsClosedOnUnreadableCache(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	cityRoot := t.TempDir()
	const (
		a  = "https://github.com/example-org/bridge-a/tree/main/packs/ops"
		b  = "https://github.com/example-org/bridge-b/tree/main/packs/ops"
		ca = "4e05dae7bf6e1f5be09ab2846c858f3b40762fd5"
		cb = "2222222222222222222222222222222222222222"
	)
	lock := "[packs.\"" + a + "\"]\ncommit = \"" + ca + "\"\n\n[packs.\"" + b + "\"]\ncommit = \"" + cb + "\"\n"
	if err := os.WriteFile(filepath.Join(cityRoot, "packs.lock"), []byte(lock), 0o644); err != nil {
		t.Fatalf("WriteFile(packs.lock): %v", err)
	}
	sealed := GlobalRepoCachePath(gcHome, a, ca)
	if err := os.MkdirAll(filepath.Join(sealed, "cities"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chmod(sealed, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })
	if err := os.MkdirAll(filepath.Join(GlobalRepoCachePath(gcHome, b, cb), "cities"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cfg := &City{
		Imports: map[string]Import{"a": {Source: a}, "b": {Source: b}},
		Mail:    MailConfig{CrossCity: &MailCrossCityConfig{City: "westeros", Cities: []string{"qlandia"}}},
	}
	root, pin, err := cfg.MailCrossCityRosterSource(cityRoot)
	if err == nil {
		t.Fatalf("MailCrossCityRosterSource = (%q, %q, nil), want an error for the unreadable clone", root, pin)
	}
	if !strings.Contains(err.Error(), sealed) {
		t.Errorf("err = %v, want the unreadable clone %s named", err, sealed)
	}
}
