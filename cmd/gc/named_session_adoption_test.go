package main

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestNamedSessionPoolShadowConverges is ga-dfp1b L2's convergence contract:
// a live ephemeral+pool_managed session bead backing a configured
// single-session [[named_session]] template (the "pool shadow" — minted by an
// older binary or a raced pool create) must not wedge the identity. Within a
// bounded number of build+sync ticks the city converges to exactly ONE open
// session bead for the identity, carrying the full configured_named_* stamp
// set, no pool_managed, the identity alias, the canonical session_name, on
// the DURABLE tier; it counts as the identity's holder (no duplicate
// provisioning) and the store-level resolver lookup agrees (canonical, no
// conflict) so mail/nudge to the identity resolves. Desired state must be
// stable tick-over-tick once converged — the config-drift restart loop
// re-derived the same mismatch every tick before this fix, and the builder's
// conflict skip burned 4600+ silent ticks.
//
// Two shadow variants, matching the two field shapes measured on ga-dfp1b:
// "alias_shadow" also holds alias=<identity> (the builder conflict-skip
// wedge); "template_shadow" holds only the backing template (the adopt/rekey
// drift-churn path). Convergence may legitimately pass through the standing
// "reconfigured" close+recreate (one bounded restart of the shadow runtime);
// what it must never do is loop or strand.
func TestNamedSessionPoolShadowConverges(t *testing.T) {
	for _, tc := range []struct {
		name        string
		shadowAlias string
	}{
		{name: "alias_shadow", shadowAlias: "agent-a"},
		{name: "template_shadow", shadowAlias: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			clk := &clock.Fake{Time: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
			maxOne := 1
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Agents: []config.Agent{{
					Name:              "agent-a",
					StartCommand:      "true",
					MaxActiveSessions: &maxOne,
				}},
				NamedSessions: []config.NamedSession{{
					Template: "agent-a",
					Mode:     "always",
				}},
			}
			spec, ok := findNamedSessionSpec(cfg, "test-city", "agent-a")
			if !ok {
				t.Fatalf("findNamedSessionSpec(agent-a) not found")
			}

			shadowSessionName := "agent-a-shdw01"
			shadowMeta := map[string]string{
				"session_name":   shadowSessionName,
				"template":       "agent-a",
				"agent_name":     "agent-a",
				"session_origin": "ephemeral",
				"pool_managed":   "true",
				"state":          "awake",
			}
			if tc.shadowAlias != "" {
				shadowMeta["alias"] = tc.shadowAlias
			}
			if _, err := store.Create(beads.Bead{
				Title:    "agent-a",
				Type:     sessionBeadType,
				Status:   "open",
				Labels:   []string{sessionBeadLabel},
				Metadata: shadowMeta,
			}); err != nil {
				t.Fatalf("Create(shadow): %v", err)
			}
			if err := sp.Start(context.Background(), shadowSessionName, runtime.Config{}); err != nil {
				t.Fatalf("Start(shadow runtime): %v", err)
			}

			var prevKeys, keys []string
			for tick := 1; tick <= 5; tick++ {
				var stderr bytes.Buffer
				dsResult := buildDesiredState("test-city", cityPath, clk.Now().UTC(), cfg, sp, store, &stderr)

				keys = nil
				for name, tp := range dsResult.State {
					// Key + identity + live fingerprint: fingerprint
					// stability is the restart-loop guard — the reconciler
					// restarts on started-vs-desired fingerprint mismatch,
					// so a desired fingerprint alternating tick-over-tick
					// IS the config-drift churn this bead measured.
					keys = append(keys, name+"|identity="+tp.ConfiguredNamedIdentity+"|fp="+runtime.LiveFingerprint(templateParamsToConfig(tp)))
				}
				sort.Strings(keys)
				t.Logf("tick %d desired: %v", tick, keys)
				if msg := stderr.String(); strings.Contains(msg, "blocked by conflicting session bead") {
					t.Logf("tick %d build stderr: %s", tick, msg)
				}
				// Stability window: ticks 4..5 must be identical — churn
				// budget for close+recreate is ticks 1..3.
				if tick == 5 && strings.Join(prevKeys, ",") != strings.Join(keys, ",") {
					t.Errorf("desired state still changing between tick 4 and 5:\n prev: %v\n  now: %v", prevKeys, keys)
				}
				prevKeys = keys

				var syncErr bytes.Buffer
				syncSessionBeads(cityPath, store, dsResult.State, sp, allConfiguredDS(dsResult.State), cfg, clk, &syncErr, false)
				if msg := syncErr.String(); msg != "" {
					t.Logf("tick %d sync stderr: %s", tick, msg)
				}
			}

			namedEntries := 0
			for _, k := range keys {
				if strings.Contains(k, "|identity=agent-a|") {
					namedEntries++
				}
			}
			if namedEntries != 1 {
				t.Errorf("final tick named desired entries for agent-a = %d, want exactly 1 (keys: %v)", namedEntries, keys)
			}

			all, err := store.ListByLabel(sessionBeadLabel, 0)
			if err != nil {
				t.Fatalf("ListByLabel: %v", err)
			}
			var open []beads.Bead
			for _, b := range all {
				t.Logf("bead %s status=%s ephemeral=%v meta=%v", b.ID, b.Status, b.Ephemeral, b.Metadata)
				if b.Status != "closed" {
					open = append(open, b)
				}
			}
			if len(open) != 1 {
				t.Fatalf("open session beads after convergence = %d, want 1", len(open))
			}
			converged := open[0]
			if got := converged.Metadata[namedSessionMetadataKey]; got != "true" {
				t.Errorf("configured_named_session = %q, want true", got)
			}
			if got := converged.Metadata[namedSessionIdentityMetadata]; got != "agent-a" {
				t.Errorf("configured_named_identity = %q, want agent-a", got)
			}
			if got := converged.Metadata[poolManagedMetadataKey]; got != "" {
				t.Errorf("pool_managed = %q, want cleared", got)
			}
			if got := converged.Metadata["alias"]; got != "agent-a" {
				t.Errorf("alias = %q, want agent-a", got)
			}
			if got := converged.Metadata["session_name"]; got != spec.SessionName {
				t.Errorf("session_name = %q, want canonical %q", got, spec.SessionName)
			}
			// Katya's insurance: durable tier even though durable is
			// by-construction today — a future create site that sets
			// Ephemeral would silently reintroduce the wisp bug.
			if converged.Ephemeral {
				t.Errorf("converged bead is on the wisp tier, want durable")
			}

			// Resolver agreement: the store-level lookup the CLI resolver
			// uses must see the converged bead as canonical with no conflict.
			lookup, err := sessionpkg.LookupConfiguredNamedSession(store, spec)
			if err != nil {
				t.Fatalf("LookupConfiguredNamedSession: %v", err)
			}
			if !lookup.HasCanonical || lookup.Canonical.ID != converged.ID {
				t.Errorf("resolver canonical = (%v, %s), want (true, %s)", lookup.HasCanonical, lookup.Canonical.ID, converged.ID)
			}
			if lookup.HasConflict {
				t.Errorf("resolver still sees conflict bead %s after convergence", lookup.Conflict.ID)
			}
		})
	}
}

// TestNamedSessionAliasShadowWithDistinctIdentityStaysLoud pins the boundary
// of the ga-dfp1b L2 adoption carve-out: when the named session's identity
// DIFFERS from its backing template, the identity-keyed session-name binding
// cannot find the shadow, so adoption cannot proceed — the builder must keep
// reporting the conflict (the L1 diagnostic), never suppress it silently.
func TestNamedSessionAliasShadowWithDistinctIdentityStaysLoud(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	maxOne := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "agent-b",
			StartCommand:      "true",
			MaxActiveSessions: &maxOne,
		}},
		NamedSessions: []config.NamedSession{{
			Name:     "agent-lead",
			Template: "agent-b",
			Mode:     "always",
		}},
	}
	shadow, err := store.Create(beads.Bead{
		Title:  "agent-b",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":   "agent-b-shdw02",
			"template":       "agent-b",
			"agent_name":     "agent-b",
			"alias":          "agent-lead",
			"session_origin": "ephemeral",
			"pool_managed":   "true",
			"state":          "awake",
		},
	})
	if err != nil {
		t.Fatalf("Create(shadow): %v", err)
	}

	var stderr bytes.Buffer
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	for name, tp := range dsResult.State {
		if tp.ConfiguredNamedIdentity == "agent-lead" {
			t.Fatalf("named identity materialized despite unadoptable alias shadow: %s", name)
		}
	}
	out := stderr.String()
	if !strings.Contains(out, "blocked by conflicting session bead") || !strings.Contains(out, shadow.ID) {
		t.Fatalf("expected the conflict-skip diagnostic naming bead %s (suppressing it would trade a loud wedge for a silent one), got stderr:\n%s", shadow.ID, out)
	}
}

// TestPoolCreateForNamedBackedTemplateMintsCanonical pins ga-dfp1b L2's
// create-side guard: a pool create routed at a template whose only
// legitimate session is a configured single-session named identity mints the
// CANONICAL named bead — full configured_named_* stamps, identity alias,
// canonical session_name, no pool_managed — so the pool-shadow shape is
// never born. Durable-tier asserted per the L2 review condition.
func TestPoolCreateForNamedBackedTemplateMintsCanonical(t *testing.T) {
	store := beads.NewMemStore()
	cityPath := t.TempDir()
	maxOne := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "agent-a",
			StartCommand:      "true",
			MaxActiveSessions: &maxOne,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "agent-a",
			Mode:     "always",
		}},
	}
	spec, ok := findNamedSessionSpec(cfg, "test-city", "agent-a")
	if !ok {
		t.Fatalf("findNamedSessionSpec(agent-a) not found")
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = newSessionBeadSnapshot(nil)

	info, err := createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], "agent-a", "agent-a-1", 1, nil)
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithGuardedAlias: %v", err)
	}
	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	if got := bead.Metadata[namedSessionMetadataKey]; got != "true" {
		t.Errorf("configured_named_session = %q, want true", got)
	}
	if got := bead.Metadata[namedSessionIdentityMetadata]; got != "agent-a" {
		t.Errorf("configured_named_identity = %q, want agent-a", got)
	}
	if got := bead.Metadata["session_name"]; got != spec.SessionName {
		t.Errorf("session_name = %q, want canonical %q", got, spec.SessionName)
	}
	if got := bead.Metadata["alias"]; got != "agent-a" {
		t.Errorf("alias = %q, want agent-a", got)
	}
	if got := bead.Metadata["session_origin"]; got != "named" {
		t.Errorf("session_origin = %q, want named", got)
	}
	if got := bead.Metadata[poolManagedMetadataKey]; got != "" {
		t.Errorf("pool_managed = %q, want unset", got)
	}
	if bead.Ephemeral {
		t.Errorf("minted bead is on the wisp tier, want durable")
	}
}

// TestPoolCreateForNamedBackedTemplateRefusesWhenCanonicalHolds pins the
// guard's refusal leg: when the canonical named session already holds the
// identity, a raced pool create errors instead of minting a duplicate — the
// holder IS the template's capacity, so this refusal cannot strand demand.
func TestPoolCreateForNamedBackedTemplateRefusesWhenCanonicalHolds(t *testing.T) {
	store := beads.NewMemStore()
	cityPath := t.TempDir()
	maxOne := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "agent-a",
			StartCommand:      "true",
			MaxActiveSessions: &maxOne,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "agent-a",
			Mode:     "always",
		}},
	}
	spec, ok := findNamedSessionSpec(cfg, "test-city", "agent-a")
	if !ok {
		t.Fatalf("findNamedSessionSpec(agent-a) not found")
	}
	if _, err := store.Create(beads.Bead{
		Title:  "agent-a",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               spec.SessionName,
			"template":                   "agent-a",
			"agent_name":                 "agent-a",
			"alias":                      "agent-a",
			"state":                      "awake",
			"session_origin":             "named",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "agent-a",
			namedSessionModeMetadata:     "always",
		},
	}); err != nil {
		t.Fatalf("Create(canonical): %v", err)
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = newSessionBeadSnapshot(nil)

	if _, err := createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], "agent-a", "agent-a-1", 1, nil); err == nil {
		t.Fatalf("expected refusal when canonical named session already holds the identity, got nil error")
	}
	all, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	openCount := 0
	for _, b := range all {
		if b.Status != "closed" {
			openCount++
		}
	}
	if openCount != 1 {
		t.Fatalf("open session beads = %d, want only the pre-existing canonical", openCount)
	}
}

// TestPoolCreateForMultiSessionNamedTemplateKeepsPoolMint pins the guard's
// scope: a named session backed by a MULTI-session template keeps the
// ordinary pool mint (pool workers beside the named session are legitimate
// there).
func TestPoolCreateForMultiSessionNamedTemplateKeepsPoolMint(t *testing.T) {
	store := beads.NewMemStore()
	cityPath := t.TempDir()
	maxTwo := 2
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "agent-a",
			StartCommand:      "true",
			MaxActiveSessions: &maxTwo,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "agent-a",
			Mode:     "always",
		}},
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", cityPath, cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = newSessionBeadSnapshot(nil)

	info, err := createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], "agent-a", "agent-a-1", 1, nil)
	if err != nil {
		t.Fatalf("createPoolSessionBeadWithGuardedAlias: %v", err)
	}
	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	if got := bead.Metadata[poolManagedMetadataKey]; got != "true" {
		t.Errorf("pool_managed = %q, want true for multi-session template", got)
	}
	if got := bead.Metadata[namedSessionMetadataKey]; got != "" {
		t.Errorf("configured_named_session = %q, want unset for pool mint", got)
	}
}
