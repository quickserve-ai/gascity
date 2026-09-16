package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// ga-dt5ffp, END TO END: the obligation must be built from the PRE-CLOSE
// snapshot of the session bead. Only a real close can prove this, because only a
// real close runs the mutation that makes the difference observable.
//
// Manager.CloseDetailed calls retireConfiguredNamedSessionIdentifiers, which
// blanks session_name. A capture taken from the bead AFTER that point records a
// degraded identifier set by construction, so the withheld work held under that
// name becomes unfindable by precisely the record meant to find it.
//
// WHAT THIS TEST DOES AND DOES NOT PIN — stated because the obvious reading is
// wrong, and it was mine until the negative control contradicted it:
//
//   - IT CATCHES a capture built from a post-close READ of the bead. Verified by
//     negative control: re-pointing the obligation at a post-close
//     sessStore.Get fails this test on the missing session_name.
//   - IT DOES NOT CATCH merely moving the publish CALL to after CloseDetailed.
//     Verified the same way — that reordering still passes. It passes correctly:
//     cmdSessionClose reads the bead into closedSessionBead before the close (for
//     ga-9n8hjv), and the obligation is built from that snapshot, so the call's
//     position does not change its contents. What the call's position does
//     govern is the crash-window asymmetry — publish-before-close leaves an
//     inert obligation on a still-open session, publish-after loses it entirely
//     — and NOTHING here covers that, because a test cannot crash the process
//     between the two writes. That property rests on the ordering comment at the
//     call site, not on this test.
//
// SESSION_NAME IS THE ONLY LOAD-BEARING ASSERTION below. The other two
// identifiers survive a post-close read anyway: configured_named_identity is not
// cleared by the retire, and the alias is recoverable because
// sessionBeadAssigneeIdentities reads alias_history — which is exactly where the
// retire puts it. They are asserted as completeness checks on the capture, not
// as evidence of ordering. (The release paths themselves do NOT read
// alias_history; the capture does.)
//
// The control below is what earns the session_name assertion: without proof that
// the close really blanked it, a pass is indistinguishable from the retire path
// never having fired.
func TestSessionCloseCapturesTheDeferredReleaseBeforeRetiringIdentifiers(t *testing.T) {
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

	const (
		sessionName  = "worker-named"
		namedIdent   = "gastown.worker"
		currentAlias = "worker-ga-abc12"
	)
	// configured_named_identity is what makes wasConfiguredNamedSession true, and
	// therefore what makes the close retire these identifiers. Without it the
	// retire never fires and the ordering this test exists to pin is untested.
	sessionBead, err := store.Create(beads.Bead{
		Title:  "named seat session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":                       sessionName,
			session.NamedSessionIdentityMetadata: namedIdent,
			"alias":                              currentAlias,
			"template":                           "worker",
			"state":                              "active",
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	// Break the config after the city exists, so this is the nil-cfg leg rather
	// than a city that never came up. Truncated TOML mid-table is the realistic
	// shape — a half-written file, what a config-drift roll racing an edit
	// actually produces.
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"),
		[]byte("[workspace]\nname = \"test-city\"\n[[agent\n"), 0o644); err != nil {
		t.Fatalf("corrupt city.toml: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionClose([]string{sessionBead.ID}, &stdout, &stderr)

	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	after, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("re-read session bead: %v", err)
	}

	// THE CONTROL. If the close did not actually retire the identifiers, then
	// capturing before or after it makes no difference and the assertion below
	// proves nothing. Fail loudly rather than report a false pass.
	if strings.TrimSpace(after.Metadata["session_name"]) != "" {
		t.Fatalf("CONTROL FAILED: session_name is still %q after the close, so this close did not "+
			"retire its identifiers and this test cannot distinguish a pre-close capture from a "+
			"post-close one (exit=%d, stderr=%s)",
			after.Metadata["session_name"], code, stderr.String())
	}

	raw := after.Metadata[beadmeta.ReleaseDeferredMetadataKey]
	if raw == "" {
		t.Fatalf("no deferred release obligation on the closed session bead: a close that withholds "+
			"the work release must leave something for a later actor to find (exit=%d, stderr=%s)",
			code, stderr.String())
	}
	var obligation sessionReleaseObligation
	if err := json.Unmarshal([]byte(raw), &obligation); err != nil {
		t.Fatalf("decode obligation %q: %v", raw, err)
	}

	if obligation.Capture != releaseCaptureFull {
		t.Errorf("Capture = %q, want %q: the session bead was readable at close time",
			obligation.Capture, releaseCaptureFull)
	}
	// THE ASSERTION THE CONTROL EARNS, and the only one here that pins ordering.
	// session_name no longer exists on the bead; if the obligation still names
	// it, the capture came from the pre-close snapshot.
	if !slices.Contains(obligation.Identities, sessionName) {
		t.Errorf("obligation identities %v omit %q, which the close has since blanked — "+
			"the capture ran AFTER the retire and the withheld work under that name is unfindable",
			obligation.Identities, sessionName)
	}
	// Completeness, not ordering: the retire does not clear this key.
	if !slices.Contains(obligation.Identities, namedIdent) {
		t.Errorf("obligation identities %v omit the configured named identity %q",
			obligation.Identities, namedIdent)
	}
	// Completeness, not ordering: the alias is reachable either way, because the
	// capture reads alias_history and that is where the retire moves it.
	if !slices.Contains(obligation.Identities, currentAlias) {
		t.Errorf("obligation identities %v omit the alias %q", obligation.Identities, currentAlias)
	}
	if obligation.RigStoresKnown {
		t.Error("RigStoresKnown = true: rig stores are enumerated only when the config loaded, " +
			"so on this path the scope is unknown and a drain must not read it as empty")
	}
	if obligation.Reason == "" {
		t.Error("Reason is empty: the obligation must record why the release was withheld")
	}

	// And the operator must be told, in the same breath as the withhold itself.
	if !strings.Contains(stderr.String(), "deferred work release") {
		t.Errorf("stderr does not mention the deferred work release; got: %s", stderr.String())
	}

	// THE EVENT MUST REACH THE FILE, not just a recorder. The unit test asserts
	// on a fake recorder, which proves the payload and proves nothing about the
	// wiring: a wrong path, a RuntimeRoot that moved, or a recorder that failed to
	// open would all leave the fake test green while the fleet saw nothing. A
	// withhold visible only in one pane's stderr is the silence this event exists
	// to end, so assert the durable half here.
	eventsPath := filepath.Join(cityDir, ".gc", "events.jsonl")
	raw2, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read %s: %v — the close published no event log at all", eventsPath, err)
	}
	if !strings.Contains(string(raw2), "session.release_deferred") {
		t.Errorf("no session.release_deferred event in %s: the withhold is observable only in "+
			"the stderr of whoever ran the close", eventsPath)
	}
	if !strings.Contains(string(raw2), sessionBead.ID) {
		t.Errorf("the event log does not name the session bead %s", sessionBead.ID)
	}
}

// The counterpart: when the config DOES load, the release runs normally and no
// obligation is published. An obligation left on every ordinary close would give
// a future drain a backlog of beads with nothing withheld, and the first thing it
// would do with them is release live work.
func TestSessionCloseWithAWorkingConfigPublishesNoDeferredRelease(t *testing.T) {
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
			"session_name":                       "worker-named",
			session.NamedSessionIdentityMetadata: "gastown.worker",
			"template":                           "worker",
			"state":                              "active",
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionClose([]string{sessionBead.ID}, &stdout, &stderr)

	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	after, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("re-read session bead: %v", err)
	}
	if raw := after.Metadata[beadmeta.ReleaseDeferredMetadataKey]; raw != "" {
		t.Errorf("a close with a working config published a deferred release obligation (%q); "+
			"only a withheld release owes one (exit=%d, stderr=%s)", raw, code, stderr.String())
	}
}
