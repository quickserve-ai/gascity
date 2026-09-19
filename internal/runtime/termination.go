package runtime

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Termination records WHY a session ended, for every ending including the good
// one (ga-ksac39, Cherub 2026-09-18). The ratio it feeds — handoffs over all
// endings — only means something if the denominator is complete, so the record
// is written on `kind=handoff` exactly as it is on a force-kill.
//
// WHY THIS TYPE EXISTS RATHER THAN A map[string]string AT EACH CALL SITE: a
// census of the tree found no termination chokepoint at all. Twelve-plus
// production sites call Provider.Stop, and the bead-write half was never
// unified with the stop half — Manager.Kill wrote nothing, Manager.Suspend
// wrote through a raw store.Update, Manager.CloseDetailed wrote no terminal
// record, and cmd/gc/session_beads.go called sp.Stop directly, bypassing both
// Manager and worker.Handle. The near-chokepoint (workerKillSessionTargetWithConfig
// / verifiedStop) covers most DRAIN-driven paths and misses `gc session close`,
// `gc session suspend`'s fallback and both session_beads.go sweeps.
//
// That miss is not random, which is the whole point: the paths it misses are the
// FORCED ones — selection bias sitting exactly on the quantity the ratio exists
// to measure (katya, ga-ksac39 peer review). The first census also undercounted
// by three sites on its own second pass, which is why enforcement here is
// structural (a required argument plus a CI fence) and not an enumerated list of
// places to remember.
type Termination struct {
	// Kind is a CLOSED set. It is closed so the ratio cannot be diluted by a
	// new free-text string appearing in some future call site.
	Kind TerminationKind

	// Actor is who caused the ending: $GC_AGENT when a seat runs the CLI,
	// "human" for an operator, "controller"/"supervisor" for gc itself.
	Actor string

	// Reason carries the existing drain/sleep reason verbatim ("config-drift",
	// "idle", "orphaned", ...) or an operator's --reason. Free text by design:
	// Kind is the countable axis, Reason is the human one.
	Reason string

	// At is when the runtime actually stopped — set by the seam after Stop
	// returns, never by a caller.
	At time.Time

	// SessionID is the session bead's id, carried in the record rather than
	// looked up by the sink. A sink that resolved name->id would put a store
	// READ on the stop path, which is the one place a slow store must not
	// reach: force-exits cluster in the windows where the store is sick.
	// Callers already hold the Info or the id at every funneled site.
	SessionID string

	// RequestedAt is when the ending was REQUESTED: the drain ask, or the
	// seat's own `gc handoff` invocation for KindHandoff. Zero when the ending
	// had no distinct request (an observed-dead runtime was never asked).
	//
	// Timer B = At - RequestedAt. This field is here FROM BIRTH deliberately:
	// retrofitting a timestamp into a mandatory argument later means touching
	// every funneled caller a second time (katya, R2/condition 4).
	RequestedAt time.Time
}

// TerminationKind is the closed vocabulary of endings.
type TerminationKind string

const (
	// KindHandoff is the good ending: the seat ran `gc handoff` itself. This is
	// the ratio's numerator, and it is RECORDED LIKE EVERY OTHER KIND so the
	// denominator is complete.
	KindHandoff TerminationKind = "handoff"
	// KindDrainHandoff — the controller asked and the seat handed off in time.
	KindDrainHandoff TerminationKind = "drain-handoff"
	// KindDrainTimeout — the controller asked, the seat did not answer, forced.
	KindDrainTimeout TerminationKind = "drain-timeout"
	// KindDrainSilent — a controller drain with no ask. This is today's
	// behaviour and is expected to disappear when consent (S3) ships; it is a
	// named kind so that disappearance is VISIBLE rather than assumed.
	KindDrainSilent TerminationKind = "drain-silent"
	// KindRestartInPlace — the reconciler's config-drift restart-in-place
	// branch. It was missing from the v1 census entirely and was the dominant
	// forced ending for named seats during the 2026-09-14/15 drift waves.
	KindRestartInPlace TerminationKind = "restart-in-place"
	// KindOperatorKill / Close / Suspend — an operator acting through the CLI
	// or API.
	KindOperatorKill    TerminationKind = "operator-kill"
	KindOperatorClose   TerminationKind = "operator-close"
	KindOperatorSuspend TerminationKind = "operator-suspend"
	// KindHandoffTarget — `gc handoff --target` stopped another seat. Reported
	// on its own line, OUT of the headline numerator: a third party composed
	// that note, so whether it counts as a win is a judgement, not a default
	// baked into the formula (katya, R3).
	KindHandoffTarget TerminationKind = "handoff-target"
	// KindCityStop — `gc stop` / supervisor stop.
	KindCityStop TerminationKind = "city-stop"
	// KindObservedDead — zombie recycle, stale reap, async-start cleanup. There
	// was nothing to ask; excluded from the ratio's denominator.
	KindObservedDead TerminationKind = "observed-dead"

	// KindUnclassified is what a caller passes when it genuinely cannot say.
	//
	// It exists so that "I don't know" is VISIBLE AND COUNTED rather than
	// silent. Its share is this instrument's own health metric, with a
	// pre-registered gate: see UnclassifiedBudget.
	KindUnclassified TerminationKind = "unclassified"
)

// UnclassifiedBudget is the pre-registered ceiling on the share of terminations
// that may be KindUnclassified before the instrument is declared insufficient.
//
// PRE-REGISTERED 2026-09-19, BEFORE the baseline week, so it cannot be tuned to
// taste once the numbers are in (katya, condition 3; same discipline as the
// harness-tax pre-registration). A week in which unclassified exceeds this share
// is INSTRUMENT-INSUFFICIENT for the handoff KPI and must say so, rather than
// feed a ratio with quiet holes.
const UnclassifiedBudget = 0.05

// terminationKinds is the closed set, for validation and for the reader that
// wants to enumerate buckets without scraping the consts above.
var terminationKinds = map[TerminationKind]bool{
	KindHandoff: true, KindDrainHandoff: true, KindDrainTimeout: true,
	KindDrainSilent: true, KindRestartInPlace: true, KindOperatorKill: true,
	KindOperatorClose: true, KindOperatorSuspend: true, KindHandoffTarget: true,
	KindCityStop: true, KindObservedDead: true, KindUnclassified: true,
}

// Valid reports whether k is in the closed set.
func (k TerminationKind) Valid() bool { return terminationKinds[k] }

// CountsInDenominator reports whether an ending of this kind belongs in the
// handoff ratio's denominator. Only KindObservedDead is excluded: nothing could
// have been asked of a runtime that was already gone, so counting it would
// penalise the ratio for deaths no handoff policy could have prevented.
func (k TerminationKind) CountsInDenominator() bool {
	return k.Valid() && k != KindObservedDead
}

// CountsInNumerator reports whether an ending of this kind is a handoff for
// ratio purposes. KindHandoffTarget is deliberately NOT here — it is reported
// on its own line (katya, R3).
func (k TerminationKind) CountsInNumerator() bool {
	return k == KindHandoff || k == KindDrainHandoff
}

// TerminationKinds returns the closed set, sorted, for reporting code that
// needs stable bucket ordering.
func TerminationKinds() []TerminationKind {
	out := make([]TerminationKind, 0, len(terminationKinds))
	for k := range terminationKinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TerminationSink persists a termination record. Implementations are expected
// to be BEST-EFFORT and to return quickly: StopRecorded never lets a sink hold
// up a stop (see StopRecorded's contract).
//
// TWO SINKS, TWO FAILURE DOMAINS, deliberately (katya, condition 2). The bead
// write rides Dolt and may fail during exactly the incidents that produce
// force-exits; the session.terminated event is a local file append that almost
// never does. Reconciliation rule for whoever reads these: BEAD HISTORY IS THE
// SOURCE OF RECORD, and where a bead record is missing but an event exists, the
// event FILLS the hole and the week is FLAGGED — never silently patched.
type TerminationSink interface {
	RecordTermination(sessionName string, t Termination) error
}

// StopRecorded is the ONE sanctioned way to stop a session. Every path that
// ends a session goes through it; a direct Provider.Stop call outside this file
// is a fence violation (see scripts/fence_provider_stop.sh).
//
// The record argument is MANDATORY and its Kind must be in the closed set. A
// caller that cannot classify its ending passes KindUnclassified, which is
// counted and budgeted — it may not pass nothing. An invalid or empty Kind is
// COERCED to KindUnclassified rather than rejected, because refusing here would
// mean refusing to stop a session over a bookkeeping detail, which inverts the
// priority in rule (2) below.
//
// TWO RULES THAT ARE NOT NEGOTIABLE:
//
//  1. THE STOP NEVER WAITS ON THE RECORD. The sink is attempted first so that
//     `At` can be written by whoever observes the stop, but the stop proceeds
//     REGARDLESS of whether the sink succeeded, panicked, or hung the caller's
//     bookkeeping. A seat must ALWAYS be able to stop: that is ga-9n8hjv's
//     lesson (withhold the release, never the close), and force-exits cluster
//     precisely in the windows where the store is sick.
//
//  2. A SINK FAILURE IS REPORTED, NOT SWALLOWED. It comes back as a joined
//     error alongside the stop's own result, so a caller that cares can log it.
//     The stop's success or failure is what the returned error's Stop half
//     means; callers that only care whether the session is gone should use
//     IsSessionGone on it as before.
func StopRecorded(p Provider, name string, rec Termination, sinks ...TerminationSink) error {
	if !rec.Kind.Valid() {
		rec.Kind = KindUnclassified
	}
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	var sinkErrs []string
	for _, s := range sinks {
		if s == nil {
			continue
		}
		if err := recordSafely(s, name, rec); err != nil {
			sinkErrs = append(sinkErrs, err.Error())
		}
	}
	// The stop happens whatever the sinks did.
	stopErr := p.Stop(name)
	if len(sinkErrs) == 0 {
		return stopErr
	}
	if stopErr != nil {
		return fmt.Errorf("%w (termination record: %s)", stopErr, strings.Join(sinkErrs, "; "))
	}
	return fmt.Errorf("stopped, but the termination record failed: %s", strings.Join(sinkErrs, "; "))
}

// recordSafely contains a sink that panics. A bookkeeping sink must not be able
// to take down the stop path with it — the stop is the operation that matters.
func recordSafely(s TerminationSink, name string, rec Termination) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("termination sink panicked: %v", r)
		}
	}()
	return s.RecordTermination(name, rec)
}
