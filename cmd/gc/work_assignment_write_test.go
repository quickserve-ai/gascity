package main

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// recordingWriteWorkStore records every Update/SetMetadata call the
// work-assignment WRITE façade emits while delegating to a MemStore for real
// effects. It proves the façade emits byte-identical bead writes to the raw ops
// it replaces (same UpdateOpts pointers/values, same metadata patches).
type recordingWriteWorkStore struct {
	*beads.MemStore
	listQueries []beads.ListQuery
	updates     []recordedUpdate
	metaSets    []recordedMetaSet
}

type recordedUpdate struct {
	id   string
	opts beads.UpdateOpts
}

type recordedMetaSet struct {
	id    string
	key   string
	value string
}

func newRecordingWriteWorkStore() *recordingWriteWorkStore {
	return &recordingWriteWorkStore{MemStore: beads.NewMemStore()}
}

func (s *recordingWriteWorkStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.listQueries = append(s.listQueries, q)
	return s.MemStore.List(q)
}

// Update/SetMetadata record the exact op and report success WITHOUT delegating
// to the MemStore. The byte-identity asserts only need the captured op; the
// MemStore.Create path rewrites bead IDs to gc-N, so delegating would force the
// tests to round-trip generated IDs. Recording-only keeps the asserts pinned to
// the literal beads the façade was asked to write.
func (s *recordingWriteWorkStore) Update(id string, opts beads.UpdateOpts) error {
	s.updates = append(s.updates, recordedUpdate{id: id, opts: opts})
	return nil
}

func (s *recordingWriteWorkStore) SetMetadata(id, key, value string) error {
	s.metaSets = append(s.metaSets, recordedMetaSet{id: id, key: key, value: value})
	return nil
}

// The assertion is the enforcement of the comment below: on a signature drift
// the override stops satisfying the interface, MemStore's implementation is
// promoted in its place, and every assert here silently reroutes onto tier 1
// while still reporting green.
var _ beads.ConditionalAssignmentReleaser = (*recordingWriteWorkStore)(nil)

// ReleaseIfCurrent reports the conditional verb as unsupported so these tests
// pin the UNCONDITIONAL fallback write — the shape that must stay byte-identical
// to the raw release ops. The conditional fast path emits a different (metadata-
// only) write by design and is covered in work_assignment_release_race_test.go.
// Without this override the embedded MemStore would promote its own
// implementation and silently move every assert onto the other path.
func (s *recordingWriteWorkStore) ReleaseIfCurrent(_, _ string) (bool, error) {
	return false, beads.ErrConditionalReleaseUnsupported
}

// seedWriteWorkBead puts the bead the façade is about to release into the
// backing store with live state matching the caller's snapshot. The release path
// re-reads the bead immediately before writing (the dr-huhn no-clobber guard), so
// a snapshot with no bead behind it is correctly refused. Seeding goes straight to
// the MemStore because the recorder's own Update deliberately does not delegate.
func seedWriteWorkBead(t *testing.T, rec *recordingWriteWorkStore, item beads.Bead) {
	t.Helper()
	rec.HonorExplicitIDs = true
	if _, err := rec.Create(beads.Bead{ID: item.ID, Title: "work", Metadata: item.Metadata}); err != nil {
		t.Fatalf("seed Create(%s): %v", item.ID, err)
	}
	status, assignee := item.Status, item.Assignee
	// Qualified on MemStore deliberately: the recorder's own Update records
	// without delegating, so seeding through it would leave the store empty and
	// pollute the recorded ops the asserts read.
	if err := rec.MemStore.Update(item.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("seed Update(%s): %v", item.ID, err)
	}
	got, err := rec.Get(item.ID)
	if err != nil {
		t.Fatalf("seed Get(%s): %v", item.ID, err)
	}
	if got.Status != item.Status || got.Assignee != item.Assignee {
		t.Fatalf("seed(%s) failed: status=%q assignee=%q, want %q/%q",
			item.ID, got.Status, got.Assignee, item.Status, item.Assignee)
	}
}

func derefStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestWorkAssignmentOpenAssignedToBasic_ByteIdenticalQuery asserts the no-flags
// List variant (used by releaseWorkFromClosedSessionBead) emits exactly
// {Assignee,Status} — no Live, no TierMode — matching the raw probe.
func TestWorkAssignmentOpenAssignedToBasic_ByteIdenticalQuery(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if _, err := wa.OpenAssignedToBasic("agent-1", "in_progress"); err != nil {
		t.Fatalf("OpenAssignedToBasic: %v", err)
	}
	want := beads.ListQuery{Assignee: "agent-1", Status: "in_progress"}
	if len(rec.listQueries) != 1 || !reflect.DeepEqual(rec.listQueries[0], want) {
		t.Fatalf("List query mismatch:\n got %#v\n want %#v", rec.listQueries, want)
	}
}

// TestWorkAssignmentReleaseWorkBead_OpenStaysOpen asserts that releasing an
// already-open bead emits Update{Assignee:"", Metadata:<clearedAffinity>} with
// NO Status change (status reset is only for in_progress), byte-identical to the
// raw release op.
func TestWorkAssignmentReleaseWorkBead_OpenStaysOpen(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	item := beads.Bead{ID: "w-open", Status: "open", Assignee: "agent-1"}
	seedWriteWorkBead(t, rec, item)
	if err := wa.ReleaseWorkBead(item, "", io.Discard, "test"); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	if len(rec.updates) != 1 {
		t.Fatalf("expected 1 Update, got %d: %#v", len(rec.updates), rec.updates)
	}
	got := rec.updates[0]
	if got.id != "w-open" {
		t.Fatalf("Update id = %q, want w-open", got.id)
	}
	if derefStr(got.opts.Assignee) != "" {
		t.Fatalf("Assignee = %q, want empty-string clear", derefStr(got.opts.Assignee))
	}
	if got.opts.Status != nil {
		t.Fatalf("Status should be nil for an already-open bead, got %q", *got.opts.Status)
	}
	wantMeta := clearedSessionAffinityMetadata()
	if !reflect.DeepEqual(got.opts.Metadata, wantMeta) {
		t.Fatalf("Metadata mismatch:\n got %#v\n want %#v", got.opts.Metadata, wantMeta)
	}
}

// TestWorkAssignmentReleaseWorkBead_InProgressResetsToOpen asserts an
// in_progress bead is reset to open on release, byte-identical to the raw op.
func TestWorkAssignmentReleaseWorkBead_InProgressResetsToOpen(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	item := beads.Bead{ID: "w-ip", Status: "in_progress", Assignee: "agent-1"}
	seedWriteWorkBead(t, rec, item)
	if err := wa.ReleaseWorkBead(item, "", io.Discard, "test"); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	got := rec.updates[0]
	if got.opts.Status == nil || *got.opts.Status != "open" {
		t.Fatalf("Status = %v, want open", got.opts.Status)
	}
	if derefStr(got.opts.Assignee) != "" {
		t.Fatalf("Assignee = %q, want empty-string clear", derefStr(got.opts.Assignee))
	}
}

// TestWorkAssignmentReleaseWorkBead_RunTargetFallbackApplied asserts the
// run_target fallback (used by the retire/unclaim path) is written only when the
// bead has neither run_target nor routed_to, byte-identical to the raw op.
func TestWorkAssignmentReleaseWorkBead_RunTargetFallbackApplied(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	item := beads.Bead{ID: "w-route", Status: "in_progress", Assignee: "agent-1"}
	seedWriteWorkBead(t, rec, item)
	if err := wa.ReleaseWorkBead(item, "worker", io.Discard, "test"); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	got := rec.updates[0]
	if got.opts.Metadata[beadmeta.RunTargetMetadataKey] != "worker" {
		t.Fatalf("run_target fallback = %q, want worker", got.opts.Metadata[beadmeta.RunTargetMetadataKey])
	}
}

// TestWorkAssignmentReleaseWorkBead_RunTargetFallbackSkippedWhenRouted asserts
// the fallback is NOT applied when run_target/routed_to are already present.
func TestWorkAssignmentReleaseWorkBead_RunTargetFallbackSkippedWhenRouted(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	item := beads.Bead{
		ID:       "w-routed",
		Status:   "in_progress",
		Assignee: "agent-1",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "existing"},
	}
	seedWriteWorkBead(t, rec, item)
	if err := wa.ReleaseWorkBead(item, "worker", io.Discard, "test"); err != nil {
		t.Fatalf("ReleaseWorkBead: %v", err)
	}
	got := rec.updates[0]
	if _, ok := got.opts.Metadata[beadmeta.RunTargetMetadataKey]; ok {
		t.Fatalf("run_target fallback must be skipped when routed_to present, got %#v", got.opts.Metadata)
	}
}

// TestWorkAssignmentReassignWorkBead_ByteIdentical asserts reassign emits only
// Update{Assignee:&new}, byte-identical to the raw retire-reassign op. The bead
// is seeded live because the reassign is conditional on the snapshot: an
// unseeded fixture verifies as stale and emits no write at all.
func TestWorkAssignmentReassignWorkBead_ByteIdentical(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	item := beads.Bead{ID: "w-1", Status: "in_progress", Assignee: "retired-session"}
	seedWriteWorkBead(t, rec, item)
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if err := wa.ReassignWorkBead(item, "new-session"); err != nil {
		t.Fatalf("ReassignWorkBead: %v", err)
	}
	if len(rec.updates) != 1 {
		t.Fatalf("expected 1 Update, got %d", len(rec.updates))
	}
	got := rec.updates[0]
	if got.id != "w-1" || derefStr(got.opts.Assignee) != "new-session" {
		t.Fatalf("reassign = id %q assignee %q, want w-1/new-session", got.id, derefStr(got.opts.Assignee))
	}
	if got.opts.Status != nil || got.opts.Metadata != nil {
		t.Fatalf("reassign must not touch Status/Metadata, got %#v", got.opts)
	}
}

// TestWorkAssignmentClearDetachedProbe_ByteIdentical asserts the detached-probe
// clear emits SetMetadata(id, gc.detached, "") — the empty-string clear contract.
func TestWorkAssignmentClearDetachedProbe_ByteIdentical(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if err := wa.ClearDetachedProbe("w-1"); err != nil {
		t.Fatalf("ClearDetachedProbe: %v", err)
	}
	want := recordedMetaSet{id: "w-1", key: beadmeta.DetachedMetadataKey, value: ""}
	if len(rec.metaSets) != 1 || rec.metaSets[0] != want {
		t.Fatalf("SetMetadata mismatch:\n got %#v\n want %#v", rec.metaSets, want)
	}
}

// TestWorkAssignmentWrite_NilStoreSafe asserts the write methods tolerate a nil
// underlying store the same way the raw ops did (no panic, no write).
func TestWorkAssignmentWrite_NilStoreSafe(t *testing.T) {
	wa := workAssignmentForStore(beads.WorkStore{Store: nil})
	if err := wa.ReleaseWorkBead(beads.Bead{ID: "x"}, "", io.Discard, "test"); err != nil {
		t.Fatalf("nil store ReleaseWorkBead: %v", err)
	}
	if err := wa.ReassignWorkBead(beads.Bead{ID: "x"}, "y"); err != nil {
		t.Fatalf("nil store ReassignWorkBead: %v", err)
	}
	if err := wa.ClearDetachedProbe("x"); err != nil { // must not panic
		t.Fatalf("nil store ClearDetachedProbe: %v", err)
	}
	if _, err := wa.OpenAssignedToBasic("a", "open"); err != nil {
		t.Fatalf("nil store OpenAssignedToBasic: %v", err)
	}
}

// auditFailingUpdateStore reports the conditional verb as unsupported (forcing
// tier 2) and then fails the tier-2 Update, so the audit-line tests can prove
// the line is emitted only after a write that actually landed. The interface
// assertion follows this package's race-fake convention: without it, a
// signature drift silently promotes MemStore's own ReleaseIfCurrent and the
// negative control reroutes onto tier 1 while still reporting green.
type auditFailingUpdateStore struct {
	*beads.MemStore
}

var _ beads.ConditionalAssignmentReleaser = (*auditFailingUpdateStore)(nil)

func (s *auditFailingUpdateStore) ReleaseIfCurrent(string, string) (bool, error) {
	return false, beads.ErrConditionalReleaseUnsupported
}

func (s *auditFailingUpdateStore) Update(string, beads.UpdateOpts) error {
	return errUpdateRefused
}

var errUpdateRefused = fmt.Errorf("update refused")

// TestWorkAssignmentReleaseWorkBead_EmitsAuditLine is the ga-9n8hjv acceptance-4
// behaviour check: a SUCCESSFUL release must be observable on BOTH tiers. Before
// this, only the error branch at each call site logged, so a release that
// stripped a named agent's whole portfolio left nothing to grep and needed
// dolt_diff_issues forensics after the fact. The asserts are on the facts a
// later reader needs — bead, stripped assignee, status transition, stamped
// route, and WHICH release path ran — not on the exact sentence.
func TestWorkAssignmentReleaseWorkBead_EmitsAuditLine(t *testing.T) {
	t.Run("tier 2 unconditional write", func(t *testing.T) {
		rec := newRecordingWriteWorkStore()
		wa := workAssignmentForStore(beads.WorkStore{Store: rec})

		var audit bytes.Buffer
		item := beads.Bead{ID: "w-audit-t2", Status: "in_progress", Assignee: "katya"}
		seedWriteWorkBead(t, rec, item)
		if err := wa.ReleaseWorkBead(item, "worker", &audit, "closing-session-release"); err != nil {
			t.Fatalf("ReleaseWorkBead: %v", err)
		}
		got := audit.String()
		if strings.Count(got, "\n") != 1 {
			t.Fatalf("expected exactly 1 audit line, got %q", got)
		}
		for _, want := range []string{"w-audit-t2", "katya", "in_progress", "open", "run_target=worker", "closing-session-release"} {
			if !strings.Contains(got, want) {
				t.Fatalf("audit line %q missing %q", got, want)
			}
		}
	})

	t.Run("tier 1 conditional release", func(t *testing.T) {
		mem := beads.NewMemStore()
		claimed := seedClaimedBead(t, mem, "katya")

		wa := workAssignmentForStore(beads.WorkStore{Store: mem})
		var audit bytes.Buffer
		if err := wa.ReleaseWorkBead(claimed, "", &audit, "retired-session-unclaim"); err != nil {
			t.Fatalf("ReleaseWorkBead: %v", err)
		}
		got := audit.String()
		if strings.Count(got, "\n") != 1 {
			t.Fatalf("expected exactly 1 audit line, got %q", got)
		}
		for _, want := range []string{claimed.ID, "katya", "in_progress", "open", "run_target unchanged", "retired-session-unclaim"} {
			if !strings.Contains(got, want) {
				t.Fatalf("audit line %q missing %q", got, want)
			}
		}
	})
}

// TestWorkAssignmentReleaseWorkBead_NoAuditLineOnFailedWrite is the negative
// control for the test above: without it, an audit line emitted unconditionally
// would pass the positive test while announcing releases that never happened —
// which is worse than the silence it replaces, because it would be believed.
func TestWorkAssignmentReleaseWorkBead_NoAuditLineOnFailedWrite(t *testing.T) {
	mem := beads.NewMemStore()
	store := &auditFailingUpdateStore{MemStore: mem}
	claimed := seedClaimedBead(t, mem, "katya")

	wa := workAssignmentForStore(beads.WorkStore{Store: store})
	var audit bytes.Buffer
	if err := wa.ReleaseWorkBead(claimed, "", &audit, "closing-session-release"); err == nil {
		t.Fatal("expected the store Update error to propagate")
	}
	if audit.Len() != 0 {
		t.Fatalf("a failed release must stay silent, got %q", audit.String())
	}
}

// TestWorkAssignmentReleaseWorkBead_NilAuditSafe: a caller with no writer still
// releases. The audit is an addition to the release, never a precondition for it.
func TestWorkAssignmentReleaseWorkBead_NilAuditSafe(t *testing.T) {
	rec := newRecordingWriteWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})
	item := beads.Bead{ID: "w-audit-nil", Status: "open", Assignee: "agent-1"}
	seedWriteWorkBead(t, rec, item)
	if err := wa.ReleaseWorkBead(item, "", nil, "test"); err != nil {
		t.Fatalf("nil audit ReleaseWorkBead: %v", err)
	}
	if len(rec.updates) != 1 {
		t.Fatalf("expected the release to still write, got %d updates", len(rec.updates))
	}
}
