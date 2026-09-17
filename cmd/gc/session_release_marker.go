package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
)

// Deferred release obligations (ga-dt5ffp).
//
// WHAT THIS EXISTS FOR. "gc session close" releases the work beads still
// assigned to the dying session so the pool picks the demand back up. That
// release is cfg-driven: the config is what distinguishes a still-configured
// [[named_session]] — whose portfolio must survive the close — from a retired
// or pool session, whose work must be freed. When the city config does not
// load, the close FAILS CLOSED and withholds every name-shaped identifier,
// because releasing blind strips a named agent's whole portfolio (the
// 2026-09-11 wave: 50 beads cleared in one write, 11 downgraded in_progress ->
// open, which made the owner's own resume check report "no work").
//
// Failing closed is right. The defect is what happens next: NOTHING reclaims
// what the close withholds. CloseDetailed has already closed the session bead,
// the reconciler's snapshot drops closed sessions by design
// (session_bead_snapshot.go), and releaseOrphanedPoolAssignments skips unrouted
// work. So the close is the only actor that can release this work, and it makes
// an irreversible decision while missing the information it needs to decide.
//
// This file supplies the missing half: before the close, publish a durable,
// immutable OBLIGATION on the session bead recording what was withheld and why,
// so an actor that later holds a real config can find it. It deliberately
// releases NOTHING itself — under a nil config there is no safe release to
// make, and the whole point of the fail-closed guard is that guessing here is
// what caused the incident.
//
// WHAT THIS IS NOT. There is no drain yet. Publishing the obligation turns an
// invisible withhold into a queued and observable one; it does not recover the
// work. That is deliberate sequencing, not an oversight: as measured on hq
// (2026-09-15) every alias-held work bead in the store is CLOSED, so there is
// currently zero live exposure for a drain to be validated against, and a drain
// written blind against no live case is how the easy version of this fix would
// have quietly widened the race it was meant to close.
//
// THE FIVE PROPERTIES THE OBLIGATION HAS TO HAVE, and why each one is here:
//
//  1. CAPTURED BEFORE THE CLOSE. Manager.CloseDetailed calls
//     retireConfiguredNamedSessionIdentifiers, which blanks session_name and
//     session_name_explicit and pushes the alias into alias_history. Two of the
//     identifiers a release needs are gone the moment the close returns, so a
//     post-close capture reads degraded state BY CONSTRUCTION. Pre-close is the
//     only moment the set is authoritative.
//
//  2. THE CRASH WINDOW ORDERED AS THE SAFE ASYMMETRY. Marker-before-close means
//     a crash between the two leaves an obligation on a session that is still
//     OPEN. That is inert — a drain's precondition is "session bead is closed
//     AND an obligation is present AND this generation is unacknowledged", and
//     the reconciler goes on handling the open session normally.
//     Marker-after-close would lose the obligation entirely on the same crash.
//     Order it so the failure mode is an IGNORED obligation, never a MISSING
//     one.
//
//  3. WRITE-ONCE PER GENERATION, ENFORCED BY THE STORE. A second close on the
//     same bead reads the already-retired identifiers (property 1) and would
//     otherwise overwrite a complete obligation with a degraded one. The
//     mutation lock inside the process does not serialize two CLI invocations,
//     so the obligation has to defend itself rather than rely on a lock we do
//     not have: it is published through CompareAndSetMetadataKey against an
//     absent value, so exactly one writer wins.
//
//  4. COMPLETENESS RECORDED EXPLICITLY. The pre-close read can fail, leaving
//     only the resolved session ID (cmd_session.go's ID-only fallback). A short
//     identifier list must never read as "there was nothing more to release".
//     An id_only obligation is a DIFFERENT OBJECT from a full one: it does not
//     satisfy the write-once check, so a later close that manages a full
//     capture may upgrade it — the one legal rewrite, and it bumps the
//     generation so an acknowledgement of the degraded obligation does not
//     discharge the complete one.
//
//  5. STORE SCOPE STATED, NOT IMPLIED. Rig stores are enumerated only under
//     "cityErr == nil && cfg != nil", because buildStandaloneRigStores needs the
//     very config that failed to load. So on this path the rig set is
//     UNENUMERATED, not empty, and the obligation says so. A drain that read an
//     absent rig list as "no rig stores" would silently drop outstanding rig
//     cleanup — and would then clear the obligation, making the drop permanent.
//
// The obligation is immutable. A drain records its progress under
// beadmeta.ReleaseDeferredAckMetadataKey, per generation and per store swept,
// so discharge never contends with the obligation for one value.
//
// DO NOT CLEAR THE OBLIGATION KEY — not even to tidy up after a successful
// drain. CompareAndSetMetadataKey's expected=="" matches a key that is ABSENT or
// PRESENT-BUT-EMPTY (the two are indistinguishable to callers), and
// readSessionReleaseObligation treats an empty value as "no obligation" for the
// same reason. So clearing the key does not mark the obligation discharged, it
// RE-ARMS publication: the next close of that bead wins the swap and mints a
// fresh generation-1 obligation for work that was already released. Discharge is
// recorded by ADDING an acknowledgement, never by removing the obligation.

// Capture completeness values for sessionReleaseObligation.Capture.
const (
	// releaseCaptureFull means the pre-close session bead was read, so the
	// identifier set is everything the bead could be assigned under.
	releaseCaptureFull = "full"
	// releaseCaptureIDOnly means the pre-close read failed and the only known
	// identifier is the resolved session ID. Treat the list as a floor.
	releaseCaptureIDOnly = "id_only"
)

// sessionReleaseObligation is the immutable JSON document stored at
// beadmeta.ReleaseDeferredMetadataKey.
//
// Identities is the complete capture, which includes the session's own bead ID.
// The close releases that one form itself (a bead ID cannot be a configured
// named identity, so freeing it needs no config), so a drain will find it
// already unassigned. It stays in the list because this is a record of what the
// session could be assigned under, not a diff of what remains — and a drain
// re-releasing an already-released identifier is idempotent, whereas a drain
// working from a set someone pre-filtered is not auditable.
type sessionReleaseObligation struct {
	// Generation is 1 for a first publish. It rises only when a degraded
	// id_only obligation is replaced by a full capture. An acknowledgement
	// names one generation; it never discharges the key as a whole.
	Generation int `json:"generation"`
	// DeferredAt is when the obligation was published, RFC3339 UTC.
	DeferredAt string `json:"deferred_at"`
	// Reason is why the city config was unavailable, as reported at close time.
	Reason string `json:"reason,omitempty"`
	// Capture is releaseCaptureFull or releaseCaptureIDOnly.
	Capture string `json:"capture"`
	// Identities is every identifier under which work could be assigned to this
	// session, captured before the close mutated them.
	Identities []string `json:"identities,omitempty"`
	// RigStoresKnown is false when rig stores could not be enumerated. A drain
	// must read false as "scope unknown", never as "no rig stores".
	RigStoresKnown bool `json:"rig_stores_known"`
}

// complete reports whether this obligation carries the full pre-close
// identifier set. Only a complete obligation is write-once; an incomplete one
// may be upgraded exactly once, by a later close that manages a full capture.
func (o sessionReleaseObligation) complete() bool {
	return o.Capture == releaseCaptureFull
}

// recognized reports whether this decoded document is one of the shapes this
// build writes. An unrecognized document is never rewritten — see the refusal in
// publishDeferredReleaseObligation for why treating it as "not complete, so
// upgradeable" is the dangerous reading.
func (o sessionReleaseObligation) recognized() bool {
	switch o.Capture {
	case releaseCaptureFull, releaseCaptureIDOnly:
	default:
		return false
	}
	return o.Generation >= 1
}

// deferredReleaseResult reports what publishing the obligation did, so the
// caller can tell the operator and the event bus the same story.
//
// Persisted is the field that matters. It is true when this call published an
// obligation OR found an equal-or-better one already there; false means no
// automated path can ever find this withheld work.
type deferredReleaseResult struct {
	// Persisted is whether a usable obligation is on the bead now.
	Persisted bool
	// Published is whether THIS call wrote it (false when one already existed).
	Published bool
	// Obligation is what is on the bead now, as far as this call could tell.
	Obligation sessionReleaseObligation
	// ConditionalWrite is whether the store's compare-and-set primitive backed
	// the write-once guarantee. False means the store did not offer it and a
	// read-then-write fallback ran, which two simultaneous closes could race.
	ConditionalWrite bool
	// Err is why nothing usable is on the bead, when Persisted is false.
	Err error
}

// buildSessionReleaseObligation captures the pre-close obligation for a session
// whose work release is about to be withheld.
//
// captureOK must be the caller's own answer to "did I actually read the session
// bead", not a guess from the bead's contents: a failed read yields a synthetic
// shell carrying the session ID, which is indistinguishable from a real bead
// that happens to have no metadata.
//
// A SUCCESSFUL READ IS NOT ENOUGH TO CALL THE CAPTURE COMPLETE, and getting this
// wrong re-opens the exact loss the obligation exists to prevent. "Before the
// close" means before ANY close, not before this invocation's. Concretely:
// invocation A's pre-close read fails and publishes an id_only obligation; A's
// CloseDetailed then retires session_name and closes the bead; invocation B
// reads that closed, retired bead SUCCESSFULLY and — on captureOK alone — would
// label a degraded snapshot "full" and be authorized by the write-once rule to
// replace the id_only obligation with it. The work held under the retired name
// would then be omitted permanently.
//
// A closed session bead is therefore treated as an incomplete capture. Status is
// the right discriminator because it is structural rather than one of the
// metadata fields whose corruption is this bug class (ga-hoy4vl): the retire
// runs inside the same CloseDetailed that sets the status, so "closed" implies
// "identifiers may already be degraded" without having to detect the degradation
// field by field.
//
// RigStoresKnown is always false. Obligations are published only on the close
// path where the city config failed to load, and rig stores can only be
// enumerated from that config (buildStandaloneRigStores), so the scope is never
// known here. The field stays in the persisted record and the event payload so
// a drain reads false as "scope unknown", never as "no rig stores".
func buildSessionReleaseObligation(sessionBead beads.Bead, captureOK bool, reason string, now time.Time) sessionReleaseObligation {
	preRetirement := captureOK && !strings.EqualFold(strings.TrimSpace(sessionBead.Status), "closed")
	capture := releaseCaptureIDOnly
	identities := []string{}
	if preRetirement {
		capture = releaseCaptureFull
		identities = sessionBeadAssigneeIdentities(sessionBead)
	} else if captureOK {
		// The bead was readable but already closed. Keep whatever identifiers
		// survive — they are a floor, not the set — and label the capture
		// incomplete so nothing treats this as authoritative.
		identities = sessionBeadAssigneeIdentities(sessionBead)
	} else if id := strings.TrimSpace(sessionBead.ID); id != "" {
		identities = []string{id}
	}
	return sessionReleaseObligation{
		Generation:     1,
		DeferredAt:     now.UTC().Format(time.RFC3339),
		Reason:         strings.TrimSpace(reason),
		Capture:        capture,
		Identities:     identities,
		RigStoresKnown: false,
	}
}

// publishDeferredReleaseObligation writes the obligation to the session bead,
// write-once, and reports what it found or did.
//
// It is called BEFORE the close (property 1) and it never releases work.
func publishDeferredReleaseObligation(store beads.Store, sessionID string, obligation sessionReleaseObligation) deferredReleaseResult {
	sessionID = strings.TrimSpace(sessionID)
	if store == nil || sessionID == "" {
		return deferredReleaseResult{Err: fmt.Errorf("no session store or session id to publish the deferred release obligation on")}
	}
	encoded, err := json.Marshal(obligation)
	if err != nil {
		return deferredReleaseResult{Err: fmt.Errorf("encoding the deferred release obligation: %w", err)}
	}

	// MetadataCASWriterFor, NOT ConditionalWriterFor — this is load-bearing and it
	// was wrong in the first version of this file.
	//
	// ConditionalWriterFor asks for the full four-method ConditionalWriter and does
	// NOT follow a wrapper's declared ConditionalWritesResolveTarget. The store the
	// CLI actually holds is *beadPolicyStore, which embeds the Store INTERFACE (so
	// optional capabilities are not promoted through it) and participates only by
	// declaring that target. So ConditionalWriterFor returns false on every real
	// invocation and this function silently took the non-atomic fallback in
	// production, while a test against a bare MemStore — which implements
	// ConditionalWriter directly — passed. A present-but-inapplicable guard, which
	// is the same failure mode as ga-9n8hjv itself.
	//
	// MetadataCASWriterFor follows the resolve target and asks only for the one
	// method we need. Its doc comment names the cmd/gc policy store as the reason
	// it exists. It also reaches stores (native Dolt) that offer a sound
	// single-key CAS without being able to fence on a revision.
	writer, conditional := beads.MetadataCASWriterFor(store)
	if !conditional {
		// The store does not offer compare-and-set, so the write-once guarantee
		// degrades to read-then-write. Say so in the result rather than letting
		// the caller assume property 3 held: two simultaneous closes can both
		// read "absent" here and the second can land a degraded obligation over
		// a complete one.
		return publishDeferredReleaseObligationUnconditionally(store, sessionID, obligation, string(encoded))
	}

	swapped, err := writer.CompareAndSetMetadataKey(sessionID, beadmeta.ReleaseDeferredMetadataKey, "", string(encoded))
	if err != nil {
		return deferredReleaseResult{ConditionalWrite: true, Err: fmt.Errorf("publishing the deferred release obligation: %w", err)}
	}
	if swapped {
		return deferredReleaseResult{Persisted: true, Published: true, Obligation: obligation, ConditionalWrite: true}
	}

	// Lost the swap: an obligation is already there. Whether we may replace it
	// depends on which object it is (property 4), so read it rather than assume.
	existing, existingRaw, readErr := readSessionReleaseObligation(store, sessionID)
	if readErr != nil {
		// An obligation exists but we cannot read it. Do NOT overwrite it — an
		// undecodable obligation is still someone's evidence, and destroying it
		// to install a tidier one is the one outcome with no recovery.
		return deferredReleaseResult{ConditionalWrite: true, Err: fmt.Errorf("a deferred release obligation is already present on %s but could not be read, so it was left untouched: %w", sessionID, readErr)}
	}
	if !existing.recognized() {
		// The document decoded but is not one of the two shapes this code writes —
		// an unknown capture value, a non-positive generation, a bare "{}". Treat
		// it exactly like the undecodable case: leave it alone and report. Reading
		// "not complete" off an unrecognized document and then "upgrading" it would
		// overwrite a future writer's format, and would mint generation 1 for a
		// bead whose acknowledgements may already name generation 1.
		return deferredReleaseResult{ConditionalWrite: true, Err: fmt.Errorf("the deferred release obligation already on %s is not a shape this build recognizes (capture=%q generation=%d), so it was left untouched", sessionID, existing.Capture, existing.Generation)}
	}
	if existing.complete() || !obligation.complete() {
		// Write-once: a complete obligation is never rewritten, and a degraded
		// capture never replaces anything.
		return deferredReleaseResult{Persisted: true, Obligation: existing, ConditionalWrite: true}
	}

	// The one legal rewrite: a full capture upgrading an id_only obligation.
	// The generation rises so an acknowledgement of the degraded obligation does
	// not discharge this one.
	upgraded := obligation
	upgraded.Generation = existing.Generation + 1
	upgradedEncoded, err := json.Marshal(upgraded)
	if err != nil {
		return deferredReleaseResult{Persisted: true, Obligation: existing, ConditionalWrite: true, Err: fmt.Errorf("encoding the upgraded deferred release obligation: %w", err)}
	}
	swapped, err = writer.CompareAndSetMetadataKey(sessionID, beadmeta.ReleaseDeferredMetadataKey, existingRaw, string(upgradedEncoded))
	if err != nil {
		return deferredReleaseResult{Persisted: true, Obligation: existing, ConditionalWrite: true, Err: fmt.Errorf("upgrading the deferred release obligation: %w", err)}
	}
	if !swapped {
		// Someone else moved it between our read and our write. Theirs stands;
		// the previous obligation is still on the bead, so nothing is lost.
		return deferredReleaseResult{Persisted: true, Obligation: existing, ConditionalWrite: true}
	}
	return deferredReleaseResult{Persisted: true, Published: true, Obligation: upgraded, ConditionalWrite: true}
}

// publishDeferredReleaseObligationUnconditionally is the fallback for a store
// with no compare-and-set metadata primitive. It preserves the write-once
// DECISION (never overwrite a complete obligation) but not its atomicity, and
// the result says so.
func publishDeferredReleaseObligationUnconditionally(store beads.Store, sessionID string, obligation sessionReleaseObligation, encoded string) deferredReleaseResult {
	existing, _, readErr := readSessionReleaseObligation(store, sessionID)
	upgrading := false
	switch {
	case readErr != nil && !isMissingReleaseObligation(readErr):
		return deferredReleaseResult{Err: fmt.Errorf("reading any existing deferred release obligation on %s: %w", sessionID, readErr)}
	case readErr == nil && (existing.complete() || !obligation.complete()):
		return deferredReleaseResult{Persisted: true, Obligation: existing}
	case readErr == nil && !existing.recognized():
		return deferredReleaseResult{Err: fmt.Errorf("the deferred release obligation already on %s is not a shape this build recognizes (capture=%q generation=%d), so it was left untouched", sessionID, existing.Capture, existing.Generation)}
	case readErr == nil:
		upgrading = true
		obligation.Generation = existing.Generation + 1
		reEncoded, err := json.Marshal(obligation)
		if err != nil {
			return deferredReleaseResult{Persisted: true, Obligation: existing, Err: fmt.Errorf("encoding the upgraded deferred release obligation: %w", err)}
		}
		encoded = string(reEncoded)
	}
	if err := store.Update(sessionID, beads.UpdateOpts{
		Metadata: map[string]string{beadmeta.ReleaseDeferredMetadataKey: encoded},
	}); err != nil {
		// An UPGRADE that fails leaves the previous obligation in place, so
		// something usable IS still on the bead. Reporting Persisted=false here
		// would print "NOTHING will find it" at an operator whose obligation is
		// intact — a false alarm in the one message that must never cry wolf.
		if upgrading {
			return deferredReleaseResult{Persisted: true, Obligation: existing, Err: fmt.Errorf("upgrading the deferred release obligation: %w", err)}
		}
		return deferredReleaseResult{Err: fmt.Errorf("publishing the deferred release obligation: %w", err)}
	}
	return deferredReleaseResult{Persisted: true, Published: true, Obligation: obligation}
}

// errNoReleaseObligation reports that a session bead carries no deferred
// release obligation. It is a normal answer, not a failure.
var errNoReleaseObligation = fmt.Errorf("no deferred release obligation")

// isMissingReleaseObligation reports whether err is the benign "there is no
// obligation here" answer rather than a real read failure.
func isMissingReleaseObligation(err error) bool {
	return err == errNoReleaseObligation //nolint:errorlint // sentinel is returned directly, never wrapped
}

// readSessionReleaseObligation decodes the obligation on a session bead,
// returning it alongside the RAW stored string — which the compare-and-set
// upgrade needs, since it must swap against the exact bytes on the bead rather
// than a re-encoding of them.
func readSessionReleaseObligation(store beads.Store, sessionID string) (sessionReleaseObligation, string, error) {
	bead, err := store.Get(sessionID)
	if err != nil {
		return sessionReleaseObligation{}, "", err
	}
	// raw is returned UNTRIMMED. The upgrade path compares it byte-for-byte in a
	// compare-and-set, so handing back a trimmed copy of a value stored with
	// surrounding whitespace would decode fine and then lose every swap, silently.
	// Trim only for the emptiness test and the decode.
	raw := bead.Metadata[beadmeta.ReleaseDeferredMetadataKey]
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return sessionReleaseObligation{}, "", errNoReleaseObligation
	}
	var obligation sessionReleaseObligation
	if err := json.Unmarshal([]byte(trimmed), &obligation); err != nil {
		return sessionReleaseObligation{}, raw, fmt.Errorf("decoding the deferred release obligation: %w", err)
	}
	return obligation, raw, nil
}

// deferMissingConfigWorkRelease publishes the obligation for a close that is
// about to withhold the work release, tells the operator what happened, and
// records it on the event bus.
//
// The operator line and the event carry the SAME facts on purpose. stderr
// reaches whoever ran the close; the event reaches everyone else, and the
// withhold was previously visible only in one pane — the same silence that let
// the 2026-09-11 portfolio wave stay invisible for weeks.
//
// It never fails the close. A session must always be able to shut down: making
// the close refuse when config is unavailable would stop every session closing
// during a city-wide bad config, which is strictly worse than a withheld
// release. So every failure here is reported, never returned.
func deferMissingConfigWorkRelease(store beads.Store, sessionID string, sessionBead beads.Bead, captureOK bool, reason string, rec events.Recorder, now time.Time, stderr io.Writer) deferredReleaseResult {
	obligation := buildSessionReleaseObligation(sessionBead, captureOK, reason, now)
	result := publishDeferredReleaseObligation(store, sessionID, obligation)

	// Report the PERSISTED obligation, not the one we tried to write: when an
	// earlier close already published, the generation and capture on the bead
	// are what a drain will act on.
	reported := result.Obligation
	if !result.Persisted {
		reported = obligation
	}
	markerErr := ""
	if result.Err != nil {
		markerErr = result.Err.Error()
	}

	switch {
	case !result.Persisted:
		fmt.Fprintf(stderr, "gc session close: WARNING: could not record the deferred work release for session %s (%v). "+ //nolint:errcheck // best-effort stderr
			"The work held under this session's name is withheld and NOTHING will find it. "+
			"%s\n", sessionID, result.Err, deferredReleaseRecoveryAdvice)
	case result.Published && reported.Capture == releaseCaptureIDOnly:
		fmt.Fprintf(stderr, "gc session close: recorded a deferred work release for session %s (generation %d), "+ //nolint:errcheck // best-effort stderr
			"but the capture is INCOMPLETE — the session bead was unreadable or already closed, so the work held "+
			"under a NAME may not be listed. %s\n", sessionID, reported.Generation, deferredReleaseRecoveryAdvice)
	case result.Published:
		fmt.Fprintf(stderr, "gc session close: recorded a deferred work release for session %s (generation %d, %d identifiers). "+ //nolint:errcheck // best-effort stderr
			"The withheld work is NOT released. %s\n",
			sessionID, reported.Generation, len(reported.Identities), deferredReleaseRecoveryAdvice)
	default:
		fmt.Fprintf(stderr, "gc session close: a deferred work release was already recorded for session %s "+ //nolint:errcheck // best-effort stderr
			"(generation %d, capture %s); leaving it as it stands.\n", sessionID, reported.Generation, reported.Capture)
	}
	if result.Persisted && result.Err != nil {
		fmt.Fprintf(stderr, "gc session close: warning: %v\n", result.Err) //nolint:errcheck // best-effort stderr
	}

	if rec != nil {
		rec.Record(events.Event{
			Type:      events.SessionReleaseDeferred,
			Ts:        now.UTC(),
			Actor:     "gc",
			Subject:   sessionID,
			Message:   formatDeferredReleaseMessage(sessionID, reported, result),
			SessionID: sessionID,
			Payload: api.SessionReleaseDeferredPayloadJSON(sessionID, reported.Generation, reported.Reason,
				reported.Capture, reported.Identities, reported.RigStoresKnown, result.Persisted, markerErr, result.ConditionalWrite),
		})
	}
	return result
}

// formatDeferredReleaseMessage renders the obligation as operator text. It
// leads with the unrecoverable case, because that is the one a reader must not
// skim past.
// It says "CLOSING", not "closed", and that is not pedantry: this runs BEFORE
// CloseDetailed, which can still fail while stopping the runtime, clearing
// overrides, retiring identifiers, or closing the bead. An event asserting a
// completed close would be permanently false for every close that then failed,
// and it is durable — so a consumer joining on it would believe a session ended
// that is still running.
func formatDeferredReleaseMessage(sessionID string, obligation sessionReleaseObligation, result deferredReleaseResult) string {
	if !result.Persisted {
		return fmt.Sprintf("session %s is CLOSING with its work release withheld (%s), and the deferred release could NOT be recorded: %v",
			sessionID, obligation.Reason, result.Err)
	}
	scope := "city store only; rig stores unenumerated"
	if obligation.RigStoresKnown {
		scope = "city and rig stores"
	}
	return fmt.Sprintf("session %s is CLOSING with its work release withheld (%s); deferred release recorded at generation %d, capture %s, %d identifiers, scope %s",
		sessionID, obligation.Reason, obligation.Generation, obligation.Capture, len(obligation.Identities), scope)
}

// deferredReleaseRecoveryAdvice is what an operator can actually DO, and it
// deliberately does NOT say "re-run this close once the config loads".
//
// That instruction was in the first version of these messages and it does not
// work for the case that matters. Once the first close has run,
// retireConfiguredNamedSessionIdentifiers has blanked session_name and moved the
// current alias into alias_history — and the work-release path reads
// sessionAssignmentIdentifiers, which covers the bead ID, session_name, the
// configured identity and the CURRENT alias, but NOT alias_history. So a re-run
// cannot see the alias the pool work is actually held under, and exits 0 having
// released nothing. An instruction that runs and silently does nothing is worse
// than none.
const deferredReleaseRecoveryAdvice = "Re-running this close will NOT recover work held under the session's retired " +
	"alias (the release path does not read alias_history); reassign that work by hand, using the identifiers in the " +
	"gc.release_deferred obligation on the session bead."

// openDeferredReleaseEventRecorder opens the city event log for one
// session.release_deferred emission, returning a no-op recorder when it cannot.
//
// cityPath can legitimately be empty here: resolveCity failing is one of the
// two legs that leaves cfg nil, and it is the leg where the close has no city
// to write an event into. That degrades observability to the stderr line, which
// is why the stderr line carries the full story rather than deferring to the
// event.
// STDERR, NOT io.Discard, is passed as the recorder's error sink. FileRecorder's
// Record is void and best-effort: it drops the event on a cross-process lock
// timeout or a write failure such as ENOSPC and reports that only through the
// writer given here. Handing it io.Discard — as the first version of this did —
// means an append failure produces neither an event nor a warning, on the one
// signal that exists to stop this withhold being silent, while the comment
// claimed every failure was reported.
func openDeferredReleaseEventRecorder(cityPath string, stderr io.Writer) (events.Recorder, func()) {
	if strings.TrimSpace(cityPath) == "" {
		return events.Discard, func() {}
	}
	rec, err := events.NewFileRecorder(filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl"), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc session close: warning: events recorder unavailable, the deferred work release is recorded on the bead but not on the event bus: %v\n", err) //nolint:errcheck // best-effort stderr
		return events.Discard, func() {}
	}
	return rec, func() {
		if cerr := rec.Close(); cerr != nil {
			fmt.Fprintf(stderr, "gc session close: warning: closing the events recorder: %v\n", cerr) //nolint:errcheck // best-effort stderr
		}
	}
}
