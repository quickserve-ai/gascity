package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// doctorManagedSessionNames returns the managed-name lister the orphan-sessions
// doctor check consults before calling a running session an orphan
// (ga-n2f1ph). The supervisor runs named sessions whose alias differs from
// their template, and namepool-themed pool instances, under runtime names that
// agent.SessionNameFor never derives; each has an OPEN session bead whose
// session_name records that runtime name. Without these names the check flags
// live crew seats as orphans and its Fix stops them.
//
// The returned closure is lazy: it opens this city's store only when called
// (the check calls it only when a running session is not template-derived),
// reads through the session coordination-class store so a
// [beads.classes.sessions] relocation is honored, and closes the handle it
// opened. Any error is returned, never swallowed: the check fails closed on it.
//
// A relocation the config declares but the one-shot routes do not carry is an
// error too. cliStorageRoutes answers nil when it cannot load the city's
// config, and nil routes send the session read to the WORK store, where a city
// that relocated its sessions has none: the read would succeed EMPTY and every
// candidate would read as an orphan.
func doctorManagedSessionNames(cityPath string, cfg *config.City, openStore func(string) (beads.Store, error)) func() (map[string]struct{}, error) {
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
		return openSessionRuntimeNames(cliSessionStore(store, cfg, cityPath))
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

// openSessionRuntimeNames returns the runtime (tmux) name of every session
// bead in sessStore that is not closed. It reads Info.SessionName, which is the
// session_name metadata when set and otherwise the s-<id> name the session
// manager starts such a bead under, so every name the supervisor could be
// running for an open bead is claimed. A partial listing is an error: an
// incomplete managed set would let a managed session read as an orphan.
func openSessionRuntimeNames(sessStore beads.Store) (map[string]struct{}, error) {
	infos, err := loadOpenSessionInfos(sessStore)
	if err != nil {
		return nil, err
	}
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
	return names, nil
}
