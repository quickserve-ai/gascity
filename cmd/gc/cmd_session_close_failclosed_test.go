package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// ga-9n8hjv: `gc session close` releases the closing session's work through
// unclaimWorkAssignedToRetiredSessionBead, and the guard that keeps a still-
// configured [[named_session]]'s backlog (added for ga-sdynmb) is CONFIG-DRIVEN.
// cmdSessionClose loaded that config with `cfg, _ = loadCityConfig(...)` and
// discarded the error, so a config that failed to load produced a nil cfg —
// and a nil cfg matches no named identity, which makes EVERY assignee read as
// retired. The sweep then released the lot.
//
// That is not hypothetical. On 2026-09-11T05:08:17Z a close on this path cleared
// 50 beads off one named seat in a single write, 11 of them downgraded
// in_progress -> open. The binary serving that day was gc 5f4055046, which
// CARRIED the ga-sdynmb guard — verified by content against a pre-guard control.
// So the guard was present and simply had nothing to match against: a
// present-but-inapplicable guard is the whole failure mode, which is why the
// ga-sdynmb regression suite passes and did not catch this.
//
// The falsifiable floor: on unpatched source the work bead below comes back
// assignee="" and status="open", exactly as the 50 did.
func TestSessionCloseFailsClosedWhenCityConfigDoesNotLoad(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	sessionBead, err := store.Create(beads.Bead{
		Title:  "named seat session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-named",
			"template":     "worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:    "the seat's in-flight work",
		Type:     "task",
		Assignee: sessionBead.ID,
	})
	if err != nil {
		t.Fatalf("Create(work bead): %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}

	// Break the config AFTER the store and beads exist, so this exercises the
	// nil-cfg leg specifically rather than a city that never came up. Truncated
	// TOML mid-table is the realistic shape: a half-written file, which is what a
	// config-drift roll racing an edit actually produces.
	cityTOML := filepath.Join(cityDir, "city.toml")
	if err := os.WriteFile(cityTOML, []byte("[workspace]\nname = \"test-city\"\n[[agent\n"), 0o644); err != nil {
		t.Fatalf("corrupt city.toml: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionClose([]string{sessionBead.ID}, &stdout, &stderr)

	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work bead: %v", err)
	}

	// THE ASSERTION THAT MATTERS. Not "the close succeeded" — the close may
	// legitimately fail on a broken config, and that is fine. What must never
	// happen is the work being detached from its owner on the way out.
	if got.Assignee != sessionBead.ID {
		t.Errorf("work bead assignee = %q, want %q: an unloadable config must not "+
			"detach work from its owner (close exit=%d, stderr=%s)",
			got.Assignee, sessionBead.ID, code, stderr.String())
	}
	if got.Status != "in_progress" {
		t.Errorf("work bead status = %q, want in_progress: downgrading to open is what "+
			"makes the owner's own resume check report \"no work\" (stderr=%s)",
			got.Status, stderr.String())
	}

	// And it must SAY so. A silent skip is how this stayed invisible for four
	// days; the operator needs to know work was deliberately not released and
	// why, so the close can be re-run once the config loads.
	if !strings.Contains(stderr.String(), "config unavailable") {
		t.Errorf("stderr does not name the reason work was withheld; got: %s", stderr.String())
	}
}
