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

func (failingLivenessStore) Get(context.Context, string) (liveness.Snapshot, error) {
	return liveness.Snapshot{}, errors.New("liveness table unavailable")
}

func (failingLivenessStore) GetMany(context.Context, []string) (map[string]liveness.Snapshot, error) {
	return nil, errors.New("liveness table unavailable")
}

func (failingLivenessStore) Now() time.Time { return time.Now().UTC() }

func (failingLivenessStore) Close() error { return nil }
