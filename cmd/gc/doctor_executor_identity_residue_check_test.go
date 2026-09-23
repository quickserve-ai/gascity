package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestExecutorIdentityResidueCheckFlagsAndFixesStaleStamp(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		residueRanSession("RAN-1", "gascity--builder"),
		{ID: "CITY-1", Title: "stale stamp", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
			"gc.work_dir":     filepath.Join(cityDir, "worktrees/gascity/builder-1"),
			"work_dir":        filepath.Join(cityDir, "legacy/worktrees/gascity/builder-1"),
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "CITY-1") {
		t.Fatalf("details missing CITY-1:\n%s", details)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}

	bd, err := cityStore.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, key := range []string{"gc.session_name", "gc.work_dir", "work_dir"} {
		if v := bd.Metadata[key]; v != "" {
			t.Fatalf("expected %s cleared, got %q", key, v)
		}
	}
	if bd.Metadata["gc.routed_to"] != "gascity/reviewer" {
		t.Fatalf("Fix must not touch gc.routed_to, got %q", bd.Metadata["gc.routed_to"])
	}

	result = check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status after fix = %v, want ok: %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckSkipsInProgressBead(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "in flight", Type: "task", Status: "in_progress", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
			"gc.work_dir":     "/worktrees/gascity/builder-1",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (in_progress beads must never be flagged): %#v", result.Status, result)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}

	bd, err := cityStore.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.session_name"] != "gascity--builder" || bd.Metadata["gc.work_dir"] != "/worktrees/gascity/builder-1" {
		t.Fatalf("Fix must not touch an in_progress bead's stamps, got %+v", bd.Metadata)
	}
}

func TestExecutorIdentityResidueCheckFixIsIdempotent(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := &residueSetMetadataBatchSpyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		residueRanSession("RAN-1", "gascity--builder"),
		{ID: "CITY-1", Title: "stale stamp", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
		}},
	}, nil)}

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("first Fix returned error: %v", err)
	}
	if cityStore.calls != 1 {
		t.Fatalf("expected exactly 1 write after first Fix, got %d", cityStore.calls)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("second Fix returned error: %v", err)
	}
	if cityStore.calls != 1 {
		t.Fatalf("expected zero additional writes on second Fix, got %d total", cityStore.calls)
	}
}

func TestExecutorIdentityResidueCheckAllowsCanonicalSessionNameEncoding(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "still current", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (session_name canonicalizes to current routed_to, not drift): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckSkipsSessionBeadWithoutSessionName(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "session bead", Type: "session", Status: "open", Metadata: map[string]string{
			"work_dir": "/worktrees/gascity/builder-1",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a session bead's bare work_dir must not be flagged without gc.session_name): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckSkipsDrainStepWithoutSessionName(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "drain step", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.work_dir": "/worktrees/gascity/builder-1",
			"work_dir":    "/worktrees/gascity/builder-1",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a drain step/member carries both work_dir keys but never gc.session_name, so it must not be flagged): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckSkipsOpenPoolBeadWithoutSessionName(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "pool ready", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.work_dir": "/worktrees/gascity/builder-1",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (an open pool-ready bead has gc.work_dir but no gc.session_name, so it must not be flagged): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckSkipsWorkflowRunRootDespiteSessionNameStamp(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "workflow run root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind":         "workflow",
			"gc.session_name": "gascity--builder",
			"gc.work_dir":     "/worktrees/gascity/builder-1",
			"gc.routed_to":    "gascity/reviewer",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a workflow run root is topology, never itself claimed; a completed step's visibility stamp on gc.session_name/gc.work_dir must not read as residue even though it mismatches gc.routed_to): %#v", result.Status, result)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	bd, err := cityStore.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.session_name"] != "gascity--builder" || bd.Metadata["gc.work_dir"] != "/worktrees/gascity/builder-1" {
		t.Fatalf("Fix must not clear a workflow run root's visibility stamp, got %+v", bd.Metadata)
	}
}

func TestExecutorIdentityResidueCheckAllowsCustomSessionTemplateEncoding(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Agents: residueTestAgents(), Workspace: config.Workspace{
		Name:            "acmecity",
		SessionTemplate: "{{.City}}-{{.Name}}",
	}}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "custom template session", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "acmecity-builder",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (gc.session_name minted via a custom session_template forward-encodes from the current gc.routed_to, so it is current, not drift): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckDescribeNamesTriggeringKeys(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		residueRanSession("RAN-1", "gascity--builder"),
		{ID: "CITY-1", Title: "session-name residue", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
		}},
		{ID: "CITY-2", Title: "work_dir residue", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to": "gascity/builder",
			"gc.work_dir":  filepath.Join(cityDir, "worktrees/gascity/builder-1"),
			"work_dir":     filepath.Join(cityDir, "legacy/worktrees/gascity/builder-1"),
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}

	sessionDetail := residueDetailFor(t, result.Details, "CITY-1")
	if !strings.Contains(sessionDetail, "(gc.session_name)") {
		t.Fatalf("describe() must name the triggering key for a session-name finding, got:\n%s", sessionDetail)
	}
	if strings.Contains(sessionDetail, "work_dir") {
		t.Fatalf("describe() must not name work_dir keys for a session-name-only finding, got:\n%s", sessionDetail)
	}

	workDirDetail := residueDetailFor(t, result.Details, "CITY-2")
	if !strings.Contains(workDirDetail, "(gc.work_dir, work_dir)") {
		t.Fatalf("describe() must name both triggering keys for a work_dir finding, got:\n%s", workDirDetail)
	}
	if strings.Contains(workDirDetail, "session_name") {
		t.Fatalf("describe() must not name gc.session_name for a work_dir-only finding, got:\n%s", workDirDetail)
	}
}

// residueDetailFor returns the single detail line naming beadID, failing the
// test if zero or more than one line matches.
func residueDetailFor(t *testing.T, details []string, beadID string) string {
	t.Helper()
	var match string
	count := 0
	for _, d := range details {
		if strings.Contains(d, beadID) {
			match = d
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 detail line for bead %s, got %d:\n%s", beadID, count, strings.Join(details, "\n"))
	}
	return match
}

type residueSetMetadataBatchSpyStore struct {
	beads.Store
	calls int
}

func (s *residueSetMetadataBatchSpyStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.calls++
	return s.Store.SetMetadataBatch(id, kvs)
}

// Round 2 (ga-p5eymu, amended ruling ga-6af29d decision 1) test cases below.

func TestExecutorIdentityResidueCheckSkipsOpenBeadWithEmptyRoutedTo(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "detached handoff orphan", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.session_name": "gascity--builder",
			"gc.work_branch":  "builder/ga-abc123",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (empty gc.routed_to is the ordinary post-claim/detached-orphan state, never residue): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckDistinguishesLegitimatePoolInstanceFromStaleReroute(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "SESSION-1", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "gascity--builder-2",
			"template":     "gascity/builder",
		}},
		{ID: "CITY-1", Title: "legitimate pool instance", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder-2",
		}},
		{ID: "CITY-2", Title: "stale re-route", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--deployer",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (CITY-2 is a genuine re-route hazard): %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if strings.Contains(details, "CITY-1") {
		t.Fatalf("CITY-1 carries a legitimate pool-instance identity (session bead SESSION-1 records gascity--builder-2 as a real member of gascity/builder's route) and must not be flagged:\n%s", details)
	}
	if !strings.Contains(details, "CITY-2") {
		t.Fatalf("CITY-2's session name matches no session bead and no route encoding; it must still be flagged as a true positive:\n%s", details)
	}
}

func TestExecutorIdentityResidueCheckScansWithLiveOpenQuery(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	store := &residueListQuerySpyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "warrant", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder",
		}},
	}, nil)}

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok: %#v", result.Status, result)
	}

	found := false
	for _, q := range store.queries {
		if q.Status == "open" && q.Live {
			found = true
			if !q.AllowScan {
				t.Fatalf("live open scan query %+v must set AllowScan", q)
			}
			if q.IncludeClosed {
				t.Fatalf("live open scan query %+v should not also request IncludeClosed (Status=open already excludes closed; requesting both wastes the backing-store fetch)", q)
			}
		}
	}
	if !found {
		t.Fatalf("expected a Status=%q Live=true scan query (gc-4zb pattern: mapBdStatus folds bd's raw review/testing/blocked into \"open\", so only a Live query reaches the backing store's own raw --status=open filter that actually excludes them), got queries: %+v", "open", store.queries)
	}
}

func TestExecutorIdentityResidueCheckNeverFlagsClosedBead(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "closed with stale stamp", Type: "task", Status: "closed", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a stamp surviving close is documented behavior -- stampRunSessionIdentity keeps the completed-run->session link durable; closed beads are out of scope entirely): %#v", result.Status, result)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	bd, err := cityStore.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.session_name"] != "gascity--builder" {
		t.Fatalf("Fix must not clear a closed bead's stamp, got %+v", bd.Metadata)
	}
}

func TestExecutorIdentityResidueCheckFlagsLegacyCanonicalWorkDirDisagreement(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "work_dir disagreement, session_name current", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder",
			"gc.work_dir":     filepath.Join(cityDir, "worktrees/gascity/builder-1"),
			"work_dir":        filepath.Join(cityDir, "legacy/worktrees/gascity/builder-1"),
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (legacy work_dir and canonical gc.work_dir disagree -- the ga-6af29d 20-bead class -- independent of gc.session_name, which is current): %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "CITY-1") {
		t.Fatalf("details missing CITY-1:\n%s", details)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	bd, err := cityStore.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.work_dir"] != "" || bd.Metadata["work_dir"] != "" {
		t.Fatalf("Fix must clear the disagreeing work_dir keys, got %+v", bd.Metadata)
	}
	if bd.Metadata["gc.session_name"] != "gascity--builder" {
		t.Fatalf("Fix must not clear gc.session_name when only the work_dir trigger fired (it is current, not a separate finding), got %q", bd.Metadata["gc.session_name"])
	}
}

type residueListQuerySpyStore struct {
	beads.Store
	queries []beads.ListQuery
}

func (s *residueListQuerySpyStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, q)
	return s.Store.List(q)
}

// Round 3 (ga-dkfmdu) test cases below.

func TestExecutorIdentityResidueCheckPreservesWorkDirOnWorktreeOwningBead(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		residueRanSession("RAN-1", "gascity--builder-9"),
		{ID: "CITY-1", Title: "worktree-owning bead, retired-slot session_name", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":      "gascity/reviewer",
			"gc.session_name":   "gascity--builder-9",
			"gc.work_dir":       "/worktrees/gascity/builder-1",
			"work_dir":          "/legacy/worktrees/gascity/builder-1",
			"gc.worktree_root":  "/worktrees/gascity/builder-1",
			"gc.worktree_repo":  "gascity",
			"gc.worktree_owner": "builder-1",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (the retired-slot gc.session_name is a genuine independent finding): %#v", result.Status, result)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	bd, err := cityStore.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.work_dir"] != "/worktrees/gascity/builder-1" || bd.Metadata["work_dir"] != "/legacy/worktrees/gascity/builder-1" {
		t.Fatalf("Fix must not clear work_dir keys on a bead carrying worktree-ownership evidence (clearing them defeats worktreeSpecForBead's fail-closed legacy/canonical conflict check by erasing the evidence it inspects), got %+v", bd.Metadata)
	}
	if bd.Metadata["gc.session_name"] != "" {
		t.Fatalf("Fix must still clear the independently-stale gc.session_name, got %q", bd.Metadata["gc.session_name"])
	}
}

func TestExecutorIdentityResidueCheckDefersToPoolSlotWorkDirRepair(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	bd := beads.Bead{ID: "CITY-1", Title: "canonical clobbered with pool-slot label", Type: "task", Status: "open", Metadata: map[string]string{
		"gc.routed_to": "gascity/builder",
		"gc.work_dir":  ".gc/worktrees/gascity/builder-1",
		"work_dir":     "/legacy/worktrees/gascity/builder-1",
	}}
	if poolSlotWorkDirRepairFor(cfg, bd) == nil {
		t.Fatalf("fixture premise broken: bead must match poolSlotWorkDirRepairFor's precondition (canonical pool-slot-shaped, legacy differs and is not)")
	}

	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{bd}, nil)
	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a bead matching poolSlotWorkDirRepairFor's precondition is a repair candidate, not residue -- flagging it races the repair sweep and risks destroying the same evidence the repair exists to restore): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckAllowsAliasOnlySessionIdentity(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "SESSION-1", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"alias":    "mayor",
			"template": "gascity/mayor",
		}},
		{ID: "CITY-1", Title: "named session identity", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/mayor",
			"gc.session_name": "mayor",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (SESSION-1 is a named session identified by alias, not session_name -- sessionBeadIdentifier falls back to alias, and CITY-1's gc.session_name matches that legitimate identity for gascity/mayor): %#v", result.Status, result)
	}
}

// Round 4 (#6135 fix-merge) test cases below: session beads live in the
// session coordination class (the city store by default), never in a rig
// store, so a rig scope must resolve executor identities from there.

func TestExecutorIdentityResidueCheckHonorsCitySessionIdentityForRigScopePoolSlot(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Agents: residueTestAgents(), Rigs: []config.Rig{{Name: "gascity", Path: rigDir}}}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "SESSION-1", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "gascity--builder-2",
			"template":     "gascity/builder",
		}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RIG-1", Title: "legitimate pool instance in a rig", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder-2",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, residueStoreFactory(t, cityDir, cityStore, rigDir, rigStore))

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (SESSION-1 lives in the city store because session beads are session-class; a rig scope must still see gascity--builder-2 as a legitimate identity for gascity/builder): %#v", result.Status, result)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	bd, err := rigStore.Get("RIG-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.session_name"] != "gascity--builder-2" {
		t.Fatalf("Fix must not clear a legitimate pool-slot identity on a rig bead, got %q", bd.Metadata["gc.session_name"])
	}
}

func TestExecutorIdentityResidueCheckHonorsCityAliasSessionIdentityForRigScope(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Agents: residueTestAgents(), Rigs: []config.Rig{{Name: "gascity", Path: rigDir}}}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "SESSION-1", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"alias":    "mayor",
			"template": "gascity/mayor",
		}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RIG-1", Title: "named session identity in a rig", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/mayor",
			"gc.session_name": "mayor",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, residueStoreFactory(t, cityDir, cityStore, rigDir, rigStore))

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (an alias-only named session in the city store is a legitimate identity for a rig bead routed to gascity/mayor): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckHonorsLabelOnlySessionBeadIdentity(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "SESSION-1", Status: "open", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "gascity--builder-2",
			"template":     "gascity/builder",
		}},
		{ID: "CITY-1", Title: "pool instance", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder-2",
		}},
	}, nil)

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a crash/migration-damaged session bead keeps the gc:session label with an empty type; ListAllSessionBeads unions type and label, so its identity still counts): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckFixSkipsBeadClaimedSinceCollection(t *testing.T) {
	cityDir := t.TempDir()
	cfg := residueTestCity()
	cityStore := &residueClaimedOnGetSpyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "claimed between collect and write", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
		}},
	}, nil)}

	check := newResidueTestCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (the listed snapshot is still open): %#v", result.Status, result)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if cityStore.writes != 0 {
		t.Fatalf("expected zero writes when the live bead is in_progress at write time (a claim consumes gc.routed_to and puts the stamp back in service; clearing it would strip identity off work in flight), got %d", cityStore.writes)
	}
	bd, err := cityStore.Store.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.session_name"] != "gascity--builder" {
		t.Fatalf("Fix must leave the stamp on a bead claimed since collection, got %q", bd.Metadata["gc.session_name"])
	}
}

// residueStoreFactory returns a newStore func serving cityStore at cityDir and
// rigStore at rigDir, failing the test on any other path.
func residueStoreFactory(t *testing.T, cityDir string, cityStore beads.Store, rigDir string, rigStore beads.Store) func(string) (beads.Store, error) {
	t.Helper()
	return func(path string) (beads.Store, error) {
		switch path {
		case cityDir:
			return cityStore, nil
		case rigDir:
			return rigStore, nil
		}
		return nil, fmt.Errorf("unexpected store path %q", path)
	}
}

// residueClaimedOnGetSpyStore lists beads as open but returns them in_progress
// from Get, standing in for a worker that claimed the bead in the window
// between collection and the write.
type residueClaimedOnGetSpyStore struct {
	beads.Store
	writes int
}

func (s *residueClaimedOnGetSpyStore) Get(id string) (beads.Bead, error) {
	bd, err := s.Store.Get(id)
	if err != nil {
		return bd, err
	}
	bd.Status = "in_progress"
	return bd, nil
}

func (s *residueClaimedOnGetSpyStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.writes++
	return s.Store.SetMetadataBatch(id, kvs)
}

// residueTestAgents configures the local routes the fixtures in this file
// use. Rule 1 (ga-n2f1ph) judges only beads routed to a route this city
// configures, so a fixture that means "a local bead" must configure it.
func residueTestAgents() []config.Agent {
	return []config.Agent{
		{Dir: "gascity", Name: "builder"},
		{Dir: "gascity", Name: "reviewer"},
		{Dir: "gascity", Name: "deployer"},
		{Dir: "gascity", Name: "mayor"},
	}
}

func residueTestCity() *config.City {
	return &config.City{Agents: residueTestAgents()}
}

// residueRanSession is a closed session bead showing this city once ran
// sessionName. Rule 4 clears a stale gc.session_name only when this city has
// run that session; without one, the stamp could be another town's live
// session under a route both towns configure.
func residueRanSession(id, sessionName string) beads.Bead {
	return beads.Bead{ID: id, Type: sessionBeadType, Status: "closed", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"session_name": sessionName,
		"template":     "qcore/elsewhere",
	}}
}

// newResidueTestCheck builds the check with a stat that reports every path
// absent, so no fixture touches the real filesystem (rule 3). Tests that
// exercise rule 3 replace statPath themselves.
func newResidueTestCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *executorIdentityResidueCheck {
	check := newExecutorIdentityResidueCheck(cfg, cityPath, newStore)
	check.statPath = residueFakeStat(nil)
	return check
}

// residueFakeStat returns a stat func answering from results: a path mapped
// to nil exists, a path mapped to an error returns it, and an unmapped path
// does not exist.
func residueFakeStat(results map[string]error) func(string) (os.FileInfo, error) {
	return func(path string) (os.FileInfo, error) {
		if err, ok := results[path]; ok {
			return nil, err
		}
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
}

// residueSingleStoreFactory serves store at cityDir only.
func residueSingleStoreFactory(t *testing.T, cityDir string, store beads.Store) func(string) (beads.Store, error) {
	t.Helper()
	return func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}
}

// residueRecordingStore records every SetMetadataBatch write.
type residueRecordingStore struct {
	beads.Store
	writes []map[string]string
}

func (s *residueRecordingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	cp := make(map[string]string, len(kvs))
	for k, v := range kvs {
		cp[k] = v
	}
	s.writes = append(s.writes, cp)
	return s.Store.SetMetadataBatch(id, kvs)
}

// Round 5 (ga-n2f1ph item 2): the 2026-09-22 21:08Z --fix run on the shared
// qcore store blanked other towns' stamps, a cert-wait park held by a
// running session, a local bead held by a live session, and work_dir
// pointers to worktrees still on disk.

func TestExecutorIdentityResidueCheckSparesStampOfOpenSession(t *testing.T) {
	cases := []struct {
		name        string
		route       string
		sessionName string
		// sessionStatus is the status of a session bead whose runtime name is
		// sessionName; "" means no such session bead exists.
		sessionStatus  string
		wantFlagged    bool
		wantReportOnly bool
	}{
		{name: "local route, open session", route: "gascity/reviewer", sessionName: "qcore--mallory", sessionStatus: "open"},
		{name: "pseudo-route cert-wait, open session", route: "cert-wait", sessionName: "qcore--ray", sessionStatus: "open"},
		{name: "foreign route, open session", route: "qcore/rock", sessionName: "qcore--rock", sessionStatus: "open"},
		// Controls: rule 2 is what spares the local-route row, not something
		// else about the fixture.
		// #123 review round 2: a name no session bead of this city ever held
		// is likely another town's live session under a route both towns
		// configure. It is reported, never cleared.
		{name: "local route, no session bead", route: "gascity/reviewer", sessionName: "qcore--mallory", wantFlagged: true, wantReportOnly: true},
		{name: "local route, session bead closed", route: "gascity/reviewer", sessionName: "qcore--mallory", sessionStatus: "closed", wantFlagged: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			rows := []beads.Bead{
				{ID: "CITY-1", Title: "held stamp", Type: "task", Status: "open", Metadata: map[string]string{
					"gc.routed_to":    tc.route,
					"gc.session_name": tc.sessionName,
				}},
			}
			if tc.sessionStatus != "" {
				rows = append(rows, beads.Bead{ID: "SESSION-1", Type: sessionBeadType, Status: tc.sessionStatus, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
					"session_name": tc.sessionName,
					"template":     "qcore/elsewhere",
				}})
			}
			store := &residueRecordingStore{Store: beads.NewMemStoreFrom(0, rows, nil)}
			check := newResidueTestCheck(residueTestCity(), cityDir, residueSingleStoreFactory(t, cityDir, store))

			result := check.Run(&doctor.CheckContext{})
			flagged := strings.Contains(strings.Join(result.Details, "\n"), "CITY-1")
			if flagged != tc.wantFlagged {
				t.Fatalf("flagged = %v, want %v: %#v", flagged, tc.wantFlagged, result)
			}
			if err := check.Fix(&doctor.CheckContext{}); err != nil {
				t.Fatalf("Fix returned error: %v", err)
			}
			bd, err := store.Get("CITY-1")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if reportOnly := strings.Contains(strings.Join(result.Details, "\n"), "REPORTED ONLY"); reportOnly != tc.wantReportOnly {
				t.Fatalf("reported only = %v, want %v: %#v", reportOnly, tc.wantReportOnly, result)
			}
			want := tc.sessionName
			if tc.wantFlagged && !tc.wantReportOnly {
				want = ""
			}
			if got := bd.Metadata["gc.session_name"]; got != want {
				t.Fatalf("gc.session_name after Fix = %q, want %q", got, want)
			}
		})
	}
}

func TestExecutorIdentityResidueCheckSkipsBeadsNotRoutedToALocalAgent(t *testing.T) {
	cases := []struct {
		name  string
		route string
	}{
		{name: "foreign pool", route: "qcore/pool.womp"},
		{name: "foreign crew seat", route: "qcore/crew-kaladin.seat"},
		{name: "foreign named", route: "qcore/dalinar"},
		{name: "foreign rock", route: "qcore/rock"},
		{name: "pseudo-route cert-wait", route: "cert-wait"},
		{name: "pseudo-route human", route: "human"},
		{name: "pseudo-route admission", route: "admission"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			// Both triggers would fire on a local bead: no open session holds
			// the stamp, and the disagreeing work_dir paths are absent.
			store := &residueRecordingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
				{ID: "CITY-1", Title: "not ours to judge", Type: "task", Status: "open", Metadata: map[string]string{
					"gc.routed_to":    tc.route,
					"gc.session_name": "qcore--someone-else",
					"gc.work_dir":     "/worktrees/qcore/other-1",
					"work_dir":        "/legacy/worktrees/qcore/other-1",
				}},
			}, nil)}
			check := newResidueTestCheck(residueTestCity(), cityDir, residueSingleStoreFactory(t, cityDir, store))

			result := check.Run(&doctor.CheckContext{})
			if result.Status != doctor.StatusOK {
				t.Fatalf("status = %v, want ok (route %q is not configured in this city; its stamps cannot be judged against local config): %#v", result.Status, tc.route, result)
			}
			if err := check.Fix(&doctor.CheckContext{}); err != nil {
				t.Fatalf("Fix returned error: %v", err)
			}
			if len(store.writes) != 0 {
				t.Fatalf("Fix wrote %v to a bead routed to %q", store.writes, tc.route)
			}
		})
	}
}

func TestExecutorIdentityResidueCheckStandsDownOnWorkDirThatExists(t *testing.T) {
	// Paths are relative to the city dir unless a case gives its own. Rule 4
	// (#123 review): a pair this machine cannot prove local -- an empty
	// route, a path outside the city's roots, a ~ spelling -- is reported
	// but never cleared, because a stat here says nothing about a worktree
	// on another town's machine.
	const canonical = "worktrees/gascity/builder-1"
	const legacy = "legacy/worktrees/gascity/builder-1"
	cases := []struct {
		name                     string
		route                    string
		canonical, legacy        string // absolute or ~ overrides; "" = under the city dir
		stat                     map[string]string
		wantFlagged, wantCleared bool
		wantReportOnly           bool
	}{
		{name: "legacy exists", route: "gascity/builder", stat: map[string]string{legacy: "exists"}},
		{name: "canonical exists", route: "gascity/builder", stat: map[string]string{canonical: "exists"}},
		{name: "legacy stat permission error", route: "gascity/builder", stat: map[string]string{legacy: "denied"}},
		{name: "empty route, legacy exists", route: "", stat: map[string]string{legacy: "exists"}},
		{name: "both absent", route: "gascity/builder", wantFlagged: true, wantCleared: true},
		{name: "empty route, both absent", route: "", wantFlagged: true, wantReportOnly: true},
		{name: "another machine's paths", route: "gascity/builder", canonical: "/Users/alex/worktrees/gascity/builder-1", legacy: "/Users/alex/legacy/builder-1", wantFlagged: true, wantReportOnly: true},
		{name: "tilde path", route: "gascity/builder", legacy: "~/legacy/builder-1", wantFlagged: true, wantReportOnly: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			resolve := func(override, rel string) string {
				if override != "" {
					return override
				}
				return filepath.Join(cityDir, rel)
			}
			canonicalPath, legacyPath := resolve(tc.canonical, canonical), resolve(tc.legacy, legacy)
			stat := map[string]error{}
			for rel, outcome := range tc.stat {
				err := error(nil)
				if outcome == "denied" {
					err = fs.ErrPermission
				}
				stat[filepath.Join(cityDir, rel)] = err
			}
			md := map[string]string{
				"gc.work_dir": canonicalPath,
				"work_dir":    legacyPath,
			}
			if tc.route != "" {
				md["gc.routed_to"] = tc.route
			}
			store := &residueRecordingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
				{ID: "CITY-1", Title: "work_dir disagreement", Type: "task", Status: "open", Metadata: md},
			}, nil)}
			check := newResidueTestCheck(residueTestCity(), cityDir, residueSingleStoreFactory(t, cityDir, store))
			check.statPath = residueFakeStat(stat)

			result := check.Run(&doctor.CheckContext{})
			details := strings.Join(result.Details, "\n")
			if flagged := strings.Contains(details, "CITY-1"); flagged != tc.wantFlagged {
				t.Fatalf("flagged = %v, want %v: %#v", flagged, tc.wantFlagged, result)
			}
			if reportOnly := strings.Contains(details, "REPORTED ONLY"); reportOnly != tc.wantReportOnly {
				t.Fatalf("reported only = %v, want %v: %#v", reportOnly, tc.wantReportOnly, result)
			}
			if err := check.Fix(&doctor.CheckContext{}); err != nil {
				t.Fatalf("Fix returned error: %v", err)
			}
			bd, err := store.Get("CITY-1")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			cleared := bd.Metadata["gc.work_dir"] == "" && bd.Metadata["work_dir"] == ""
			if cleared != tc.wantCleared {
				t.Fatalf("work_dir keys cleared = %v, want %v (metadata %+v)", cleared, tc.wantCleared, bd.Metadata)
			}
			if !tc.wantCleared && len(store.writes) != 0 {
				t.Fatalf("Fix wrote %v, want no write", store.writes)
			}
		})
	}
}

func TestExecutorIdentityResidueJudgePathProvablyLocal(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	judge := &executorIdentityResidueJudge{
		cfg:      &config.City{Rigs: []config.Rig{{Name: "qcore", Path: rigDir}, {Name: "rel", Path: "rigs/rel"}}},
		cityPath: cityDir,
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{filepath.Join(cityDir, ".gc/worktrees/x"), true},
		{filepath.Join(rigDir, ".worktrees/x"), true},
		{filepath.Join(cityDir, "rigs/rel/x"), true},
		{cityDir, true},
		{filepath.Join(cityDir, "../elsewhere/x"), false},
		{cityDir + "-sibling/x", false},
		{"/Users/alex/worktrees/x", false},
		{"~/worktrees/x", false},
		{"$HOME/worktrees/x", false},
		{"worktrees/x", false},
	} {
		if got := judge.pathProvablyLocal(tc.path); got != tc.want {
			t.Errorf("pathProvablyLocal(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestExecutorIdentityResidueCheckDefaultStatSeesRealDirectory(t *testing.T) {
	cityDir := t.TempDir()
	legacy := filepath.Join(cityDir, "legacy-worktree")
	if err := os.Mkdir(legacy, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "legacy worktree on disk", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to": "gascity/builder",
			"gc.work_dir":  filepath.Join(cityDir, "gone"),
			"work_dir":     legacy,
		}},
	}, nil)
	// The production constructor, so the default stat is the one exercised.
	check := newExecutorIdentityResidueCheck(residueTestCity(), cityDir, residueSingleStoreFactory(t, cityDir, store))

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (the legacy work_dir exists on disk and may be the only pointer to unpushed work): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckFailsClosedWhenOpenSessionsUnavailable(t *testing.T) {
	cityDir := t.TempDir()
	store := &residueRecordingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		residueRanSession("RAN-1", "gascity--builder"),
		residueRanSession("RAN-2", "gascity--deployer"),
		{ID: "CITY-1", Title: "session_name candidate", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
		}},
		{ID: "CITY-2", Title: "second session_name candidate, plus work_dir residue", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--deployer",
			"gc.work_dir":     filepath.Join(cityDir, "worktrees/gascity/deployer-1"),
			"work_dir":        filepath.Join(cityDir, "legacy/worktrees/gascity/deployer-1"),
		}},
	}, nil)}
	check := newResidueTestCheck(residueTestCity(), cityDir, residueSingleStoreFactory(t, cityDir, store))
	loads := 0
	check.loadOpenSessionNames = func(beads.Store) (map[string]struct{}, error) {
		loads++
		return nil, errors.New("listing session beads: dolt unreachable")
	}

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	if loads != 1 {
		t.Fatalf("open-session loader called %d times in one Run, want exactly 1", loads)
	}
	if !strings.Contains(result.Message, "unconfirmed") {
		t.Fatalf("message must say session_name findings are unconfirmed, got %q", result.Message)
	}
	d1 := residueDetailFor(t, result.Details, "CITY-1")
	if !strings.Contains(d1, "UNCONFIRMED") || strings.Contains(d1, "stamp residue (") {
		t.Fatalf("CITY-1 must be reported unconfirmed, never as fixable residue, got:\n%s", d1)
	}
	d2 := residueDetailFor(t, result.Details, "CITY-2")
	if !strings.Contains(d2, "(gc.work_dir, work_dir)") || !strings.Contains(d2, "UNCONFIRMED") {
		t.Fatalf("CITY-2 must report its confirmed work_dir keys and its unconfirmed gc.session_name, got:\n%s", d2)
	}

	err := check.Fix(&doctor.CheckContext{})
	if err == nil || !strings.Contains(err.Error(), "unconfirmed") {
		t.Fatalf("Fix must report the uncleared unconfirmed stamps, got %v", err)
	}
	for _, w := range store.writes {
		if _, ok := w[beadmeta.SessionNameMetadataKey]; ok {
			t.Fatalf("Fix cleared gc.session_name while open sessions were unavailable: %v", w)
		}
	}
	for id, want := range map[string]string{"CITY-1": "gascity--builder", "CITY-2": "gascity--deployer"} {
		bd, getErr := store.Get(id)
		if getErr != nil {
			t.Fatalf("Get %s: %v", id, getErr)
		}
		if bd.Metadata["gc.session_name"] != want {
			t.Fatalf("%s gc.session_name = %q, want %q kept", id, bd.Metadata["gc.session_name"], want)
		}
	}
	bd, getErr := store.Get("CITY-2")
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if bd.Metadata["gc.work_dir"] != "" || bd.Metadata["work_dir"] != "" {
		t.Fatalf("Fix must still clear CITY-2's confirmed work_dir keys, got %+v", bd.Metadata)
	}
}

func TestExecutorIdentityResidueCheckLoadsOpenSessionsOnlyForACandidate(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		residueRanSession("RAN-1", "qcore--rock"),
		{ID: "CITY-1", Title: "canonical stamp", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder",
		}},
		{ID: "CITY-2", Title: "foreign stamp", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "qcore/rock",
			"gc.session_name": "qcore--rock",
		}},
	}, nil)
	check := newResidueTestCheck(residueTestCity(), cityDir, residueSingleStoreFactory(t, cityDir, store))
	loads := 0
	check.loadOpenSessionNames = func(beads.Store) (map[string]struct{}, error) {
		loads++
		return nil, errors.New("must not be called")
	}

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok: %#v", result.Status, result)
	}
	if loads != 0 {
		t.Fatalf("open-session loader called %d times with no session_name candidate, want 0", loads)
	}
}

func TestExecutorIdentityResidueCheckFixClearsExactlyAGenuineStaleStampsKeys(t *testing.T) {
	cityDir := t.TempDir()
	store := &residueRecordingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		residueRanSession("RAN-1", "gascity--builder-7"),
		{ID: "SESSION-1", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "gascity--mayor",
			"template":     "gascity/mayor",
		}},
		{ID: "CITY-1", Title: "genuinely stale", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder-7",
			"gc.work_dir":     filepath.Join(cityDir, "worktrees/gascity/builder-7"),
			"work_dir":        filepath.Join(cityDir, "legacy/worktrees/gascity/builder-7"),
			"gc.work_branch":  "builder/ga-abc123",
		}},
	}, nil)}
	check := newResidueTestCheck(residueTestCity(), cityDir, residueSingleStoreFactory(t, cityDir, store))

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	detail := residueDetailFor(t, result.Details, "CITY-1")
	if !strings.Contains(detail, "(gc.work_dir, work_dir, gc.session_name)") {
		t.Fatalf("detail must name exactly the three triggering keys, got:\n%s", detail)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if len(store.writes) != 1 {
		t.Fatalf("expected exactly 1 write, got %d: %v", len(store.writes), store.writes)
	}
	wantKeys := map[string]bool{"gc.session_name": true, "gc.work_dir": true, "work_dir": true}
	if len(store.writes[0]) != len(wantKeys) {
		t.Fatalf("Fix wrote %v, want exactly the keys %v", store.writes[0], wantKeys)
	}
	for k, v := range store.writes[0] {
		if !wantKeys[k] || v != "" {
			t.Fatalf("Fix wrote %v, want exactly the keys %v cleared", store.writes[0], wantKeys)
		}
	}
	bd, err := store.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.routed_to"] != "gascity/reviewer" || bd.Metadata["gc.work_branch"] != "builder/ga-abc123" {
		t.Fatalf("Fix must not touch keys no trigger named, got %+v", bd.Metadata)
	}
}
