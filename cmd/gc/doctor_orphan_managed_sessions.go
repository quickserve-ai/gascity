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
func doctorManagedSessionNames(cityPath string, cfg *config.City, openStore func(string) (beads.Store, error)) func() (map[string]struct{}, error) {
	return func() (map[string]struct{}, error) {
		if openStore == nil {
			return nil, fmt.Errorf("no bead store opener")
		}
		store, err := openStore(cityPath)
		if err != nil {
			return nil, fmt.Errorf("opening city bead store: %w", err)
		}
		defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close of a one-shot read handle
		return openSessionRuntimeNames(cliSessionStore(store, cfg, cityPath))
	}
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
