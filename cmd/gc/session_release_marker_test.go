package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// ga-dt5ffp. "gc session close" fails CLOSED when the city config does not load:
// it withholds the work release, because with no config it cannot tell a
// still-configured [[named_session]] from a retired or pool one, and releasing
// blind strips a named seat's whole portfolio (ga-9n8hjv, the 2026-09-11 wave).
//
// The defect these tests pin is what happens AFTER the withhold. Nothing
// reclaims the work: CloseDetailed has already closed the session bead, the
// reconciler's snapshot drops closed sessions, and releaseOrphanedPoolAssignments
// skips unrouted work. So the close is the only actor that can release this work
// and it makes an irreversible decision while missing what it needs to decide.
//
// The fix under test publishes a durable obligation instead. It releases
// nothing — there is no drain yet, deliberately — so what must hold is that the
// obligation is captured at the right moment, records its own completeness
// honestly, and is never silently degraded.

func releaseObligationOn(t *testing.T, store beads.Store, id string) sessionReleaseObligation {
	t.Helper()
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	raw := bead.Metadata[beadmeta.ReleaseDeferredMetadataKey]
	if raw == "" {
		t.Fatalf("no deferred release obligation on %s; metadata=%v", id, bead.Metadata)
	}
	var obligation sessionReleaseObligation
	if err := json.Unmarshal([]byte(raw), &obligation); err != nil {
		t.Fatalf("decode obligation %q: %v", raw, err)
	}
	return obligation
}

func seedSessionBead(t *testing.T, store beads.Store, meta map[string]string) beads.Bead {
	t.Helper()
	bead, err := store.Create(beads.Bead{
		Title:    "session",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}
	return bead
}

// A first publish records the full pre-close identifier set, and says both that
// the capture is complete and that the rig-store scope is NOT.
//
// The rig assertion is not bookkeeping. Rig stores are enumerated only under
// "cityErr == nil && cfg != nil", so on this path they are unenumerated — and a
// drain that read the absent list as "there were no rig stores" would clear the
// obligation while rig cleanup was still outstanding, making the drop permanent.
func TestDeferredReleasePublishesFullPreCloseCapture(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedSessionBead(t, store, map[string]string{
		"session_name":                       "worker-named",
		session.NamedSessionIdentityMetadata: "gastown.worker",
		"alias":                              "worker-ga-abc12",
	})

	result := publishDeferredReleaseObligation(store, bead.ID,
		buildSessionReleaseObligation(bead, true, "loading city config: unexpected EOF", false, time.Now()))

	if !result.Persisted || !result.Published {
		t.Fatalf("publish result = %+v, want persisted and published; err=%v", result, result.Err)
	}
	if !result.ConditionalWrite {
		t.Error("ConditionalWrite = false on a store that implements CompareAndSetMetadataKey: " +
			"the write-once guarantee silently degraded to a racy read-then-write")
	}

	got := releaseObligationOn(t, store, bead.ID)
	if got.Generation != 1 {
		t.Errorf("Generation = %d, want 1", got.Generation)
	}
	if got.Capture != releaseCaptureFull {
		t.Errorf("Capture = %q, want %q", got.Capture, releaseCaptureFull)
	}
	if got.RigStoresKnown {
		t.Error("RigStoresKnown = true, want false: rig stores cannot be enumerated without the " +
			"config that failed to load, and a drain must read the scope as unknown rather than empty")
	}
	if got.Reason == "" {
		t.Error("Reason is empty: the obligation must say why the release was withheld")
	}
	for _, want := range []string{bead.ID, "worker-named", "gastown.worker", "worker-ga-abc12"} {
		if !slices.Contains(got.Identities, want) {
			t.Errorf("identities %v missing %q: work assigned under that form would never be found",
				got.Identities, want)
		}
	}
}

// THE WRITE-ONCE REGRESSION. A second close on the same bead reads identifiers
// the FIRST close already retired — session_name blanked, alias emptied — so an
// overwriting publish would replace a complete record of what to release with an
// almost-empty one, and the withheld work would become unfindable by exactly the
// mechanism meant to find it.
//
// This asserts on the STORED obligation, not the returned one: a publisher that
// returns the old value while writing the new one would pass a weaker check.
func TestDeferredReleaseNeverOverwrittenByARetiredSecondClose(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedSessionBead(t, store, map[string]string{
		"session_name":                       "worker-named",
		session.NamedSessionIdentityMetadata: "gastown.worker",
		"alias":                              "worker-ga-abc12",
	})

	first := publishDeferredReleaseObligation(store, bead.ID,
		buildSessionReleaseObligation(bead, true, "first close", false, time.Now()))
	if !first.Published {
		t.Fatalf("first publish did not land: %+v", first)
	}

	// Exactly what retireConfiguredNamedSessionIdentifiers leaves behind: the
	// name-shaped identifiers gone, the alias pushed into alias_history, which
	// no release path reads.
	retired := beads.Bead{ID: bead.ID, Metadata: map[string]string{
		"session_name":                       "",
		session.NamedSessionIdentityMetadata: "",
		"alias":                              "",
		"alias_history":                      "worker-ga-abc12",
	}}
	second := publishDeferredReleaseObligation(store, bead.ID,
		buildSessionReleaseObligation(retired, true, "second close", false, time.Now()))
	if second.Published {
		t.Error("the second close REPUBLISHED over a complete obligation; " +
			"it must leave a complete obligation exactly as it stands")
	}
	if !second.Persisted {
		t.Errorf("second publish reports nothing persisted, but an obligation is present: %+v", second)
	}

	got := releaseObligationOn(t, store, bead.ID)
	if got.Generation != 1 {
		t.Errorf("Generation = %d, want 1: a no-op publish must not advance the generation, "+
			"or an acknowledgement of the real obligation stops discharging it", got.Generation)
	}
	if got.Reason != "first close" {
		t.Errorf("Reason = %q, want %q: the stored obligation was overwritten", got.Reason, "first close")
	}
	for _, want := range []string{"worker-named", "gastown.worker"} {
		if !slices.Contains(got.Identities, want) {
			t.Errorf("identities %v lost %q to the second close — this is the whole defect",
				got.Identities, want)
		}
	}
}

// The one legal rewrite: a degraded ID-only obligation may be replaced by a full
// capture, and the generation rises so an acknowledgement of the degraded one
// does not discharge the complete one.
func TestDeferredReleaseUpgradesIDOnlyCaptureAndBumpsGeneration(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedSessionBead(t, store, map[string]string{
		"session_name": "worker-named",
		"alias":        "worker-ga-abc12",
	})

	// captureOK=false: the pre-close read failed, so only the ID is known.
	degraded := publishDeferredReleaseObligation(store, bead.ID,
		buildSessionReleaseObligation(beads.Bead{ID: bead.ID}, false, "read failed", false, time.Now()))
	if !degraded.Published {
		t.Fatalf("degraded publish did not land: %+v", degraded)
	}
	if first := releaseObligationOn(t, store, bead.ID); first.Capture != releaseCaptureIDOnly {
		t.Fatalf("Capture = %q, want %q", first.Capture, releaseCaptureIDOnly)
	}

	upgraded := publishDeferredReleaseObligation(store, bead.ID,
		buildSessionReleaseObligation(bead, true, "later close read the bead", false, time.Now()))
	if !upgraded.Published {
		t.Errorf("a full capture did not replace an id_only obligation: %+v", upgraded)
	}

	got := releaseObligationOn(t, store, bead.ID)
	if got.Capture != releaseCaptureFull {
		t.Errorf("Capture = %q, want %q after the upgrade", got.Capture, releaseCaptureFull)
	}
	if got.Generation != 2 {
		t.Errorf("Generation = %d, want 2: an upgrade must not reuse the generation an "+
			"acknowledgement may already name", got.Generation)
	}
	if !slices.Contains(got.Identities, "worker-named") {
		t.Errorf("identities %v missing the name-shaped form the upgrade exists to add", got.Identities)
	}
}

// The reverse must never happen: a later close that could only read the ID must
// not replace a full capture with a shorter list, which would read to a drain as
// "there is nothing more to release".
func TestDeferredReleaseNeverDowngradesAFullCapture(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedSessionBead(t, store, map[string]string{"session_name": "worker-named"})

	if r := publishDeferredReleaseObligation(store, bead.ID,
		buildSessionReleaseObligation(bead, true, "full", false, time.Now())); !r.Published {
		t.Fatalf("full publish did not land: %+v", r)
	}
	if r := publishDeferredReleaseObligation(store, bead.ID,
		buildSessionReleaseObligation(beads.Bead{ID: bead.ID}, false, "degraded", false, time.Now())); r.Published {
		t.Error("an id_only capture overwrote a full one")
	}

	got := releaseObligationOn(t, store, bead.ID)
	if got.Capture != releaseCaptureFull {
		t.Errorf("Capture = %q, want %q", got.Capture, releaseCaptureFull)
	}
	if !slices.Contains(got.Identities, "worker-named") {
		t.Errorf("identities %v lost the name-shaped form", got.Identities)
	}
}

// An undecodable obligation is left exactly where it is. It is still someone's
// evidence that a release was withheld, and replacing it with a well-formed one
// is the single outcome with no recovery — so the publisher refuses and reports.
func TestDeferredReleaseLeavesAnUndecodableObligationAlone(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedSessionBead(t, store, map[string]string{"session_name": "worker-named"})
	if err := store.Update(bead.ID, beads.UpdateOpts{
		Metadata: map[string]string{beadmeta.ReleaseDeferredMetadataKey: "{not json"},
	}); err != nil {
		t.Fatalf("seed corrupt obligation: %v", err)
	}

	result := publishDeferredReleaseObligation(store, bead.ID,
		buildSessionReleaseObligation(bead, true, "later close", false, time.Now()))
	if result.Published {
		t.Error("the publisher overwrote an obligation it could not read")
	}
	if result.Err == nil {
		t.Error("an unreadable obligation was not reported; it would be silently skipped")
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata[beadmeta.ReleaseDeferredMetadataKey] != "{not json" {
		t.Errorf("stored obligation = %q, want the original bytes preserved",
			got.Metadata[beadmeta.ReleaseDeferredMetadataKey])
	}
}

// storeWithoutConditionalWrites hides a store's ConditionalWriter methods by
// embedding the INTERFACE rather than the concrete type, so ConditionalWriterFor
// declines it exactly as it would a backend that cannot compare-and-set.
type storeWithoutConditionalWrites struct{ beads.Store }

// On a store with no compare-and-set primitive the publisher still refuses to
// overwrite a complete obligation, but it must ADMIT that the guarantee is no
// longer atomic rather than report the same confidence as the conditional path.
func TestDeferredReleaseFallbackReportsTheLostAtomicity(t *testing.T) {
	inner := beads.NewMemStore()
	bead := seedSessionBead(t, inner, map[string]string{"session_name": "worker-named"})
	store := storeWithoutConditionalWrites{Store: inner}

	if _, ok := beads.ConditionalWriterFor(store); ok {
		t.Fatal("the fallback harness still exposes a ConditionalWriter, so this test proves nothing")
	}

	first := publishDeferredReleaseObligation(store, bead.ID,
		buildSessionReleaseObligation(bead, true, "fallback first", false, time.Now()))
	if !first.Published {
		t.Fatalf("fallback publish did not land: %+v", first)
	}
	if first.ConditionalWrite {
		t.Error("ConditionalWrite = true without a compare-and-set primitive: the caller would " +
			"report a write-once guarantee the store did not provide")
	}

	second := publishDeferredReleaseObligation(store, bead.ID,
		buildSessionReleaseObligation(bead, true, "fallback second", false, time.Now()))
	if second.Published {
		t.Error("the fallback overwrote a complete obligation")
	}
	if got := releaseObligationOn(t, inner, bead.ID); got.Reason != "fallback first" {
		t.Errorf("Reason = %q, want %q", got.Reason, "fallback first")
	}
}

// failingStore fails every write, standing in for a store that is unreachable at
// close time.
type failingStore struct{ beads.Store }

func (failingStore) Update(string, beads.UpdateOpts) error {
	return fmt.Errorf("store unreachable")
}

// When the obligation cannot be written the close must SHOUT. The withheld work
// is then findable by nothing, which is strictly worse than the state the
// operator thinks they are in, so a quiet warning is not enough.
func TestDeferredReleaseAnnouncesAnUnrecordableObligation(t *testing.T) {
	inner := beads.NewMemStore()
	bead := seedSessionBead(t, inner, map[string]string{"session_name": "worker-named"})
	store := storeWithoutConditionalWrites{Store: failingStore{Store: inner}}

	var stderr bytes.Buffer
	result := deferMissingConfigWorkRelease(store, bead.ID, bead, true, "config gone", false, nil, time.Now(), &stderr)

	if result.Persisted {
		t.Fatalf("result claims the obligation persisted through a failing store: %+v", result)
	}
	out := stderr.String()
	// "reassign that work by hand", not "re-run this close" — see
	// TestDeferredReleaseAdviceDoesNotPromiseARerunRecovery for why the re-run
	// advice this test originally asserted was removed.
	for _, want := range []string{"NOTHING will find it", "by hand"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("stderr does not say %q; got: %s", want, out)
		}
	}
}

// deferredReleaseRecorder captures emitted events for inspection.
type deferredReleaseRecorder struct{ recorded []events.Event }

func (r *deferredReleaseRecorder) Record(e events.Event) { r.recorded = append(r.recorded, e) }

// A close that withholds must be observable OFF-PANE. The withhold was
// previously visible only in the stderr of whoever ran the close, which is the
// same silence that let the 2026-09-11 portfolio wave go unnoticed for days.
func TestDeferredReleaseEmitsAnObservableEvent(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedSessionBead(t, store, map[string]string{
		"session_name": "worker-named",
		"alias":        "worker-ga-abc12",
	})

	rec := &deferredReleaseRecorder{}
	var stderr bytes.Buffer
	deferMissingConfigWorkRelease(store, bead.ID, bead, true, "config gone", false, rec, time.Now(), &stderr)

	if len(rec.recorded) != 1 {
		t.Fatalf("recorded %d events, want exactly 1", len(rec.recorded))
	}
	got := rec.recorded[0]
	if got.Type != "session.release_deferred" {
		t.Errorf("event type = %q, want session.release_deferred", got.Type)
	}
	if got.Subject != bead.ID {
		t.Errorf("event subject = %q, want the session bead %q", got.Subject, bead.ID)
	}

	var payload struct {
		Generation      int      `json:"generation"`
		Capture         string   `json:"capture"`
		Identities      []string `json:"identities"`
		RigStoresKnown  bool     `json:"rig_stores_known"`
		MarkerPersisted bool     `json:"marker_persisted"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("decode payload %s: %v", got.Payload, err)
	}
	if !payload.MarkerPersisted {
		t.Error("marker_persisted = false on a successful publish: a subscriber would " +
			"escalate a withhold that is in fact queued")
	}
	if payload.Capture != releaseCaptureFull || payload.Generation != 1 {
		t.Errorf("payload capture=%q generation=%d, want %q and 1",
			payload.Capture, payload.Generation, releaseCaptureFull)
	}
	if payload.RigStoresKnown {
		t.Error("rig_stores_known = true, want false")
	}
	if !slices.Contains(payload.Identities, "worker-named") {
		t.Errorf("payload identities %v omit the name-shaped form a drain needs", payload.Identities)
	}
}

// casFailingStore accepts reads but fails every compare-and-set, standing in for
// the realistic shape of the ID-only case: the pre-close read failed because the
// session bead is not reachable, so the write against it cannot land either.
type casFailingStore struct{ *beads.MemStore }

func (casFailingStore) CompareAndSetMetadataKey(string, string, string, string) (bool, error) {
	return false, fmt.Errorf("store unreachable")
}

// The conditional path has its own failure branch, and it is the one that fires
// in production when the session bead cannot be reached: publishing must report
// the failure rather than return a result that reads like a successful publish.
//
// This is a separate test from the fallback-store case on purpose. That one
// exercises Update failing; this one exercises the compare-and-set failing, and a
// fix to either branch is the natural way to break the other.
func TestDeferredReleaseReportsAFailedConditionalPublish(t *testing.T) {
	inner := beads.NewMemStore()
	bead := seedSessionBead(t, inner, map[string]string{"session_name": "worker-named"})
	store := casFailingStore{MemStore: inner}

	if _, ok := beads.ConditionalWriterFor(store); !ok {
		t.Fatal("harness no longer exposes a ConditionalWriter, so this test does not reach the branch it names")
	}

	var stderr bytes.Buffer
	result := deferMissingConfigWorkRelease(store, bead.ID, bead, true, "config gone", false, nil, time.Now(), &stderr)

	if result.Persisted || result.Published {
		t.Fatalf("result = %+v, want neither persisted nor published when the swap failed", result)
	}
	if result.Err == nil {
		t.Error("a failed compare-and-set was not reported")
	}
	if !bytes.Contains(stderr.Bytes(), []byte("NOTHING will find it")) {
		t.Errorf("stderr does not warn that the withheld work is unfindable; got: %s", stderr.String())
	}
	if got, err := inner.Get(bead.ID); err != nil {
		t.Fatalf("Get: %v", err)
	} else if raw := got.Metadata[beadmeta.ReleaseDeferredMetadataKey]; raw != "" {
		t.Errorf("an obligation was persisted anyway (%q) — the result and the store disagree", raw)
	}
}

// policyShapedStore mimics the store the CLI actually holds: *beadPolicyStore
// embeds the beads.Store INTERFACE — so optional capabilities are NOT promoted
// through it — and participates in capability lookup only by declaring a
// resolve target. Asserting CompareAndSetMetadataKey on this shape fails; only a
// lookup that follows ConditionalWritesResolveTarget finds the capability.
type policyShapedStore struct {
	beads.Store
	target beads.Store
}

func (s policyShapedStore) ConditionalWritesResolveTarget() beads.Store { return s.target }

// THE TEST THAT WAS MISSING, and whose absence let a real defect ship green.
//
// The first version of publishDeferredReleaseObligation used
// beads.ConditionalWriterFor, which asks for the full four-method
// ConditionalWriter and does NOT follow a wrapper's declared resolve target. Every
// other test here passes a bare *MemStore, which implements ConditionalWriter
// directly — so they all reported ConditionalWrite=true while the production
// store, wrapped in *beadPolicyStore, silently took the non-atomic fallback.
//
// A test built on a fixture that is more capable than production proves the
// fixture. This one is built on the production SHAPE.
func TestDeferredReleaseUsesMetadataCASThroughAPolicyShapedWrapper(t *testing.T) {
	inner := beads.NewMemStore()
	bead := seedSessionBead(t, inner, map[string]string{"session_name": "worker-named"})
	store := policyShapedStore{Store: inner, target: inner}

	// Control: prove the harness really is the hard shape. If a direct
	// ConditionalWriter assertion succeeded, this test would pass for the wrong
	// reason and prove nothing about the lookup.
	if _, ok := beads.ConditionalWriterFor(store); ok {
		t.Fatal("CONTROL FAILED: ConditionalWriterFor resolves this wrapper, so it does not " +
			"reproduce the production shape and this test cannot detect the defect it exists for")
	}
	if _, ok := beads.MetadataCASWriterFor(store); !ok {
		t.Fatal("MetadataCASWriterFor cannot resolve the wrapper either; the harness is wrong")
	}

	result := publishDeferredReleaseObligation(store, bead.ID,
		buildSessionReleaseObligation(bead, true, "config gone", false, time.Now()))

	if !result.Published {
		t.Fatalf("publish did not land through the wrapper: %+v", result)
	}
	if !result.ConditionalWrite {
		t.Error("ConditionalWrite = false through a policy-shaped wrapper: the publish fell back " +
			"to a racy read-then-write on the shape production actually uses, so write-once is " +
			"not enforced where it matters")
	}
	if got := releaseObligationOn(t, inner, bead.ID); got.Generation != 1 {
		t.Errorf("Generation = %d, want 1", got.Generation)
	}
}

// FINDING 2. A successful read of an ALREADY-CLOSED session bead is not a
// pre-close capture. Without this rule, a second close reads the closed, retired
// bead, labels the degraded snapshot "full", and the write-once rule AUTHORIZES it
// to replace a good id_only obligation — losing the retired identifiers for good.
func TestDeferredReleaseWillNotCallAClosedBeadsSnapshotComplete(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedSessionBead(t, store, map[string]string{"session_name": "worker-named"})

	// Invocation A: its pre-close read failed, so only the ID is known.
	if r := publishDeferredReleaseObligation(store, bead.ID,
		buildSessionReleaseObligation(beads.Bead{ID: bead.ID}, false, "read failed", false, time.Now())); !r.Published {
		t.Fatalf("degraded publish did not land: %+v", r)
	}

	// A's close then retires the identifiers and closes the bead. Invocation B now
	// reads that bead SUCCESSFULLY — but what it reads is post-retirement.
	retiredAndClosed := beads.Bead{
		ID:     bead.ID,
		Status: "closed",
		Metadata: map[string]string{
			"session_name":  "",
			"alias":         "",
			"alias_history": "worker-ga-abc12",
		},
	}
	obligation := buildSessionReleaseObligation(retiredAndClosed, true, "second close", false, time.Now())
	if obligation.Capture != releaseCaptureIDOnly {
		t.Errorf("Capture = %q, want %q: a closed bead cannot yield a pre-retirement capture",
			obligation.Capture, releaseCaptureIDOnly)
	}

	second := publishDeferredReleaseObligation(store, bead.ID, obligation)
	if second.Published {
		t.Error("a post-close snapshot was allowed to upgrade the obligation")
	}
	got := releaseObligationOn(t, store, bead.ID)
	if got.Reason != "read failed" || got.Generation != 1 {
		t.Errorf("stored obligation = reason %q generation %d, want the original untouched",
			got.Reason, got.Generation)
	}
}

// FINDING 7. A document this build does not recognize — an unknown capture, a
// non-positive generation, a bare {} — is left alone, not "upgraded". Reading
// "not complete" off an unrecognized shape and rewriting it would overwrite a
// future writer's format and mint a generation an acknowledgement may already
// name.
func TestDeferredReleaseLeavesAnUnrecognizedObligationAlone(t *testing.T) {
	for _, tc := range []struct{ name, stored string }{
		{"empty object", `{}`},
		{"unknown capture", `{"generation":1,"capture":"partial"}`},
		{"zero generation", `{"generation":0,"capture":"id_only"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			bead := seedSessionBead(t, store, map[string]string{"session_name": "worker-named"})
			if err := store.Update(bead.ID, beads.UpdateOpts{
				Metadata: map[string]string{beadmeta.ReleaseDeferredMetadataKey: tc.stored},
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			result := publishDeferredReleaseObligation(store, bead.ID,
				buildSessionReleaseObligation(bead, true, "later close", false, time.Now()))
			if result.Published {
				t.Error("an unrecognized obligation was overwritten")
			}
			if result.Err == nil {
				t.Error("an unrecognized obligation was not reported")
			}
			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Metadata[beadmeta.ReleaseDeferredMetadataKey] != tc.stored {
				t.Errorf("stored = %q, want the original bytes preserved",
					got.Metadata[beadmeta.ReleaseDeferredMetadataKey])
			}
		})
	}
}

// FINDING 3. The operator message must not tell anyone to re-run the close as a
// recovery. Once the first close has retired session_name and moved the alias
// into alias_history, the release path — which reads the CURRENT alias and not
// alias_history — cannot see what the pool work is held under, so a re-run exits
// 0 having released nothing. An instruction that runs and silently does nothing
// is worse than no instruction.
func TestDeferredReleaseAdviceDoesNotPromiseARerunRecovery(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedSessionBead(t, store, map[string]string{"session_name": "worker-named"})

	var stderr bytes.Buffer
	deferMissingConfigWorkRelease(store, bead.ID, bead, true, "config gone", false, nil, time.Now(), &stderr)
	out := stderr.String()

	if bytes.Contains([]byte(out), []byte("Re-run this close once the city config loads")) {
		t.Errorf("stderr still promises a re-run recovery that cannot work; got: %s", out)
	}
	for _, want := range []string{"will NOT recover", "alias_history", "by hand"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("stderr does not say %q; got: %s", want, out)
		}
	}
}

// FINDING 4. The event is emitted BEFORE CloseDetailed, which can still fail. It
// must not assert a completed close — that record is durable, and a consumer
// joining on it would believe a still-running session had ended.
func TestDeferredReleaseEventDoesNotClaimTheCloseHappened(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedSessionBead(t, store, map[string]string{"session_name": "worker-named"})

	rec := &deferredReleaseRecorder{}
	var stderr bytes.Buffer
	deferMissingConfigWorkRelease(store, bead.ID, bead, true, "config gone", false, rec, time.Now(), &stderr)

	if len(rec.recorded) != 1 {
		t.Fatalf("recorded %d events, want 1", len(rec.recorded))
	}
	msg := rec.recorded[0].Message
	if strings.Contains(msg, "closed with") {
		t.Errorf("event message asserts the close completed, but it is emitted before "+
			"CloseDetailed and that call can fail; got: %s", msg)
	}
	if !strings.Contains(msg, "CLOSING") {
		t.Errorf("event message does not describe the close as in progress; got: %s", msg)
	}
}

// FINDING 5. An UPGRADE that fails to write leaves the previous obligation in
// place, so something usable is still on the bead. Reporting it as unpersisted
// prints "NOTHING will find it" at an operator whose obligation is intact — a
// false alarm in the one message that must never cry wolf.
func TestDeferredReleaseFailedUpgradeStillReportsTheExistingObligation(t *testing.T) {
	inner := beads.NewMemStore()
	bead := seedSessionBead(t, inner, map[string]string{"session_name": "worker-named"})
	if r := publishDeferredReleaseObligation(inner, bead.ID,
		buildSessionReleaseObligation(beads.Bead{ID: bead.ID}, false, "degraded first", false, time.Now())); !r.Published {
		t.Fatalf("degraded publish did not land: %+v", r)
	}

	// A store that reads fine but cannot write, with no CAS capability, so the
	// upgrade takes the unconditional path and its Update fails.
	store := storeWithoutConditionalWrites{Store: failingStore{Store: inner}}
	var stderr bytes.Buffer
	result := deferMissingConfigWorkRelease(store, bead.ID, bead, true, "second close", false, nil, time.Now(), &stderr)

	if !result.Persisted {
		t.Error("a failed UPGRADE reported nothing persisted, but the earlier obligation is still there")
	}
	if result.Err == nil {
		t.Error("the failed upgrade was not reported at all")
	}
	if bytes.Contains(stderr.Bytes(), []byte("NOTHING will find it")) {
		t.Errorf("stderr cried wolf on a bead that still carries a usable obligation; got: %s", stderr.String())
	}
}
