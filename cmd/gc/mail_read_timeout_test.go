package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clientcontext"
	"github.com/gastownhall/gascity/internal/config"
)

func TestCityMailReadTimeoutReadsCityToml(t *testing.T) {
	dir := t.TempDir()
	toml := "[workspace]\nname = \"c\"\n\n[mail]\nread_timeout = \"50s\"\n"
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := cityMailReadTimeout(dir); got != 50*time.Second {
		t.Fatalf("cityMailReadTimeout() = %v, want 50s", got)
	}
}

func TestCityMailReadTimeoutDefaultsWithoutKeyOrFile(t *testing.T) {
	dir := t.TempDir()
	if got := cityMailReadTimeout(dir); got != config.DefaultMailReadTimeout {
		t.Fatalf("cityMailReadTimeout(no city.toml) = %v, want %v", got, config.DefaultMailReadTimeout)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"c\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := cityMailReadTimeout(dir); got != config.DefaultMailReadTimeout {
		t.Fatalf("cityMailReadTimeout(no key) = %v, want %v", got, config.DefaultMailReadTimeout)
	}
}

func TestRemoteClientOptionsMailReadTimeout(t *testing.T) {
	ctx := &clientcontext.Context{Name: "hub", URL: "https://hub.example.test", MailReadTimeout: "55s"}
	opts, err := remoteClientOptions(&remoteTarget{BaseURL: ctx.URL, CityName: "hub", Ctx: ctx})
	if err != nil {
		t.Fatalf("remoteClientOptions: %v", err)
	}
	if opts.MailReadTimeout != 55*time.Second {
		t.Fatalf("MailReadTimeout = %v, want 55s", opts.MailReadTimeout)
	}
	for _, bad := range []string{"soon", "0s", "-5s"} {
		ctx.MailReadTimeout = bad
		if _, err := remoteClientOptions(&remoteTarget{BaseURL: ctx.URL, CityName: "hub", Ctx: ctx}); err == nil {
			t.Errorf("remoteClientOptions accepted mail_read_timeout %q", bad)
		}
	}
}
