package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Pins the cross-town shared-pool invariant (ga-h4iqzr, cross-town R2a): a
// seat executes only molecules instantiated for its own host — accept iff the
// gc.formula_source lies under a host root OR the formula declares
// path_agnostic — and every refusal is loud and countable (declined_foreign),
// never a silent skip. On 8/29 and 8/30 an unguarded claim produced duplicate
// cross-town execution of the same shared-store molecule.

func TestFormulaSourceForeignToHost(t *testing.T) {
	roots := []string{"/Users/test/gascity", "/Users/test"}
	cases := []struct {
		name  string
		src   string
		roots []string
		want  bool
	}{
		{"empty source is not formula-instantiated", "", roots, false},
		{"relative source resolves locally", "formulas/mol-x.toml", roots, false},
		{"under city root", "/Users/test/gascity/formulas/mol-x.toml", roots, false},
		{"under home root", "/Users/test/.gc/cache/packs/x/f.toml", roots, false},
		{"exactly a root", "/Users/test/gascity", roots, false},
		{"foreign /data path", "/data/city/westeros/packs/westeros-review/formulas/f.toml", roots, true},
		{"prefix must be a path boundary", "/Users/testevil/f.toml", roots, true},
		{"no roots fails closed", "/anything/abs.toml", nil, true},
		{"blank roots fail closed", "/anything/abs.toml", []string{" ", ""}, true},
		{"trailing slash on root still matches", "/Users/test/gascity/f.toml", []string{"/Users/test/gascity/"}, false},
	}
	for _, tc := range cases {
		if got := formulaSourceForeignToHost(tc.src, tc.roots); got != tc.want {
			t.Errorf("%s: formulaSourceForeignToHost(%q, %v) = %v, want %v", tc.name, tc.src, tc.roots, got, tc.want)
		}
	}
}

type foreignClaimSpy struct{ ids []string }

func (s *foreignClaimSpy) fn(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
	s.ids = append(s.ids, id)
	return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{}}, true, nil
}

func foreignGuardOpsOpts(workQueryJSON string, spy *foreignClaimSpy) (hookClaimOps, hookClaimOptions) {
	ops := hookClaimOps{
		Runner:            func(string, string) (string, error) { return workQueryJSON, nil },
		Claim:             spy.fn,
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
		StampWorkMeta:     noopStampWorkMeta,
		PublishRunMap:     func(string, string, ...string) error { return nil },
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		HostRoots:          []string{"/Users/test/gascity", "/Users/test"},
		JSON:               true,
	}
	return ops, opts
}

func TestHookClaimDeclinesForeignInstantiatedMolecule(t *testing.T) {
	spy := &foreignClaimSpy{}
	ops, opts := foreignGuardOpsOpts(
		`[{"id":"qc-foreign","status":"open","metadata":{"gc.routed_to":"worker","gc.formula_source":"/data/city/westeros/packs/westeros-review/formulas/f.toml"}}]`,
		spy,
	)

	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	if len(spy.ids) != 0 {
		t.Fatalf("claim mutations = %v, want none — the foreign candidate must be declined before any claim", spy.ids)
	}
	if !strings.Contains(stderr.String(), "declined-foreign: 1") {
		t.Errorf("stderr = %q, want a loud declined-foreign summary", stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not one JSON line: %v (%q)", err, stdout.String())
	}
	if result["action"] != "drain" || result["reason"] != "declined_foreign" {
		t.Errorf("drain record = %v, want action=drain reason=declined_foreign", result)
	}
	if result["declined_foreign"] != float64(1) {
		t.Errorf("declined_foreign = %v, want 1 — the count is the consumable fact", result["declined_foreign"])
	}
}

func TestHookClaimPathAgnosticForeignIsClaimed(t *testing.T) {
	spy := &foreignClaimSpy{}
	ops, opts := foreignGuardOpsOpts(
		`[{"id":"qc-agnostic","status":"open","metadata":{"gc.routed_to":"worker","gc.formula_source":"/data/city/westeros/packs/f.toml","gc.formula_path_agnostic":"true"}}]`,
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (path-agnostic foreign molecule is claimable); stderr=%s", code, stderr.String())
	}
	if len(spy.ids) != 1 || spy.ids[0] != "qc-agnostic" {
		t.Fatalf("claim mutations = %v, want the path-agnostic candidate claimed", spy.ids)
	}
	if strings.Contains(stderr.String(), "declined-foreign") {
		t.Errorf("stderr = %q, want no declined-foreign report for a declared path-agnostic formula", stderr.String())
	}
}

func TestHookClaimForeignDeclinedLocalStillClaimed(t *testing.T) {
	spy := &foreignClaimSpy{}
	ops, opts := foreignGuardOpsOpts(
		`[{"id":"qc-foreign","status":"open","metadata":{"gc.routed_to":"worker","gc.formula_source":"/data/gc-home/cache/repos/f.toml"}},`+
			`{"id":"ga-local","status":"open","metadata":{"gc.routed_to":"worker","gc.formula_source":"/Users/test/gascity/formulas/mol-x.toml"}}]`,
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(spy.ids) != 1 || spy.ids[0] != "ga-local" {
		t.Fatalf("claim mutations = %v, want only the locally instantiated candidate", spy.ids)
	}
	if !strings.Contains(stderr.String(), "declined-foreign: 1") {
		t.Errorf("stderr = %q, want the declined-foreign report even on a successful pass", stderr.String())
	}
	if !strings.Contains(stderr.String(), "qc-foreign") {
		t.Errorf("stderr = %q, want the declined id named", stderr.String())
	}
}

func TestHookClaimCandidateWithoutFormulaSourceIsUnaffected(t *testing.T) {
	spy := &foreignClaimSpy{}
	ops, opts := foreignGuardOpsOpts(
		`[{"id":"ga-plain","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (non-formula work is outside the guard); stderr=%s", code, stderr.String())
	}
	if len(spy.ids) != 1 || spy.ids[0] != "ga-plain" {
		t.Fatalf("claim mutations = %v, want the plain candidate claimed", spy.ids)
	}
}

// pinHostHomes models westeros (GC_HOME=/data/gc-home, outside $HOME) with
// three disjoint temp dirs: HOME (os.UserHomeDir reads $HOME on darwin and
// linux), GC_HOME and GC_CITY_PATH. Pinning HOME means the cases never depend
// on where the system temp dir lives and never skip.
func pinHostHomes(t *testing.T) (userHome, gcHome, cityPath string) {
	t.Helper()
	userHome, gcHome, cityPath = t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_CITY_PATH", cityPath)
	return userHome, gcHome, cityPath
}

func assertHostRoots(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("hookClaimHostRoots() = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hookClaimHostRoots()[%d] = %q, want %q (full = %v)", i, got[i], want[i], got)
		}
	}
}

// gc-m61x: formula-instantiated roots are stamped with a gc.formula_source
// under the gc home's cache (internal/gchome: GC_HOME first, then $HOME/.gc).
// On a host whose GC_HOME lies outside $HOME (westeros: /data/gc-home) the
// host roots must include it, or every locally poured molecule is declined as
// foreign and the pool bricks (2026-09-23 17:02Z).
func TestHookClaimHostRootsIncludeGCHome(t *testing.T) {
	userHome, gcHome, cityPath := pinHostHomes(t)

	assertHostRoots(t, hookClaimHostRoots(), []string{cityPath, gcHome, userHome})
}

func TestHookClaimHostRootsDefaultGCHomeUnderUserHome(t *testing.T) {
	userHome, _, cityPath := pinHostHomes(t)
	t.Setenv("GC_HOME", "")

	assertHostRoots(t, hookClaimHostRoots(), []string{cityPath, filepath.Join(userHome, ".gc"), userHome})
}

// Unstable provenance is never a root. With no GC_HOME and no resolvable user
// home, gchome.ResolveReadOnly falls back to the process-unique
// `<tmp>/gc-home-<pid>` candidate: nothing was instantiated under it, and a
// foreign source could match the string by coincidence.
func TestHookClaimHostRootsIgnoreLastResortGCHome(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_CITY_PATH", cityPath)
	t.Setenv("GC_HOME", "")
	t.Setenv("HOME", "") // os.UserHomeDir fails on darwin/linux with an empty $HOME

	if _, err := os.UserHomeDir(); err == nil {
		t.Fatalf("precondition: os.UserHomeDir still resolves with HOME empty; the last-resort branch is not reached")
	}
	lastResort := filepath.Join(os.TempDir(), fmt.Sprintf("gc-home-%d", os.Getpid()))

	roots := hookClaimHostRoots()

	assertHostRoots(t, roots, []string{cityPath})
	for _, r := range roots {
		if r == lastResort || strings.Contains(r, "gc-home-") {
			t.Errorf("hookClaimHostRoots() = %v, must not include the last-resort fallback %q", roots, lastResort)
		}
	}
}

func TestHookClaimHostRootsCleanAndDeduplicate(t *testing.T) {
	userHome, gcHome, cityPath := pinHostHomes(t)
	cases := []struct {
		name   string
		gcHome string
		city   string
		want   []string
	}{
		{"trailing /. is cleaned", gcHome + "/.", cityPath, []string{cityPath, gcHome, userHome}},
		{"trailing / is cleaned", gcHome + "/", cityPath, []string{cityPath, gcHome, userHome}},
		{"city path cleaned too", gcHome, cityPath + "/./", []string{cityPath, gcHome, userHome}},
		{"gc home equal to user home appears once", userHome + "/.", cityPath, []string{cityPath, userHome}},
		{"empty city path dropped", gcHome, "", []string{gcHome, userHome}},
		{"blank city path dropped", gcHome, "  ", []string{gcHome, userHome}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_HOME", tc.gcHome)
			t.Setenv("GC_CITY_PATH", tc.city)
			assertHostRoots(t, hookClaimHostRoots(), tc.want)
		})
	}
}

func TestHookClaimLocalMoleculeUnderGCHomeIsClaimed(t *testing.T) {
	_, gcHome, _ := pinHostHomes(t)
	src := filepath.Join(gcHome, "cache", "repos", "abc", "internal", "bootstrap", "packs", "core", "formulas", "mol-scoped-work.toml")

	spy := &foreignClaimSpy{}
	ops, opts := foreignGuardOpsOpts(
		`[{"id":"ga-local-gchome","status":"open","metadata":{"gc.routed_to":"worker","gc.formula_source":"`+src+`"}}]`,
		spy,
	)
	opts.HostRoots = nil // the default resolver must run

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (molecule poured under this host's GC_HOME is local); stderr=%s", code, stderr.String())
	}
	if len(spy.ids) != 1 || spy.ids[0] != "ga-local-gchome" {
		t.Fatalf("claim mutations = %v, want the GC_HOME-instantiated candidate claimed, not declined-foreign; stderr=%s", spy.ids, stderr.String())
	}
	if strings.Contains(stderr.String(), "declined-foreign") {
		t.Errorf("stderr = %q, want no declined-foreign report for a locally instantiated molecule", stderr.String())
	}
}
