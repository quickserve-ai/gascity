package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
)

// incidentNow is the moment gc doctor is imagined to run: 15m21s after
// ga-wisp-r4jw5ee stamped its pending_create_started_at, which is roughly
// where woodhouse found the wedge on 2026-08-13.
var incidentNow = time.Date(2026, 8, 14, 1, 0, 0, 0, time.UTC)

// refineryIncidentConfig rebuilds the qcore rig's refinery named session:
// identity "qcore/gastown.refinery", backed by the same-named agent template,
// mode on_demand, and (with an empty session_template) the runtime session
// name "qcore--gastown__refinery" the incident reported.
func refineryIncidentConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "gascity"},
		Agents: []config.Agent{
			{Name: "refinery", Dir: "qcore", BindingName: "gastown"},
			{Name: "witness", Dir: "qcore", BindingName: "gastown"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "refinery", Dir: "qcore", BindingName: "gastown", Mode: "on_demand"},
			{Template: "witness", Dir: "qcore", BindingName: "gastown", Mode: "on_demand"},
		},
	}
}

// squattingRefineryBead reconstructs ga-wisp-r4jw5ee as woodhouse observed it:
// a half-created session bead that owns the refinery's fixed runtime name,
// carries the configured_named_session FLAG but no configured_named_identity,
// and never left start-pending. No tmux session and no refinery process
// existed behind it.
func squattingRefineryBead() beads.Bead {
	return beads.Bead{
		ID:     "ga-wisp-r4jw5ee",
		Type:   session.BeadType,
		Status: "open",
		Title:  "qcore/gastown.refinery",
		Labels: []string{session.LabelSession, "template:qcore/gastown.refinery"},
		Metadata: map[string]string{
			"state":                     "start-pending",
			"pending_create_claim":      "true",
			"pending_create_started_at": "2026-08-14T00:44:39Z",
			"wake_attempts":             "0",
			"configured_named_session":  "true",
			"configured_named_mode":     "on_demand",
			"session_name":              "qcore--gastown__refinery",
			"session_name_explicit":     "true",
			"template":                  "qcore/gastown.refinery",
		},
	}
}

// healthyWitnessBead is ga-y6muzy's shape: the canonical bead for a configured
// named identity, stamped with configured_named_identity and running.
func healthyWitnessBead() beads.Bead {
	return beads.Bead{
		ID:        "ga-y6muzy",
		Type:      session.BeadType,
		Status:    "open",
		Title:     "qcore/gastown.witness",
		Labels:    []string{session.LabelSession},
		CreatedAt: incidentNow.Add(-72 * time.Hour),
		Metadata: map[string]string{
			"state":                      "active",
			"configured_named_session":   "true",
			"configured_named_identity":  "qcore/gastown.witness",
			"configured_named_mode":      "on_demand",
			"session_name":               "qcore--gastown__witness",
			"session_name_explicit":      "true",
			"template":                   "qcore/gastown.witness",
			"agent_name":                 "qcore/gastown.witness",
			"instance_token":             "tok-witness",
			"creation_complete_at":       "2026-08-11T01:00:00Z",
			"started_config_hash":        "hash",
			"continuation_epoch":         "1",
			"generation":                 "1",
			"configured_named_mode_seen": "on_demand",
		},
	}
}

func infosForTest(t *testing.T, in ...beads.Bead) []session.Info {
	t.Helper()
	rows := session.ReconcileRowsFromBeads(in)
	out := make([]session.Info, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Info)
	}
	return out
}

// runningNames returns an isRunning probe backed by a fixed live-session set.
func runningNames(names ...string) func(string) bool {
	live := make(map[string]bool, len(names))
	for _, n := range names {
		live[n] = true
	}
	return func(name string) bool { return live[name] }
}

// livenessFactory adapts a fixed live-session set to the check's lazy probe
// factory.
func livenessFactory(names ...string) func() func(string) bool {
	probe := runningNames(names...)
	return func() func(string) bool { return probe }
}

// TestSessionCreateWedgeFiresOnRefineryIncident is the ga-2otk73 regression:
// the exact bead metadata that took qcore/gastown.refinery down for ~15
// minutes must produce a loud, actionable finding that names BOTH the blocked
// identity and the squatting bead ID.
func TestSessionCreateWedgeFiresOnRefineryIncident(t *testing.T) {
	cfg := refineryIncidentConfig()
	infos := infosForTest(t, squattingRefineryBead(), healthyWitnessBead())

	findings := analyzeSessionCreateWedge(cfg, "gascity", infos, runningNames("qcore--gastown__witness"), incidentNow)

	if len(findings.blocked) != 1 {
		t.Fatalf("blocked findings = %d (%v), want 1", len(findings.blocked), findings.blocked)
	}
	got := findings.blocked[0]
	for _, want := range []string{
		"qcore/gastown.refinery",
		"qcore--gastown__refinery",
		"ga-wisp-r4jw5ee",
		"state=start-pending",
		"gc bd close ga-wisp-r4jw5ee",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("blocked finding missing %q\n got: %s", want, got)
		}
	}
	// The squatter's blocked-identity line already carries its state and age,
	// so it must not be repeated as a bare wedged-session line.
	if len(findings.wedged) != 0 {
		t.Errorf("wedged findings = %v, want none (the squatter is reported once, with its blast radius)", findings.wedged)
	}

	result := sessionCreateWedgeResult("session-create-wedge", findings)
	if result.Status != doctor.StatusError {
		t.Errorf("status = %v, want StatusError — a named identity that cannot start is an outage", result.Status)
	}
	if result.Severity != doctor.SeverityAdvisory {
		t.Errorf("severity = %v, want SeverityAdvisory", result.Severity)
	}
	if !strings.Contains(result.FixHint, "gc bd close") {
		t.Errorf("fix hint does not tell the operator what to do: %q", result.FixHint)
	}
	t.Logf("message: %s", result.Message)
	t.Logf("detail:  %s", result.Details[0])
	t.Logf("hint:    %s", result.FixHint)
}

// TestSessionCreateWedgeQuietForHealthyCity is the accept case that stops this
// check from becoming noise: a fully healthy named session, a fresh session
// still legitimately mid-create, and a slow-but-live create all stay silent.
func TestSessionCreateWedgeQuietForHealthyCity(t *testing.T) {
	cfg := refineryIncidentConfig()

	freshRefinery := beads.Bead{
		ID:        "ga-fresh01",
		Type:      session.BeadType,
		Status:    "open",
		Labels:    []string{session.LabelSession},
		CreatedAt: incidentNow.Add(-20 * time.Second),
		Metadata: map[string]string{
			"state":                     "start-pending",
			"pending_create_claim":      "true",
			"pending_create_started_at": incidentNow.Add(-20 * time.Second).Format(time.RFC3339),
			"configured_named_session":  "true",
			"configured_named_identity": "qcore/gastown.refinery",
			"session_name":              "qcore--gastown__refinery",
			"template":                  "qcore/gastown.refinery",
		},
	}
	// A pool worker whose create is genuinely slow but whose runtime IS up:
	// the liveness probe must suppress it however old the lease looks.
	slowButLive := beads.Bead{
		ID:        "ga-live001",
		Type:      session.BeadType,
		Status:    "open",
		Labels:    []string{session.LabelSession},
		CreatedAt: incidentNow.Add(-6 * time.Hour),
		Metadata: map[string]string{
			"state":                     "creating",
			"pending_create_claim":      "true",
			"pending_create_started_at": incidentNow.Add(-6 * time.Hour).Format(time.RFC3339),
			"session_name":              "polecat-slot-1",
			"template":                  "qcore/polecat",
			"pool_managed":              "true",
		},
	}
	// A bead with no timestamp anywhere is unmeasurable: fail quiet, never
	// manufacture a finding out of missing data.
	noTimestamps := beads.Bead{
		ID:     "ga-notime1",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        "creating",
			"session_name": "mystery-session",
			"template":     "qcore/polecat",
		},
	}
	// Closed beads are not wedged creates.
	closedOld := beads.Bead{
		ID:        "ga-closed1",
		Type:      session.BeadType,
		Status:    "closed",
		Labels:    []string{session.LabelSession},
		CreatedAt: incidentNow.Add(-30 * 24 * time.Hour),
		Metadata: map[string]string{
			"state":        "start-pending",
			"session_name": "qcore--gastown__refinery",
			"template":     "qcore/gastown.refinery",
		},
	}

	infos := infosForTest(t, healthyWitnessBead(), freshRefinery, slowButLive, noTimestamps, closedOld)
	isRunning := runningNames("qcore--gastown__witness", "polecat-slot-1")

	findings := analyzeSessionCreateWedge(cfg, "gascity", infos, isRunning, incidentNow)
	if len(findings.blocked) != 0 || len(findings.wedged) != 0 {
		t.Fatalf("healthy city produced findings: blocked=%v wedged=%v", findings.blocked, findings.wedged)
	}

	result := sessionCreateWedgeResult("session-create-wedge", findings)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v (%s), want StatusOK", result.Status, result.Message)
	}
	t.Logf("message: %s", result.Message)
}

// TestSessionCreateWedgeQuietForLegacyNamedHolder covers the precision case
// that decides whether this check survives contact with a real city: a
// pre-identity-stamp bead that owns a configured identity's reserved name by
// its template signal alone, has settled into a live state, and whose runtime
// is currently down (a stopped city, or an agent between restarts). It is not
// a squatter and must stay silent — while the SAME bead still mid-create is
// the incident and must fire.
func TestSessionCreateWedgeQuietForLegacyNamedHolder(t *testing.T) {
	cfg := refineryIncidentConfig()
	legacy := beads.Bead{
		ID:        "ga-legacy1",
		Type:      session.BeadType,
		Status:    "open",
		Labels:    []string{session.LabelSession},
		CreatedAt: incidentNow.Add(-90 * 24 * time.Hour),
		Metadata: map[string]string{
			"state":        "asleep",
			"session_name": "qcore--gastown__refinery",
			"template":     "qcore/gastown.refinery",
		},
	}

	settled := analyzeSessionCreateWedge(cfg, "gascity", infosForTest(t, legacy), runningNames(), incidentNow)
	if len(settled.blocked) != 0 || len(settled.wedged) != 0 {
		t.Fatalf("settled legacy holder produced findings: blocked=%v wedged=%v", settled.blocked, settled.wedged)
	}

	// Same bead, still mid-create: that is ga-2otk73 and must fire.
	midCreate := legacy
	midCreate.Metadata = map[string]string{
		"state":        "start-pending",
		"session_name": "qcore--gastown__refinery",
		"template":     "qcore/gastown.refinery",
	}
	wedged := analyzeSessionCreateWedge(cfg, "gascity", infosForTest(t, midCreate), runningNames(), incidentNow)
	if len(wedged.blocked) != 1 {
		t.Fatalf("mid-create legacy holder blocked = %v, want 1 finding", wedged.blocked)
	}
	t.Logf("detail: %s", wedged.blocked[0])
}

// TestSessionCreateWedgeReportsUnnamedWedgeAsWarning covers the ga-qcuz36 half:
// a session stuck in a transitional state with no live runtime is reported
// even when no configured named identity is blocked, but only as a warning.
func TestSessionCreateWedgeReportsUnnamedWedgeAsWarning(t *testing.T) {
	cfg := refineryIncidentConfig()
	// The ga-qcuz36 shape: state=creating, untouched for fourteen days.
	stuckPool := beads.Bead{
		ID:        "ga-stuck14",
		Type:      session.BeadType,
		Status:    "open",
		Labels:    []string{session.LabelSession},
		CreatedAt: incidentNow.Add(-14 * 24 * time.Hour),
		Metadata: map[string]string{
			"state":        "creating",
			"session_name": "polecat-slot-9",
			"template":     "qcore/polecat",
			"pool_managed": "true",
		},
	}

	findings := analyzeSessionCreateWedge(cfg, "gascity", infosForTest(t, stuckPool), runningNames(), incidentNow)
	if len(findings.blocked) != 0 {
		t.Fatalf("blocked = %v, want none", findings.blocked)
	}
	if len(findings.wedged) != 1 {
		t.Fatalf("wedged = %v, want 1 finding", findings.wedged)
	}
	for _, want := range []string{"ga-stuck14", "state=creating", "336h0m0s", "no live runtime"} {
		if !strings.Contains(findings.wedged[0], want) {
			t.Errorf("wedged finding missing %q\n got: %s", want, findings.wedged[0])
		}
	}

	result := sessionCreateWedgeResult("session-create-wedge", findings)
	if result.Status != doctor.StatusWarning || result.Severity != doctor.SeverityAdvisory {
		t.Fatalf("status/severity = %v/%v, want warning/advisory", result.Status, result.Severity)
	}
	t.Logf("message: %s", result.Message)
	t.Logf("detail:  %s", result.Details[0])
}

// TestSessionCreateWedgeCheckSkipsWithoutRuntimeProbe pins the fail-quiet
// contract: with no liveness probe the check reports a skip, never a finding.
func TestSessionCreateWedgeCheckSkipsWithoutRuntimeProbe(t *testing.T) {
	check := newSessionCreateWedgeCheck(refineryIncidentConfig(), t.TempDir(),
		func(string) (beads.Store, error) { return beads.NewMemStore(), nil },
		func() func(string) bool { return nil })
	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning || !strings.Contains(result.Message, "skipped") {
		t.Fatalf("result = %v/%q, want a skip warning", result.Status, result.Message)
	}
	if len(result.Details) != 0 {
		t.Fatalf("skip produced findings: %v", result.Details)
	}
}

// TestSessionCreateWedgeCheckRunsEndToEnd wires the real store read to the
// analysis so the incident fires through Run(), not just the pure core.
func TestSessionCreateWedgeCheckRunsEndToEnd(t *testing.T) {
	store := newWedgeSpyStore(squattingRefineryBead(), healthyWitnessBead())

	check := newSessionCreateWedgeCheck(refineryIncidentConfig(), t.TempDir(),
		func(string) (beads.Store, error) { return store, nil },
		livenessFactory("qcore--gastown__witness"))
	check.now = func() time.Time { return incidentNow }

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v (%s), want StatusError", result.Status, result.Message)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "ga-wisp-r4jw5ee") {
		t.Fatalf("details = %v, want the squatting bead ID", result.Details)
	}
	t.Logf("message: %s", result.Message)
	t.Logf("detail:  %s", result.Details[0])
}

// TestLoadSessionCreateWedgeInfosStaysBounded is the cost guard. Doctor runs
// every check under a per-check budget and this codebase has a recurring
// defect class where a check outgrows it (ga-22tvtm, ga-ciymkk). Worst case
// here is fixed: two indexed queries, both tiers, closed history excluded,
// never a scan.
func TestLoadSessionCreateWedgeInfosStaysBounded(t *testing.T) {
	closed := beads.Bead{
		ID:       "ga-closed9",
		Type:     session.BeadType,
		Status:   "closed",
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"state": "creating", "session_name": "gone"},
	}
	store := newWedgeSpyStore(squattingRefineryBead(), closed)

	infos, err := loadSessionCreateWedgeInfos(store)
	if err != nil {
		t.Fatalf("loadSessionCreateWedgeInfos: %v", err)
	}
	for _, info := range infos {
		if info.SessionNameMetadata == "gone" {
			t.Fatalf("closed session bead leaked into the wedge scan: %+v", info)
		}
	}
	if len(store.queries) != 2 {
		t.Fatalf("store queries = %d (%+v), want exactly 2", len(store.queries), store.queries)
	}
	for _, q := range store.queries {
		if q.AllowScan {
			t.Errorf("query %+v used AllowScan; the wedge scan must stay on indexed selectors", q)
		}
		if q.IncludeClosed {
			t.Errorf("query %+v included closed history; it grows without bound", q)
		}
		if q.TierMode != beads.TierBoth {
			t.Errorf("query %+v must read both tiers — a half-created session bead can land in the wisp tier where the reconciler's own list cannot see it", q)
		}
	}
}

// wedgeSpyStore records the queries the check issues and answers them from a
// fixed fixture set. MemStore mints its own IDs and CreatedAt on Create, which
// would erase the incident's bead ID and lease age, so the fixtures are served
// verbatim; the embedded MemStore only satisfies the rest of beads.Store.
type wedgeSpyStore struct {
	*beads.MemStore
	fixtures []beads.Bead
	queries  []beads.ListQuery
}

func newWedgeSpyStore(fixtures ...beads.Bead) *wedgeSpyStore {
	return &wedgeSpyStore{MemStore: beads.NewMemStore(), fixtures: fixtures}
}

func (s *wedgeSpyStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	var out []beads.Bead
	for _, b := range s.fixtures {
		if b.Status == "closed" && !query.IncludeClosed {
			continue
		}
		if query.Type != "" && b.Type != query.Type {
			continue
		}
		if query.Label != "" && !wedgeFixtureHasLabel(b, query.Label) {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

func wedgeFixtureHasLabel(b beads.Bead, label string) bool {
	for _, l := range b.Labels {
		if l == label {
			return true
		}
	}
	return false
}
