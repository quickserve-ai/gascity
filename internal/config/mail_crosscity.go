package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// MailCrossCityConfig enables city-qualified mail addressing: recipients of
// the form <city>/<address>, where the first path segment names a city and
// the remainder is that city's own address form. Authoring the section is the
// feature flag; omitting it keeps every recipient resolving exactly as today.
type MailCrossCityConfig struct {
	// City is this city's own segment in city-qualified addresses, and it is
	// REQUIRED when the section is present: the CLI and the API derive the
	// effective city name differently (site binding, workspace name, the
	// supervisor's registered name), so a defaulted name could stamp two
	// spellings of this city on mail and strand replies. <City>/<address> and
	// <address> are one mailbox.
	City string `toml:"city"`
	// Cities lists the peer cities addressable as <city>/<address>. A
	// recipient naming a listed city resolves against the roster and is never
	// looked up in the local session store.
	Cities []string `toml:"cities"`
}

// MailCityRoster returns the local city name and peer cities for
// cross-city mail addressing, or ("", nil) when the section is absent.
// fallbackCityName seeds the local name when neither [mail.crosscity] city
// nor the workspace identity names it — callers pass the city directory's
// base name, mirroring every other effective-city-name derivation.
func (c *City) MailCityRoster(fallbackCityName string) (string, []string) {
	if c == nil || c.Mail.CrossCity == nil {
		return "", nil
	}
	cc := c.Mail.CrossCity
	local := strings.TrimSpace(cc.City)
	if local == "" {
		local = EffectiveCityName(c, fallbackCityName)
	}
	return local, cc.Cities
}

// ValidateMailCrossCity validates the [mail.crosscity] section of a composed
// config: a present section must name this city explicitly (city) and list at
// least one syntactically valid, duplicate-free peer city; the local city must
// not list itself; and no rig or agent directory may share a name with a listed city — the first mail-address segment must
// bind unambiguously to either a rig or a city, never both (a collision here
// would silently rebind existing rig-qualified mail). cityRoot supplies the
// directory-derived fallback for the effective local city name.
func ValidateMailCrossCity(cfg *City, cityRoot string) error {
	if cfg == nil || cfg.Mail.CrossCity == nil {
		return nil
	}
	cc := cfg.Mail.CrossCity
	if strings.TrimSpace(cc.City) == "" {
		return fmt.Errorf("[mail.crosscity] city is required: name this city's own address segment explicitly (the CLI and the API must agree on it)")
	}
	if len(cc.Cities) == 0 {
		return fmt.Errorf("[mail.crosscity] cities must list at least one peer city")
	}
	fallback := ""
	if cityRoot != "" {
		fallback = filepath.Base(filepath.Clean(cityRoot))
	}
	local, _ := cfg.MailCityRoster(fallback)
	if err := validateMailCityName(local); err != nil {
		return fmt.Errorf("[mail.crosscity] city: %w", err)
	}
	seen := make(map[string]bool, len(cc.Cities))
	for _, city := range cc.Cities {
		if err := validateMailCityName(city); err != nil {
			return fmt.Errorf("[mail.crosscity] cities: %w", err)
		}
		if seen[city] {
			return fmt.Errorf("[mail.crosscity] cities lists duplicate city %q", city)
		}
		seen[city] = true
		if city == local {
			return fmt.Errorf("[mail.crosscity] cities must not list this city's own city %q", city)
		}
	}
	for i := range cfg.Rigs {
		if seen[cfg.Rigs[i].Name] || cfg.Rigs[i].Name == local {
			return fmt.Errorf("rig %q collides with a [mail.crosscity] city of the same name: the first mail-address segment must name either a rig or a city, never both", cfg.Rigs[i].Name)
		}
	}
	for i := range cfg.Agents {
		dir := cfg.Agents[i].Dir
		if dir == "" {
			continue
		}
		// A nested dir ("projects/backend") is addressed by its LEADING
		// segment ("projects/backend/worker" starts with "projects"), and
		// that segment is what city classification reads, so the collision
		// is between the leading segment and a city name.
		segment := LeadingAddressSegment(dir)
		if seen[segment] || segment == local {
			return fmt.Errorf("agent %q dir %q collides with a [mail.crosscity] city of the same name: the first mail-address segment must name either a local agent directory or a city, never both", cfg.Agents[i].Name, dir)
		}
	}
	return nil
}

// LeadingAddressSegment returns the first "/"-delimited segment of a local
// address prefix (a rig name or an agent dir): the part a mail address
// starts with, and the only part city classification ever compares.
func LeadingAddressSegment(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if segment, _, found := strings.Cut(prefix, "/"); found {
		return segment
	}
	return prefix
}

// RigNames returns the configured rig names. Cross-city mail resolution uses
// them as the first-segment scopes that stay local, so a failed rig-qualified
// lookup keeps its ordinary error instead of an unknown-city refusal.
func (c *City) RigNames() []string {
	if c == nil || len(c.Rigs) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Rigs))
	for i := range c.Rigs {
		if c.Rigs[i].Name != "" {
			names = append(names, c.Rigs[i].Name)
		}
	}
	return names
}

// validateMailCityName enforces the city-segment syntax: non-empty, and
// composed of letters, digits, "-", "_", or "." — above all no "/", which is
// the address separator, and no whitespace.
func validateMailCityName(name string) error {
	if name == "" {
		return fmt.Errorf("city name is empty")
	}
	if strings.IndexFunc(name, func(r rune) bool { return !isMailCityNameRune(r) }) >= 0 {
		return fmt.Errorf("invalid city name %q: use letters, digits, '-', '_', or '.'", name)
	}
	return nil
}

func isMailCityNameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
}
