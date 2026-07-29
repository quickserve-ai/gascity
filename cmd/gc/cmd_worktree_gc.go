package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

// newWorktreeGCCmd exposes a conservative operator preview. The CLI does not
// own the controller's runtime provider, so candidates with recorded session
// ownership report unknown liveness and are skipped rather than predicted.
// Mutation remains controller-owned and kill-switchable.
func newWorktreeGCCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "worktree-gc",
		Short: "Preview closed-bead worktree reclamation",
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
			fmt.Fprintln(stdout, "Worktree GC preview (no files will be changed; recorded session owners are skipped when liveness is unavailable):") //nolint:errcheck
			reapClosedBeadWorktrees(cityPath, cfg, stores, nil, nil, stdout, true)
			return nil
		},
	}
}
