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
//
// The read is bounded. A live Get is a `bd show` with its own long subprocess
// timeout and retries, and this runs inside the reconcile tick, so a stalled
// store must not stall the tick. All of a tick's reads run concurrently under
// ONE budget (certParkReadBudget), and at most certParkReadSlots reads are ever
// outstanding. A read that misses the budget, or that finds no free slot, is
// "not parked" — the owner wakes, exactly as before this fix.

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// certParkReadBudget bounds how long one reconcile tick waits for all of its
// cert-park label reads together.
const certParkReadBudget = 2 * time.Second

// certParkReadSlots bounds the cert-park reads outstanding at once, across
// ticks. A read abandoned at the budget keeps its slot until its subprocess
// returns, so a stalled store caps the leaked reads at this number instead of
// adding a batch every tick. It caps a tick's reads only while they outlast
// the launch loop: a read that returns first frees its slot for a later row,
// so a fast store can serve more than this many rows in one tick. That is
// harmless, since the bound exists for the slow store (ga-isk41m).
var certParkReadSlots = make(chan struct{}, 8)

// certParkedWorkProbe reports which of the given assignedWorkBeads indexes are,
// right now, parked on a certification wait. An index absent from the result
// is not parked.
type certParkedWorkProbe func(rows []int) map[int]bool

// newLiveCertParkedWorkProbe reads work beads' labels through the LIVE handle of
// the store each row was read through, bounded by certParkReadBudget. It
// returns nil when rows and stores are not index-aligned, which leaves every
// assignment as wake demand.
//
// Every failure answers "not parked": a read error, a read that misses the
// budget or finds no free slot, a bead that is no longer in_progress, or one
// that changed hands. Each of those degrades to the pre-fix behavior (wake the
// owner), never to a stall.
func newLiveCertParkedWorkProbe(workBeads []beads.Bead, stores []beads.Store, stderr io.Writer) certParkedWorkProbe {
	return newCertParkedWorkProbeWithBudget(workBeads, stores, stderr, certParkReadBudget)
}

func newCertParkedWorkProbeWithBudget(workBeads []beads.Bead, stores []beads.Store, stderr io.Writer, budget time.Duration) certParkedWorkProbe {
	if len(workBeads) == 0 || len(stores) != len(workBeads) {
		return nil
	}
	if stderr == nil {
		stderr = io.Discard
	}
	readOne := func(i int) bool {
		row := workBeads[i]
		id := strings.TrimSpace(row.ID)
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
	return func(rows []int) map[int]bool {
		type verdict struct {
			row    int
			parked bool
		}
		// Buffered for every launch, so a read that lands after the budget
		// never blocks on a receiver that has gone.
		results := make(chan verdict, len(rows))
		launched := 0
		for _, i := range rows {
			if i < 0 || i >= len(workBeads) || stores[i] == nil || strings.TrimSpace(workBeads[i].ID) == "" {
				continue
			}
			select {
			case certParkReadSlots <- struct{}{}:
			default:
				fmt.Fprintf(stderr, "session reconciler: cert park label read for %s skipped: %d reads already outstanding (treating it as unparked)\n", workBeads[i].ID, cap(certParkReadSlots)) //nolint:errcheck
				continue
			}
			launched++
			go func(i int) {
				v := verdict{row: i, parked: readOne(i)}
				// Free the slot before reporting, so a tick that received every
				// verdict leaves no slot held; only a read abandoned at the
				// budget keeps one, until its subprocess returns.
				<-certParkReadSlots
				results <- v
			}(i)
		}
		parked := make(map[int]bool)
		if launched == 0 {
			return parked
		}
		timer := time.NewTimer(budget)
		defer timer.Stop()
		for received := 0; received < launched; received++ {
			select {
			case v := <-results:
				if v.parked {
					parked[v.row] = true
				}
			case <-timer.C:
				fmt.Fprintf(stderr, "session reconciler: %d of %d cert park label reads missed the %s budget (treating them as unparked)\n", launched-received, launched, budget) //nolint:errcheck
				return parked
			}
		}
		return parked
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
	var wanted, rows []int
	for j := range input.WorkBeads {
		wb := input.WorkBeads[j]
		if wb.Status != "in_progress" || wb.Blocked || wb.CertParked {
			continue
		}
		if !certParkReadWanted(input, decisions, wb) {
			continue
		}
		wanted = append(wanted, j)
		rows = append(rows, workSources[j])
	}
	if len(wanted) == 0 {
		return decisions
	}
	parked := probe(rows)
	marked := false
	for _, j := range wanted {
		if parked[workSources[j]] {
			input.WorkBeads[j].CertParked = true
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
