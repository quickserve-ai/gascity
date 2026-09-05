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

// cacheHeartbeatScopeForRig returns the heartbeat scope label a rig store must
// publish under, or "" when it must publish none.
//
// The scope label space is FLAT and shared with the city store, and "city" is
// not a reserved rig name (ValidateRigs reserves only the orders wildcard). A
// rig literally named "city" would therefore write its liveness into the CITY
// store's record: one file, two live reconcilers, last writer wins. A healthy
// rig would then keep that record fresh while the city cache sat stalled —
// masking exactly the ga-yc0chj shape this watch exists to catch, and doing it
// silently, with a green tick.
//
// Such a rig publishes nothing and is not watched. That is a declared blind
// spot rather than a false green. (The full fix is a kind-qualified namespace —
// separate "city" and "rig/<name>" key spaces — which is a wire change to the
// on-disk record layout and belongs in its own bead.)
func cacheHeartbeatScopeForRig(rigName string) string {
	name := strings.TrimSpace(rigName)
	if name == "" || name == cacheHeartbeatCityScope {
		return ""
	}
	return name
}

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
		if scope := cacheHeartbeatScopeForRig(rig.Name); scope != "" {
			scopes = append(scopes, scope)
		}
	}
	return scopes
}

// newBeadsCacheReconcileCheck builds the doctor check that alarms on the
// ABSENCE of a beads-cache reconcile heartbeat.
func newBeadsCacheReconcileCheck(cityPath string, cfg *config.City, controllerRunning bool) doctor.Check {
	return doctor.NewBeadsCacheReconcileCheck(cityPath, cacheHeartbeatWatchedScopes(cityPath, cfg), controllerRunning)
}
