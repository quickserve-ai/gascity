package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

type providerCatalogDoctorCheck struct {
	cityPath string
}

func newProviderCatalogDoctorCheck(cityPath string) *providerCatalogDoctorCheck {
	return &providerCatalogDoctorCheck{cityPath: cityPath}
}

func (c *providerCatalogDoctorCheck) Name() string { return "provider-catalog" }

func (c *providerCatalogDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusOK
		r.Message = "provider catalog check skipped until expanded config loads"
		return r
	}
	refs := config.MissingProviderReferences(cfg)
	if len(refs) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "provider references are explicit"
		return r
	}
	r.Status = doctor.StatusError
	r.Message = fmt.Sprintf("%d provider reference(s) missing from [providers]", len(refs))
	r.Details = providerReferenceDetails(refs)
	if len(missingBuiltinProviderRefs(refs)) > 0 {
		r.FixHint = "run `gc doctor --fix` to add missing builtin provider aliases"
	} else {
		r.FixHint = "add the missing [providers.*] entries manually"
	}
	return r
}

func (c *providerCatalogDoctorCheck) CanFix() bool {
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		return false
	}
	return len(missingBuiltinProviderRefs(config.MissingProviderReferences(cfg))) > 0
}

func (c *providerCatalogDoctorCheck) Fix(_ *doctor.CheckContext) error {
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		return err
	}
	return appendBuiltinProviderAliases(c.cityPath, missingBuiltinProviderRefs(config.MissingProviderReferences(cfg)))
}

func (c *providerCatalogDoctorCheck) WarmupEligible() bool { return false }

type providerCatalogReadinessAdvisoryCheck struct {
	cityPath string
}

func newProviderCatalogReadinessAdvisoryCheck(cityPath string) *providerCatalogReadinessAdvisoryCheck {
	return &providerCatalogReadinessAdvisoryCheck{cityPath: cityPath}
}

func (c *providerCatalogReadinessAdvisoryCheck) Name() string {
	return "provider-catalog-local-readiness"
}

func (c *providerCatalogReadinessAdvisoryCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusOK
		r.Message = "local provider advisory skipped until expanded config loads"
		return r
	}
	names := api.ProviderReadinessNames()
	items, err := initProbeProvidersReadiness(context.Background(), names, true)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("could not probe local provider readiness: %v", err)
		r.Severity = doctor.SeverityAdvisory
		return r
	}
	var missing []string
	for _, name := range names {
		item, ok := items[name]
		if !ok || item.Status != api.ProbeStatusConfigured {
			continue
		}
		if _, explicit := cfg.Providers[name]; explicit {
			continue
		}
		displayName := strings.TrimSpace(item.DisplayName)
		if displayName == "" {
			displayName = name
		}
		missing = append(missing, fmt.Sprintf("%s is configured locally but not listed in [providers.%s]", displayName, name))
	}
	if len(missing) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "configured local providers are explicit"
		return r
	}
	r.Status = doctor.StatusWarning
	r.Severity = doctor.SeverityAdvisory
	r.Message = fmt.Sprintf("%d configured local provider(s) not explicit", len(missing))
	r.Details = missing
	r.FixHint = "add provider aliases for local CLIs you want this city to use"
	return r
}

func (c *providerCatalogReadinessAdvisoryCheck) CanFix() bool { return false }

func (c *providerCatalogReadinessAdvisoryCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *providerCatalogReadinessAdvisoryCheck) WarmupEligible() bool { return false }

func loadCityConfigAllowMissingProviderReferences(cityPath string) (*config.City, error) {
	tomlPath := filepath.Join(cityPath, citylayout.CityConfigFile)
	if err := ensureBuiltinPacksForConfigLoad(fsys.OSFS{}, tomlPath, io.Discard); err != nil {
		return nil, err
	}
	cfg, _, err := config.LoadWithIncludesOptions(
		fsys.OSFS{},
		tomlPath,
		config.LoadOptions{AllowMissingProviderReferences: true},
	)
	if err != nil {
		return nil, err
	}
	applyFeatureFlags(cfg)
	return cfg, nil
}

func providerReferenceDetails(refs []config.ProviderReference) []string {
	details := make([]string, 0, len(refs))
	for _, ref := range refs {
		switch ref.Kind {
		case "workspace":
			details = append(details, fmt.Sprintf("workspace.provider %q", ref.Provider))
		case "agent":
			details = append(details, fmt.Sprintf("agent %q provider %q", ref.Agent, ref.Provider))
		default:
			details = append(details, fmt.Sprintf("%s provider %q", ref.Kind, ref.Provider))
		}
	}
	return details
}

func missingBuiltinProviderRefs(refs []config.ProviderReference) []string {
	builtins := config.BuiltinProviders()
	seen := make(map[string]bool)
	for _, ref := range refs {
		if _, ok := builtins[ref.Provider]; !ok {
			continue
		}
		seen[ref.Provider] = true
	}
	var out []string
	for _, name := range config.BuiltinProviderOrder() {
		if seen[name] {
			out = append(out, name)
		}
	}
	return out
}

// appendBuiltinProviderAliases is exempt from config.ResolveCityRewritePath:
// os.WriteFile truncates through a symlinked city.toml and the append keeps
// every existing byte, so neither link identity nor unknown keys are at
// risk. Switching this to a temp-file + rename write revokes the exemption.
func appendBuiltinProviderAliases(cityPath string, providers []string) error {
	if len(providers) == 0 {
		return nil
	}
	tomlPath := filepath.Join(cityPath, citylayout.CityConfigFile)
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		return err
	}
	var b strings.Builder
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteString("\n")
	}
	for _, provider := range providers {
		b.WriteString("\n")
		b.WriteString("[providers.")
		b.WriteString(provider)
		b.WriteString("]\n")
		b.WriteString("base = \"")
		b.WriteString(config.BasePrefixBuiltin)
		b.WriteString(provider)
		b.WriteString("\"\n")
	}
	return os.WriteFile(tomlPath, append(data, []byte(b.String())...), 0o644)
}

// ambiguousWindowModelPins maps a model choice whose emitted model token the
// context meter CANNOT tell apart from a different-window sibling, to the
// explanation an operator needs. Selecting one of these is legal and the seat
// will run fine — what breaks is the CONTEXT READING for that seat.
//
// "fable-5-200k" emits a bare `claude-fable-5`, which is byte-identical to what
// a 1M fable session writes into its transcript (the "[1m]" suffix is a
// launch-flag artifact and never reaches the transcript). sessionlog and
// context_inject therefore resolve BOTH to 1M. For a seat genuinely on the 200k
// tier that meter reads 5x too EMPTY, which is the dangerous direction: the seat
// sails past the handoff bands and hits its real ceiling without warning.
//
// The pin stays in the enum because StripFlags needs it to migrate command
// strings baked before ga-ljcm7c. It is a migration artifact, not a seat choice,
// and nothing should be selecting it — hence a doctor check rather than a
// refusal, so an existing city says so out loud instead of failing to load
// (ga-a306b1, katya's review condition).
var ambiguousWindowModelPins = map[string]string{
	"fable-5-200k": "emits a bare claude-fable-5, which the context meter reads as 1M; " +
		"a seat really on the 200k tier will read 5x too empty and blow past its handoff bands",
}

type providerModelWindowAmbiguityCheck struct{ cityPath string }

func newProviderModelWindowAmbiguityCheck(cityPath string) *providerModelWindowAmbiguityCheck {
	return &providerModelWindowAmbiguityCheck{cityPath: cityPath}
}

func (c *providerModelWindowAmbiguityCheck) Name() string { return "provider-model-window" }

func (c *providerModelWindowAmbiguityCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusOK
		r.Message = "model-window advisory skipped until expanded config loads"
		return r
	}

	var found []string
	for name, spec := range cfg.Providers {
		if why, bad := ambiguousWindowModelPins[spec.OptionDefaults["model"]]; bad {
			found = append(found, fmt.Sprintf("[providers.%s] model = %q: %s",
				name, spec.OptionDefaults["model"], why))
		}
	}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if why, bad := ambiguousWindowModelPins[agent.OptionDefaults["model"]]; bad {
			found = append(found, fmt.Sprintf("agent %s: model = %q: %s",
				agent.QualifiedName(), agent.OptionDefaults["model"], why))
		}
	}
	if len(found) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "no seat selects a model pin the context meter cannot resolve"
		return r
	}

	sort.Strings(found) // stable output: map iteration order is random
	r.Status = doctor.StatusWarning
	r.Severity = doctor.SeverityAdvisory
	r.Message = fmt.Sprintf("%d seat(s) select a model pin with an unresolvable context window", len(found))
	r.Details = found
	r.FixHint = "use \"fable\" (latest, 1M) or \"fable-5-1\"; the 200k pin exists only so pre-ga-ljcm7c command strings still migrate"
	return r
}

func (c *providerModelWindowAmbiguityCheck) CanFix() bool { return false }

func (c *providerModelWindowAmbiguityCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible returns TRUE, unlike its neighbors in this file. katya's
// review condition on ga-a306b1 was that selecting an unresolvable pin be loud
// at CONFIG OR LAUNCH, and doctor-only does not meet it: nothing in orders/ or
// packs/*/orders runs `gc doctor` on a schedule, so a doctor-only warning is
// heard only when a human happens to run it. `gc start`'s warmup pass DOES
// surface eligible checks — warmup collects every result at StatusWarning or
// above, mails the report and writes it to stderr — so this is the launch-time
// surface the condition asks for.
//
// Safe to run in warmup because it is pure config: it loads the expanded city
// config and walks two maps. No network, no Dolt, no filesystem beyond the
// config read, and it reports OK when the config cannot be expanded yet.
func (c *providerModelWindowAmbiguityCheck) WarmupEligible() bool { return true }
