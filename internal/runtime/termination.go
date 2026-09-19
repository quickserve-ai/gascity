package runtime

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Termination records WHY a session ended, for every ending including the good
// one (ga-ksac39, Cherub 2026-09-18). The ratio it feeds — handoffs over all
// endings — only means something if the denominator is complete, so the record
// is written on `kind=handoff` exactly as it is on a force-kill.
//
// *** termination.* RECORDS THE ATTEMPT, NOT THE CONFIRMED OUTCOME. ***
//
// The sink pass runs BEFORE p.Stop returns — deliberately, so the record
// survives a stop that then wedges — which means a termination record asserts
// "an ending of this kind was attempted at this moment", never "the session is
// definitely gone". In production the reconciler's unavailable-probe window is
// exactly the case where the stop's outcome is UNKNOWN, and a record there
// legitimately disagrees with a session that turns out to still be running.
// Stated here because the next reader will otherwise call that disagreement
// corruption and go looking for a bug that is not there (katya, S1 review).
//
// RETRY INFLATION FOLLOWS FROM THE SAME WINDOW: a drain that cannot confirm
// completion retries, so ONE ending can write N history rows. The reader
// deduplicates on (SessionID, Kind, RequestedAt). That key works because
// RequestedAt is the drain's stable startedAt rather than a per-attempt clock
// — the same field that makes Timer B measurable is what makes the retries
// collapse to one ending. A writer that "helpfully" refreshed RequestedAt per
// attempt would break the dedup and inflate the denominator.
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

	// Actor is who caused the ending: $GC_AGENT when a seat runs the CLI, or
	// "controller"/"supervisor"/"reconciler"/"doctor"/"api" for gc itself.
	//
	// IT IS NEVER "human", DELIBERATELY. Nothing on this path can distinguish a
	// human at a terminal from a seat running the same command — both arrive
	// with the same environment — so asserting it would be a checkable but
	// wrong attribution, which is harder to unwind than a missing one. EMPTY is
	// a legitimate value: the sink applies the default that knows which process
	// it is running in, and the ga-fbzz9u reader reports empty on its own line.
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
	// behavior and is expected to disappear when consent (S3) ships; it is a
	// named kind so that disappearance is VISIBLE rather than assumed.
	KindDrainSilent TerminationKind = "drain-silent"
	// KindRestartInPlace — the reconciler's config-drift restart-in-place
	// branch. It was missing from the v1 census entirely and was the dominant
	// forced ending for named seats during the 2026-09-14/15 drift waves.
	KindRestartInPlace TerminationKind = "restart-in-place"
	// KindOperatorKill — an operator ended the session through `gc session
	// kill` or the API, without asking it first.
	KindOperatorKill TerminationKind = "operator-kill"
	// KindOperatorClose — an operator closed the session for good.
	KindOperatorClose TerminationKind = "operator-close"
	// KindOperatorSuspend — an operator suspended the session.
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
	// KindInterruptRestart — the runtime was stopped so it could be restarted
	// IMMEDIATELY AND IN PLACE, mid-conversation, with the seat's work resumed
	// on the other side: a hard-restart interrupt, or the fallback when an
	// interrupt's idle or boundary wait times out (internal/session/submit.go).
	//
	// EXCLUDED FROM THE DENOMINATOR ON CONTEXT SURVIVAL, which is the sharper
	// principle katya named in S1 review and is NOT observed-dead's
	// nothing-to-ask. The kind table has two axes — did the seat's CONTEXT end,
	// and was a handoff POSSIBLE — and this kind is the case where context
	// survives. That is also why KindRestartInPlace stays IN: the reconciler's
	// config-drift restart really does end the context.
	//
	// Counting these would be actively misleading rather than merely noisy:
	// interrupts are routine and high-traffic, so a busy day of them would read
	// as a day of force-exits and the ratio would fall for a reason that has
	// nothing to do with handoff discipline.
	//
	// EXCLUDED BUT REPORTED, on its own line like KindHandoffTarget. A bucket
	// that leaves the ratio must still be visible, or the exclusion becomes
	// indistinguishable from the events never happening.
	//
	// *** NAMED CAVEAT: THIS KIND ASSUMES THE RESUME SUCCEEDS, AND THE SEAM
	// CANNOT VERIFY THAT. *** The exclusion is only sound while the restart
	// actually restores the seat's context. A restart that silently fails to
	// resume IS a lost context wearing this label, and it would be excluded
	// from the denominator precisely when it should count. Nothing on the stop
	// path can observe the other side of the restart, so this is recorded as an
	// assumption rather than defended as a fact (katya, condition 2).
	//
	// DISTINCT FROM KindRestartInPlace, which is the reconciler's config-drift
	// restart. That one really does end a seat's session without asking, and
	// belongs in the denominator.
	KindInterruptRestart TerminationKind = "interrupt-restart"

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

// TerminationSinkBudget is the WALL-CLOCK ceiling on the whole sink pass before
// StopRecorded proceeds to the stop regardless.
//
// PRE-REGISTERED 2026-09-19 beside UnclassifiedBudget, and it exists because
// rule 1 below was ASPIRATIONAL without it (katya, S1 review). "The stop never
// waits on the record" was enforced against sink FAILURE and sink PANIC, but
// not against a sink that simply does not return. The bead sink's write is
// store.ApplyPatch riding the Dolt pool, whose natural bound under load is the
// mysql driver's ~2-minute silent read timeout — measured on this box at ~118s
// giving up on a ~176s write (ga-pz7oqg). So `gc session kill`, which is an
// INCIDENT REMEDY run precisely when the store is sick, would block up to two
// minutes before killing anything, and `gc stop` would pay it once per session.
//
// FAILURE-TOLERATED IS NOT LATENCY-BOUNDED. That distinction is the whole
// defect: every sink here is allowed to fail, and none was allowed to take its
// time, and only one of those was actually enforced.
const TerminationSinkBudget = 5 * time.Second

// terminationKinds is the closed set, for validation and for the reader that
// wants to enumerate buckets without scraping the consts above.
var terminationKinds = map[TerminationKind]bool{
	KindHandoff: true, KindDrainHandoff: true, KindDrainTimeout: true,
	KindDrainSilent: true, KindRestartInPlace: true, KindOperatorKill: true,
	KindOperatorClose: true, KindOperatorSuspend: true, KindHandoffTarget: true,
	KindCityStop: true, KindObservedDead: true, KindInterruptRestart: true,
	KindUnclassified: true,
}

// Valid reports whether k is in the closed set.
func (k TerminationKind) Valid() bool { return terminationKinds[k] }

// CountsInDenominator reports whether an ending of this kind belongs in the
// handoff ratio's denominator. TWO kinds are excluded, on one principle: no
// handoff policy could have prevented either, so counting them would penalize
// the ratio for endings it does not govern. KindObservedDead is a runtime that
// was already gone — nothing could have been asked of it. KindInterruptRestart
// is not an ending at all — the session restarts in place and the conversation
// continues across it.
func (k TerminationKind) CountsInDenominator() bool {
	return k.Valid() && k != KindObservedDead && k != KindInterruptRestart
}

// CountsInNumerator reports whether an ending of this kind is a handoff for
// ratio purposes. KindHandoffTarget is deliberately NOT here — it is reported
// on its own line (katya, R3).
func (k TerminationKind) CountsInNumerator() bool {
	return k == KindHandoff || k == KindDrainHandoff
}

// MustCarryRequestedAt reports whether an ending of this kind is REQUIRED to
// carry a RequestedAt, so a zero there is an instrument defect rather than a
// design choice.
//
// THE AMBIGUITY IS RESOLVED BY KIND, NOT BY A SENTINEL VALUE (katya, S1
// review). A sentinel inside a timestamp field is a trap for every future
// parser: it reads as a real instant to anything that does not know the
// convention, and there is no way to tell a parser that does not know. So zero
// stays zero and MEANS different things depending on the kind, which this
// function makes checkable instead of conventional.
//
//	drain-handoff, drain-timeout  MUST carry it — the controller asked at a
//	                              distinct earlier moment, and At - RequestedAt
//	                              is Timer B. Zero here is a DEFECT and counts
//	                              against the instrument's health budget
//	                              alongside unclassified.
//	handoff                       carries the seat's own invocation time.
//	operator-*, city-stop,        NEVER carry it — a synchronous call has no
//	observed-dead, and the rest   request distinct from the action, and a
//	                              stamped "now" would mint a Timer B of ~0ms
//	                              that reads as a measurement.
//
// Frozen here BEFORE the baseline week so ga-fbzz9u's reader and this writer
// cannot drift into disagreeing about what an empty field means.
func (k TerminationKind) MustCarryRequestedAt() bool {
	return k == KindDrainHandoff || k == KindDrainTimeout
}

// ReportedSeparately reports whether a kind that is OUT of the headline ratio
// must still be shown on its own line.
//
// A BUCKET THAT LEAVES THE RATIO MUST NOT LEAVE THE REPORT. Otherwise the
// exclusion is indistinguishable from the events never having happened, which
// is the same quiet-hole failure the denominator rules exist to prevent
// (katya, condition 1; the precedent is KindHandoffTarget).
func (k TerminationKind) ReportedSeparately() bool {
	return k == KindHandoffTarget || k == KindInterruptRestart || k == KindObservedDead
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
	stopErr, recErr := StopRecordedDetailed(p, name, rec, sinks...)
	if recErr == nil {
		return stopErr
	}
	if stopErr != nil {
		return fmt.Errorf("%w (termination record: %w)", stopErr, recErr)
	}
	return fmt.Errorf("stopped, but the termination record failed: %w", recErr)
}

// ErrTerminationSinkTimeout marks a sink pass that outlived
// TerminationSinkBudget. It is a sentinel so a reader can tell "the store was
// slow" from "the record failed" — those are different diagnoses and the first
// one clusters in exactly the incident windows that produce force-exits.
var ErrTerminationSinkTimeout = errors.New("termination record timed out")

// ErrTerminationRecord wraps every sink failure StopRecordedDetailed reports,
// so a caller can ask whether a non-nil error is ONLY a bookkeeping problem.
var ErrTerminationRecord = errors.New("termination record")

// StopRecordedDetailed is StopRecorded with the two outcomes kept APART:
// stopErr is whether the session is gone, recErr is whether the ending was
// written down. They are separate questions and a caller usually only has a
// contract about the first.
//
// THIS EXISTS BECAUSE CONFLATING THEM BREAKS CALLERS, which is not theoretical:
// wiring the event sink into worker.RuntimeHandle immediately turned every
// successful Kill into a non-nil error, because the event sink deliberately
// reports "emitted to a recorder that cannot acknowledge it" through a plain
// void Recorder — the common configuration. A handle whose contract is `Kill
// returns an error when the kill failed` must not start failing because
// bookkeeping was merely unconfirmable. That is a worse outcome than an
// uncounted ending: it makes a working stop look broken.
//
// SINK ERRORS KEEP THEIR IDENTITY. They are joined with errors.Join and
// wrapped with %w, not stringified, so terminationevents.IsUnacknowledgedEvent
// and any other sentinel still answer through the returned error. The previous
// implementation joined err.Error() strings, which silently made every such
// sentinel untestable through this function — the sentinels existed and could
// never fire.
func StopRecordedDetailed(p Provider, name string, rec Termination, sinks ...TerminationSink) (stopErr, recErr error) {
	if !rec.Kind.Valid() {
		rec.Kind = KindUnclassified
	}
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	// THE SINK PASS RUNS UNDER A WALL-CLOCK BUDGET. Within it the semantics are
	// exactly what they were: every sink is attempted, failures are collected
	// and reported. Past it, the stop proceeds and the timeout is reported AS a
	// sink error — rule 2 intact, reported and not swallowed.
	//
	// THE LATE WRITE IS LEFT TO LAND. A timed-out sink is not canceled: the
	// goroutine keeps running and its write arrives whenever the store
	// recovers. That is safe because TerminationPatch is idempotent for the
	// same values, so a late landing writes what a timely one would have. The
	// alternative — canceling — would turn "slow store" into "lost record",
	// which is the outcome this whole seam exists to prevent.
	done := make(chan []error, 1)
	go func() {
		var errs []error
		for _, s := range sinks {
			if s == nil {
				continue
			}
			if err := recordSafely(s, name, rec); err != nil {
				errs = append(errs, err)
			}
		}
		done <- errs
	}()

	var sinkErrs []error
	select {
	case sinkErrs = <-done:
	case <-time.After(TerminationSinkBudget):
		sinkErrs = []error{fmt.Errorf("%w after %s; the write is left to land late",
			ErrTerminationSinkTimeout, TerminationSinkBudget)}
	}

	// The stop happens whatever the sinks did.
	stopErr = p.Stop(name)
	if len(sinkErrs) > 0 {
		recErr = fmt.Errorf("%w: %w", ErrTerminationRecord, errors.Join(sinkErrs...))
	}
	return stopErr, recErr
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
