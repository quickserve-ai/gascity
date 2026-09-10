package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// claudeAccountDeclarationDoctorCheck statically flags claude-family seats
// that declare no CLAUDE_CONFIG_DIR at any env layer while the machine
// carries an ambient one (the supervisor service env, or this process's
// environment). At spawn time the ga-ai7gz2 account guard REFUSES exactly
// these seats, and buildDesiredState then drops the agent from desired state
// with one stderr line — the seat is simply absent and nothing else says why
// (ga-xd3bjx item 4). This check surfaces the condition pre-spawn, at config
// level, without waiting for a refusal to fire.
type claudeAccountDeclarationDoctorCheck struct {
	cityPath string
}

func newClaudeAccountDeclarationDoctorCheck(cityPath string) *claudeAccountDeclarationDoctorCheck {
	return &claudeAccountDeclarationDoctorCheck{cityPath: cityPath}
}

func (c *claudeAccountDeclarationDoctorCheck) Name() string { return "claude-account-declaration" }

func (c *claudeAccountDeclarationDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	cfg, err := loadCityConfigAllowMissingProviderReferences(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusOK
		r.Message = "claude account declaration check skipped until expanded config loads"
		return r
	}
	ambientSource := ambientClaudeConfigDirSource()
	if ambientSource == "" {
		r.Status = doctor.StatusOK
		r.Message = "no ambient CLAUDE_CONFIG_DIR to inherit"
		return r
	}
	undeclared := undeclaredClaudeAccountSeats(cfg)
	if len(undeclared) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "every claude-family seat declares its account"
		return r
	}
	r.Status = doctor.StatusWarning
	r.Severity = doctor.SeverityAdvisory
	r.Message = fmt.Sprintf(
		"%d claude-family seat(s) declare no CLAUDE_CONFIG_DIR while %s carries an ambient one",
		len(undeclared), ambientSource)
	r.Details = undeclared
	r.FixHint = "declare env.CLAUDE_CONFIG_DIR on the provider (or workspace/agent env); at spawn the ga-ai7gz2 guard refuses these seats and buildDesiredState silently drops them"
	return r
}

func (c *claudeAccountDeclarationDoctorCheck) CanFix() bool { return false }

func (c *claudeAccountDeclarationDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *claudeAccountDeclarationDoctorCheck) WarmupEligible() bool { return false }

// undeclaredClaudeAccountSeats mirrors the runtime guard's merged-env view
// (workspace -> provider -> agent, ga-ai7gz2) per configured agent: a seat is
// flagged when its provider resolves to the claude family and NO layer sets
// CLAUDE_CONFIG_DIR.
func undeclaredClaudeAccountSeats(cfg *config.City) []string {
	if cfg == nil {
		return nil
	}
	workspaceDeclared := strings.TrimSpace(cfg.Workspace.Env["CLAUDE_CONFIG_DIR"]) != ""
	var out []string
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		// The permissive lookPath keeps this a CONFIG check: a provider whose
		// binary is missing from doctor's PATH still has an account-declaration
		// answer, and the ga-ai7gz2 guard fires regardless of PATH.
		resolved, err := config.ResolveProvider(agent, &cfg.Workspace, cfg.Providers, func(name string) (string, error) { return name, nil })
		if err != nil || resolved == nil {
			continue
		}
		if resolved.AccountFamily() != "claude" {
			continue
		}
		if workspaceDeclared ||
			strings.TrimSpace(resolved.Env["CLAUDE_CONFIG_DIR"]) != "" ||
			strings.TrimSpace(agent.Env["CLAUDE_CONFIG_DIR"]) != "" {
			continue
		}
		providerName := strings.TrimSpace(resolved.Name)
		if providerName == "" {
			providerName = strings.TrimSpace(agent.Provider)
		}
		out = append(out, fmt.Sprintf("agent %q (provider %q)", agent.Name, providerName))
	}
	sort.Strings(out)
	return out
}

// ambientClaudeConfigDirSource names where an ambient CLAUDE_CONFIG_DIR would
// be inherited from, or "" when there is none. The supervisor service file is
// checked first because supervisor-spawned sessions inherit ITS env, not the
// shell's; the process env is the fallback for shell-run `gc start`.
func ambientClaudeConfigDirSource() string {
	if v, ok := supervisorServiceFileClaudeConfigDir(); ok && strings.TrimSpace(v) != "" {
		return "the supervisor service env"
	}
	if strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) != "" {
		return "the controller process env"
	}
	return ""
}

func supervisorServiceFileClaudeConfigDir() (string, bool) {
	data, err := os.ReadFile(supervisorLaunchdPlistPath())
	if err != nil {
		return "", false
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return "", false
		}
		if err != nil {
			return "", false
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "dict" {
			continue
		}
		root, err := parsePlistDict(dec)
		if err != nil {
			return "", false
		}
		env, ok := root["EnvironmentVariables"]
		if !ok || env.dict == nil {
			return "", false
		}
		v, ok := env.dict["CLAUDE_CONFIG_DIR"]
		if !ok {
			return "", false
		}
		return v.text, true
	}
}
