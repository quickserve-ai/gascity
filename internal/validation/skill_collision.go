// Package validation hosts startup-time validators that guard against
// configurations the Gas City runtime cannot safely materialize. The
// validators are pure: they take a parsed *config.City and return
// diagnostic structs, never touching I/O.
package validation

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
)

// citySentinel is the ScopeRoot marker used for city-scoped groupings.
// The validator operates on an in-memory *config.City and does not know
// the filesystem path of the city root; callers that want to substitute
// a real path (e.g. the doctor check) can do so when formatting errors.
const citySentinel = "<city>"

// SkillCollision describes a skill name that agents materializing into
// the same scope root and the same on-disk sink cannot all have: an
// agent-local skill name provided by two or more agents, or a name that
// resolves to two or more different source directories across the
// agents' agent-local skills and opted-in skills. Providers are grouped
// by resolved sink, not by name: distinct providers that share a sink
// (codex and pi both use .agents/skills) collide with each other.
type SkillCollision struct {
	// ScopeRoot is the scope root the colliding agents materialize
	// into. For rig-scoped agents this is the rig's configured path
	// (which may be relative to the city root). For city-scoped
	// agents this is the sentinel "<city>".
	ScopeRoot string
	// Vendor names one provider contributing to the collision — the
	// one belonging to the first agent in AgentNames. Grouping is by
	// resolved sink, so when providers share a sink the other
	// contributors' providers may differ; this field exists to give
	// operator-facing messages a concrete provider to name.
	Vendor string
	// SkillName is the colliding agent-local skill name.
	SkillName string
	// AgentNames lists, in sorted order, every agent providing the
	// same skill name into this (ScopeRoot, sink) pair.
	AgentNames []string
	// OptIn reports that at least one of the agents provides the name by
	// opting in to a pack's opt-in skill (Agent.OptInSkills) rather than
	// from its own agent-local skills.
	OptIn bool
}

// OptInAtScopeRoot reports whether stage 1 writes an agent's opted-in
// skills into its scope-root sink. An agent with its own work_dir gets
// them in that work_dir from the per-session pass instead, where only its
// own skills are wanted, so they cannot collide with another agent's.
type OptInAtScopeRoot func(agent *config.Agent) bool

// ValidateSkillCollisions checks agent-local skills only: two agents
// providing one agent-local skill name into the same sink collide. It is
// ValidateSkillCollisionsWithOptIn without opt-in skills.
func ValidateSkillCollisions(cfg *config.City) []SkillCollision {
	return ValidateSkillCollisionsWithOptIn(cfg, nil)
}

// ValidateSkillCollisionsWithOptIn groups agents by (scope-root, resolved
// sink), builds the multi-map skill-name → [agent-names] over each agent's
// agent-local skills and, where atScopeRoot places them in that sink, its
// opted-in skills, and returns one SkillCollision entry per name that two
// or more agents provide as agent-local skills, or that resolves to two or
// more source directories. Two agents opting in to the same skill share
// one source and do not collide. An agent's own agent-local skill
// overrides its opt-in of the same name, as it overrides a shared skill.
// A nil atScopeRoot counts no opt-in skills. Returns nil when there are no
// collisions. The sink comes from
// materialize.VendorSink, so this gate stays in step with what the
// materializer actually writes — including providers that share a sink.
//
// Scope-root derivation mirrors the spec:
//   - agent.Scope == "city" → scope root = city sentinel
//   - agent.Scope == "rig"  → scope root = rig path (from agent.Dir
//     looked up in cfg.Rigs); if no matching rig is found the agent's
//     Dir is used as-is (supports inline agents with a custom Dir)
//   - empty scope is treated as "rig" (the default)
//
// Agents whose provider resolves to no sink contribute nothing — they
// have nowhere to collide. Agents with no agent-local skills and no
// opt-in skills also contribute nothing.
//
// Collisions are returned sorted by (ScopeRoot, Vendor, SkillName) so
// tests and user-facing output are stable.
func ValidateSkillCollisionsWithOptIn(cfg *config.City, atScopeRoot OptInAtScopeRoot) []SkillCollision {
	if cfg == nil || len(cfg.Agents) == 0 {
		return nil
	}

	rigPath := make(map[string]string, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		rigPath[rig.Name] = rig.Path
	}

	type bucketKey struct{ scope, sink string }
	// buckets: scope+sink → skillName → the agents providing it.
	buckets := make(map[bucketKey]map[string]*skillProviders)

	for i := range cfg.Agents {
		a := &cfg.Agents[i]

		// Agent provider falls back to workspace provider when not
		// set per-agent — matches the effective-provider resolution
		// used throughout the binary. Without this fallback,
		// workspace-level "provider = claude" configs with
		// non-overriding agents would bypass collision detection.
		// TrimSpace mirrors cmd/gc/skill_integration.go's
		// effectiveAgentProvider so whitespace-only overrides don't
		// bypass either the materializer or this gate.
		vendor := strings.TrimSpace(a.Provider)
		if vendor == "" {
			vendor = cfg.Workspace.Provider
		}
		sink, ok := materialize.VendorSink(vendor)
		if !ok {
			continue
		}

		scope := scopeRootFor(a, rigPath)
		if scope == "" {
			// Rig-scoped agent without a resolvable rig — skip. It
			// can't contribute a concrete sink anyway.
			continue
		}

		names := listAgentLocalSkills(a.SkillsDir)
		// Opt-in names resolve against the catalogs in the agent's
		// scope, and count in this sink only where stage 1 writes them.
		// An unresolvable name failed config load already, and a read
		// error leaves the unread opt-ins out, as a read error leaves
		// agent-local skills out above.
		var optIn []config.OptInSkill
		if atScopeRoot != nil && len(a.OptInSkills) > 0 && atScopeRoot(a) {
			optIn, _ = config.AgentOptInSkills(fsys.OSFS{}, cfg, a)
		}
		if len(names) == 0 && len(optIn) == 0 {
			continue
		}

		key := bucketKey{scope: scope, sink: sink}
		bucket := buckets[key]
		if bucket == nil {
			bucket = make(map[string]*skillProviders)
			buckets[key] = bucket
		}
		provide := func(name string) *skillProviders {
			p := bucket[name]
			if p == nil {
				p = &skillProviders{agents: make(map[string]string), sources: make(map[string]bool)}
				bucket[name] = p
			}
			p.agents[a.QualifiedName()] = vendor
			return p
		}
		local := make(map[string]bool, len(names))
		for _, name := range names {
			local[name] = true
			p := provide(name)
			p.localAgents++
			p.sources[filepath.Join(a.SkillsDir, name)] = true
		}
		for _, s := range optIn {
			if local[s.Name] {
				continue
			}
			p := provide(s.Name)
			p.optIn = true
			p.sources[s.Dir] = true
		}
	}

	var collisions []SkillCollision
	for key, bucket := range buckets {
		for skillName, p := range bucket {
			if p.localAgents < 2 && len(p.sources) < 2 {
				continue
			}
			names := make([]string, 0, len(p.agents))
			for n := range p.agents {
				names = append(names, n)
			}
			sort.Strings(names)
			collisions = append(collisions, SkillCollision{
				ScopeRoot: key.scope,
				// Deterministic: the provider of the first agent
				// in sorted order.
				Vendor:     p.agents[names[0]],
				SkillName:  skillName,
				AgentNames: names,
				OptIn:      p.optIn,
			})
		}
	}

	sort.Slice(collisions, func(i, j int) bool {
		if collisions[i].ScopeRoot != collisions[j].ScopeRoot {
			return collisions[i].ScopeRoot < collisions[j].ScopeRoot
		}
		if collisions[i].Vendor != collisions[j].Vendor {
			return collisions[i].Vendor < collisions[j].Vendor
		}
		return collisions[i].SkillName < collisions[j].SkillName
	})
	return collisions
}

// skillProviders records, for one skill name in one sink, the agents
// providing it (agent name → that agent's provider, kept so a collision
// can name a concrete provider), how many provide it as an agent-local
// skill, the source directories it resolves to, and whether any agent
// provides it by opting in.
type skillProviders struct {
	agents      map[string]string
	localAgents int
	sources     map[string]bool
	optIn       bool
}

// scopeRootFor returns the scope-root key for an agent. Empty scope is
// treated as rig-scoped per the spec. Rig-scoped agents resolve their
// rig by Dir against cfg.Rigs; if no rig matches, Dir is used as-is
// (inline-agent case). Rig-scoped agents with empty Dir collapse to
// the city sentinel — which is defensive; a well-formed expanded
// config always stamps Dir = rigName for rig-scoped agents.
func scopeRootFor(a *config.Agent, rigPath map[string]string) string {
	scope := a.Scope
	if scope == "" {
		scope = "rig"
	}
	switch scope {
	case "city":
		return citySentinel
	case "rig":
		if a.Dir == "" {
			// No rig stamped — fall back to city sentinel so we
			// at least bucket the agent somewhere. In practice
			// pack expansion sets Dir for rig-scoped agents.
			return citySentinel
		}
		if path, ok := rigPath[a.Dir]; ok && path != "" {
			return path
		}
		return a.Dir
	default:
		// Unknown scope values are validated elsewhere
		// (config.ValidateAgents). Treat as rig for best-effort
		// bucketing.
		return a.Dir
	}
}

// listAgentLocalSkills returns the sorted list of skill names under the
// agent's local skills directory. A subdirectory counts as a skill if
// it contains a SKILL.md file (case-sensitive — matches the vendor
// convention and every existing caller).
func listAgentLocalSkills(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md")); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}
