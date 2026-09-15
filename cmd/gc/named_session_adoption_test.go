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

// distinctIdentityNamedSessionCity is a city whose configured named session
// identity ("agent-lead") differs from its single-session backing template
// ("agent-b").
func distinctIdentityNamedSessionCity() *config.City {
	maxOne := 1
	return &config.City{
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
}

// createDistinctIdentityAliasShadow creates a live pool shadow for
// distinctIdentityNamedSessionCity: an ephemeral pool-managed agent-b session
// with no configured_named_* stamps that holds the identity's alias.
func createDistinctIdentityAliasShadow(t *testing.T, store beads.Store, sessionName string) beads.Bead {
	t.Helper()
	shadow, err := store.Create(beads.Bead{
		Title:  "agent-b",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":   sessionName,
			"template":       "agent-b",
			"agent_name":     "agent-b",
			"alias":          "agent-lead",
			"session_origin": "ephemeral",
			"pool_managed":   "true",
			"state":          "awake",
		},
	})
	if err != nil {
		t.Fatalf("Create(shadow %s): %v", sessionName, err)
	}
	return shadow
}

// TestNamedSessionAliasShadowWithDistinctIdentityConverges pins how a sole
// alias-holding pool shadow converges when the named session's identity
// DIFFERS from its backing template. The identity-keyed session-name binding
// cannot find such a shadow, and it does not need to: the alias pass in
// session.FindCanonicalNamedSessionInfo takes the sole template-matching alias
// holder as the identity's canonical session, the builder keys the desired
// entry on that bead's own session_name, sync stamps it, and the standing
// "reconfigured" close+recreate (one bounded restart of the shadow runtime)
// moves the identity onto its canonical runtime name. Nothing is wedged, so the
// conflict-skip diagnostic must never fire.
func TestNamedSessionAliasShadowWithDistinctIdentityConverges(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	cfg := distinctIdentityNamedSessionCity()
	spec, ok := findNamedSessionSpec(cfg, "test-city", "agent-lead")
	if !ok {
		t.Fatalf("findNamedSessionSpec(agent-lead) not found")
	}

	const shadowSessionName = "agent-b-shdw02"
	shadow := createDistinctIdentityAliasShadow(t, store, shadowSessionName)
	if err := sp.Start(context.Background(), shadowSessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start(shadow runtime): %v", err)
	}

	var prevKeys, keys []string
	for tick := 1; tick <= 5; tick++ {
		var stderr bytes.Buffer
		dsResult := buildDesiredState("test-city", cityPath, clk.Now().UTC(), cfg, sp, store, &stderr)

		keys = nil
		for name, tp := range dsResult.State {
			keys = append(keys, name+"|identity="+tp.ConfiguredNamedIdentity+"|fp="+runtime.LiveFingerprint(templateParamsToConfig(tp)))
		}
		sort.Strings(keys)
		t.Logf("tick %d desired: %v", tick, keys)
		if msg := stderr.String(); strings.Contains(msg, "blocked by conflicting session bead") {
			t.Errorf("tick %d: conflict-skip diagnostic logged for a sole alias-holding shadow:\n%s", tick, msg)
		}
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

	all, err := store.ListByLabel(sessionBeadLabel, 0, beads.IncludeClosed)
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
	if got := converged.Metadata[namedSessionIdentityMetadata]; got != "agent-lead" {
		t.Errorf("configured_named_identity = %q, want agent-lead", got)
	}
	if got := converged.Metadata["alias"]; got != "agent-lead" {
		t.Errorf("alias = %q, want agent-lead", got)
	}
	if got := converged.Metadata["session_name"]; got != spec.SessionName {
		t.Errorf("session_name = %q, want canonical %q", got, spec.SessionName)
	}
	if got := converged.Metadata[poolManagedMetadataKey]; got != "" {
		t.Errorf("pool_managed = %q, want cleared", got)
	}
	if converged.Ephemeral {
		t.Errorf("converged bead is on the wisp tier, want durable")
	}

	retired, err := store.Get(shadow.ID)
	if err != nil {
		t.Fatalf("Get(shadow %s): %v", shadow.ID, err)
	}
	if retired.Status != "closed" {
		t.Errorf("shadow %s status = %q, want closed", shadow.ID, retired.Status)
	}
	if got := retired.Metadata["state"]; got != "reconfigured" {
		t.Errorf("shadow %s state = %q, want reconfigured", shadow.ID, got)
	}
	if got, want := retired.Metadata["close_reason"], sessionpkg.CanonicalCloseReason("reconfigured"); got != want {
		t.Errorf("shadow %s close_reason = %q, want %q", shadow.ID, got, want)
	}
	if sp.IsRunning(shadowSessionName) {
		t.Errorf("shadow runtime %q still running after the reconfigured close", shadowSessionName)
	}

	// Resolver agreement: the store-level lookup the CLI resolver uses must see
	// the converged bead as canonical with no conflict.
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
}

// TestNamedSessionAliasShadowsWithDistinctIdentityStayLoud pins the conflict
// the alias pass leaves standing. When TWO template-matching pool shadows hold
// a distinct identity's alias there is no unique canonical holder, and neither
// shadow is adoptable (the identity-keyed session-name binding cannot find a
// shadow named for the backing template), so the builder must skip the
// identity and name the first conflicting bead (the ga-dfp1b L1 diagnostic)
// instead of skipping it silently.
func TestNamedSessionAliasShadowsWithDistinctIdentityStayLoud(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := distinctIdentityNamedSessionCity()
	first := createDistinctIdentityAliasShadow(t, store, "agent-b-shdw02")
	second := createDistinctIdentityAliasShadow(t, store, "agent-b-shdw03")

	var stderr bytes.Buffer
	dsResult := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), store, &stderr)
	for name, tp := range dsResult.State {
		if tp.ConfiguredNamedIdentity == "agent-lead" {
			t.Fatalf("named identity materialized despite two alias holders: %s", name)
		}
	}
	out := stderr.String()
	if want := `named session "agent-lead" blocked by conflicting session bead ` + first.ID + " "; !strings.Contains(out, want) {
		t.Fatalf("expected the conflict-skip diagnostic naming the first alias holder %s (second holder %s), want %q in stderr:\n%s", first.ID, second.ID, want, out)
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

// rigScopedNamedGuardCity is a city whose mode=always named session is backed
// by a single-session rig template, so the identity ("rig/agent-a") and its
// runtime session_name ("rig--agent-a") are different strings and each census
// check of the guard is exercised on its own.
func rigScopedNamedGuardCity(rigPath string) *config.City {
	maxOne := 1
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig", Path: rigPath}},
		Agents: []config.Agent{{
			Name:              "agent-a",
			Dir:               "rig",
			StartCommand:      "true",
			MaxActiveSessions: &maxOne,
		}},
		NamedSessions: []config.NamedSession{{
			Template: "agent-a",
			Dir:      "rig",
			Mode:     "always",
		}},
	}
}

// requireNamedGuardRoutes fails unless a pool create for the city's only
// agent reaches the guard's canonical mint. Upstream's pool mint already
// fences with the lock-time census, so a fixture that fell through to it would
// pass the named-guard tests below vacuously.
func requireNamedGuardRoutes(t *testing.T, cfg *config.City) {
	t.Helper()
	if _, ok := poolCreateBacksSingleNamedSession(cfg, "test-city", cfg.Agents[0].QualifiedName()); !ok {
		t.Fatalf("pool create for %q does not route to the named-session guard", cfg.Agents[0].QualifiedName())
	}
}

// TestPoolCreateForNamedBackedTemplateRefusesForeignCensusHolder pins the
// guard's lock-time census: a live session holding the identity's alias or
// its session_name in a rig census leg refuses the canonical mint. The
// primary-store checks cannot see that leg, and the holder arrives after
// planning, so only a census re-read under the identifier locks proves the
// identity free; without it the mint births a second holder of the identity.
func TestPoolCreateForNamedBackedTemplateRefusesForeignCensusHolder(t *testing.T) {
	for _, tc := range []struct {
		name      string
		holdAlias bool
	}{
		{name: "alias", holdAlias: true},
		{name: "session_name", holdAlias: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := beads.NewMemStore()
			// Rig-leg IDs start past the primary's, so a refusal naming the
			// holder cannot be naming a primary bead.
			foreign := beads.NewMemStoreFrom(100, nil, nil)
			cfg := rigScopedNamedGuardCity(t.TempDir())
			requireNamedGuardRoutes(t, cfg)
			spec, ok := findNamedSessionSpec(cfg, "test-city", "rig/agent-a")
			if !ok {
				t.Fatalf("findNamedSessionSpec(rig/agent-a) not found")
			}
			if spec.Identity == spec.SessionName {
				t.Fatalf("fixture identity %q must differ from session_name %q", spec.Identity, spec.SessionName)
			}
			var stderr bytes.Buffer
			bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), primary, &stderr)
			primeGuardedPoolCrossStoreCensus(t, bp, map[string]beads.Store{"rig": foreign})
			if len(bp.sessionOccupancyInfos) != 0 {
				t.Fatalf("pre-lock census = %#v, want empty", bp.sessionOccupancyInfos)
			}

			alias, sessionName := "", "manual-agent-a"
			if tc.holdAlias {
				alias = spec.Identity
			} else {
				sessionName = spec.SessionName
			}
			holder := seedGuardedPoolSessionHolder(t, foreign, "rig-leg identity holder", alias, sessionName)

			_, qualifiedInstance, slot := poolDesiredRequestIdentity(&cfg.Agents[0], 1)
			created, err := createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], cfg.Agents[0].QualifiedName(), qualifiedInstance, slot, nil)
			if err == nil || !strings.Contains(err.Error(), holder.ID) {
				t.Fatalf("guarded named create = (%#v, %v), want refusal naming rig-leg holder %s", created, err, holder.ID)
			}
			if created.ID != "" || len(bp.sessionBeads.OpenInfos()) != 0 {
				t.Fatalf("refused create info=%#v writeback=%#v, want no primary mutation", created, bp.sessionBeads.OpenInfos())
			}
			primaryInfos, listErr := sessionFrontDoor(primary).ListAll(sessionpkg.ListAllOptions{})
			if listErr != nil || len(primaryInfos) != 0 {
				t.Fatalf("primary sessions = %#v err=%v, want none", primaryInfos, listErr)
			}
		})
	}
}

// TestPoolCreateForNamedBackedTemplateCensusLegErrorNeverMints pins the
// guard's fail-closed leg: a census leg that cannot be read at lock time
// makes the identity's absence unprovable, so the canonical mint refuses
// rather than trusting the primary store alone.
func TestPoolCreateForNamedBackedTemplateCensusLegErrorNeverMints(t *testing.T) {
	primary := beads.NewMemStore()
	foreign := &toggleListFailStore{Store: beads.NewMemStore()}
	cfg := rigScopedNamedGuardCity(t.TempDir())
	requireNamedGuardRoutes(t, cfg)
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), primary, &stderr)
	primeGuardedPoolCrossStoreCensus(t, bp, map[string]beads.Store{"rig": foreign})
	foreign.fail = true // the rig leg degrades after planning, before the lock-time proof

	_, qualifiedInstance, slot := poolDesiredRequestIdentity(&cfg.Agents[0], 1)
	created, err := createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], cfg.Agents[0].QualifiedName(), qualifiedInstance, slot, nil)
	if err == nil ||
		!strings.Contains(err.Error(), `"rig/agent-a"`) ||
		!strings.Contains(err.Error(), `session census leg "rig:rig"`) ||
		!strings.Contains(err.Error(), "cross-store list failed") {
		t.Fatalf("guarded named create = (%#v, %v), want wrapped lock-time rig-leg census failure naming the identity", created, err)
	}
	if created.ID != "" || len(bp.sessionBeads.OpenInfos()) != 0 {
		t.Fatalf("census-refused create info=%#v writeback=%#v, want no primary mutation", created, bp.sessionBeads.OpenInfos())
	}
	primaryInfos, listErr := sessionFrontDoor(primary).ListAll(sessionpkg.ListAllOptions{})
	if listErr != nil || len(primaryInfos) != 0 {
		t.Fatalf("primary sessions = %#v err=%v, want none", primaryInfos, listErr)
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
