package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// ga-n0rvsk regression coverage: the controller's own spawn path
// (buildPreparedStart) must name claude sessions — the 2026-09-06 re-land
// appended --name only inside config.BuildProviderLaunchCommand*, which the
// reconciler's template composition (template_resolve.go CommandString())
// never calls, so the entire supervised fleet launched nameless. These tests
// pin the spawn-site append AND its two safety contracts: the name must use
// the ADDRESSABLE identity (binding-imported agents: "qcore/mallory", not the
// template-qualified "qcore/cherub-law.mallory"), and it must never enter the
// config fingerprint (the ga-2pcujo restart-wave mechanism).

func namedClaudeResolvedProvider(display string) *config.ResolvedProvider {
	return &config.ResolvedProvider{
		Name:               "claude",
		Command:            "claude",
		BuiltinAncestor:    "claude",
		SessionDisplayName: display,
	}
}

func newNamedSessionCandidate(t *testing.T, store beads.Store, identityMeta map[string]string, display string) startCandidate {
	t.Helper()
	meta := map[string]string{
		"session_name": "qcore--mallory",
		"template":     "qcore/cherub-law.mallory",
		"state":        "asleep",
	}
	for k, v := range identityMeta {
		meta[k] = v
	}
	session, err := store.Create(beads.Bead{
		Title:    "qcore--mallory",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	return startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp: TemplateParams{
			TemplateName:     "qcore/cherub-law.mallory",
			SessionName:      "qcore--mallory",
			Command:          "claude --effort high",
			ResolvedProvider: namedClaudeResolvedProvider(display),
		},
	}
}

func TestBuildPreparedStartNamesClaudeSessionWithAddressableIdentity(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newNamedSessionCandidate(t, store, map[string]string{
		"configured_named_identity": "qcore/mallory",
		"alias":                     "qcore/mallory",
		"agent_name":                "qcore/mallory",
	}, "qcore/cherub-law.mallory")

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--name qcore/mallory") {
		t.Fatalf("prepared command = %q, want --name qcore/mallory", prepared.cfg.Command)
	}
	if strings.Contains(prepared.cfg.Command, "cherub-law.mallory") {
		t.Fatalf("prepared command = %q, template-qualified identity must not win over the addressable one", prepared.cfg.Command)
	}
	// The stored template command is the fingerprint input; the name lives
	// only on the executed command.
	if strings.Contains(candidate.tp.Command, "--name") {
		t.Fatalf("tp.Command mutated: %q", candidate.tp.Command)
	}
}

// TestBuildPreparedStartSessionNameMatchesDriftHash pins the ga-2pcujo
// contract: the appended --name must be invisible to the config fingerprint,
// so the coreHash stamped at start equals the hash the reconciler recomputes
// from the template on every later tick. If the name ever leaks into a hashed
// field, this fails — and in production every claude seat drains at once.
func TestBuildPreparedStartSessionNameMatchesDriftHash(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newNamedSessionCandidate(t, store, map[string]string{
		"configured_named_identity": "qcore/mallory",
	}, "qcore/cherub-law.mallory")

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--name") {
		t.Fatalf("prepared command = %q, want a --name append for this test to mean anything", prepared.cfg.Command)
	}
	want := runtime.CoreFingerprint(sessionCoreConfigForHashInfo(candidate.tp, candidate.info))
	if prepared.coreHash != want {
		t.Fatalf("prepared coreHash = %s, want drift hash %s\nprepared command: %q\ndrift command:    %q",
			prepared.coreHash,
			want,
			prepared.cfg.Command,
			sessionCoreConfigForHashInfo(candidate.tp, candidate.info).Command)
	}
}

func TestBuildPreparedStartSessionNameFallsBackToDisplayName(t *testing.T) {
	store := beads.NewMemStore()
	// No identity metadata at all: unbound agents (woodhouse, katya) coincide
	// in both forms, so the resolution's SessionDisplayName is the fallback.
	candidate := newNamedSessionCandidate(t, store, nil, "woodhouse")

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--name woodhouse") {
		t.Fatalf("prepared command = %q, want fallback --name woodhouse", prepared.cfg.Command)
	}
}

func TestBuildPreparedStartSessionNameSkipsNonClaude(t *testing.T) {
	store := beads.NewMemStore()
	candidate := newNamedSessionCandidate(t, store, map[string]string{
		"configured_named_identity": "deacon",
	}, "deacon")
	candidate.tp.Command = "omp --hook gc-hook.ts"
	candidate.tp.ResolvedProvider = &config.ResolvedProvider{
		Name:               "omp",
		Command:            "omp",
		BuiltinAncestor:    "omp",
		SessionDisplayName: "deacon",
	}

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	if strings.Contains(prepared.cfg.Command, "--name") {
		t.Fatalf("prepared command = %q, omp must not carry --name", prepared.cfg.Command)
	}
}
