package workdir

import (
	"bytes"
	"strings"
	"text/template"

	"github.com/gastownhall/gascity/internal/config"
)

// SessionSetupContext holds the template variables for session_setup,
// pre_start, session_live, and start_command expansion. The reconciler's
// create path (cmd/gc resolveTemplate) and every resume path render through
// this one context so a templated start_command means the same thing no
// matter which path launches the runtime (ga-b1u4yg).
type SessionSetupContext struct {
	Session   string // tmux session name
	Agent     string // qualified agent name
	AgentBase string // unqualified agent name or pool instance name
	Rig       string // rig name (empty for city-scoped)
	RigRoot   string // absolute path to the rig root (empty for city-scoped)
	CityRoot  string // city directory path
	CityName  string // workspace name
	WorkDir   string // agent working directory
	ConfigDir string // source directory where agent config was defined
	// DefaultBranch mirrors PathContext.DefaultBranch: the rig's configured
	// mainline branch, empty for city-scoped agents and for rigs with no
	// default_branch. Configured value only — prompts' {{.DefaultBranch}}
	// additionally falls back to a live origin/HEAD probe, but setup-command
	// expansion runs on reconciler hot paths and must not spawn git. Setup
	// scripts should keep their own probe fallback.
	DefaultBranch string
}

// SessionSetupContextForAgent builds the identity half of a
// SessionSetupContext for the agent whose qualified name is qualifiedName.
// Callers fill Session, WorkDir, and ConfigDir for the concrete session.
func SessionSetupContextForAgent(cityPath, cityName, qualifiedName string, a config.Agent, rigs []config.Rig) SessionSetupContext {
	ctx := PathContextForQualifiedName(cityPath, cityName, qualifiedName, a, rigs)
	return SessionSetupContext{
		Agent:         qualifiedName,
		AgentBase:     ctx.AgentBase,
		Rig:           ctx.Rig,
		RigRoot:       ctx.RigRoot,
		CityRoot:      cityPath,
		CityName:      cityName,
		DefaultBranch: ctx.DefaultBranch,
	}
}

// SessionSetupContextForSession builds the full SessionSetupContext for one
// concrete session: the identity half from SessionSetupContextForAgent plus
// Session, WorkDir (the city root when empty), and ConfigDir. Every
// non-reconciler start path builds its render context here, so the fields the
// reconciler's create path sets (cmd/gc resolveTemplate Step 11) are set the
// same way (ga-b1u4yg).
func SessionSetupContextForSession(cityPath, cityName, qualifiedName string, a config.Agent, rigs []config.Rig, sessionName, workDir string) SessionSetupContext {
	ctx := SessionSetupContextForAgent(cityPath, cityName, qualifiedName, a, rigs)
	ctx.Session = sessionName
	ctx.WorkDir = workDir
	if strings.TrimSpace(ctx.WorkDir) == "" {
		ctx.WorkDir = cityPath
	}
	ctx.ConfigDir = ConfigDirForSource(cityPath, a.SourceDir)
	return ctx
}

// RenderResolvedProviderCommandForSession renders resolved's launch command
// for the concrete session of agent a whose qualified identity is
// qualifiedName. It is the one entry point the CLI and API create and resume
// paths use, so a templated start_command (the control dispatcher's
// `--follow {{.Agent}}`) launches with the target's own identity rather than
// the raw template text (ga-b1u4yg).
func RenderResolvedProviderCommandForSession(resolved *config.ResolvedProvider, cityPath, cityName, qualifiedName string, a config.Agent, rigs []config.Rig, sessionName, workDir string) *config.ResolvedProvider {
	if resolved == nil {
		return nil
	}
	if strings.TrimSpace(qualifiedName) == "" {
		qualifiedName = a.QualifiedName()
	}
	return RenderResolvedProviderCommand(resolved, SessionSetupContextForSession(cityPath, cityName, qualifiedName, a, rigs, sessionName, workDir))
}

// ConfigDirForSource returns the directory agent config-relative paths
// resolve against: the agent's SourceDir, or the city root when unset.
func ConfigDirForSource(cityPath, sourceDir string) string {
	if sourceDir != "" {
		return sourceDir
	}
	return cityPath
}

// ExpandSessionTemplates expands Go text/template strings in session commands.
// On parse or execute error, the raw command is kept (graceful fallback).
func ExpandSessionTemplates(cmds []string, ctx SessionSetupContext) []string {
	if len(cmds) == 0 {
		return nil
	}
	result := make([]string, len(cmds))
	for i, raw := range cmds {
		tmpl, err := template.New("setup").Parse(raw)
		if err != nil {
			result[i] = raw
			continue
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, ctx); err != nil {
			result[i] = raw
			continue
		}
		result[i] = buf.String()
	}
	return result
}

// ExpandSessionCommand renders one launch command through
// ExpandSessionTemplates. Commands without "{{" are returned unchanged, which
// matches the reconciler's create-path guard.
func ExpandSessionCommand(command string, ctx SessionSetupContext) string {
	if !strings.Contains(command, "{{") {
		return command
	}
	return ExpandSessionTemplates([]string{command}, ctx)[0]
}

// RenderResolvedProviderCommand returns resolved with its launch command
// (Command and ACPCommand — where an agent or workspace start_command lands)
// rendered for the session described by ctx. The input is never mutated: a
// shallow copy is returned when anything changes, and resolved itself when
// nothing needs rendering.
func RenderResolvedProviderCommand(resolved *config.ResolvedProvider, ctx SessionSetupContext) *config.ResolvedProvider {
	if resolved == nil {
		return nil
	}
	command := ExpandSessionCommand(resolved.Command, ctx)
	acpCommand := ExpandSessionCommand(resolved.ACPCommand, ctx)
	if command == resolved.Command && acpCommand == resolved.ACPCommand {
		return resolved
	}
	rendered := *resolved
	rendered.Command = command
	rendered.ACPCommand = acpCommand
	return &rendered
}
