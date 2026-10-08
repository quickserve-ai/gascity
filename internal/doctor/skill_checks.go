package doctor

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/validation"
)

// SkillCollisionCheck surfaces agent-local skill-name collisions that
// the materializer cannot satisfy — two agents sharing a scope-root
// sink that both want to write the same skill name.
//
// The check is a thin wrapper around validation.ValidateSkillCollisionsWithOptIn;
// the validator is the single source of truth. The same function is
// invoked at `gc start` and every supervisor tick (Phase 4A); surfacing
// it here lets operators diagnose outside a startup gate.
type SkillCollisionCheck struct {
	cfg              *config.City
	cityPath         string
	optInAtScopeRoot validation.OptInAtScopeRoot
}

// NewSkillCollisionCheck builds a check that scans cfg for skill
// collisions: agent-local skills, and opted-in skills in the sinks
// optInAtScopeRoot places them in (nil checks agent-local skills only).
// cityPath is used to rewrite the "<city>" sentinel in error messages to
// the actual city root when available.
func NewSkillCollisionCheck(cfg *config.City, cityPath string, optInAtScopeRoot validation.OptInAtScopeRoot) *SkillCollisionCheck {
	return &SkillCollisionCheck{cfg: cfg, cityPath: cityPath, optInAtScopeRoot: optInAtScopeRoot}
}

// Name returns the check identifier.
func (c *SkillCollisionCheck) Name() string { return "skill-collision" }

// Run reports a hard error when any two agents share the same
// (scope-root, vendor) sink and the same agent-local skill name.
func (c *SkillCollisionCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no config loaded"
		return r
	}

	collisions := validation.ValidateSkillCollisionsWithOptIn(c.cfg, c.optInAtScopeRoot)
	if len(collisions) == 0 {
		r.Status = StatusOK
		r.Message = "no agent-local skill collisions"
		return r
	}

	r.Status = StatusError
	r.Message = FormatSkillCollisions(collisions, c.cityPath)
	r.FixHint = "rename one of the colliding agent-local skills to resolve"
	return r
}

// CanFix returns false — collisions require renaming a user's skill.
func (c *SkillCollisionCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *SkillCollisionCheck) Fix(_ *CheckContext) error { return nil }

// FormatSkillCollisions renders a user-facing multi-line message
// describing every collision. cityPath substitutes for the "<city>"
// sentinel when non-empty.
//
// Format per collision (matches engdocs/proposals/skill-materialization.md):
//
//	agent-local skill collision at scope root <path> (<vendor>):
//	  "<name>" is provided by both <agent1> and <agent2>
//	  rename one of the colliding skills to resolve
//
// A collision involving an opted-in skill (SkillCollision.OptIn) reads:
//
//	skill collision at scope root <path> (<vendor>):
//	  "<name>" resolves to different skills for both <agent1> and <agent2>
//	  select a different opt-in skill, or rename the agent-local one, to resolve
func FormatSkillCollisions(collisions []validation.SkillCollision, cityPath string) string {
	if len(collisions) == 0 {
		return ""
	}
	var b strings.Builder
	for i, c := range collisions {
		if i > 0 {
			b.WriteString("\n")
		}
		scope := c.ScopeRoot
		if scope == "<city>" && cityPath != "" {
			scope = cityPath
		}
		if c.OptIn {
			fmt.Fprintf(&b, "skill collision at scope root %s (%s):\n", scope, c.Vendor)
			fmt.Fprintf(&b, "  %q resolves to different skills for %s\n", c.SkillName, joinAgentsHuman(c.AgentNames))
			b.WriteString("  select a different opt-in skill, or rename the agent-local one, to resolve")
			continue
		}
		fmt.Fprintf(&b, "agent-local skill collision at scope root %s (%s):\n", scope, c.Vendor)
		fmt.Fprintf(&b, "  %q is provided by %s\n", c.SkillName, joinAgentsHuman(c.AgentNames))
		b.WriteString("  rename one of the colliding skills to resolve")
	}
	return b.String()
}

// joinAgentsHuman formats a list of agent names for the collision
// message. Two names use "both X and Y"; three or more use
// "X, Y, and Z" to keep the line readable.
func joinAgentsHuman(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return "both " + names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
	}
}
