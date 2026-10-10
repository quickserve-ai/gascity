package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestProviderCatalogDoctorFixAddsMissingBuiltinAlias(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
provider = "claude"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	check := newProviderCatalogDoctorCheck(cityDir)
	before := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if before.Status != doctor.StatusError {
		t.Fatalf("status before fix = %v, want error", before.Status)
	}
	if !check.CanFix() {
		t.Fatal("CanFix = false, want true for missing builtin provider")
	}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	after := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if after.Status != doctor.StatusOK {
		t.Fatalf("status after fix = %v, want OK; details=%v", after.Status, after.Details)
	}
	data, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[providers.claude]\nbase = \"builtin:claude\"") {
		t.Fatalf("city.toml missing builtin alias:\n%s", data)
	}
}

func TestProviderCatalogReadinessAdvisoryCountsImportedProvidersAsExplicit(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, "packs", "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[imports.local]
source = "packs/local"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "packs", "local", "pack.toml"), []byte(`[pack]
name = "local"
schema = 2

[providers.codex]
base = "builtin:codex"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	oldProbe := initProbeProvidersReadiness
	initProbeProvidersReadiness = func(_ context.Context, providers []string, fresh bool) (map[string]api.ReadinessItem, error) {
		if !fresh {
			t.Fatal("readiness advisory must use fresh probes")
		}
		out := make(map[string]api.ReadinessItem, len(providers))
		for _, provider := range providers {
			out[provider] = api.ReadinessItem{
				Name:        provider,
				DisplayName: provider,
				Status:      api.ProbeStatusNotInstalled,
			}
		}
		out["claude"] = api.ReadinessItem{Name: "claude", DisplayName: "Claude Code", Status: api.ProbeStatusConfigured}
		out["codex"] = api.ReadinessItem{Name: "codex", DisplayName: "Codex", Status: api.ProbeStatusConfigured}
		return out, nil
	}
	t.Cleanup(func() { initProbeProvidersReadiness = oldProbe })

	result := newProviderCatalogReadinessAdvisoryCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning", result.Status)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "Claude Code") {
		t.Fatalf("details = %v, want only missing claude advisory", result.Details)
	}
	if strings.Contains(strings.Join(result.Details, "\n"), "Codex") {
		t.Fatalf("imported codex provider should count as explicit: %v", result.Details)
	}
}

// TestProviderModelWindowFlagsTheAmbiguous200kPin covers katya's review
// condition on ga-a306b1. sessionlog resolves fable to 1M WITHOUT the "[1m]"
// suffix, because the suffix never reaches the transcript — which is right for
// the 13 providers really on the 1M tier, but makes a seat genuinely pinned to
// fable-5-200k read 5x too EMPTY. Over-empty is the dangerous direction (the
// seat sails past its handoff bands), so the trade-off must not be silent: it
// cannot be resolved in the transcript, but it CAN be named at config time.
func TestProviderModelWindowFlagsTheAmbiguous200kPin(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
provider = "claude"

[providers.claude]
base = "builtin:claude"

[providers.fable-legacy]
base = "builtin:claude"
option_defaults = { model = "fable-5-200k" }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	result := newProviderModelWindowAmbiguityCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning; message=%q details=%v", result.Status, result.Message, result.Details)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "fable-legacy") {
		t.Fatalf("details must name the offending provider: %v", result.Details)
	}
	if !strings.Contains(joined, "5x too empty") {
		t.Fatalf("details must name the CONSEQUENCE, not just the pin: %v", result.Details)
	}
}

// The other half: the pins a seat SHOULD be using must not warn, or the check
// becomes noise and gets ignored — which is how a real warning goes unread.
func TestProviderModelWindowAcceptsTheSupportedFablePins(t *testing.T) {
	for _, model := range []string{"fable", "fable-5-1", "fable-5", "opus", "sonnet"} {
		t.Run(model, func(t *testing.T) {
			cityDir := t.TempDir()
			toml := `[workspace]
provider = "claude"

[providers.claude]
base = "builtin:claude"
option_defaults = { model = "` + model + `" }
`
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
				t.Fatal(err)
			}
			result := newProviderModelWindowAmbiguityCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
			if result.Status != doctor.StatusOK {
				t.Fatalf("model %q: status = %v, want OK; details=%v", model, result.Status, result.Details)
			}
		})
	}
}

// The launch-time surface is the whole point of this check, so lock it: a
// doctor-only warning is heard only when someone runs gc doctor by hand, and
// nothing schedules that. If this ever flips back to false the check silently
// stops meeting the condition it was written for (ga-a306b1, katya's review).
func TestProviderModelWindowIsSurfacedAtStartupWarmup(t *testing.T) {
	if !newProviderModelWindowAmbiguityCheck(t.TempDir()).WarmupEligible() {
		t.Fatal("WarmupEligible() = false; gc start would not surface the model-window warning")
	}
}
