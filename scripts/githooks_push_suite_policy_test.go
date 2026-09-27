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
		t.Fatalf("no marker: exit %d, output %q; want 0 and silence (unchanged behaviour)", code, out)
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

func TestPushSuitePolicyHonoursThePolicyDirOverride(t *testing.T) {
	dir := t.TempDir()
	writeCIOnlyMarker(t, dir, "")
	code, out := runPushSuitePolicy(t, []string{"HOME=" + t.TempDir(), "GC_HOST_POLICY_DIR=" + dir})
	if code != 3 {
		t.Fatalf("marker in GC_HOST_POLICY_DIR: exit %d, want 3\n%s", code, out)
	}
}

// The hook must consult the policy BEFORE it hands off to the suite, and only
// exit 3 may skip: any other policy failure still runs the suite.
func TestPrePushConsultsThePolicyBeforeTheSuite(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".githooks", "pre-push"))
	if err != nil {
		t.Fatal(err)
	}
	hook := string(data)
	policy := strings.Index(hook, "push-suite-policy.sh")
	suite := strings.Index(hook, `exec "$repo_root/.githooks/lib/push-suite.sh"`)
	if policy < 0 || suite < 0 || policy > suite {
		t.Fatalf("pre-push must call push-suite-policy.sh before it execs the suite dispatcher push-suite.sh (policy at %d, suite at %d)", policy, suite)
	}
	if !strings.Contains(hook, `if [ "$policy_rc" -eq 3 ]; then`) {
		t.Fatal("pre-push must skip the suite only on policy exit 3")
	}
}
