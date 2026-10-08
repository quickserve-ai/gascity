package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// OptInSkillsDir names the directory under a pack's skills/ that holds its
// opt-in skills: skills/opt-in/<name>/SKILL.md. The shared-catalog readers
// list only skills/<name>/SKILL.md, so no agent receives a skill under this
// directory unless its opt_in_skills names it (Agent.OptInSkills).
const OptInSkillsDir = "opt-in"

// DiscoveredSkillCatalog is a convention-discovered shared skills/
// catalog from a pack. One entry represents one pack-level skills root.
type DiscoveredSkillCatalog struct {
	SourceDir   string
	PackDir     string
	PackName    string
	BindingName string
}

// DiscoverPackSkills reports the top-level shared skills/ directory for
// a pack when it exists.
func DiscoverPackSkills(fs fsys.FS, packDir, packName string) ([]DiscoveredSkillCatalog, error) {
	skillsDir := filepath.Join(packDir, "skills")
	info, err := fs.Stat(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}
	return []DiscoveredSkillCatalog{{
		SourceDir: skillsDir,
		PackDir:   packDir,
		PackName:  packName,
	}}, nil
}

// SharedSkillCatalogs returns the imported shared skill catalogs in scope
// for rigName: the rig's own imports first, then the city's. An empty
// rigName returns the city's catalogs alone. The city pack's own skills/
// root is PackSkillsDir and is not part of the result.
func (c *City) SharedSkillCatalogs(rigName string) []DiscoveredSkillCatalog {
	if c == nil {
		return nil
	}
	var catalogs []DiscoveredSkillCatalog
	if rigName != "" && c.RigPackSkills != nil {
		catalogs = append(catalogs, c.RigPackSkills[rigName]...)
	}
	return append(catalogs, c.PackSkills...)
}

// SkillRigScope returns the declared rig whose shared skill catalogs reach
// agent, or "" when only the city's catalogs do. Only a rig-scoped agent
// attached to a declared rig gets a rig: a city-scoped agent, or an inline
// agent whose Dir is only a working-directory hint, must not pull in a rig's
// catalogs because its Dir happens to match a rig name.
func SkillRigScope(agent *Agent, rigs []Rig) string {
	if agent == nil {
		return ""
	}
	if strings.TrimSpace(agent.Scope) == "city" {
		return ""
	}
	dir := strings.TrimSpace(agent.Dir)
	if dir == "" {
		return ""
	}
	for _, rig := range rigs {
		if rig.Name == dir {
			return rig.Name
		}
	}
	return ""
}

// OptInSkill is one opt-in skill a pack ships under skills/opt-in/<leaf>/.
type OptInSkill struct {
	// Name is the catalog name an agent selects the skill by:
	// "<binding>.<leaf>" for an imported pack, "<leaf>" for the city pack's
	// own skills. It is also the skill's name in the agent's skill directory.
	Name string
	// Dir is the skill directory, the one holding SKILL.md.
	Dir string
	// Origin labels the catalog the skill came from, as the shared catalog
	// does: "city", or the import's binding (else pack) name.
	Origin string
}

// DiscoverOptInSkills lists the opt-in skills under the city pack's skills
// root (packSkillsDir, unqualified names) and under each imported catalog
// (binding-qualified names). When two roots ship one name, the first wins,
// in the precedence the shared catalog uses: the city pack, then catalogs
// in order. A directory counts as a skill when it holds a SKILL.md, as in
// the shared catalog. The result is sorted by Name.
//
// An opt-in root that cannot be read does not stop discovery: the skills
// of every readable root are returned, with an error that joins one error
// per unreadable root. The opt-in index is optional, so a caller building
// the default catalog reports the error and keeps going.
func DiscoverOptInSkills(fs fsys.FS, packSkillsDir string, catalogs []DiscoveredSkillCatalog) ([]OptInSkill, error) {
	var out []OptInSkill
	var errs []error
	seen := make(map[string]bool)
	add := func(skillsRoot, binding, origin string) {
		root := filepath.Join(skillsRoot, OptInSkillsDir)
		entries, err := fs.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				return
			}
			if info, statErr := fs.Stat(root); statErr == nil && !info.IsDir() {
				return
			}
			errs = append(errs, fmt.Errorf("reading opt-in skills %q: %w", root, err))
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root, e.Name())
			if _, err := fs.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
				continue
			}
			name := e.Name()
			if binding != "" {
				name = binding + "." + name
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, OptInSkill{Name: name, Dir: dir, Origin: origin})
		}
	}
	if packSkillsDir != "" {
		add(packSkillsDir, "", "city")
	}
	for _, catalog := range catalogs {
		binding := strings.TrimSpace(catalog.BindingName)
		origin := binding
		if origin == "" {
			origin = strings.TrimSpace(catalog.PackName)
		}
		if origin == "" {
			origin = "import"
		}
		add(catalog.SourceDir, binding, origin)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errors.Join(errs...)
}

// AgentOptInSkills resolves agent's opt_in_skills against the opt-in skills
// in its scope, returning the selected skills in name order with
// duplicates dropped. A name that resolves to no opt-in skill is skipped;
// ValidateOptInSkills rejects such a config at load. An unreadable opt-in
// root returns the skills resolved from the readable ones with the read
// error (DiscoverOptInSkills).
func AgentOptInSkills(fs fsys.FS, cfg *City, agent *Agent) ([]OptInSkill, error) {
	if cfg == nil || agent == nil || len(agent.OptInSkills) == 0 {
		return nil, nil
	}
	available, err := DiscoverOptInSkills(fs, cfg.PackSkillsDir, cfg.SharedSkillCatalogs(SkillRigScope(agent, cfg.Rigs)))
	return selectOptInSkills(available, agent.OptInSkills), err
}

func selectOptInSkills(available []OptInSkill, names []string) []OptInSkill {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[strings.TrimSpace(n)] = true
	}
	var out []OptInSkill
	for _, s := range available {
		if want[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// ValidateOptInSkills checks that every name in every agent's opt_in_skills
// resolves to an opt-in skill in that agent's scope. An unknown name is a
// config error naming the agent, the key, the name and the opt-in skills the
// agent could select. So is a name a default skill in scope also has: the
// sink holds one link per name and the default wins, so the selection would
// silently deliver the default; the error names both directories. (Legacy
// compatibility bootstrap packs are not consulted; the catalog still records
// such a shadow.) It runs once composition, patches and rig overrides have
// settled each agent's list.
func ValidateOptInSkills(fs fsys.FS, cfg *City) error {
	if cfg == nil {
		return nil
	}
	byScope := make(map[string][]OptInSkill)
	readErr := make(map[string]error)
	loaded := make(map[string]bool)
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if len(a.OptInSkills) == 0 {
			continue
		}
		scope := SkillRigScope(a, cfg.Rigs)
		if !loaded[scope] {
			available, err := DiscoverOptInSkills(fs, cfg.PackSkillsDir, cfg.SharedSkillCatalogs(scope))
			byScope[scope] = available
			readErr[scope] = err
			loaded[scope] = true
		}
		known := make(map[string]OptInSkill, len(byScope[scope]))
		for _, s := range byScope[scope] {
			known[s.Name] = s
		}
		for _, name := range a.OptInSkills {
			if s, ok := known[strings.TrimSpace(name)]; ok {
				if dir, shadowed := defaultSkillDir(fs, cfg.PackSkillsDir, cfg.SharedSkillCatalogs(scope), s.Name); shadowed {
					return fmt.Errorf("agent %q: opt_in_skills names %q, but the default skill %s has the same name and every agent in scope gets it instead of the opt-in skill %s; rename one of them",
						a.QualifiedName(), name, dir, s.Dir)
				}
				continue
			}
			err := fmt.Errorf("agent %q: opt_in_skills names %q, which is not an opt-in skill in this agent's scope (a pack's skills/%s/<name>/SKILL.md, named <binding>.<name>); available: %s",
				a.QualifiedName(), name, OptInSkillsDir, describeOptInSkills(byScope[scope]))
			if readErr[scope] != nil {
				return fmt.Errorf("%w; some opt-in skills could not be read: %w", err, readErr[scope])
			}
			return err
		}
	}
	return nil
}

func describeOptInSkills(skills []OptInSkill) string {
	if len(skills) == 0 {
		return "none"
	}
	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}

// defaultSkillDir returns the directory of the default skill named name in
// the scope of packSkillsDir and catalogs, mapping names as the shared
// catalog does: "<leaf>" in the city pack's skills root, "<binding>.<leaf>"
// in a bound catalog, "<leaf>" in an unbound one.
func defaultSkillDir(fs fsys.FS, packSkillsDir string, catalogs []DiscoveredSkillCatalog, name string) (string, bool) {
	if packSkillsDir != "" {
		if dir, ok := skillDirAt(fs, packSkillsDir, name); ok {
			return dir, true
		}
	}
	for _, c := range catalogs {
		leaf := name
		if binding := strings.TrimSpace(c.BindingName); binding != "" {
			var found bool
			if leaf, found = strings.CutPrefix(name, binding+"."); !found {
				continue
			}
		}
		if dir, ok := skillDirAt(fs, c.SourceDir, leaf); ok {
			return dir, true
		}
	}
	return "", false
}

// skillDirAt reports whether root/leaf is a skill as the shared catalog
// reads one: a directory entry (not a symlink to one) holding SKILL.md.
func skillDirAt(fs fsys.FS, root, leaf string) (string, bool) {
	if leaf == "" || strings.ContainsRune(leaf, filepath.Separator) {
		return "", false
	}
	dir := filepath.Join(root, leaf)
	info, err := fs.Lstat(dir)
	if err != nil || !info.IsDir() {
		return "", false
	}
	if _, err := fs.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		return "", false
	}
	return dir, true
}

// OptInLayoutWarnings reports skills roots whose reserved opt-in/
// directory itself holds a SKILL.md. The default catalog then treats the
// directory as one default skill named "<binding>.opt-in" (or "opt-in" in
// the city pack) and delivers it to every agent, which is not what a pack
// putting skills under opt-in/ means.
func OptInLayoutWarnings(fs fsys.FS, cfg *City) []string {
	if cfg == nil {
		return nil
	}
	type root struct{ dir, binding string }
	roots := []root{}
	if cfg.PackSkillsDir != "" {
		roots = append(roots, root{dir: cfg.PackSkillsDir})
	}
	add := func(catalogs []DiscoveredSkillCatalog) {
		for _, c := range catalogs {
			roots = append(roots, root{dir: c.SourceDir, binding: strings.TrimSpace(c.BindingName)})
		}
	}
	add(cfg.PackSkills)
	rigNames := make([]string, 0, len(cfg.RigPackSkills))
	for name := range cfg.RigPackSkills {
		rigNames = append(rigNames, name)
	}
	sort.Strings(rigNames)
	for _, name := range rigNames {
		add(cfg.RigPackSkills[name])
	}
	var out []string
	seen := make(map[root]bool)
	for _, r := range roots {
		if seen[r] {
			continue
		}
		seen[r] = true
		if _, ok := skillDirAt(fs, r.dir, OptInSkillsDir); !ok {
			continue
		}
		name := OptInSkillsDir
		if r.binding != "" {
			name = r.binding + "." + name
		}
		out = append(out, fmt.Sprintf("%s: skills/%s/ is reserved for opt-in skills, but this SKILL.md makes it a default skill %q that every agent in scope receives; move it to skills/%s/<name>/SKILL.md",
			filepath.Join(r.dir, OptInSkillsDir, "SKILL.md"), OptInSkillsDir, name, OptInSkillsDir))
	}
	return out
}
