package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// gcHomeOutsideUserHome returns a GC_HOME candidate that is NOT under the
// user's home, mirroring westeros (GC_HOME=/data/gc-home). t.TempDir lands
// under the system temp dir; if that happens to sit under $HOME the case
// cannot distinguish the fix from the old behavior and is skipped.
func gcHomeOutsideUserHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if home, err := os.UserHomeDir(); err == nil && !formulaSourceForeignToHost(dir, []string{home}) {
		t.Skipf("temp dir %s sits under user home %s; cannot model a GC_HOME outside $HOME here", dir, home)
	}
	return dir
}

// gc-m61x: formula-instantiated roots are stamped with a gc.formula_source
// under the gc home's cache (internal/gchome: GC_HOME first, then $HOME/.gc).
// On a host whose GC_HOME lies outside $HOME (westeros: /data/gc-home) the
// host roots must include it, or every locally poured molecule is declined as
// foreign and the pool bricks (2026-09-23 17:02Z).
func TestHookClaimHostRootsIncludeGCHome(t *testing.T) {
	gcHome := gcHomeOutsideUserHome(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_CITY_PATH", cityPath)
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	roots := hookClaimHostRoots()

	want := []string{cityPath, gcHome, userHome}
	if len(roots) != len(want) {
		t.Fatalf("hookClaimHostRoots() = %v, want exactly %v (city path, gc home, user home)", roots, want)
	}
	for i := range want {
		if roots[i] != want[i] {
			t.Errorf("hookClaimHostRoots()[%d] = %q, want %q (full = %v)", i, roots[i], want[i], roots)
		}
	}
}

func TestHookClaimHostRootsDropEmptyAndDuplicate(t *testing.T) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_HOME", userHome)

	roots := hookClaimHostRoots()

	if len(roots) != 1 || roots[0] != userHome {
		t.Fatalf("hookClaimHostRoots() = %v, want just %q (empty city path dropped, gc home == user home deduplicated)", roots, userHome)
	}
}

func TestHookClaimLocalMoleculeUnderGCHomeIsClaimed(t *testing.T) {
	gcHome := gcHomeOutsideUserHome(t)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_CITY_PATH", t.TempDir())
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
