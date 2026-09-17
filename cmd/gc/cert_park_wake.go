package main

// A bead parked for certification must not wake its owner (ga-mzovhi).
//
// Park-and-pivot (ga-5zosxs) keeps a PR's carrier bead in_progress under its
// owner, labeled hold:cert-wait, while the westeros queue runs (7-12h). The
// assigned-work wake arm read "in_progress and assigned to you" as demand, so a
// pool seat that drain-acked because it had nothing actionable was respawned on
// the next tick, found the same parked bead, and drained again — every 1-2
// minutes, each cycle a full provider boot.
//
// The hard constraint is the resume. The cert-landing-patrol flips the bead
// durably (hold:cert-wait -> cert:landed | cert:action) and only THEN nudges the
// owner, because a nudge does not survive a restart. So the park is read from
// the bead's labels every tick it matters, and removing the hold restores the
// assigned-work demand by itself: the flip wakes the owner even if the nudge is
// lost.
//
// Why a live read: the controller's cached in_progress rows come from a
// reconcile scan that passes `bd list --skip-labels`, so their labels are empty
// (or stale from the last event). Trusting them would make this fix a silent
// no-op in production, and a stale cached hold would suppress a real resume.
// The read is scoped to exactly the work whose assigned-work arm is about to
// START a sleeping owner, so a live seat, an always-on named seat, or a seat
// woken for any other reason costs nothing, and a parked sleeping owner costs
// one bead read per tick.

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// certParkedWorkProbe reports whether assignedWorkBeads[i] is, right now,
// parked on a certification wait.
type certParkedWorkProbe func(i int) bool

// newLiveCertParkedWorkProbe reads a work bead's labels through the LIVE handle
// of the store the row was read through. It returns nil when rows and stores
// are not index-aligned, which leaves every assignment as wake demand.
//
// Every failure answers "not parked": a read error, a bead that is no longer
// in_progress, or one that changed hands. Each of those degrades to the pre-fix
// behavior (wake the owner), never to a stall.
func newLiveCertParkedWorkProbe(workBeads []beads.Bead, stores []beads.Store, stderr io.Writer) certParkedWorkProbe {
	if len(workBeads) == 0 || len(stores) != len(workBeads) {
		return nil
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return func(i int) bool {
		if i < 0 || i >= len(workBeads) || stores[i] == nil {
			return false
		}
		row := workBeads[i]
		id := strings.TrimSpace(row.ID)
		if id == "" {
			return false
		}
		fresh, err := beads.HandlesFor(stores[i]).Live.Get(id)
		if err != nil {
			fmt.Fprintf(stderr, "session reconciler: reading cert park labels for %s: %v (treating it as unparked)\n", id, err) //nolint:errcheck
			return false
		}
		if fresh.Status != "in_progress" || strings.TrimSpace(fresh.Assignee) != strings.TrimSpace(row.Assignee) {
			return false
		}
		return beadmeta.CertParkSuppressesAssignedWake(fresh.Labels)
	}
}

// computeAwakeSetWithCertParks is ComputeAwakeSet with parked certification
// waits taken out of assigned-work demand. It computes the awake set, reads the
// park labels of the in_progress work whose assigned-work arm would start a
// sleeping owner, marks the parked ones on input, and recomputes only when it
// marked something. workSources maps input.WorkBeads to the probe's row index.
func computeAwakeSetWithCertParks(input *AwakeInput, workSources []int, probe certParkedWorkProbe) map[string]AwakeDecision {
	decisions := ComputeAwakeSet(*input)
	if probe == nil || len(workSources) != len(input.WorkBeads) {
		return decisions
	}
	marked := false
	for j := range input.WorkBeads {
		wb := &input.WorkBeads[j]
		if wb.Status != "in_progress" || wb.Blocked || wb.CertParked {
			continue
		}
		if !certParkReadWanted(input, decisions, *wb) {
			continue
		}
		if probe(workSources[j]) {
			wb.CertParked = true
			marked = true
		}
	}
	if !marked {
		return decisions
	}
	return ComputeAwakeSet(*input)
}

// certParkReadWanted reports whether the park verdict for wb could change a
// wake this tick: some owner of wb is not running, is not an always-on named
// seat (which wakes regardless of its work), and is being woken by the
// assigned-work arm. A running owner is never read — a live seat decides for
// itself what to do with a parked bead.
func certParkReadWanted(input *AwakeInput, decisions map[string]AwakeDecision, wb AwakeWorkBead) bool {
	assignee := strings.TrimSpace(wb.Assignee)
	if assignee == "" {
		return false
	}
	wanted := false
	for _, bead := range input.SessionBeads {
		if bead.State == "closed" || !sessionAssigneeMatches(input.NamedSessions, bead, assignee) {
			continue
		}
		if input.RunningSessions[bead.SessionName] {
			return false
		}
		if isAlwaysNamedSession(input.NamedSessions, bead) {
			continue
		}
		if d := decisions[bead.SessionName]; d.ShouldWake && d.Reason == "assigned-work" {
			wanted = true
		}
	}
	return wanted
}

// certParkedOwnerDecision reports whether a session's only assigned work is
// parked on a certification wait. Such a seat is asleep by design, not
// stranded: the stranded-worker repair must not unassign its parked bead.
func certParkedOwnerDecision(decision AwakeDecision, hasDecision bool) bool {
	return hasDecision && decision.HasCertParkedWork && !decision.HasAssignedWork
}
