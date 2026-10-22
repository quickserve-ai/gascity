package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSkillRejectsTopicMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "work"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("gc skill work should fail")
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr = %q, want 'unknown subcommand'", stderr.String())
	}
}

func TestSkillListCityCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, cityDir, "skills/code-review/SKILL.md", "city skill")

	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "list"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skill list exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"NAME", "code-review", "city", "skills/code-review/SKILL.md"} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill list output missing %q:\n%s", want, out)
		}
	}
}

func TestSkillListJSON(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, cityDir, "skills/code-review/SKILL.md", "city skill")

	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "list", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skill list --json exited %d: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	var payload struct {
		SchemaVersion string `json:"schema_version"`
		Count         int    `json:"count"`
		Entries       []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
			Path   string `json:"path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.SchemaVersion != "1" || payload.Count == 0 || len(payload.Entries) == 0 {
		t.Fatalf("payload = %+v", payload)
	}
	found := false
	for _, got := range payload.Entries {
		if got.Name == "code-review" && got.Source == "city" && got.Path == "skills/code-review/SKILL.md" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("city skill missing from %+v", payload.Entries)
	}
}

func TestSkillListAgentCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, cityDir, "skills/code-review/SKILL.md", "city skill")
	writeCatalogFile(t, cityDir, "agents/mayor/skills/private-workflow/SKILL.md", "agent skill")

	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "list", "--agent", "mayor"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skill list --agent exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"code-review", "city", "private-workflow", "agent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill list --agent output missing %q:\n%s", want, out)
		}
	}
}

func TestSkillListImportedSharedCatalog(t *testing.T) {
	clearGCEnv(t)
	rootDir := t.TempDir()
	cityDir := filepath.Join(rootDir, "city")
	packDir := filepath.Join(rootDir, "helper")
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, packDir, "pack.toml", "[pack]\nname = \"helper\"\nversion = \"0.1.0\"\nschema = 2\n")
	writeCatalogFile(t, packDir, "skills/code-review/SKILL.md", "imported skill")
	writeCatalogFile(t, cityDir, "pack.toml", "[pack]\nname = \"city\"\nversion = \"0.1.0\"\nschema = 2\n\n[imports.helper]\nsource = \"../helper\"\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "list"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skill list exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"helper.code-review", "helper"} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill list output missing %q:\n%s", want, out)
		}
	}
}

func TestSkillListAgentCityScopedDirMatchingRigDoesNotShowRigSharedSkills(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "fe")
	rigSkills := filepath.Join(cityDir, "imports", "helper", "skills")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCatalogFile(t, cityDir, "imports/helper/skills/plan/SKILL.md", "rig-import skill")

	cfg := &config.City{
		Rigs: []config.Rig{{Name: "fe", Path: rigDir}},
		RigPackSkills: map[string][]config.DiscoveredSkillCatalog{
			"fe": {{
				SourceDir:   rigSkills,
				BindingName: "helper",
				PackName:    "helper",
			}},
		},
		Agents: []config.Agent{
			{Name: "mayor", Scope: "city", Dir: "fe"},
		},
	}

	entries, err := listVisibleSkillEntries(cityDir, cfg, nil, "mayor", "")
	if err != nil {
		t.Fatalf("listVisibleSkillEntries: %v", err)
	}
	for _, entry := range entries {
		if entry.Name == "helper.plan" {
			t.Fatalf("city-scoped agent should not list rig-shared skill: %+v", entries)
		}
	}
}

func TestSkillListSessionCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, cityDir, "skills/code-review/SKILL.md", "city skill")
	writeCatalogFile(t, cityDir, "agents/mayor/skills/private-workflow/SKILL.md", "agent skill")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "mayor session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":     "mayor",
			"session_name": "s-mayor-1",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "list", "--session", bead.ID}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skill list --session exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"code-review", "city", "private-workflow", "agent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("skill list --session output missing %q:\n%s", want, out)
		}
	}
}

// TestSkillListAgentShowsFullCityCatalog verifies that an agent-scoped
// `gc skill list --agent mayor` returns the entire city catalog plus the
// agent's private skills. Per engdocs/proposals/skill-materialization.md
// there is no attachment filtering — every agent sees every city skill.
// The `skills = [...]` tombstone on the agent is accepted but ignored.
func TestSkillListAgentShowsFullCityCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	// mayor declares an attachment list — this is a v0.15.0 tombstone and
	// must be ignored; other-skill should still appear in the agent's view.
	writeCatalogFile(t, cityDir, "agents/mayor/agent.toml", "provider = \"codex\"\nstart_command = \"echo\"\nskills = [\"attached-skill\"]\n")
	writeCatalogFile(t, cityDir, "skills/attached-skill/SKILL.md", "attached")
	writeCatalogFile(t, cityDir, "skills/other-skill/SKILL.md", "other")
	writeCatalogFile(t, cityDir, "agents/mayor/skills/private-workflow/SKILL.md", "agent-local")

	var stdout, stderr bytes.Buffer
	code := run([]string{"skill", "list", "--agent", "mayor"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skill list --agent mayor exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "attached-skill") {
		t.Errorf("attached-skill missing from output:\n%s", out)
	}
	if !strings.Contains(out, "private-workflow") {
		t.Errorf("agent-local private-workflow missing from output:\n%s", out)
	}
	if !strings.Contains(out, "other-skill") {
		t.Errorf("other-skill must remain visible — no attachment filtering:\n%s", out)
	}
}

func writeCatalogFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// writeOptInSkillListCity writes the bead's fixture city: an imported pack
// account-fleet with one default skill and one opt-in skill, a town-level
// mayor opted in through [[patches.agent]] opt_in_skills_append, a
// crew-style agent with its own work_dir, and a pool agent.
func writeOptInSkillListCity(t *testing.T) (cityDir, fleetDir string) {
	t.Helper()
	clearGCEnv(t)
	rootDir := t.TempDir()
	cityDir = filepath.Join(rootDir, "city")
	fleetDir = filepath.Join(rootDir, "account-fleet")
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, fleetDir, "pack.toml", "[pack]\nname = \"account-fleet\"\nversion = \"0.1.0\"\nschema = 2\n")
	writeCatalogFile(t, fleetDir, "skills/fleet-status/SKILL.md", "default skill")
	writeCatalogFile(t, fleetDir, "skills/opt-in/fleet-login/SKILL.md", "opt-in skill")
	writeCatalogFile(t, cityDir, "pack.toml", "[pack]\nname = \"city\"\nversion = \"0.1.0\"\nschema = 2\n\n[imports.account-fleet]\nsource = \"../account-fleet\"\n")
	writeCatalogFile(t, cityDir, "agents/crew/agent.toml", "scope = \"city\"\nprovider = \"claude\"\nstart_command = \"echo\"\nwork_dir = \".gc/agents/crew\"\n")
	writeCatalogFile(t, cityDir, "agents/polecat/agent.toml", "scope = \"city\"\nprovider = \"claude\"\nstart_command = \"echo\"\nwork_dir = \".gc/agents/{{.AgentBase}}\"\nmax_active_sessions = 3\n")
	writeCatalogFile(t, cityDir, "city.toml", `[workspace]

[beads]
provider = "file"

[[patches.agent]]
name = "mayor"
opt_in_skills_append = ["account-fleet.fleet-login"]
`)
	return cityDir, fleetDir
}

func skillListEntries(t *testing.T, args ...string) []visibilityEntry {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"skill", "list", "--json"}, args...), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc skill list %v exited %d: %s", args, code, stderr.String())
	}
	var payload skillListJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	return payload.Entries
}

func countSkillListName(entries []visibilityEntry, name string) int {
	n := 0
	for _, e := range entries {
		if e.Name == name {
			n++
		}
	}
	return n
}

func TestSkillListAgentListsAnOptedInSkillOnceForThatAgentOnly(t *testing.T) {
	_, fleetDir := writeOptInSkillListCity(t)

	mayor := skillListEntries(t, "--agent", "mayor")
	if got := countSkillListName(mayor, "account-fleet.fleet-login"); got != 1 {
		t.Fatalf("mayor lists account-fleet.fleet-login %d times, want once: %+v", got, mayor)
	}
	for _, e := range mayor {
		if e.Name != "account-fleet.fleet-login" {
			continue
		}
		wantPath := filepath.ToSlash(filepath.Join(fleetDir, "skills", "opt-in", "fleet-login", "SKILL.md"))
		if e.Source != "account-fleet" || e.Path != wantPath {
			t.Errorf("opted-in entry = %+v, want source account-fleet and path %s", e, wantPath)
		}
	}

	for _, agent := range []string{"crew", "polecat"} {
		entries := skillListEntries(t, "--agent", agent)
		if got := countSkillListName(entries, "account-fleet.fleet-login"); got != 0 {
			t.Errorf("%s lists the opt-in skill it did not select: %+v", agent, entries)
		}
		if got := countSkillListName(entries, "account-fleet.fleet-status"); got != 1 {
			t.Errorf("%s lists the default skill %d times, want once: %+v", agent, got, entries)
		}
	}

	city := skillListEntries(t)
	if got := countSkillListName(city, "account-fleet.fleet-status"); got != 1 {
		t.Errorf("city listing has the default skill %d times, want once: %+v", got, city)
	}
	if got := countSkillListName(city, "account-fleet.fleet-login"); got != 0 {
		t.Errorf("city listing shows the opt-in skill as if every agent had it: %+v", city)
	}
	if got := countSkillListName(mayor, "account-fleet.fleet-status"); got != 1 {
		t.Errorf("mayor lists the default skill %d times, want once: %+v", got, mayor)
	}
}
