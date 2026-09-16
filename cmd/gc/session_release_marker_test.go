package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
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
	for _, want := range []string{"NOTHING will find it", "re-run this close"} {
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
