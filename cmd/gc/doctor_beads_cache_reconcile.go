package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// cacheHeartbeatCityScope is the scope label the city-level beads cache files
// its reconcile heartbeat under. Rig stores use the rig name, so the label
// space is exactly "city" plus the configured rig names.
const cacheHeartbeatCityScope = "city"

// cacheHeartbeatWatchedScopes returns the scopes whose beads cache the
// controller is expected to be reconciling: the city store plus every rig
// whose EFFECTIVE suspension state is resumed and whose path is bound.
//
// The list is deliberately derived from the same predicate the controller uses
// to decide whether to arm a reconciler (rigStoreBackgroundRefresh). A rig that
// is suspended has no reconciler by design, and its leftover heartbeat record
// must never be read as a stalled cache — excluding it here is what keeps the
// watch free of the standing false positive that would get it ignored.
func cacheHeartbeatWatchedScopes(cityPath string, cfg *config.City) []string {
	scopes := []string{cacheHeartbeatCityScope}
	if cfg == nil {
		return scopes
	}
	suspState, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) {
			continue
		}
		if name := strings.TrimSpace(rig.Name); name != "" {
			scopes = append(scopes, name)
		}
	}
	return scopes
}

// newBeadsCacheReconcileCheck builds the doctor check that alarms on the
// ABSENCE of a beads-cache reconcile heartbeat.
func newBeadsCacheReconcileCheck(cityPath string, cfg *config.City, controllerRunning bool) doctor.Check {
	return doctor.NewBeadsCacheReconcileCheck(cityPath, cacheHeartbeatWatchedScopes(cityPath, cfg), controllerRunning)
}
