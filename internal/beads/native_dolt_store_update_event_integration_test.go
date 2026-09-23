//go:build integration

package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreUpdateEmitsCommittedEventOnServerBackend pins ga-0sbrp3:
// a gc-runtime Update ("gc: update bead <id>", the RunInTransaction route)
// must leave an "updated" events row with a non-empty actor. On the server backend doltTransaction.UpdateIssue called
// UpdateIssueWithoutEventInTx and marked only the issue table dirty, so 0% of
// gc-layer updates had an event while the bd path sat at ~99%. Upstream beads
// 4458d409e (#5886) fixed it; upstream has close/label/dependency event tests
// on this route but none for UpdateIssue, so a revert would pass its suite.
//
// It must run on the SERVER backend. An embedded store always emitted, so the
// same assertions against a plain temp dir pass on unpatched source.
//
// It does not check that the row is COMMITTED with the issue write: a fresh
// store never versions the events table (absent at HEAD, never in
// dolt_status), so a dolt_status check passes with the event left unstaged.
// Measured against that mutant; do not add the check back without a control.
func TestNativeDoltStoreUpdateEmitsCommittedEventOnServerBackend(t *testing.T) {
	ctx := context.Background()
	scopeRoot := t.TempDir()
	port := startTestDoltServer(t)
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("create .beads directory: %v", err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	storage, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Fatalf("open the server-backed native storage: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Errorf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	if _, ok := storage.(testRawDBGetter); !ok {
		t.Fatalf("storage %T exposes no raw DB; this is not the server backend the defect lives on", storage)
	}
	store, err := newNativeDoltStoreAt(ctx, scopeRoot, nil)
	if err != nil {
		t.Fatalf("newNativeDoltStoreAt: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	created, err := store.Create(Bead{Title: "update event probe"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	countUpdated := func() (int, string) {
		t.Helper()
		events, err := storage.GetEvents(ctx, created.ID, 100)
		if err != nil {
			t.Fatalf("GetEvents: %v", err)
		}
		n, actor := 0, ""
		for _, e := range events {
			if e.EventType == beadslib.EventUpdated {
				n++
				actor = e.Actor
			}
		}
		return n, actor
	}
	before, _ := countUpdated()

	assignee := "qcore/probe"
	if err := store.Update(created.ID, UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	after, actor := countUpdated()
	if after != before+1 {
		t.Fatalf("updated events = %d after one gc Update, want %d: the gc-runtime write left no ledger row (ga-0sbrp3)", after, before+1)
	}
	if actor == "" {
		t.Fatal("updated event has an empty actor; the runtime write is unattributable")
	}
}
