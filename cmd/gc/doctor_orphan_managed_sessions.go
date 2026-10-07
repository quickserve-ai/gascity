package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// doctorOrphanRigStores opens every bound rig's bead store for the orphan
// lister's census. It is the opener the controller and `gc ready` use; a var
// so a test can hand the census rig stores without a rig on disk.
//
// residency:allow — a test seam over openStandaloneRigStores (baselined). It
// opens the scope stores the census is handed and answers no residency
// question: the census plans the legs over them.
var doctorOrphanRigStores = openStandaloneRigStores

// doctorManagedSessionNames returns the managed-name lister the orphan-sessions
// doctor check consults before calling a running session an orphan
// (ga-n2f1ph). The supervisor runs named sessions whose alias differs from
// their template, and namepool-themed pool instances, under runtime names that
// agent.SessionNameFor never derives; each has an OPEN session bead whose
// session_name records that runtime name. Without these names the check flags
// live crew seats as orphans and its Fix stops them.
//
// The returned closure is lazy: it opens this city's stores only when called
// (the check calls it only when a running session is not template-derived) and
// closes every handle it opened. It reads the reconciler's own session census
// (collectAllOpenSessionAvailabilityInfos): the sessions binding, the city work
// store and every rig store, so a pool instance whose open bead lives only in
// a rig store is claimed too. The availability variant keeps every identifier
// projection of a bead held in more than one leg, because a name ANY copy
// claims must stay reserved. Any error is returned, never swallowed: a rig
// store that will not open, a census leg that fails or answers partially, and
// a refused city all make the check fail closed.
//
// Suspended rigs are READ, unlike the reconciler's census, which skips them
// (servingRigStores) because a dark suspended rig would make every tick
// partial and pin the fleet against reaping. Here the danger runs the other
// way: a session claimed only in a skipped rig would read as an orphan and be
// stopped. So every bound rig is a leg, and a suspended rig whose store is
// dark makes Fix refuse rather than guess. A rig whose bd-owned proxied store
// is stopped is never opened, since opening it would start its proxy and Dolt
// (doctorGatedRigStoreOpener): it is a failed leg, and Fix refuses the same
// way.
//
// A relocation the config declares but the one-shot routes do not carry is an
// error too, and the census does not catch it on its own. cliStorageRoutes
// answers nil when it cannot load the city's config; nil routes yield no
// binding (residencyBindingsFromRoutes) and no error, so the census plans over
// the work and rig stores alone, where a city that relocated its sessions has
// none: the read would succeed without the binding's beads and every seat
// claimed only there would read as an orphan.
func doctorManagedSessionNames(cityPath string, cfg *config.City, openStore func(string) (beads.Store, error), gate *doctorStoreGate) func() (map[string]struct{}, error) {
	return func() (map[string]struct{}, error) {
		if openStore == nil {
			return nil, fmt.Errorf("no bead store opener")
		}
		if configRelocatesSessions(cfg) && !cliSessionsRelocated(cityPath) {
			return nil, fmt.Errorf("city config binds the sessions class to %q but the storage routes do not relocate it; reading session beads would read the work store", cfg.EffectiveStorage().Classes.Sessions)
		}
		store, err := openStore(cityPath)
		if err != nil {
			return nil, fmt.Errorf("opening city bead store: %w", err)
		}
		defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close of a one-shot read handle
		rigStores, failures := doctorOrphanRigStores(cfg, cityPath, doctorGatedRigStoreOpener(gate, oneShotRigStoreOpener(cfg)))
		defer func() {
			for _, rigStore := range rigStores {
				closeBeadStoreHandle(rigStore) //nolint:errcheck // best-effort close of a one-shot read handle
			}
		}()
		if len(failures) > 0 {
			errs := make([]error, 0, len(failures))
			for _, f := range failures {
				errs = append(errs, fmt.Errorf("opening rig %q bead store: %w", f.rig, f.err))
			}
			return nil, errors.Join(errs...)
		}
		// nil suspended paths: every bound rig is a census leg (see above).
		infos, err := collectAllOpenSessionAvailabilityInfos(cityPath, cfg, store, rigStores, nil)
		if err != nil {
			return nil, fmt.Errorf("session census: %w", err)
		}
		return sessionRuntimeNames(infos), nil
	}
}

// doctorGatedRigStoreOpener answers the gate's skip error for a rig whose
// bd-owned proxied store is stopped or suspended instead of opening it: doctor
// never starts a server (#6817, #7102). rigPath is the scope root the other gated rig checks hand
// the gate (rig.Path).
func doctorGatedRigStoreOpener(gate *doctorStoreGate, open rigStoreOpener) rigStoreOpener {
	return func(rigPath, cityPath string) (beads.Store, error) {
		if err := gate.skipErr(rigPath); err != nil {
			return nil, err
		}
		return open(rigPath, cityPath)
	}
}

// configRelocatesSessions reports whether cfg binds the sessions coordination
// class anywhere but the reserved work binding. It reads configuration only,
// the same [storage.classes] assignment storageSplitShapeOf classifies. An
// authored [storage] whose sessions binding is blank counts as relocated: the
// read must then prove its routing rather than fall back to the work store.
func configRelocatesSessions(cfg *config.City) bool {
	if cfg == nil || cfg.Storage == nil {
		return false
	}
	binding := cfg.EffectiveStorage().Classes.BindingFor(config.StorageClassSessions)
	return strings.TrimSpace(binding) != config.StorageWorkBinding
}

// sessionRuntimeNames returns the runtime (tmux) name of every session in
// infos that is not closed. It reads Info.SessionName, which is the
// session_name metadata when set and otherwise the s-<id> name the session
// manager starts such a bead under, so every name the supervisor could be
// running for an open bead is claimed.
func sessionRuntimeNames(infos []session.Info) map[string]struct{} {
	names := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		if info.Closed {
			continue
		}
		if name := strings.TrimSpace(info.SessionName); name != "" {
			names[name] = struct{}{}
		}
		if name := strings.TrimSpace(info.SessionNameMetadata); name != "" {
			names[name] = struct{}{}
		}
	}
	return names
}
