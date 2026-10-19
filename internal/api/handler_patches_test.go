package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// --- Agent patch tests ---

func TestHandleAgentPatchList(t *testing.T) {
	fs := newFakeState(t)
	suspended := true
	fs.cfg.Patches.Agents = []config.AgentPatch{
		{Dir: "rig1", Name: "worker", Suspended: &suspended},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/patches/agents"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp listResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
}

func TestHandleAgentPatchList_Empty(t *testing.T) {
	fs := newFakeState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/patches/agents"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp listResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0", resp.Total)
	}
}

func TestHandleAgentPatchGet(t *testing.T) {
	fs := newFakeState(t)
	suspended := true
	fs.cfg.Patches.Agents = []config.AgentPatch{
		{Dir: "rig1", Name: "worker", Suspended: &suspended},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/patches/agent/rig1/worker"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleAgentPatchGet_NotFound(t *testing.T) {
	fs := newFakeState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/patches/agent/rig1/nonexistent"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleAgentPatchSet(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"dir":"rig1","name":"worker","tmux_alias":"worker--{{.CityName}}","suspended":true}`
	req := httptest.NewRequest("PUT", cityURL(fs, "/patches/agents"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if len(fs.cfg.Patches.Agents) != 1 {
		t.Fatalf("patches.agent count = %d, want 1", len(fs.cfg.Patches.Agents))
	}
	if fs.cfg.Patches.Agents[0].Name != "worker" {
		t.Errorf("name = %q, want %q", fs.cfg.Patches.Agents[0].Name, "worker")
	}
	if fs.cfg.Patches.Agents[0].TmuxAlias == nil || *fs.cfg.Patches.Agents[0].TmuxAlias != "worker--{{.CityName}}" {
		t.Errorf("tmux alias = %v, want %q", fs.cfg.Patches.Agents[0].TmuxAlias, "worker--{{.CityName}}")
	}
}

// TestHandleAgentPatchSet_Provider verifies PUT /patches/agents wires the
// provider override through to the stored patch. This is the load-bearing
// "override a pack-imported agent's provider" path: config.AgentPatch supports
// Provider, but the HTTP input previously dropped it, making the PUT a silent
// no-op for provider. The stored patch must carry the provider, and a GET must
// surface it.
func TestHandleAgentPatchSet_Provider(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"dir":"rig1","name":"worker","provider":"claude-max"}`
	req := httptest.NewRequest("PUT", cityURL(fs, "/patches/agents"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if len(fs.cfg.Patches.Agents) != 1 {
		t.Fatalf("patches.agent count = %d, want 1", len(fs.cfg.Patches.Agents))
	}
	got := fs.cfg.Patches.Agents[0].Provider
	if got == nil {
		t.Fatal("patch Provider = nil; PUT dropped the provider override")
	}
	if *got != "claude-max" {
		t.Errorf("patch Provider = %q, want %q", *got, "claude-max")
	}

	// And it must round-trip through GET /patches/agent/{dir}/{base}.
	getReq := httptest.NewRequest("GET", cityURL(fs, "/patches/agent/rig1/worker"), nil)
	getW := httptest.NewRecorder()
	h.ServeHTTP(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body = %s", getW.Code, http.StatusOK, getW.Body.String())
	}
	var patch config.AgentPatch
	if err := json.NewDecoder(getW.Body).Decode(&patch); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if patch.Provider == nil || *patch.Provider != "claude-max" {
		t.Errorf("GET patch Provider = %v, want %q", patch.Provider, "claude-max")
	}
}

// TestHandleAgentPatchSet_OptInSkills verifies PUT /patches/agents carries
// the opt-in skill selection: opt_in_skills replaces the agent's list (an
// empty list clears it; leaving it out, or a null the decoder turns into
// nil, keeps it) and opt_in_skills_append adds to it.
func TestHandleAgentPatchSet_OptInSkills(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantReplace *[]string
		wantAppend  []string
	}{
		{name: "replace", body: `{"name":"mayor","opt_in_skills":["fleet.login"]}`, wantReplace: &[]string{"fleet.login"}},
		{name: "clear", body: `{"name":"mayor","opt_in_skills":[]}`, wantReplace: &[]string{}},
		{name: "append", body: `{"name":"mayor","opt_in_skills_append":["fleet.audit"]}`, wantAppend: []string{"fleet.audit"}},
		{name: "null leaves the list alone", body: `{"name":"mayor","opt_in_skills":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeMutatorState(t)
			h := newTestCityHandler(t, fs)
			req := httptest.NewRequest("PUT", cityURL(fs, "/patches/agents"), strings.NewReader(tt.body))
			req.Header.Set("X-GC-Request", "true")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
			}
			if len(fs.cfg.Patches.Agents) != 1 {
				t.Fatalf("patches.agent count = %d, want 1", len(fs.cfg.Patches.Agents))
			}
			got := fs.cfg.Patches.Agents[0]
			switch {
			case tt.wantReplace == nil && got.OptInSkills != nil:
				t.Errorf("OptInSkills = %v, want unset", *got.OptInSkills)
			case tt.wantReplace != nil && got.OptInSkills == nil:
				t.Errorf("OptInSkills unset, want %v", *tt.wantReplace)
			case tt.wantReplace != nil && !reflect.DeepEqual(*got.OptInSkills, *tt.wantReplace):
				t.Errorf("OptInSkills = %#v, want %#v", *got.OptInSkills, *tt.wantReplace)
			}
			if !reflect.DeepEqual(got.OptInSkillsAppend, tt.wantAppend) {
				t.Errorf("OptInSkillsAppend = %#v, want %#v", got.OptInSkillsAppend, tt.wantAppend)
			}
		})
	}
}

// TestHandleAgentPatchSet_Rig verifies PUT /patches/agents wires the new rig
// targeting key (including the "*" wildcard) through to the stored patch, and
// that such a patch round-trips through GET and DELETE addressed by its
// rig-qualified identity — rather than being silently stored or looked up as a
// city-scoped (dir="") patch. config.AgentPatch supports Rig, but the HTTP
// input previously dropped it and the lookup keyed on Dir only, so a
// rig-targeted patch was unreachable via the API.
func TestHandleAgentPatchSet_Rig(t *testing.T) {
	for _, tc := range []struct {
		name string
		rig  string
		path string // GET/DELETE path addressing the patch by its qualified identity
	}{
		{name: "specific rig", rig: "rigA", path: "/patches/agent/rigA/worker"},
		{name: "wildcard", rig: "*", path: "/patches/agent/*/worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeMutatorState(t)
			h := newTestCityHandler(t, fs)

			body := `{"rig":"` + tc.rig + `","name":"worker","provider":"claude-max"}`
			req := httptest.NewRequest("PUT", cityURL(fs, "/patches/agents"), strings.NewReader(body))
			req.Header.Set("X-GC-Request", "true")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("PUT status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
			}

			if len(fs.cfg.Patches.Agents) != 1 {
				t.Fatalf("patches.agent count = %d, want 1", len(fs.cfg.Patches.Agents))
			}
			stored := fs.cfg.Patches.Agents[0]
			if stored.Rig != tc.rig {
				t.Errorf("stored Rig = %q, want %q (PUT dropped the rig key)", stored.Rig, tc.rig)
			}
			if stored.Dir != "" {
				t.Errorf("stored Dir = %q, want empty (a rig patch must not fall back to a dir scope)", stored.Dir)
			}

			// It must round-trip through GET addressed by its rig-qualified identity.
			getReq := httptest.NewRequest("GET", cityURL(fs, tc.path), nil)
			getW := httptest.NewRecorder()
			h.ServeHTTP(getW, getReq)
			if getW.Code != http.StatusOK {
				t.Fatalf("GET status = %d, want %d; body = %s", getW.Code, http.StatusOK, getW.Body.String())
			}
			var patch config.AgentPatch
			if err := json.NewDecoder(getW.Body).Decode(&patch); err != nil {
				t.Fatalf("decode patch: %v", err)
			}
			if patch.Rig != tc.rig || patch.Name != "worker" {
				t.Errorf("GET patch = {Rig:%q Name:%q}, want {Rig:%q Name:%q}", patch.Rig, patch.Name, tc.rig, "worker")
			}

			// And DELETE by the same identity must remove it.
			delReq := httptest.NewRequest("DELETE", cityURL(fs, tc.path), nil)
			delReq.Header.Set("X-GC-Request", "true")
			delW := httptest.NewRecorder()
			h.ServeHTTP(delW, delReq)
			if delW.Code != http.StatusOK {
				t.Fatalf("DELETE status = %d, want %d; body = %s", delW.Code, http.StatusOK, delW.Body.String())
			}
			if len(fs.cfg.Patches.Agents) != 0 {
				t.Errorf("patches.agent count after delete = %d, want 0", len(fs.cfg.Patches.Agents))
			}
		})
	}
}

func TestHandleAgentPatchSet_MissingName(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"dir":"rig1","suspended":true}`
	req := httptest.NewRequest("PUT", cityURL(fs, "/patches/agents"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleAgentPatchSet_RejectsDirPlusRig verifies PUT /patches/agents
// rejects a body that sets both the legacy dir key and the new rig key
// (including the "*" wildcard) with HTTP 400. The combination is
// mutually-exclusive and would hard-fail the next config load, so the handler
// validates at the edge before persisting — mirroring the editor SetAgentPatch
// guard so the HTTP and CLI write paths reject the same invalid patch.
func TestHandleAgentPatchSet_RejectsDirPlusRig(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "dir and specific rig", body: `{"dir":"rigA","rig":"rigB","name":"worker"}`},
		{name: "dir and wildcard rig", body: `{"dir":"rigA","rig":"*","name":"worker"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeMutatorState(t)
			h := newTestCityHandler(t, fs)

			req := httptest.NewRequest("PUT", cityURL(fs, "/patches/agents"), strings.NewReader(tc.body))
			req.Header.Set("X-GC-Request", "true")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
			}
			if len(fs.cfg.Patches.Agents) != 0 {
				t.Errorf("patches.agent count = %d, want 0 (invalid patch must not be stored)", len(fs.cfg.Patches.Agents))
			}
		})
	}
}

func TestHandleAgentPatchDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	suspended := true
	fs.cfg.Patches.Agents = []config.AgentPatch{
		{Dir: "rig1", Name: "worker", Suspended: &suspended},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/patches/agent/rig1/worker"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if len(fs.cfg.Patches.Agents) != 0 {
		t.Errorf("patches.agent count = %d, want 0", len(fs.cfg.Patches.Agents))
	}
}

func TestHandleAgentPatchDelete_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/patches/agent/nonexistent"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// --- Rig patch tests ---

func TestHandleRigPatchList(t *testing.T) {
	fs := newFakeState(t)
	suspended := true
	fs.cfg.Patches.Rigs = []config.RigPatch{
		{Name: "myrig", Suspended: &suspended},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/patches/rigs"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp listResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
}

func TestHandleRigPatchSet(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"name":"myrig","default_branch":"develop","suspended":true}`
	req := httptest.NewRequest("PUT", cityURL(fs, "/patches/rigs"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if len(fs.cfg.Patches.Rigs) != 1 {
		t.Fatalf("patches.rigs count = %d, want 1", len(fs.cfg.Patches.Rigs))
	}
	if fs.cfg.Patches.Rigs[0].DefaultBranch == nil || *fs.cfg.Patches.Rigs[0].DefaultBranch != "develop" {
		t.Fatalf("DefaultBranch = %v, want develop", fs.cfg.Patches.Rigs[0].DefaultBranch)
	}
}

// A PUT replaces the complete patch, including the rig fields not present in
// the original HTTP input shape. Omitted fields on the next PUT are cleared.
// TestRigPatchSetBodyCarriesEveryRigPatchField keeps the body complete.
func TestHandleRigPatchSetFullReplacement(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)
	put := func(body string) {
		t.Helper()
		req := httptest.NewRequest("PUT", cityURL(fs, "/patches/rigs"), strings.NewReader(body))
		req.Header.Set("X-GC-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
	}

	put(`{"name":"myrig","default_branch":"develop","default_merge_strategy":"mr","formula_vars":{"test_command":"go test ./..."},"suspended_on_start":false,"beads_proxied_idle_timeout":"5m","doctor":{"census_owner_namespace":"platform"}}`)
	if len(fs.cfg.Patches.Rigs) != 1 {
		t.Fatalf("patches.rigs count = %d, want 1", len(fs.cfg.Patches.Rigs))
	}
	p := fs.cfg.Patches.Rigs[0]
	if p.FormulaVars["test_command"] != "go test ./..." || p.SuspendedOnStart == nil || *p.SuspendedOnStart ||
		p.Doctor == nil || p.Doctor.CensusOwnerNamespace == nil || *p.Doctor.CensusOwnerNamespace != "platform" ||
		p.DefaultMergeStrategy == nil || *p.DefaultMergeStrategy != "mr" ||
		p.BeadsProxiedIdleTimeout == nil || *p.BeadsProxiedIdleTimeout != "5m" {
		t.Fatalf("PUT lost rig patch fields: %+v", p)
	}
	req := httptest.NewRequest("GET", cityURL(fs, "/patches/rig/myrig"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var got config.RigPatch
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.FormulaVars["test_command"] != "go test ./..." || got.SuspendedOnStart == nil ||
		*got.SuspendedOnStart || got.Doctor == nil || got.Doctor.CensusOwnerNamespace == nil ||
		*got.Doctor.CensusOwnerNamespace != "platform" ||
		got.DefaultMergeStrategy == nil || *got.DefaultMergeStrategy != "mr" ||
		got.BeadsProxiedIdleTimeout == nil || *got.BeadsProxiedIdleTimeout != "5m" {
		t.Fatalf("GET lost rig patch fields: %+v", got)
	}

	put(`{"name":"myrig","default_branch":"main","formula_vars":{"build_command":"make build"},"suspended_on_start":true,"doctor":{"census_owner_namespace":""}}`)
	p = fs.cfg.Patches.Rigs[0]
	if len(p.FormulaVars) != 1 || p.FormulaVars["build_command"] != "make build" ||
		p.SuspendedOnStart == nil || !*p.SuspendedOnStart ||
		p.Doctor == nil || p.Doctor.CensusOwnerNamespace == nil || *p.Doctor.CensusOwnerNamespace != "" {
		t.Fatalf("PUT did not replace rig patch fields: %+v", p)
	}

	put(`{"name":"myrig","default_branch":"main"}`)
	p = fs.cfg.Patches.Rigs[0]
	if p.FormulaVars != nil || p.SuspendedOnStart != nil || p.Doctor != nil ||
		p.DefaultMergeStrategy != nil || p.BeadsProxiedIdleTimeout != nil {
		t.Fatalf("omitted fields survived replacement: %+v", p)
	}
}

// TestRigPatchSetBodyCarriesEveryRigPatchField: the PUT replaces the whole
// patch, so a RigPatch field the request body cannot carry is cleared on every
// write. Every config.RigPatch field, nested ones included, must have a body
// field with the same wire name and type, and every body field must have a
// RigPatch field to land in.
func TestRigPatchSetBodyCarriesEveryRigPatchField(t *testing.T) {
	checkRigPatchWireShape(t, "", reflect.TypeOf(RigPatchSetInput{}.Body), reflect.TypeOf(config.RigPatch{}))
}

// A PUT stores every body field, nested ones included, a second PUT replaces
// every one of them, and a PUT that omits them clears them. The body is filled
// by reflection, so a field added to both types is driven through the handler
// without editing this test.
func TestHandleRigPatchSetStoresEveryBodyField(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)
	putAndCheck := func(body any) {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("PUT", cityURL(fs, "/patches/rigs"), strings.NewReader(string(data)))
		req.Header.Set("X-GC-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s: status = %d, want 200; body = %s", data, w.Code, w.Body.String())
		}
		if len(fs.cfg.Patches.Rigs) != 1 {
			t.Fatalf("patches.rigs count = %d, want 1", len(fs.cfg.Patches.Rigs))
		}
		checkRigPatchStored(t, "", reflect.ValueOf(body).Elem(), reflect.ValueOf(fs.cfg.Patches.Rigs[0]))
	}
	filled := func(seed, strategy, timeout string) any {
		body := RigPatchSetInput{}.Body
		fillRigPatchBody(t, reflect.ValueOf(&body).Elem(), "", seed)
		// Values the handler validates.
		body.Name = "myrig"
		body.DefaultMergeStrategy = &strategy
		body.BeadsProxiedIdleTimeout = &timeout
		return &body
	}

	putAndCheck(filled("a", "mr", "5m"))
	putAndCheck(filled("b", "direct", "0"))
	omitted := RigPatchSetInput{}.Body
	omitted.Name = "myrig"
	putAndCheck(&omitted)
}

func TestHandleRigPatchSetRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "unknown merge strategy", body: `{"name":"myrig","default_merge_strategy":"pr"}`},
		{name: "idle timeout not a duration", body: `{"name":"myrig","beads_proxied_idle_timeout":"garbage"}`},
		{name: "idle timeout under the minimum", body: `{"name":"myrig","beads_proxied_idle_timeout":"30s"}`},
		{name: "idle timeout negative", body: `{"name":"myrig","beads_proxied_idle_timeout":"-5m"}`},
		{name: "idle timeout empty", body: `{"name":"myrig","beads_proxied_idle_timeout":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeMutatorState(t)
			branch := "develop"
			existing := config.RigPatch{Name: "myrig", DefaultBranch: &branch}
			fs.cfg.Patches.Rigs = []config.RigPatch{existing}
			h := newTestCityHandler(t, fs)

			req := httptest.NewRequest("PUT", cityURL(fs, "/patches/rigs"), strings.NewReader(tc.body))
			req.Header.Set("X-GC-Request", "true")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
			}
			if !reflect.DeepEqual(fs.cfg.Patches.Rigs, []config.RigPatch{existing}) {
				t.Errorf("patches.rigs = %+v, want the existing patch unchanged (invalid patch must not be stored)", fs.cfg.Patches.Rigs)
			}
		})
	}
}

func TestHandleRigPatchSetAcceptsValidValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		value string
	}{
		{name: "merge strategy cleared", field: "default_merge_strategy", value: ""},
		{name: "merge strategy direct", field: "default_merge_strategy", value: "direct"},
		{name: "merge strategy mr", field: "default_merge_strategy", value: "mr"},
		{name: "merge strategy local", field: "default_merge_strategy", value: "local"},
		{name: "idle timeout never", field: "beads_proxied_idle_timeout", value: "0"},
		{name: "idle timeout at the minimum", field: "beads_proxied_idle_timeout", value: "1m"},
		{name: "idle timeout above the minimum", field: "beads_proxied_idle_timeout", value: "90m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeMutatorState(t)
			h := newTestCityHandler(t, fs)

			body := `{"name":"myrig","` + tc.field + `":"` + tc.value + `"}`
			req := httptest.NewRequest("PUT", cityURL(fs, "/patches/rigs"), strings.NewReader(body))
			req.Header.Set("X-GC-Request", "true")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			if len(fs.cfg.Patches.Rigs) != 1 {
				t.Fatalf("patches.rigs count = %d, want 1", len(fs.cfg.Patches.Rigs))
			}
			p := fs.cfg.Patches.Rigs[0]
			got := p.DefaultMergeStrategy
			if tc.field == "beads_proxied_idle_timeout" {
				got = p.BeadsProxiedIdleTimeout
			}
			if got == nil || *got != tc.value {
				t.Errorf("stored %s = %v, want %q", tc.field, got, tc.value)
			}
		})
	}
}

// rigPatchWireFields maps the wire names of a struct's fields to the fields:
// the json name for the PUT body, the toml name for config.RigPatch (what the
// PUT writes to city.toml).
func rigPatchWireFields(t *testing.T, typ reflect.Type, tag string) map[string]reflect.StructField {
	t.Helper()
	fields := make(map[string]reflect.StructField, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get(tag), ",")
		if name == "" || name == "-" {
			t.Fatalf("%s.%s has no %s name", typ, f.Name, tag)
		}
		fields[name] = f
	}
	return fields
}

func joinWirePath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// checkRigPatchWireShape fails unless body and patch have the same wire
// fields with the same types, recursing into nested structs.
func checkRigPatchWireShape(t *testing.T, path string, body, patch reflect.Type) {
	t.Helper()
	if body.Kind() == reflect.Pointer && patch.Kind() == reflect.Pointer &&
		body.Elem().Kind() == reflect.Struct && patch.Elem().Kind() == reflect.Struct {
		checkRigPatchWireShape(t, path, body.Elem(), patch.Elem())
		return
	}
	if body.Kind() != reflect.Struct || patch.Kind() != reflect.Struct {
		if body != patch {
			t.Errorf("%s: PUT body type %s, config.RigPatch type %s", path, body, patch)
		}
		return
	}
	bodyFields := rigPatchWireFields(t, body, "json")
	patchFields := rigPatchWireFields(t, patch, "toml")
	for name, pf := range patchFields {
		bf, ok := bodyFields[name]
		if !ok {
			t.Errorf("%s (%s.%s) has no field in RigPatchSetInput.Body; a PUT clears it", joinWirePath(path, name), patch, pf.Name)
			continue
		}
		checkRigPatchWireShape(t, joinWirePath(path, name), bf.Type, pf.Type)
	}
	for name := range bodyFields {
		if _, ok := patchFields[name]; !ok {
			t.Errorf("RigPatchSetInput.Body field %s has no %s field to store it in", joinWirePath(path, name), patch)
		}
	}
}

// fillRigPatchBody sets every field under v to a non-zero value derived from
// seed, so two seeds give values that differ in every field. It fails on a
// kind it cannot fill, so a new field kind forces this filler to grow.
func fillRigPatchBody(t *testing.T, v reflect.Value, path, seed string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillRigPatchBody(t, v.Elem(), path, seed)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fillRigPatchBody(t, v.Field(i), joinWirePath(path, v.Type().Field(i).Name), seed)
		}
	case reflect.String:
		v.SetString(seed + "-" + path)
	case reflect.Bool:
		v.SetBool(seed == "a")
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String || v.Type().Elem().Kind() != reflect.String {
			t.Fatalf("%s: no filler for %s", path, v.Type())
		}
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(reflect.ValueOf(seed+"-key").Convert(v.Type().Key()), reflect.ValueOf(seed+"-"+path).Convert(v.Type().Elem()))
		v.Set(m)
	default:
		t.Fatalf("%s: no filler for kind %s", path, v.Kind())
	}
}

// checkRigPatchStored fails unless the stored patch holds exactly what the
// body sent, matched by wire name: a body field that was set is stored with
// its value, and one that was omitted is stored as unset.
func checkRigPatchStored(t *testing.T, path string, body, patch reflect.Value) {
	t.Helper()
	if body.Kind() == reflect.Pointer && patch.Kind() == reflect.Pointer {
		if body.IsNil() != patch.IsNil() {
			t.Errorf("%s: body set = %t, stored set = %t", path, !body.IsNil(), !patch.IsNil())
			return
		}
		if body.IsNil() {
			return
		}
		checkRigPatchStored(t, path, body.Elem(), patch.Elem())
		return
	}
	if body.Kind() == reflect.Struct && patch.Kind() == reflect.Struct {
		bodyFields := rigPatchWireFields(t, body.Type(), "json")
		patchFields := rigPatchWireFields(t, patch.Type(), "toml")
		for name, pf := range patchFields {
			bf, ok := bodyFields[name]
			if !ok {
				t.Errorf("%s: no body field", joinWirePath(path, name))
				continue
			}
			checkRigPatchStored(t, joinWirePath(path, name), body.FieldByIndex(bf.Index), patch.FieldByIndex(pf.Index))
		}
		return
	}
	if !reflect.DeepEqual(body.Interface(), patch.Interface()) {
		t.Errorf("%s: stored %#v, body sent %#v", path, patch.Interface(), body.Interface())
	}
}

func TestHandleRigPatchDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	suspended := true
	fs.cfg.Patches.Rigs = []config.RigPatch{
		{Name: "myrig", Suspended: &suspended},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/patches/rig/myrig"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if len(fs.cfg.Patches.Rigs) != 0 {
		t.Errorf("patches.rigs count = %d, want 0", len(fs.cfg.Patches.Rigs))
	}
}

// --- Provider patch tests ---

func TestHandleProviderPatchList(t *testing.T) {
	fs := newFakeState(t)
	cmd := "new-cmd"
	fs.cfg.Patches.Providers = []config.ProviderPatch{
		{Name: "claude", Command: &cmd},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/patches/providers"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp listResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
}

func TestHandleProviderPatchSet(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"name":"claude","command":"my-claude","acp_command":"my-claude-acp","acp_args":["serve","--stdio"],"accept_startup_dialogs":true}`
	req := httptest.NewRequest("PUT", cityURL(fs, "/patches/providers"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if len(fs.cfg.Patches.Providers) != 1 {
		t.Fatalf("patches.providers count = %d, want 1", len(fs.cfg.Patches.Providers))
	}
	if got := fs.cfg.Patches.Providers[0].ACPCommand; got == nil || *got != "my-claude-acp" {
		t.Fatalf("ACPCommand = %v, want %q", got, "my-claude-acp")
	}
	if got := fs.cfg.Patches.Providers[0].ACPArgs; len(got) != 2 || got[0] != "serve" || got[1] != "--stdio" {
		t.Fatalf("ACPArgs = %#v, want [\"serve\" \"--stdio\"]", got)
	}
	if got := fs.cfg.Patches.Providers[0].AcceptStartupDialogs; got == nil || !*got {
		t.Fatalf("AcceptStartupDialogs = %v, want true", got)
	}
}

func TestHandleProviderPatchDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	cmd := "my-claude"
	fs.cfg.Patches.Providers = []config.ProviderPatch{
		{Name: "claude", Command: &cmd},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/patches/provider/claude"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if len(fs.cfg.Patches.Providers) != 0 {
		t.Errorf("patches.providers count = %d, want 0", len(fs.cfg.Patches.Providers))
	}
}

func TestHandleProviderPatchDelete_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/patches/provider/nonexistent"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// Regression for ga-8hk1fe: config composition applies [patches] and then
// clears them from the composed config, so patch reads must come from the raw
// (pre-composition) config the patches are declared in. Before the fix every
// patch read answered 404/empty even though the patch had taken effect, and
// the unset map/struct fields of a patch serialized as null where the spec
// declares non-nullable objects.
func TestPatchReadsServeRawConfigPatchesClearedByComposition(t *testing.T) {
	fs := newFakeState(t)
	suspended := true
	branch := "trunk"
	command := "patched-cli"
	raw := *fs.cfg
	raw.Patches = config.Patches{
		Agents:    []config.AgentPatch{{Dir: "rig1", Name: "worker", Suspended: &suspended}},
		Rigs:      []config.RigPatch{{Name: "myrig", DefaultBranch: &branch}},
		Providers: []config.ProviderPatch{{Name: "claude", Command: &command}},
	}
	fs.rawCfg = &raw
	fs.cfg.Patches = config.Patches{} // composition cleared them after applying
	h := newTestCityHandler(t, fs)

	for _, path := range []string{
		"/patches/agents", "/patches/agent/rig1/worker",
		"/patches/rigs", "/patches/rig/myrig",
		"/patches/providers", "/patches/provider/claude",
	} {
		req := httptest.NewRequest("GET", cityURL(fs, path), nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200; body = %s", path, w.Code, w.Body.String())
			continue
		}
		// The spec declares the map and struct-pointer patch fields as
		// non-nullable objects; unset ones must be omitted, not sent as null.
		for _, field := range []string{"Env", "OptionDefaults", "Pool", "ContextAdvisory", "FormulaVars"} {
			if strings.Contains(w.Body.String(), `"`+field+`":null`) {
				t.Errorf("GET %s serialized unset %s as null: %s", path, field, w.Body.String())
			}
		}
		if strings.HasSuffix(path, "s") {
			var resp listResponse
			json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
			if resp.Total != 1 {
				t.Errorf("GET %s total = %d, want 1", path, resp.Total)
			}
		}
	}
}
