package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
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
	// Towns maps a listed peer city to the town whose rendered roster
	// (cities/<town>/agents.json) lists that city's seats. A send to a
	// mapped city is refused unless the seat is in that list; a peer city
	// with no mapping keeps the city-level rules only.
	Towns map[string]string `toml:"towns,omitempty"`
	// RosterRoot points at the directory holding cities/<town>/agents.json
	// and overrides discovery. Unset, the roster is the pack-cache clone of
	// the imported repository that ships cities/, at its packs.lock commit;
	// with no such import there is no list and sends behave exactly as
	// before this knob existed.
	RosterRoot string `toml:"roster_root,omitempty"`
}

// RosterPinUnknown is the pin reported for a hand-pointed roster_root, for
// which no rendered commit is known.
const RosterPinUnknown = "unknown"

// MailCrossCityTowns returns the peer-city to town mapping for cross-city
// mail address lists, or nil when the section is absent.
func (c *City) MailCrossCityTowns() map[string]string {
	if c == nil || c.Mail.CrossCity == nil {
		return nil
	}
	return c.Mail.CrossCity.Towns
}

// MailCrossCityRosterSource returns the directory that holds
// cities/<town>/agents.json and the commit it was rendered at. A configured
// roster_root wins, with RosterPinUnknown as its pin. Otherwise the source
// is discovered from the city's pinned imports: the pack-cache clone
// (<GC_HOME>/cache/repos/<key>) of the one imported repository whose clone
// ships a cities/ directory, at the commit packs.lock records — read from
// disk, never fetched. No such import means no list: ("", "", nil), which
// keeps today's send behavior. More than one such repository is ambiguous
// and is an error, never resolved by map order.
func (c *City) MailCrossCityRosterSource(cityRoot string) (root, pin string, err error) {
	if c == nil || c.Mail.CrossCity == nil {
		return "", "", nil
	}
	if explicit := strings.TrimSpace(c.Mail.CrossCity.RosterRoot); explicit != "" {
		return explicit, RosterPinUnknown, nil
	}
	lock, err := readRemoteImportLock(cityRoot)
	if err != nil {
		return "", "", err
	}
	if len(lock.Packs) == 0 {
		return "", "", nil
	}
	gcHome := ImplicitGCHome()
	if gcHome == "" {
		return "", "", fmt.Errorf("resolving the cross-city roster: no GC_HOME available to locate the pack cache")
	}
	type candidate struct{ root, commit string }
	found := map[string]candidate{}
	for _, imp := range c.allPinnedImports() {
		entry, ok := lock.Packs[imp.Source]
		if !ok || entry.Commit == "" {
			continue
		}
		key := NormalizeRemoteSource(imp.Source) + "@" + entry.Commit
		if _, seen := found[key]; seen {
			continue
		}
		clone := GlobalRepoCachePath(gcHome, imp.Source, entry.Commit)
		info, statErr := os.Stat(filepath.Join(clone, "cities"))
		if statErr != nil {
			// Absent is simply "not a candidate". Anything else (a
			// permission or I/O failure) is a discovery FAILURE: a mapped
			// town must then refuse, and a second readable repository
			// must never be chosen in its place.
			if os.IsNotExist(statErr) {
				continue
			}
			return "", "", fmt.Errorf("resolving the cross-city roster: checking %s: %w", clone, statErr)
		}
		if !info.IsDir() {
			continue
		}
		found[key] = candidate{root: clone, commit: entry.Commit}
	}
	switch len(found) {
	case 0:
		return "", "", nil
	case 1:
		for _, cand := range found {
			return cand.root, cand.commit, nil
		}
	}
	keys := make([]string, 0, len(found))
	for key := range found {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return "", "", fmt.Errorf("resolving the cross-city roster: ambiguous, %d imported repositories ship cities/ (%s); set [mail.crosscity] roster_root", len(keys), strings.Join(keys, ", "))
}

// allPinnedImports lists the city-scoped imports and every rig-scoped
// import, in a stable order, for roster discovery.
func (c *City) allPinnedImports() []Import {
	var out []Import
	appendSorted := func(m map[string]Import) {
		names := make([]string, 0, len(m))
		for name := range m {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			out = append(out, m[name])
		}
	}
	appendSorted(c.Imports)
	for i := range c.Rigs {
		appendSorted(c.Rigs[i].Imports)
	}
	return out
}

// readRemoteImportLock reads <cityRoot>/packs.lock; a missing file is an
// empty lock, not an error.
func readRemoteImportLock(cityRoot string) (remoteImportLockfile, error) {
	var lock remoteImportLockfile
	data, err := os.ReadFile(filepath.Join(cityRoot, "packs.lock"))
	if err != nil {
		if os.IsNotExist(err) {
			return lock, nil
		}
		return lock, fmt.Errorf("reading packs.lock: %w", err)
	}
	if _, err := toml.Decode(string(data), &lock); err != nil {
		return lock, fmt.Errorf("parsing packs.lock: %w", err)
	}
	return lock, nil
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
	for city, town := range cc.Towns {
		if !seen[city] {
			return fmt.Errorf("[mail.crosscity.towns] %q is not a listed peer city", city)
		}
		if err := validateMailCityName(strings.TrimSpace(town)); err != nil {
			return fmt.Errorf("[mail.crosscity.towns] %s: town: %w", city, err)
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
