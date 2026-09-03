package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/liveness"
)

// installHeartbeatBeadStore points the heartbeat path's validation store at an
// in-process store holding the given beads, and returns it.
func installHeartbeatBeadStore(t *testing.T, seed ...beads.Bead) beads.Store {
	t.Helper()
	backing := beads.NewMemStore()
	store := wrapStoreWithBeadPolicies(backing, &config.City{})
	for _, b := range seed {
		if _, err := store.Create(b); err != nil {
			t.Fatalf("seed bead: %v", err)
		}
	}
	orig := heartbeatBeadStoreOpener
	t.Cleanup(func() { heartbeatBeadStoreOpener = orig })
	heartbeatBeadStoreOpener = func(string, string, *config.City) (beads.Store, error) { return store, nil }
	return store
}

func TestHeartbeatKeyIsALivenessKey(t *testing.T) {
	// The two constants live in different packages on purpose (liveness must not
	// import beadmeta). Pin them together so a rename on either side cannot
	// silently send heartbeats back to the versioned table.
	if !liveness.IsKey(beadmeta.LastHeartbeatAtMetadataKey) {
		t.Fatalf("liveness.IsKey(%q) = false; gc bd heartbeat would commit on every beat",
			beadmeta.LastHeartbeatAtMetadataKey)
	}
	if heartbeatMetadataKey != beadmeta.LastHeartbeatAtMetadataKey {
		t.Fatalf("heartbeatMetadataKey = %q, want %q", heartbeatMetadataKey, beadmeta.LastHeartbeatAtMetadataKey)
	}
}

func TestParseBdHeartbeatArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantID  string
		wantHit bool
		wantErr bool
	}{
		{name: "not a heartbeat", args: []string{"list"}},
		{name: "empty args", args: nil},
		{name: "valid", args: []string{"heartbeat", "demo-abc"}, wantID: "demo-abc", wantHit: true},
		{name: "missing id", args: []string{"heartbeat"}, wantHit: true, wantErr: true},
		{name: "extra arg", args: []string{"heartbeat", "demo-abc", "x"}, wantHit: true, wantErr: true},
		{name: "flag-shaped", args: []string{"heartbeat", "-x"}, wantHit: true, wantErr: true},
		{name: "whitespace", args: []string{"heartbeat", "demo abc"}, wantHit: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, hit, err := parseBdHeartbeatArgs(tc.args)
			if hit != tc.wantHit {
				t.Fatalf("recognized = %v, want %v", hit, tc.wantHit)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if id != tc.wantID {
				t.Fatalf("id = %q, want %q", id, tc.wantID)
			}
		})
	}
}

func TestDoBdHeartbeatThroughLivenessWritesTheTableInsteadOfShellingBd(t *testing.T) {
	resetSessionLivenessBindingsForTest()
	t.Cleanup(resetSessionLivenessBindingsForTest)

	cityPath := t.TempDir()
	scopeRoot := filepath.Join(cityPath, "rigs", "demo")
	lv := liveness.NewMemStore()
	sessionLivenessFor(cityPath, scopeRoot).setStoreForTest(lv, liveness.ModeTable)

	fixed := time.Date(2026, 9, 3, 18, 30, 0, 0, time.UTC)
	orig := bdHeartbeatNow
	t.Cleanup(func() { bdHeartbeatNow = orig })
	bdHeartbeatNow = func() time.Time { return fixed }

	store := installHeartbeatBeadStore(t)
	created, err := store.Create(beads.Bead{Title: "worker task", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var stderr bytes.Buffer
	code, handled := doBdHeartbeatThroughLiveness(cityPath, nil, execStoreTarget{ScopeRoot: scopeRoot}, created.ID, &stderr)
	if !handled {
		t.Fatalf("handled = false, want the liveness path to own the heartbeat (stderr: %q)", stderr.String())
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	snap, err := lv.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	if got, want := snap.Values[heartbeatMetadataKey], fixed.Format(time.RFC3339); got != want {
		t.Fatalf("%s = %q, want %q", heartbeatMetadataKey, got, want)
	}
	// The first beat also commits a ONE-TIME marker so the list-path overlay
	// filter keeps looking this work bead up; later beats are table-only.
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !beadMayCarryLiveness(got) {
		t.Fatalf("heartbeated work bead is not an overlay candidate; the dashboard would read a frozen value from List")
	}
}

// TestHeartbeatRetriesTheOverlayMarkerAfterItFails is the round-2
// fix-before-merge: "already seeded?" must be read from the store's COMMITTED
// metadata, never from the bead the policy store returned. That bead's
// gc.last_heartbeat_at is the OVERLAID value — the row this very beat wrote — so
// it reads non-empty on every beat whether or not a marker ever committed.
// Keyed on that, a marker write that failed on beat #1 was never retried and the
// bead stayed invisible to beadMayCarryLiveness on List paths for its whole life.
func TestHeartbeatRetriesTheOverlayMarkerAfterItFails(t *testing.T) {
	resetSessionLivenessBindingsForTest()
	t.Cleanup(resetSessionLivenessBindingsForTest)

	cityPath := t.TempDir()
	scopeRoot := filepath.Join(cityPath, "rigs", "demo")
	lv := liveness.NewMemStore()
	sessionLivenessFor(cityPath, scopeRoot).setStoreForTest(lv, liveness.ModeTable)

	backing := &markerFailingStore{MemStore: beads.NewMemStore(), fail: true}
	store := wrapStoreWithBeadPolicies(backing, &config.City{})
	orig := heartbeatBeadStoreOpener
	t.Cleanup(func() { heartbeatBeadStoreOpener = orig })
	heartbeatBeadStoreOpener = func(string, string, *config.City) (beads.Store, error) { return store, nil }

	created, err := store.Create(beads.Bead{Title: "worker task", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var stderr bytes.Buffer
	// Beat #1: the beat lands, the marker write fails.
	if _, handled := doBdHeartbeatThroughLiveness(cityPath, nil, execStoreTarget{ScopeRoot: scopeRoot}, created.ID, &stderr); !handled {
		t.Fatalf("beat #1 was not handled by the liveness path (stderr: %q)", stderr.String())
	}
	if backing.markerAttempts != 1 {
		t.Fatalf("markerAttempts = %d after beat #1, want 1", backing.markerAttempts)
	}

	// Beat #2 must RETRY it. The bead now reads a non-empty overlaid
	// gc.last_heartbeat_at (beat #1's row), which is exactly the value the old
	// check mistook for "already committed".
	backing.fail = false
	if _, handled := doBdHeartbeatThroughLiveness(cityPath, nil, execStoreTarget{ScopeRoot: scopeRoot}, created.ID, &stderr); !handled {
		t.Fatalf("beat #2 was not handled by the liveness path")
	}
	if backing.markerAttempts != 2 {
		t.Fatalf("markerAttempts = %d after beat #2, want 2 — a failed marker is never retried and the bead stays invisible to List overlays", backing.markerAttempts)
	}

	// Beat #3 finds a committed marker and leaves it alone: one commit per bead
	// for its whole life, not one per beat.
	if _, handled := doBdHeartbeatThroughLiveness(cityPath, nil, execStoreTarget{ScopeRoot: scopeRoot}, created.ID, &stderr); !handled {
		t.Fatalf("beat #3 was not handled by the liveness path")
	}
	if backing.markerAttempts != 2 {
		t.Fatalf("markerAttempts = %d after beat #3, want 2 — a committed marker must not be rewritten on every beat", backing.markerAttempts)
	}
	committed, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if committed.Metadata[heartbeatMetadataKey] == "" {
		t.Fatalf("no marker was ever committed; List reads stay stale forever")
	}
}

// markerFailingStore fails the one-time overlay-marker write while it is armed.
type markerFailingStore struct {
	*beads.MemStore
	fail           bool
	markerAttempts int
}

func (s *markerFailingStore) SetMetadata(id, key, value string) error {
	if key == heartbeatMetadataKey {
		s.markerAttempts++
		if s.fail {
			return errors.New("commit refused")
		}
	}
	return s.MemStore.SetMetadata(id, key, value)
}

// TestDoBdHeartbeatRejectsAnUnknownBead is the review's fix-before-merge B: the
// legacy path inherited non-zero-exit-on-unknown-id from `bd update`, and the
// liveness path must not silently exit 0 while writing an orphan row.
func TestDoBdHeartbeatRejectsAnUnknownBead(t *testing.T) {
	resetSessionLivenessBindingsForTest()
	t.Cleanup(resetSessionLivenessBindingsForTest)

	cityPath := t.TempDir()
	scopeRoot := filepath.Join(cityPath, "rigs", "demo")
	lv := liveness.NewMemStore()
	sessionLivenessFor(cityPath, scopeRoot).setStoreForTest(lv, liveness.ModeTable)
	installHeartbeatBeadStore(t)

	var stderr bytes.Buffer
	code, handled := doBdHeartbeatThroughLiveness(cityPath, nil, execStoreTarget{ScopeRoot: scopeRoot}, "demo-nonexistent", &stderr)
	if !handled {
		t.Fatalf("handled = false, want the unknown id owned and refused here")
	}
	if code == 0 {
		t.Fatalf("code = 0 for an unknown bead id, want non-zero like the legacy bd path")
	}
	snap, err := lv.Get(context.Background(), "demo-nonexistent")
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	if len(snap.Values) != 0 {
		t.Fatalf("wrote an orphan liveness row for a nonexistent bead: %v", snap.Values)
	}
}

func TestDoBdHeartbeatFallsBackToBdInMetadataMode(t *testing.T) {
	resetSessionLivenessBindingsForTest()
	t.Cleanup(resetSessionLivenessBindingsForTest)

	cityPath := t.TempDir()
	scopeRoot := filepath.Join(cityPath, "rigs", "demo")
	sessionLivenessFor(cityPath, scopeRoot).setStoreForTest(liveness.NewMemStore(), liveness.ModeMetadata)

	var stderr bytes.Buffer
	_, handled := doBdHeartbeatThroughLiveness(cityPath, nil, execStoreTarget{ScopeRoot: scopeRoot}, "demo-abc", &stderr)
	if handled {
		t.Fatalf("handled = true under the metadata rollback flag, want the legacy bd path")
	}
}

func TestDoBdHeartbeatFallsBackWhenTheLivenessWriteFails(t *testing.T) {
	resetSessionLivenessBindingsForTest()
	t.Cleanup(resetSessionLivenessBindingsForTest)

	cityPath := t.TempDir()
	scopeRoot := filepath.Join(cityPath, "rigs", "demo")
	sessionLivenessFor(cityPath, scopeRoot).setStoreForTest(failingLivenessStore{}, liveness.ModeTable)

	store := installHeartbeatBeadStore(t)
	created, err := store.Create(beads.Bead{Title: "worker task", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var stderr bytes.Buffer
	_, handled := doBdHeartbeatThroughLiveness(cityPath, nil, execStoreTarget{ScopeRoot: scopeRoot}, created.ID, &stderr)
	if handled {
		t.Fatalf("handled = true after a failed liveness write; a heartbeat that costs a commit beats a heartbeat the dashboard never sees")
	}
	if stderr.Len() == 0 {
		t.Errorf("the fallback was silent; operators need to see the degrade")
	}
}

type failingLivenessStore struct{}

func (failingLivenessStore) SetBatch(context.Context, string, map[string]string) error {
	return errors.New("liveness table unavailable")
}

func (failingLivenessStore) DeleteKeys(context.Context, string, []string) error {
	return errors.New("liveness table unavailable")
}

func (failingLivenessStore) Get(context.Context, string) (liveness.Snapshot, error) {
	return liveness.Snapshot{}, errors.New("liveness table unavailable")
}

func (failingLivenessStore) GetMany(context.Context, []string) (map[string]liveness.Snapshot, error) {
	return nil, errors.New("liveness table unavailable")
}

func (failingLivenessStore) Now() time.Time { return time.Now().UTC() }

func (failingLivenessStore) Close() error { return nil }
