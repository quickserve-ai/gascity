package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func bindCloudTestCity(t *testing.T) (beads.Store, string) {
	t.Helper()
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "cloudy",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "cloudy",
			"template":     "test",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return store, created.ID
}

func TestCmdSessionBindCloudStampsBindingAndClearsSuspect(t *testing.T) {
	store, beadID := bindCloudTestCity(t)
	// Pre-poison suspect + last-outcome facts: a rebind is the explicit
	// recovery action, so it must clear them.
	for k, v := range map[string]string{
		session.MetadataCloudWakeBindingSuspect:   "refused_not_found",
		session.MetadataCloudWakeBindingSuspectAt: "2026-09-01T00:00:00Z",
		session.MetadataCloudWakeLastOutcome:      "refused_not_found",
		session.MetadataCloudWakeLastOutcomeAt:    "2026-09-01T00:00:00Z",
	} {
		if err := store.SetMetadata(beadID, k, v); err != nil {
			t.Fatal(err)
		}
	}
	accountDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := cmdSessionBindCloud(beadID, "session_01BINDTESTabcdef0123456789", accountDir, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("bind-cloud failed (%d): %s", code, stderr.String())
	}
	b, err := store.Get(beadID)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Metadata[session.MetadataCloudWakeSessionID]; got != "session_01BINDTESTabcdef0123456789" {
		t.Errorf("session id = %q", got)
	}
	if got := b.Metadata[session.MetadataCloudWakeAccountDir]; got != accountDir {
		t.Errorf("account dir = %q, want %q", got, accountDir)
	}
	for _, k := range []string{
		session.MetadataCloudWakeBindingSuspect,
		session.MetadataCloudWakeBindingSuspectAt,
		session.MetadataCloudWakeLastOutcome,
		session.MetadataCloudWakeLastOutcomeAt,
	} {
		if got := b.Metadata[k]; got != "" {
			t.Errorf("%s = %q, want cleared on rebind", k, got)
		}
	}
	if b.Metadata[session.MetadataCloudWakeBoundAt] == "" {
		t.Error("bound_at not stamped")
	}
	if !strings.Contains(stdout.String(), "Bound") {
		t.Errorf("stdout: %s", stdout.String())
	}
}

func TestCmdSessionBindCloudRefusesBadInputs(t *testing.T) {
	_, beadID := bindCloudTestCity(t)
	accountDir := t.TempDir()
	var stdout, stderr bytes.Buffer

	if code := cmdSessionBindCloud(beadID, "not-an-id", accountDir, false, &stdout, &stderr); code == 0 {
		t.Fatal("bad --cloud-id accepted")
	}
	stderr.Reset()
	if code := cmdSessionBindCloud(beadID, "session_01BINDTESTabcdef0123456789", "", false, &stdout, &stderr); code == 0 {
		t.Fatal("missing --account-dir accepted")
	}
	if !strings.Contains(stderr.String(), "ambient auth") {
		t.Errorf("missing-lineage refusal must explain ambient-auth ban: %s", stderr.String())
	}
	stderr.Reset()
	if code := cmdSessionBindCloud(beadID, "session_01BINDTESTabcdef0123456789", "/nonexistent/lineage/dir", false, &stdout, &stderr); code == 0 {
		t.Fatal("nonexistent --account-dir accepted")
	}
	stderr.Reset()
	if code := cmdSessionBindCloud(beadID, "session_01BINDTESTabcdef0123456789", accountDir, true, &stdout, &stderr); code == 0 {
		t.Fatal("--clear combined with --cloud-id accepted")
	}
}

func TestCmdSessionBindCloudClearRemovesBinding(t *testing.T) {
	store, beadID := bindCloudTestCity(t)
	accountDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := cmdSessionBindCloud(beadID, "session_01BINDTESTabcdef0123456789", accountDir, false, &stdout, &stderr); code != 0 {
		t.Fatalf("bind failed: %s", stderr.String())
	}
	if code := cmdSessionBindCloud(beadID, "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("clear failed: %s", stderr.String())
	}
	b, err := store.Get(beadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		session.MetadataCloudWakeSessionID,
		session.MetadataCloudWakeAccountDir,
		session.MetadataCloudWakeBindingSuspect,
		session.MetadataCloudWakeLastOutcome,
	} {
		if got := b.Metadata[k]; got != "" {
			t.Errorf("%s = %q, want cleared", k, got)
		}
	}
}
