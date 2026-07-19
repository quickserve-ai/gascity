package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

func rigForDir(cfg *config.City, cityPath, dir string) (config.Rig, bool) {
	rig, ok, _ := resolveRigForDir(cfg, cityPath, dir)
	return rig, ok
}

// rigFromEnvScope returns the rig named by GC_RIG when it resolves to a
// declared, path-bound rig. Mirrors resolveBdScopeTarget's env handling
// (cmd_bd.go): the controller sets GC_RIG reliably on every rig agent,
// while cwd detection fails for agent worktrees under .gc/worktrees/.
// An unknown or unbound GC_RIG returns false so callers fall through to
// cwd/city resolution rather than erroring.
func rigFromEnvScope(cfg *config.City) (config.Rig, bool) {
	gcRig := strings.TrimSpace(os.Getenv("GC_RIG"))
	if gcRig == "" {
		return config.Rig{}, false
	}
	rig, ok := rigByName(cfg, gcRig)
	if !ok || strings.TrimSpace(rig.Path) == "" {
		return config.Rig{}, false
	}
	return rig, true
}

func resolveRigForDir(cfg *config.City, cityPath, dir string) (config.Rig, bool, error) {
	dir = normalizePathForCompare(dir)
	resolveRigPaths(cityPath, cfg.Rigs)
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		rigPath := normalizePathForCompare(resolveStoreScopeRoot(cityPath, rig.Path))
		if pathWithinScope(dir, rigPath) {
			return rig, true, nil
		}
	}
	return rigFromRedirectedBeadsDir(cfg, cityPath, dir)
}

func rigFromRedirectedBeadsDir(cfg *config.City, cityPath, dir string) (config.Rig, bool, error) {
	// Redirect resolution is meaningful only when cwd lies inside cityPath.
	// When tests or commands run with a cwd outside the declared city tree
	// (e.g., a polecat worktree under a different gc city), walking up the
	// cwd chain would pick up unrelated .beads/redirect files and either
	// mis-route the command or hard-error against the test's fake cfg.
	cityScope := normalizePathForCompare(cityPath)
	if !pathWithinScope(normalizePathForCompare(dir), cityScope) {
		return config.Rig{}, false, nil
	}
	for current := dir; current != "" && current != filepath.Dir(current); current = filepath.Dir(current) {
		if !pathWithinScope(normalizePathForCompare(current), cityScope) {
			break
		}
		redirectPath := filepath.Join(current, ".beads", "redirect")
		redirectTarget, err := os.ReadFile(redirectPath)
		if err != nil {
			continue
		}
		targetBeadsDir := normalizePathForCompare(strings.TrimSpace(string(redirectTarget)))
		if targetBeadsDir == "" {
			continue
		}
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			rigBeadsDir := normalizePathForCompare(filepath.Join(resolveStoreScopeRoot(cityPath, rig.Path), ".beads"))
			if targetBeadsDir == rigBeadsDir {
				return rig, true, nil
			}
		}
		return config.Rig{}, false, fmt.Errorf("cwd redirect %s points outside declared city rigs", redirectPath)
	}
	return config.Rig{}, false, nil
}

func pathWithinScope(path, scopeRoot string) bool {
	if scopeRoot == "" {
		return false
	}
	if path == scopeRoot {
		return true
	}
	return len(path) > len(scopeRoot) && strings.HasPrefix(path, scopeRoot) && path[len(scopeRoot)] == '/'
}
