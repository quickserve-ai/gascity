package api

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime/terminationevents"
	"github.com/gastownhall/gascity/internal/session"
)

func (s *Server) sessionManager(store beads.Store) *session.Manager {
	cfg := s.state.Config()
	opts := []session.ManagerOption{session.WithCityPath(s.state.CityPath())}
	// ga-ksac39: the event sink is the Manager's SECOND failure domain. The
	// bead sink is on by default and rides Dolt; this one is a local append
	// that survives the incidents the bead write does not.
	if rec := s.state.EventProvider(); rec != nil {
		opts = append(opts, session.WithTerminationSinks(terminationevents.New(rec, "api")))
	}
	if cfg == nil {
		return session.NewManagerWithOptions(store, s.state.SessionProvider(), opts...)
	}
	opts = append(opts, session.WithTransportPolicyResolver(func(template, provider string) (string, bool) {
		return configuredSessionTransportResolution(cfg, template, provider)
	}))
	return session.NewManagerWithOptions(store, s.state.SessionProvider(), opts...)
}

func configuredSessionTransport(cfg *config.City, template, provider string) string {
	transport, _ := configuredSessionTransportResolution(cfg, template, provider)
	return transport
}

func configuredSessionTransportResolution(cfg *config.City, template, provider string) (string, bool) {
	if cfg == nil {
		return "", false
	}
	if agentCfg, ok := resolveSessionTemplateAgent(cfg, template); ok {
		resolved, err := config.ResolveProvider(
			&agentCfg,
			&cfg.Workspace,
			cfg.Providers,
			func(name string) (string, error) { return name, nil },
		)
		if err != nil {
			return strings.TrimSpace(agentCfg.Session), false
		}
		return config.ResolveSessionCreateTransport(agentCfg.Session, resolved), false
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = strings.TrimSpace(template)
	}
	if provider == "" {
		return "", false
	}
	resolved, err := config.ResolveProvider(
		&config.Agent{Provider: provider},
		&cfg.Workspace,
		cfg.Providers,
		func(name string) (string, error) { return name, nil },
	)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(resolved.ProviderSessionCreateTransport()), false
}
