package dolt_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ga-p5bmfx: pull must be able to present a remote IDENTITY, because the
// server-side DOLT_PULL procedure otherwise authenticates to the remote as the
// local SQL session user (root) and a remotesapi hub that knows a different
// account denies it (ga-tj1bgl).
//
// The password is deliberately absent from every assertion below except the one
// that proves it never reaches argv: it travels in the dolt SERVER's process
// environment (DOLT_REMOTE_PASSWORD), never in the call, because argv is
// world-readable via ps.

// writeFakeDoltRemotes writes a fake dolt that reports the supplied remotes and
// logs every invocation. Unlike writeSyncFakeDolt it HONORS "LIMIT 1", so a
// regression that reintroduces the ga-34kjld truncation actually changes what
// the script sees instead of being masked by the fake.
func writeFakeDoltRemotes(t *testing.T, dir, remotesCSV string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes"*)
    printf 'name,url\n'
    case "$*" in
      *"LIMIT 1"*) printf '` + remotesCSV + `\n' | sed -n '1p' ;;
      *)           printf '` + remotesCSV + `\n' ;;
    esac
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return logPath
}

func runPull(t *testing.T, remotesCSV string, extraEnv []string, args ...string) (string, string, error) {
	t.Helper()
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	binDir := t.TempDir()
	doltLog := writeFakeDoltRemotes(t, binDir, remotesCSV)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", append([]string{filepath.Join(root, pullScript), "--db", "app"}, args...)...)
	cmd.Env = append(filteredEnv(
		"PATH", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER",
		"GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR", "GC_CITY_PATH", "GC_PACK_DIR",
		"GC_DOLT_REMOTE_USER_APP_ORIGIN", "DOLT_REMOTE_PASSWORD",
	),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	data, readErr := os.ReadFile(doltLog)
	if readErr != nil {
		t.Fatalf("read fake dolt log: %v", readErr)
	}
	return string(out), string(data), err
}

func TestPullOmitsIdentityWhenUnconfigured(t *testing.T) {
	// The load-bearing compatibility guarantee: with no knob set, the emitted
	// SQL must be exactly what shipped before ga-p5bmfx, so bumping the pack pin
	// changes no city's behavior.
	out, log, err := runPull(t, "origin,https://example.invalid/repo", nil)
	if err != nil {
		t.Fatalf("pull failed: %v\n%s", err, out)
	}
	if !strings.Contains(log, "CALL DOLT_PULL('origin', 'main')") {
		t.Fatalf("expected the pre-ga-p5bmfx call shape\nlog:\n%s", log)
	}
	// Assert on the CALL text, not the whole log: every dolt invocation carries
	// "--user root" as a CLI connection flag, which is the local session user and
	// has nothing to do with the remote identity. Searching the log for "--user"
	// matches that flag and fails on correct output.
	if strings.Contains(log, "CALL DOLT_PULL('--user'") {
		t.Fatalf("identity leaked into an unconfigured pull\nlog:\n%s", log)
	}
}

func TestPullPassesConfiguredRemoteIdentity(t *testing.T) {
	out, log, err := runPull(t, "origin,https://example.invalid/repo",
		[]string{"GC_DOLT_REMOTE_USER_APP_ORIGIN=cherub"})
	if err != nil {
		t.Fatalf("pull failed: %v\n%s", err, out)
	}
	if !strings.Contains(log, "CALL DOLT_PULL('--user', 'cherub', 'origin', 'main')") {
		t.Fatalf("identity not passed to DOLT_PULL\nlog:\n%s", log)
	}
}

func TestPullNeverPutsRemotePasswordInArgv(t *testing.T) {
	// argv is world-readable via ps, so the remote password must never appear in
	// a dolt invocation even when it is present in the environment.
	const secret = "hub-p4ssw0rd-must-not-leak"
	out, log, err := runPull(t, "origin,https://example.invalid/repo",
		[]string{"GC_DOLT_REMOTE_USER_APP_ORIGIN=cherub", "DOLT_REMOTE_PASSWORD=" + secret})
	if err != nil {
		t.Fatalf("pull failed: %v\n%s", err, out)
	}
	if strings.Contains(log, secret) {
		t.Fatalf("remote password reached argv\nlog:\n%s", log)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("remote password reached stdout/stderr\noutput:\n%s", out)
	}
}

func TestPullRefusesSeveralRemotesWithoutSelection(t *testing.T) {
	// ga-34kjld. Pull is a MERGE, so it must not choose a remote on its own the
	// way sync's push path legitimately fans out to all of them.
	out, log, err := runPull(t,
		"origin,https://example.invalid/repo\nzbackup,https://other.invalid/repo", nil)
	if err == nil {
		t.Fatalf("pull silently chose a remote instead of refusing:\n%s", out)
	}
	if !strings.Contains(out, "origin") || !strings.Contains(out, "zbackup") {
		t.Fatalf("refusal must name the candidates:\n%s", out)
	}
	if strings.Contains(log, "DOLT_PULL") {
		t.Fatalf("pull merged despite an ambiguous remote\nlog:\n%s", log)
	}
}

func TestPullHonorsExplicitRemoteSelection(t *testing.T) {
	out, log, err := runPull(t,
		"origin,https://example.invalid/repo\nzbackup,https://other.invalid/repo",
		nil, "--remote", "zbackup")
	if err != nil {
		t.Fatalf("pull --remote failed: %v\n%s", err, out)
	}
	if !strings.Contains(log, "CALL DOLT_PULL('zbackup', 'main')") {
		t.Fatalf("--remote did not select zbackup\nlog:\n%s", log)
	}
}

func TestPullRemoteLookupIsOrderedAndComplete(t *testing.T) {
	// The lookup itself is the ga-34kjld defect site: LIMIT 1 with no ORDER BY.
	_, log, _ := runPull(t,
		"origin,https://example.invalid/repo\nzbackup,https://other.invalid/repo", nil)
	if !strings.Contains(log, "SELECT name, url FROM dolt_remotes ORDER BY name") {
		t.Fatalf("remote lookup is not ordered\nlog:\n%s", log)
	}
	if strings.Contains(log, "LIMIT 1") {
		t.Fatalf("remote lookup still truncates\nlog:\n%s", log)
	}
}
