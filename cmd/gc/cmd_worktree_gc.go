package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

// newWorktreeGCCmd exposes a conservative operator preview using the same
// bounded runtime provider and live bead stores as the controller. Any
// unavailable snapshot or probe fails closed and reports would-skip.
// Mutation remains controller-owned and kill-switchable.
func newWorktreeGCCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "worktree-gc",
		Short: "Preview per-bead and stopped agent-home reclamation",
		Long:  "Preview both worktree reclamation classes without mutation. Per-bead cleanup is controlled by daemon.auto_reap_closed_bead_worktrees; longer-lived configured named/namepool home cleanup is separately controlled by daemon.auto_reap_stopped_agent_homes. Both require authoritative runtime, session, assignment, registration, and git-safety evidence.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree-gc: %v\n", err) //nolint:errcheck
				return errExit
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree-gc: %v\n", err) //nolint:errcheck
				return errExit
			}
			cityStore, openCityErr := openStoreAtForCity(cityPath, cityPath)
			if openCityErr != nil {
				fmt.Fprintf(stderr, "gc worktree-gc: open city store: %v\n", openCityErr) //nolint:errcheck
				return errExit
			}
			sessionSnapshot, sessionErr := loadSessionBeadSnapshot(cityStore)
			if sessionErr == nil {
				sessionErr = sessionSnapshot.LoadError()
			}
			if sessionErr != nil {
				fmt.Fprintf(stderr, "gc worktree-gc: list active sessions: %v\n", sessionErr) //nolint:errcheck
				return errExit
			}
			candidateSessions, historyErr := loadConfiguredStoppedAgentHomeHistory(cfg, cityStore)
			if historyErr != nil {
				fmt.Fprintf(stderr, "gc worktree-gc: list configured agent-home history: %v\n", historyErr) //nolint:errcheck
				return errExit
			}
			stores := make(map[string]beads.Store, len(cfg.Rigs))
			for _, rig := range cfg.Rigs {
				if strings.TrimSpace(rig.Path) == "" {
					continue
				}
				store, openErr := openStoreAtForCity(rig.Path, cityPath)
				if openErr != nil {
					fmt.Fprintf(stderr, "gc worktree-gc: open rig %s store: %v\n", rig.Name, openErr) //nolint:errcheck
					return errExit
				}
				stores[rig.Name] = store
			}
			sp, providerErr := newStatusSessionProviderForCity(cfg, cityPath)
			if providerErr != nil {
				fmt.Fprintf(stderr, "gc worktree-gc: runtime provider: %v\n", providerErr) //nolint:errcheck
				return errExit
			}
			activeSessions := activeSessionBeads(sessionSnapshot.OpenInfos())
			fmt.Fprintln(stdout, "Worktree GC preview (no files will be changed; unavailable liveness or assignment probes are skipped):") //nolint:errcheck
			reapClosedBeadWorktrees(cityPath, cfg, stores, sp, nil, stdout, true, true, sessionSnapshot.OpenInfos()...)
			reapStoppedAgentHomeWorktrees(cityPath, cfg, cityStore, stores, sp, nil, stdout, true, true, candidateSessions, activeSessions)
			return nil
		},
	}
}
