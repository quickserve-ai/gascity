package main

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
)

// A STOP LEVER FOR THE IN-PROCESS POOL ORPHAN SWEEPER (ga-h5435p).
//
// releaseOrphanedPoolAssignments runs on the controller tick and rewrites work
// beads — clearing assignees and reopening status — on its own clock, with no
// operator in the loop. Upstream gascity removed the sweeper outright in gc
// 1.4.1; this fork still runs it, and until now it had no off switch at all.
//
// WHY THIS FORK CANNOT SIMPLY DROP IT TO MATCH UPSTREAM. Upstream retired the
// sweeper in favor of lease-based reclaim (bd reclaim over the leases table
// migration 0055 creates). Measured on this city 2026-08-26: none of our three
// stores has a leases table, all three sit at migration 54, our bd 1.1.0 is
// built from the carry fork and ships no heartbeat or reclaim verb, and every
// populated lease on the shared store is expired because nothing refreshes one
// (ga-1yzsa0). Deleting the sweeper here would adopt upstream's posture without
// upstream's replacement and leave NO orphan recovery at all — pool work whose
// claimant died would stay claimed forever. So the sweeper stays and gains a
// switch instead.
//
// WHY A CONFIG KNOB AND NOT AN ENVIRONMENT VARIABLE. The live caller is the
// reconciler inside the long-lived supervisor process. An env var is read at
// process start, so throwing that lever would mean cycling the supervisor —
// which can strand every session in the city (ga-4v3ckk) and is the single
// worst thing to do during a cutover window. Rig config is re-read by the
// controller, so this lever can be thrown and reverted without touching a
// process.
//
// WHY ITS OWN FILE. ga-56nq1a proposes deleting
// cmd/gc/pool_orphan_foreign_identity.go wholesale when lease-based claims land.
// This kill switch must outlive that deletion, so it does not live there.

// poolOrphanReleaseAllowed reports whether the sweeper may release a claim on
// work owned by storeRef.
//
// storeRef follows DesiredStateResult.AssignedWorkStoreRefs: the empty string
// means the city store, and any other value is a rig name. The city store has
// no rig entry to carry the knob, so city-owned work is always allowed — this
// lever exists to stop autonomous writes into a RIG store that is about to be,
// or already is, shared with another city.
func poolOrphanReleaseAllowed(cfg *config.City, storeRef string) bool {
	rigName := strings.TrimSpace(storeRef)
	if cfg == nil || rigName == "" {
		return true
	}
	return !rigOrphanReleaseDisabled(cfg, rigName)
}

// rigOrphanReleaseDisabled reports whether rigName is configured with
// orphan_release = false. An unset knob means enabled, which is the behavior
// every city had before this switch existed.
func rigOrphanReleaseDisabled(cfg *config.City, rigName string) bool {
	if cfg == nil {
		return false
	}
	rigName = strings.TrimSpace(rigName)
	if rigName == "" {
		return false
	}
	for i := range cfg.Rigs {
		if !strings.EqualFold(strings.TrimSpace(cfg.Rigs[i].Name), rigName) {
			continue
		}
		if cfg.Rigs[i].OrphanRelease == nil {
			return false
		}
		return !*cfg.Rigs[i].OrphanRelease
	}
	return false
}

// cityHasAnyOrphanReleaseDisabled reports whether ANY rig has the switch
// thrown. It exists for the ambiguous case below and for nothing else.
func cityHasAnyOrphanReleaseDisabled(cfg *config.City) bool {
	if cfg == nil {
		return false
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].OrphanRelease != nil && !*cfg.Rigs[i].OrphanRelease {
			return true
		}
	}
	return false
}

// poolOrphanReleaseAllowedForBead decides the same question when the caller has
// no store ref for this bead — DesiredStateResult omits the refs whenever their
// length does not match the work slice, and the one-shot start path can pass
// none at all.
//
// It resolves the owning rig the same two ways storeForPoolAssignment does: the
// rig prefix on gc.routed_to first, then the bead ID prefix against each rig's
// effective prefix.
//
// THE AMBIGUOUS CASE FAILS CLOSED, AND ONLY WHEN THE SWITCH IS THROWN. If the
// rig cannot be determined and some rig in this city has orphan_release =
// false, this returns false and the claim is held. A kill switch that releases
// anyway whenever provenance is murky is not a kill switch. Note this costs
// nothing on the overwhelming majority of cities, where no rig sets the knob:
// cityHasAnyOrphanReleaseDisabled is false, so an unresolvable bead is allowed
// exactly as it is today and behavior is bit-for-bit unchanged.
func poolOrphanReleaseAllowedForBead(cfg *config.City, wb beads.Bead) bool {
	if cfg == nil {
		return true
	}
	if rigName := poolOrphanReleaseRigForBead(cfg, wb); rigName != "" {
		return !rigOrphanReleaseDisabled(cfg, rigName)
	}
	return !cityHasAnyOrphanReleaseDisabled(cfg)
}

// poolOrphanReleaseRigForBead names the rig that owns wb, or "" when no rig
// claims it. Mirrors storeForPoolAssignment's resolution order.
func poolOrphanReleaseRigForBead(cfg *config.City, wb beads.Bead) string {
	if cfg == nil {
		return ""
	}
	if routed := routedToOrLegacyWorkflowTarget(wb); routed != "" {
		if slash := strings.IndexByte(routed, '/'); slash > 0 {
			candidate := strings.TrimSpace(routed[:slash])
			for i := range cfg.Rigs {
				if strings.EqualFold(strings.TrimSpace(cfg.Rigs[i].Name), candidate) {
					return cfg.Rigs[i].Name
				}
			}
		}
	}
	idPrefix := sling.BeadPrefixForCity(cfg, wb.ID)
	if strings.TrimSpace(idPrefix) == "" {
		return ""
	}
	for i := range cfg.Rigs {
		if strings.EqualFold(idPrefix, cfg.Rigs[i].EffectivePrefix()) {
			return cfg.Rigs[i].Name
		}
	}
	return ""
}

// heldByOrphanReleaseGate accumulates one sweep's held claims so the skip is
// reported once per pass rather than once per bead.
//
// This gate MUST NOT be silent. The whole reason it exists is that the sweeper
// edits work with no operator in the loop; a switch that stops it invisibly
// just moves the invisibility. The cutover runbook also needs a positive signal
// that the lever took effect — "the log says it held 13 claims in qcore" is
// evidence, whereas an absence of release lines is equally consistent with the
// lever working, the sweeper being wedged, or the config never reloading.
//
// Deliberately a sibling of protectedForeignAssignees rather than a
// generalisation of it: that type lives in the file ga-56nq1a proposes to
// delete, and this counter has to survive that.
type heldByOrphanReleaseGate struct {
	order    []string
	byRig    map[string][]string
	rigNames map[string]string
}

// heldByOrphanReleaseGateIDSampleLimit bounds the per-rig ID sample so one
// large hold cannot turn the summary into an unreadable line. The COUNT is
// always exact; only the ID list is sampled.
const heldByOrphanReleaseGateIDSampleLimit = 5

func (h *heldByOrphanReleaseGate) add(rigName, workID string) {
	key := strings.TrimSpace(rigName)
	display := key
	if key == "" {
		// An unresolvable bead held by the fail-closed branch. Name it as such
		// rather than filing it under an empty rig, or the summary reads as a
		// rig whose name failed to render.
		key = "<unresolved>"
		display = "<unresolved>"
	}
	if h.byRig == nil {
		h.byRig = make(map[string][]string, 2)
		h.rigNames = make(map[string]string, 2)
	}
	if _, seen := h.byRig[key]; !seen {
		h.order = append(h.order, key)
		h.rigNames[key] = display
	}
	h.byRig[key] = append(h.byRig[key], strings.TrimSpace(workID))
}

func (h *heldByOrphanReleaseGate) claims() int {
	total := 0
	for _, ids := range h.byRig {
		total += len(ids)
	}
	return total
}

// summary renders the once-per-sweep line, or "" when nothing was held.
func (h *heldByOrphanReleaseGate) summary() string {
	if len(h.byRig) == 0 {
		return ""
	}
	keys := make([]string, len(h.order))
	copy(keys, h.order)
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		ids := h.byRig[key]
		sample := ids
		suffix := ""
		if len(sample) > heldByOrphanReleaseGateIDSampleLimit {
			suffix = fmt.Sprintf(" +%d more", len(sample)-heldByOrphanReleaseGateIDSampleLimit)
			sample = sample[:heldByOrphanReleaseGateIDSampleLimit]
		}
		parts = append(parts, fmt.Sprintf("%q (%d: %s%s)", h.rigNames[key], len(ids), strings.Join(sample, ", "), suffix))
	}
	return fmt.Sprintf("releaseOrphanedPoolAssignments: orphan_release=false held %d claims across %d rig(s) this pass: %s",
		h.claims(), len(keys), strings.Join(parts, ", "))
}

func (h *heldByOrphanReleaseGate) log() {
	if summary := h.summary(); summary != "" {
		log.Print(summary)
	}
}
