package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestDiscoverPackSkills_PropagatesStatErrors(t *testing.T) {
	t.Parallel()

	fake := fsys.NewFake()
	packDir := "/packs/helper"
	skillsDir := filepath.Join(packDir, "skills")
	wantErr := errors.New("boom")
	fake.Errors[skillsDir] = wantErr

	_, err := DiscoverPackSkills(fake, packDir, "helper")
	if !errors.Is(err, wantErr) {
		t.Fatalf("DiscoverPackSkills() error = %v, want %v", err, wantErr)
	}
}

func TestDiscoverOptInSkills_NamesFollowTheSharedCatalog(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	citySkills := filepath.Join(dir, "city", "skills")
	fleetSkills := filepath.Join(dir, "fleet", "skills")
	opsSkills := filepath.Join(dir, "ops", "skills")
	writeTestFile(t, citySkills, "default-one/SKILL.md", "default")
	writeTestFile(t, citySkills, "opt-in/town-notes/SKILL.md", "city opt-in")
	writeTestFile(t, fleetSkills, "shared/SKILL.md", "default")
	writeTestFile(t, fleetSkills, "opt-in/login/SKILL.md", "fleet opt-in")
	writeTestFile(t, fleetSkills, "opt-in/no-skill-md/README.md", "not a skill")
	writeTestFile(t, opsSkills, "opt-in/login/SKILL.md", "ops opt-in")

	got, err := DiscoverOptInSkills(fsys.OSFS{}, citySkills, []DiscoveredSkillCatalog{
		{SourceDir: fleetSkills, BindingName: "fleet", PackName: "fleet"},
		{SourceDir: opsSkills, BindingName: "ops", PackName: "ops"},
	})
	if err != nil {
		t.Fatalf("DiscoverOptInSkills: %v", err)
	}
	want := []OptInSkill{
		{Name: "fleet.login", Dir: filepath.Join(fleetSkills, "opt-in", "login"), Origin: "fleet"},
		{Name: "ops.login", Dir: filepath.Join(opsSkills, "opt-in", "login"), Origin: "ops"},
		{Name: "town-notes", Dir: filepath.Join(citySkills, "opt-in", "town-notes"), Origin: "city"},
	}
	if len(got) != len(want) {
		t.Fatalf("DiscoverOptInSkills = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDiscoverOptInSkills_FirstSourceWinsPerName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rigSkills := filepath.Join(dir, "rig-ops", "skills")
	citySkills := filepath.Join(dir, "city-ops", "skills")
	writeTestFile(t, rigSkills, "opt-in/audit/SKILL.md", "rig copy")
	writeTestFile(t, citySkills, "opt-in/audit/SKILL.md", "city copy")

	got, err := DiscoverOptInSkills(fsys.OSFS{}, "", []DiscoveredSkillCatalog{
		{SourceDir: rigSkills, BindingName: "ops"},
		{SourceDir: citySkills, BindingName: "ops"},
	})
	if err != nil {
		t.Fatalf("DiscoverOptInSkills: %v", err)
	}
	if len(got) != 1 || got[0].Dir != filepath.Join(rigSkills, "opt-in", "audit") {
		t.Fatalf("DiscoverOptInSkills = %+v, want the rig copy of ops.audit only", got)
	}
}

func TestValidateOptInSkills_ResolvesNamesInTheAgentScope(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fleetSkills := filepath.Join(dir, "fleet", "skills")
	rigOpsSkills := filepath.Join(dir, "rig-ops", "skills")
	writeTestFile(t, fleetSkills, "opt-in/login/SKILL.md", "fleet opt-in")
	writeTestFile(t, fleetSkills, "shared/SKILL.md", "default")
	writeTestFile(t, rigOpsSkills, "opt-in/audit/SKILL.md", "rig opt-in")

	base := func(agents ...Agent) *City {
		return &City{
			Rigs:       []Rig{{Name: "fe", Path: filepath.Join(dir, "fe")}},
			PackSkills: []DiscoveredSkillCatalog{{SourceDir: fleetSkills, BindingName: "fleet"}},
			RigPackSkills: map[string][]DiscoveredSkillCatalog{
				"fe": {{SourceDir: rigOpsSkills, BindingName: "ops"}},
			},
			Agents: agents,
		}
	}

	tests := []struct {
		name    string
		agent   Agent
		wantErr []string
	}{
		{name: "town-level agent selects a city import's opt-in", agent: Agent{Name: "mayor", Scope: "city", OptInSkills: []string{"fleet.login"}}},
		{name: "rig agent selects its rig import's opt-in", agent: Agent{Name: "polecat", Dir: "fe", OptInSkills: []string{"ops.audit", "fleet.login"}}},
		{name: "no selection", agent: Agent{Name: "crew", Scope: "city"}},
		{
			name:    "unknown name",
			agent:   Agent{Name: "mayor", Scope: "city", OptInSkills: []string{"fleet.nope"}},
			wantErr: []string{`agent "mayor"`, "opt_in_skills", `"fleet.nope"`, "fleet.login"},
		},
		{
			name:    "a default skill is not an opt-in skill",
			agent:   Agent{Name: "mayor", Scope: "city", OptInSkills: []string{"fleet.shared"}},
			wantErr: []string{`agent "mayor"`, `"fleet.shared"`},
		},
		{
			name:    "a rig import's opt-in is out of a town-level agent's scope",
			agent:   Agent{Name: "mayor", Scope: "city", OptInSkills: []string{"ops.audit"}},
			wantErr: []string{`agent "mayor"`, `"ops.audit"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateOptInSkills(fsys.OSFS{}, base(tt.agent))
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ValidateOptInSkills: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateOptInSkills = nil, want an error naming %v", tt.wantErr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err.Error(), want)
				}
			}
		})
	}
}
