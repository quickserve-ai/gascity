package dolt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDoltENOSPCGuardShellHarness runs test/dolt_enospc_guard_test.sh, the
// shell harness for the Dolt ENOSPC restart guard (assets/scripts/
// dolt-enospc.sh in the bd pack) and both of its callers: gc dolt restart and
// gc-beads-bd op_recover. The harness drives them with a fake Dolt log and a
// stubbed df, including the control that the pre-fix detector refuses on an
// 8-day-old log the current guard lets through. Running it from here puts it
// in the unit sweep CI already runs.
func TestDoltENOSPCGuardShellHarness(t *testing.T) {
	harness := filepath.Join(repoRoot(t), "..", "..", "..", "test", "dolt_enospc_guard_test.sh")
	if _, err := os.Stat(harness); err != nil {
		t.Fatalf("stat ENOSPC guard harness: %v", err)
	}
	cmd := exec.Command("bash", harness)
	cmd.Env = append(filteredEnv("PATH", "TMPDIR"),
		"PATH="+os.Getenv("PATH"),
		"TMPDIR="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ENOSPC guard harness failed: %v\n%s", err, out)
	}
	t.Logf("ENOSPC guard harness:\n%s", out)
}
