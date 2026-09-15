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
// THIS TEST PINS BOTH HALVES OF THE NIL-CFG BEHAVIOR, because each half is a
// separate regression and a fix for one is the natural way to break the other:
//
//   - NAME-SHAPED assignee (here the session_name form) must be RETAINED. Losing
//     this is the 2026-09-11 portfolio wave.
//   - BEAD-ID assignee must still be RELEASED. Nothing downstream reclaims what
//     this branch withholds: CloseDetailed has already closed the session bead,
//     the reconciler snapshot drops closed sessions, so
//     repairStrandedPoolWorkerBead never sees it; and
//     releaseOrphanedPoolAssignments skips unrouted work. A blanket
//     "release nothing" skip looks safe and is not.
//
// READ THIS BEFORE TRUSTING THE SECOND HALF AS A STRAND TEST. The bead-ID form
// asserted below is NOT the shape production holds pool work in. Pool instances
// run with GC_AGENT/GC_ALIAS set to their per-instance alias and gc hook
// --claim writes that alias, so real claimed work reads e.g.
// "woodhouse-ga-m02ds". Measured 2026-09-15 on this city's store: ZERO rows in
// hq.issues have a ^(ga|gc)- assignee in any status, against 92 such rows in
// hq.wisps — a real zero, not a broken filter. So this case pins a narrow
// correctness guarantee (work bound to the dying bead ID is freed, and a named
// seat is never stripped) and does NOT demonstrate that the alias-held strand
// is closed. It is not. The durable fix is deferred cleanup that revisits
// closed sessions once cfg loads, tracked separately.
//
// The work bead below is deliberately UNROUTED (no gc.routed_to), which is the
// exact shape releaseOrphanedPoolAssignments refuses to recover.
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

	const sessionName = "worker-named"
	sessionBead, err := store.Create(beads.Bead{
		Title:  "named seat session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": sessionName,
			"template":     "worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	inProgress := "in_progress"

	// Held under a NAME. This is the portfolio the 2026-09-11 wave destroyed.
	namedWork, err := store.Create(beads.Bead{
		Title:    "the seat's in-flight work, held under its name",
		Type:     "task",
		Assignee: sessionName,
	})
	if err != nil {
		t.Fatalf("Create(named work bead): %v", err)
	}
	if err := store.Update(namedWork.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark named work in_progress: %v", err)
	}

	// Held under the dying session's BEAD ID, and unrouted: pool-shaped work that
	// nothing else can reclaim once this bead closes.
	poolWork, err := store.Create(beads.Bead{
		Title:    "pool work bound to the dying session bead",
		Type:     "task",
		Assignee: sessionBead.ID,
	})
	if err != nil {
		t.Fatalf("Create(pool work bead): %v", err)
	}
	if err := store.Update(poolWork.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark pool work in_progress: %v", err)
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

	// HALF ONE. Not "the close succeeded" — the close may legitimately fail on a
	// broken config, and that is fine. What must never happen is a seat's work
	// being detached from its owner on the way out.
	gotNamed, err := reopened.Get(namedWork.ID)
	if err != nil {
		t.Fatalf("re-read named work bead: %v", err)
	}
	if gotNamed.Assignee != sessionName {
		t.Errorf("named work assignee = %q, want %q: an unloadable config must not "+
			"detach work held under a name (close exit=%d, stderr=%s)",
			gotNamed.Assignee, sessionName, code, stderr.String())
	}
	if gotNamed.Status != "in_progress" {
		t.Errorf("named work status = %q, want in_progress: downgrading to open is what "+
			"makes the owner's own resume check report \"no work\" (stderr=%s)",
			gotNamed.Status, stderr.String())
	}

	// HALF TWO. The bead-ID form must still be released, or it is stranded for
	// good — there is no downstream reclaim for a bead whose session is already
	// closed.
	gotPool, err := reopened.Get(poolWork.ID)
	if err != nil {
		t.Fatalf("re-read pool work bead: %v", err)
	}
	if gotPool.Assignee != "" {
		t.Errorf("pool work assignee = %q, want \"\": work bound to the dying session's "+
			"BEAD ID must still be released — withholding it strands it permanently, "+
			"since the closed session never reaches repairStrandedPoolWorkerBead and "+
			"releaseOrphanedPoolAssignments skips unrouted work (stderr=%s)",
			gotPool.Assignee, stderr.String())
	}
	if gotPool.Status != "open" {
		t.Errorf("pool work status = %q, want open: a released bead must return to the "+
			"queue, or it stays invisible to the work query (stderr=%s)",
			gotPool.Status, stderr.String())
	}

	// And it must SAY so. A silent skip is how this stayed invisible for four
	// days; the operator needs to know work was deliberately withheld and why,
	// so the close can be re-run once the config loads.
	if !strings.Contains(stderr.String(), "config unavailable") {
		t.Errorf("stderr does not name the reason work was withheld; got: %s", stderr.String())
	}
}
