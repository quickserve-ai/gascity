package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const pushSuitePolicyPath = ".githooks/lib/push-suite-policy.sh"

// runPushSuitePolicy runs the policy script with only the given environment.
func runPushSuitePolicy(t *testing.T, env []string) (int, string) {
	t.Helper()
	root := repoRoot(t)
	cmd := testCommand(filepath.Join(root, pushSuitePolicyPath))
	cmd.Dir = root
	cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("run %s: %v\n%s", pushSuitePolicyPath, err, out)
	}
	return exitErr.ExitCode(), string(out)
}

func writeCIOnlyMarker(t *testing.T, dir, reason string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test-suites-ci-only"), []byte(reason), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPushSuitePolicyRunsTheSuiteWithoutAMarker(t *testing.T) {
	code, out := runPushSuitePolicy(t, []string{"HOME=" + t.TempDir()})
	if code != 0 || out != "" {
		t.Fatalf("no marker: exit %d, output %q; want 0 and silence (unchanged behavior)", code, out)
	}
}

func TestPushSuitePolicySkipsTheSuiteOnACIOnlyHost(t *testing.T) {
	home := t.TempDir()
	writeCIOnlyMarker(t, filepath.Join(home, ".gc", "host-policy"), "operator ruling ga-1zejyh\n")
	code, out := runPushSuitePolicy(t, []string{"HOME=" + home})
	if code != 3 {
		t.Fatalf("marker under HOME: exit %d, want 3 (skip)\n%s", code, out)
	}
	for _, want := range []string{"SKIPPED", "CI only", "operator ruling ga-1zejyh"} {
		if !strings.Contains(out, want) {
			t.Fatalf("skip message lacks %q:\n%s", want, out)
		}
	}
}

func TestPushSuitePolicyHonorsThePolicyDirOverride(t *testing.T) {
	dir := t.TempDir()
	writeCIOnlyMarker(t, dir, "")
	code, out := runPushSuitePolicy(t, []string{"HOME=" + t.TempDir(), "GC_HOST_POLICY_DIR=" + dir})
	if code != 3 {
		t.Fatalf("marker in GC_HOST_POLICY_DIR: exit %d, want 3\n%s", code, out)
	}
}

// The real hook, run in the pre-push fixture: on a CI-only host the suite is
// never reached and the push is not refused.
func TestPrePushSkipsTheSuiteOnACIOnlyHost(t *testing.T) {
	f := newPrePushFixture(t)
	writeCIOnlyMarker(t, f.policyDir, "fixture ruling\n")
	code, out := f.run(t, "refs/heads/main "+f.commitNew+" refs/heads/main "+f.commitOld+"\n")
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0 (skip is not a refusal)\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("suite reached on a CI-only host (make invocations = %q)", got)
	}
	if !strings.Contains(out, "SKIPPED") || !strings.Contains(out, "fixture ruling") {
		t.Fatalf("skip is silent or lacks the reason:\n%s", out)
	}
}

// A policy script that fails for any reason other than exit 3 must not disable
// the suite: the hook runs it as before.
func TestPrePushRunsTheSuiteWhenThePolicyFails(t *testing.T) {
	f := newPrePushFixture(t)
	writeCIOnlyMarker(t, f.policyDir, "")
	writeExecutable(t, filepath.Join(f.repo, pushSuitePolicyPath), "#!/usr/bin/env bash\nexit 1\n")
	code, out := f.run(t, "refs/heads/main "+f.commitNew+" refs/heads/main "+f.commitOld+"\n")
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); !strings.Contains(got, "test-fast-parallel") {
		t.Fatalf("a failing policy disabled the suite (make invocations = %q)", got)
	}
}
