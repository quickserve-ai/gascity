package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
)

func sharedSkillCatalogInputs(cfg *config.City, rigName string) []config.DiscoveredSkillCatalog {
	return cfg.SharedSkillCatalogs(rigName)
}

func loadSharedSkillCatalog(cfg *config.City, rigName string) (materialize.CityCatalog, error) {
	if cfg == nil {
		return materialize.CityCatalog{}, nil
	}
	return materialize.LoadCityCatalog(cfg.PackSkillsDir, sharedSkillCatalogInputs(cfg, rigName)...)
}

// agentRigScopeName returns the configured rig name that should
// contribute rig-local shared skills for this agent (config.SkillRigScope).
func agentRigScopeName(agent *config.Agent, rigs []config.Rig) string {
	return config.SkillRigScope(agent, rigs)
}
