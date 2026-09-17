package beadmeta

import "strings"

// Create-time label inheritance exclusion — a gc-side INTERIM (qc-p9m8oa9).
//
// `bd create --parent` copies every parent label onto the child unless the
// caller passes --no-inherit-labels. The dimensional labels (town:*, init:*,
// ready:*, domain:*) cascade by design. State/hold labels do not belong on
// that path: they describe the PARENT's situation at a moment and are false
// for a fresh child. Automation consumes them — the cert-landing patrol reads
// hold:cert-wait as "in flight" — so an inheriting child looks handled and is
// skipped forever (specimens: qc-88kjhl.1 inherited hold:cert-wait and
// needs-summon; qc-ntokpb.4.1 inherited cert:action).
//
// The real fix is bd-side (the beads owner's: exclude these families from the
// cascade). REMOVAL CONDITION: delete this file and its two callers
// (cmd/gc/bd_create_inherited_state_labels.go and BdStore.Create) once the
// bd-side exclusion ships in the fleet beads pin.

// createInheritanceExcludedPrefixes are the label families that must not
// cascade from a parent at create. hold: is matched as the open prefix the
// wait-class contract defines (see HoldLabelPrefix), never an enumeration.
var createInheritanceExcludedPrefixes = []string{HoldLabelPrefix, "cert:"}

// createInheritanceExcludedExact are single labels excluded by exact value.
var createInheritanceExcludedExact = map[string]struct{}{"needs-summon": {}}

// IsCreateInheritanceExcludedLabel reports whether label belongs to a
// state/hold family (hold:*, cert:*, needs-summon) that a child must not
// inherit from its parent at create (qc-p9m8oa9 interim).
func IsCreateInheritanceExcludedLabel(label string) bool {
	label = strings.TrimSpace(label)
	if _, ok := createInheritanceExcludedExact[label]; ok {
		return true
	}
	for _, prefix := range createInheritanceExcludedPrefixes {
		if strings.HasPrefix(label, prefix) {
			return true
		}
	}
	return false
}

// InheritedStateLabelsToStrip returns the members of inherited that belong to
// an excluded family and that the caller did NOT pass explicitly — the labels
// a create would otherwise acquire only by inheritance. Order follows
// inherited, duplicates are dropped, and explicit is compared after trimming
// whitespace the way bd normalizes explicit labels. A label the caller named
// is never returned: an explicit hold is a deliberate park, not a cascade.
func InheritedStateLabelsToStrip(inherited, explicit []string) []string {
	named := make(map[string]struct{}, len(explicit))
	for _, label := range explicit {
		named[strings.TrimSpace(label)] = struct{}{}
	}
	var strip []string
	seen := make(map[string]struct{})
	for _, label := range inherited {
		label = strings.TrimSpace(label)
		if !IsCreateInheritanceExcludedLabel(label) {
			continue
		}
		if _, ok := named[label]; ok {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		strip = append(strip, label)
	}
	return strip
}
