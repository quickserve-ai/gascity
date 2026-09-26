package scripts_test

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMolScopedWorkTeardownScript runs scripts/test-mol-scoped-work-teardown.sh,
// the behavior test for mol-scoped-work's cleanup-worktree step. That step is
// agent-executed shell inside the formula TOML, so only running its rendered
// text proves what it does. Nothing ran the script before this wrapper, and it
// went stale unnoticed when ga-w805wc replaced the block it tested; ./scripts
// is inside UNIT_COVER_PKGS_NONCMDGC, so exec'ing it here puts it in CI.
func TestMolScopedWorkTeardownScript(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts", "test-mol-scoped-work-teardown.sh")
	out, err := exec.Command("bash", script).CombinedOutput()
	if err != nil {
		exit := &exec.ExitError{}
		if !errors.As(err, &exit) {
			t.Fatalf("exec %s: %v", script, err)
		}
		t.Fatalf("%s exited %d:\n%s", script, exit.ExitCode(), out)
	}
	if strings.HasPrefix(string(out), "SKIP:") {
		t.Skip(strings.TrimSpace(string(out)))
	}
}
