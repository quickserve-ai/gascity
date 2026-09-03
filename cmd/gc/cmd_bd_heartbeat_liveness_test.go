package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/liveness"
)

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

	var stderr bytes.Buffer
	code, handled := doBdHeartbeatThroughLiveness(cityPath, execStoreTarget{ScopeRoot: scopeRoot}, "demo-abc", &stderr)
	if !handled {
		t.Fatalf("handled = false, want the liveness path to own the heartbeat (stderr: %q)", stderr.String())
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	snap, err := lv.Get(context.Background(), "demo-abc")
	if err != nil {
		t.Fatalf("liveness Get: %v", err)
	}
	if got, want := snap.Values[heartbeatMetadataKey], fixed.Format(time.RFC3339); got != want {
		t.Fatalf("%s = %q, want %q", heartbeatMetadataKey, got, want)
	}
}

func TestDoBdHeartbeatFallsBackToBdInMetadataMode(t *testing.T) {
	resetSessionLivenessBindingsForTest()
	t.Cleanup(resetSessionLivenessBindingsForTest)

	cityPath := t.TempDir()
	scopeRoot := filepath.Join(cityPath, "rigs", "demo")
	sessionLivenessFor(cityPath, scopeRoot).setStoreForTest(liveness.NewMemStore(), liveness.ModeMetadata)

	var stderr bytes.Buffer
	_, handled := doBdHeartbeatThroughLiveness(cityPath, execStoreTarget{ScopeRoot: scopeRoot}, "demo-abc", &stderr)
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

	var stderr bytes.Buffer
	_, handled := doBdHeartbeatThroughLiveness(cityPath, execStoreTarget{ScopeRoot: scopeRoot}, "demo-abc", &stderr)
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

func (failingLivenessStore) Close() error { return nil }
