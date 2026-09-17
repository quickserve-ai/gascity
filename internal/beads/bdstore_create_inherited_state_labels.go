package beads

import (
	"log"
	"slices"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// removeInheritedStateLabels is the BdStore half of the qc-p9m8oa9 interim
// (see beadmeta/create_inherited_state_labels.go for the defect and the
// removal condition: delete this once bd excludes hold:*/cert:*/needs-summon
// from create-time inheritance in the fleet beads pin).
//
// BdStore.Create shells `bd create --parent`, which inherits every parent
// label. The native store's create never inherits, so this runs only on the
// bd-CLI store (the native fallback, the bd-store-bridge, scoped BdStores).
//
// Mechanism: AFTER the create, from bd's own --json result. Create passes
// every caller label on argv, so any hold:*/cert:*/needs-summon label in the
// result that is not in explicit arrived by inheritance — explicit labels
// cannot be removed by construction. The common case (a parent with no state
// labels) costs nothing: no extra bd call. The removal is a second write and
// NOT atomic with the create; a reader in between can see the inherited
// label for one bd round-trip. That window only delays a skip the patrol
// already logs, where the defect left it forever. A pre-create parent read
// would close the window but add a bd subprocess to every child create.
//
// A failed removal keeps the create (the bead exists; reporting failure would
// invite a duplicate) and returns the labels as they actually stand, loudly.
func (s *BdStore) removeInheritedStateLabels(childID, parentID string, labels, explicit []string) []string {
	strip := beadmeta.InheritedStateLabelsToStrip(labels, explicit)
	if len(strip) == 0 || childID == "" {
		return labels
	}
	if err := s.Update(childID, UpdateOpts{RemoveLabels: strip}); err != nil {
		log.Printf("beads: WARNING: bd create %s --parent %s: child inherited state labels %q and removing them failed: %v — it may be skipped as in-flight until they are removed by hand (qc-p9m8oa9)", childID, parentID, strip, err)
		return labels
	}
	log.Printf("beads: bd create %s --parent %s: removed inherited state labels %q; dimensional labels kept (qc-p9m8oa9 interim, removed when bd excludes these at create)", childID, parentID, strip)
	return slices.DeleteFunc(slices.Clone(labels), func(label string) bool {
		return slices.Contains(strip, label)
	})
}
