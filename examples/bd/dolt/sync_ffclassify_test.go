package dolt_test

// Fast-forward-only sync classification (gc-6ommo). `gc dolt sync` must not
// blind-push a shared multi-writer DB: it fetches, classifies local vs the
// remote-tracking ref, and pushes only when the local branch is strictly ahead
// (a fast-forward). behind / diverged refuse with an actionable status; a
// fetch timeout skips without pushing; a first push (remote ref absent) is a
// fast-forward and pushes. --force still bypasses classification.
//
// The classification queries are verified against real Dolt 2.1.0:
//   ahead  = SELECT COUNT(*) FROM dolt_log('remotes/<remote>/<br>..<br>')
//   behind = SELECT COUNT(*) FROM dolt_log('<br>..remotes/<remote>/<br>')
// and an absent remote ref yields "branch not found: remotes/...".

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runFFSyncEnv sets up a one-DB ("app") SQL-mode city with the fake dolt
// already installed in binDir, runs `gc dolt sync <args>` with extraEnv
// appended after the base sync env (so an override such as
// GC_DOLT_REMOTE_<DB> takes effect), and returns combined output plus the
// command's error so callers that must assert the sync itself did not fail
// (as opposed to merely producing unexpected output) can do so.
func runFFSyncEnv(t *testing.T, binDir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = append(syncFilteredEnv(),
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
	return string(out), err
}

// runFFSync is runFFSyncEnv with no extra environment variables, discarding
// the command error: none of its callers assert on the sync's exit status,
// only on its output.
func runFFSync(t *testing.T, binDir string, args ...string) string {
	t.Helper()
	out, _ := runFFSyncEnv(t, binDir, nil, args...)
	return out
}

// runFFSyncStatus is runFFSync plus the process exit code, for tests that
// assert on the exit-code contract rather than just push/no-push behavior.
func runFFSyncStatus(t *testing.T, binDir string, args ...string) (string, int) {
	t.Helper()
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	writeSyncFakeBeadsBD(t, cityPath)

	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("gc dolt sync failed to run: %v\n%s", err, out)
		}
	}
	return string(out), exitCode
}

// fakeDoltHeader is the shared preamble: log argv and answer the remote-lookup
// + active_branch metadata queries the sync path issues before classification.
func fakeDoltHeader(logPath, branch string) string {
	return "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + logPath + "\"\n" +
		"case \"$*\" in\n" +
		"  *\"SELECT name, url FROM dolt_remotes ORDER BY name\"*)\n" +
		"    printf 'name,url\\norigin,file:///example.invalid/repo\\n' ; exit 0 ;;\n" +
		"  *\"SELECT active_branch()\"*)\n" +
		"    printf 'active_branch()\\n" + branch + "\\n' ; exit 0 ;;\n"
}

func installFFFakeDolt(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return filepath.Join(dir, "dolt.log")
}

// writeSyncFakeDoltClassify: fetch succeeds; the ahead/behind range queries
// report the given counts; DOLT_PUSH is logged and succeeds.
func writeSyncFakeDoltClassify(t *testing.T, dir string, ahead, behind int) string {
	t.Helper()
	branch := "main"
	logPath := filepath.Join(dir, "dolt.log")
	aheadPat := "dolt_log('remotes/origin/" + branch + ".." + branch + "')"
	behindPat := "dolt_log('" + branch + "..remotes/origin/" + branch + "')"
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) exit 0 ;;\n" +
		"  *\"" + aheadPat + "\"*) printf 'n\\n" + fmt.Sprintf("%d", ahead) + "\\n' ; exit 0 ;;\n" +
		"  *\"" + behindPat + "\"*) printf 'n\\n" + fmt.Sprintf("%d", behind) + "\\n' ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

func writeSyncFakeDoltMalformedClassify(t *testing.T, dir string) string {
	t.Helper()
	branch := "main"
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) exit 0 ;;\n" +
		"  *\"dolt_log(\"*) printf 'n\\n' ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// writeSyncFakeDoltFetchTimeout: the DOLT_FETCH call exits 124 (timeout).
func writeSyncFakeDoltFetchTimeout(t *testing.T, dir, branch string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) printf 'context deadline exceeded\\n' >&2 ; exit 124 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// writeSyncFakeDoltFirstPush models a brand-new branch absent on the remote:
// DOLT_FETCH errors "invalid ref spec" (exit 1) — the real Dolt 2.1.0 signal
// for a branch that does not exist on a populated remote (an empty remote
// instead errors "no branches found in remote"). Both are first-push signals;
// the push then creates the branch (a fast-forward). No classify query runs
// because fetch never establishes a remote-tracking ref.
func writeSyncFakeDoltFirstPush(t *testing.T, dir, branch string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) printf 'fetch failed: invalid ref spec\\n' >&2 ; exit 1 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

func pushed(log string) bool { return strings.Contains(log, "DOLT_PUSH") }

func readLog(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	return string(data)
}

func TestSyncAheadOnlyFastForwardPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 2, 0)
	out := runFFSync(t, binDir, "--db", "app")
	log := readLog(t, logPath)
	if !strings.Contains(log, "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("ahead-only should fast-forward push.\nout:\n%s\nlog:\n%s", out, log)
	}
	if strings.Contains(log, "--force") {
		t.Fatalf("ahead-only push must not use --force.\nlog:\n%s", log)
	}
}

func TestSyncBehindRefusesAndDoesNotPush(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 0, 3)
	out := runFFSync(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("behind DB must NOT be pushed.\nout:\n%s", out)
	}
	if !strings.Contains(out, "behind") {
		t.Fatalf("expected a 'behind' status.\nout:\n%s", out)
	}
}

func TestSyncDivergedRefusesAndDoesNotPush(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 2, 3)
	out := runFFSync(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("diverged DB must NOT be pushed.\nout:\n%s", out)
	}
	if !strings.Contains(out, "diverged") {
		t.Fatalf("expected a 'diverged' status.\nout:\n%s", out)
	}
}

func TestSyncUpToDateSkipsPush(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 0, 0)
	out := runFFSync(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("up-to-date DB must NOT be pushed.\nout:\n%s", out)
	}
	if !strings.Contains(out, "up-to-date") {
		t.Fatalf("expected an 'up-to-date' status.\nout:\n%s", out)
	}
}

func TestSyncUpToDateRefreshesMirrorFreshness(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 0, 0)
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	writeSyncFakeBeadsBD(t, cityPath)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()
	// ga-3o5xrw: the stamp is keyed <db>@<remote>. Seeding the pre-ga-3o5xrw
	// per-database key as well proves the refresh lands on the pair key and is
	// not merely overwriting whatever file happened to be there.
	stamp := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "backup-freshness", "app@origin")
	legacyStamp := filepath.Join(filepath.Dir(stamp), "app")
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		t.Fatalf("mkdir freshness dir: %v", err)
	}
	if err := os.WriteFile(stamp, []byte("pushed_at_epoch=1\nremote=old\nrefspec=old:old\n"), 0o644); err != nil {
		t.Fatalf("seed stale freshness: %v", err)
	}
	if err := os.WriteFile(legacyStamp, []byte("pushed_at_epoch=1\nremote=old\nrefspec=old:old\n"), 0o644); err != nil {
		t.Fatalf("seed legacy freshness: %v", err)
	}

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, syncScript), "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("up-to-date sync failed: %v\n%s", err, out)
	}
	if pushed(readLog(t, logPath)) {
		t.Fatalf("up-to-date DB must NOT be pushed.\nout:\n%s", out)
	}
	raw, err := os.ReadFile(stamp)
	if err != nil {
		t.Fatalf("read refreshed mirror freshness: %v\nout:\n%s", err, out)
	}
	content := string(raw)
	if strings.Contains(content, "pushed_at_epoch=1\n") || !strings.Contains(content, "remote=origin\n") || !strings.Contains(content, "refspec=main:main\n") {
		t.Fatalf("up-to-date verification did not refresh the expected target:\n%s\nout:\n%s", content, out)
	}
}

func TestSyncMalformedClassificationDoesNotRefreshMirrorFreshness(t *testing.T) {
	binDir := t.TempDir()
	writeSyncFakeDoltMalformedClassify(t, binDir)
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	writeSyncFakeBeadsBD(t, cityPath)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()
	stamp := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "backup-freshness", "app@origin")
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		t.Fatalf("mkdir freshness dir: %v", err)
	}
	oldStamp := []byte("pushed_at_epoch=1\nremote=origin\nrefspec=main:main\n")
	if err := os.WriteFile(stamp, oldStamp, 0o644); err != nil {
		t.Fatalf("seed stale freshness: %v", err)
	}

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, syncScript), "--db", "app")
	cmd.Env = append(syncFilteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("malformed classification must fail closed\nout:\n%s", out)
	}
	raw, readErr := os.ReadFile(stamp)
	if readErr != nil {
		t.Fatalf("read unchanged freshness stamp: %v", readErr)
	}
	if string(raw) != string(oldStamp) {
		t.Fatalf("malformed classification refreshed mirror freshness:\n%s\nout:\n%s", raw, out)
	}
}

// TestSyncDivergedRealRunExitsNonZero locks in the real-run (non-dry-run)
// contract that ga-co5cx's --dry-run gap (fixed alongside this test) was
// compared against: a diverged DB must fail the overall `gc dolt sync` run,
// not just print a warning, so a cron/patrol caller can detect
// reconcile-needed state from the exit code alone.
func TestSyncDivergedRealRunExitsNonZero(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 2, 3)
	out, exitCode := runFFSyncStatus(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("diverged DB must NOT be pushed.\nout:\n%s", out)
	}
	if exitCode == 0 {
		t.Fatalf("a diverged DB must exit non-zero.\nout:\n%s", out)
	}
}

// TestSyncDryRunDivergedReturnsNonZero is the ga-co5cx fix: --dry-run must
// mirror the real run's exit code (0 up-to-date/behind, 1 diverged/classify-
// failed) so a health-style probe can preview reconcile-needed state without
// side effects. Before the fix, --dry-run unconditionally returned 0.
func TestSyncDryRunDivergedReturnsNonZero(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 2, 3)
	out, exitCode := runFFSyncStatus(t, binDir, "--db", "app", "--dry-run")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("--dry-run must NEVER push.\nout:\n%s", out)
	}
	if exitCode == 0 {
		t.Fatalf("--dry-run preview of a diverged DB must exit non-zero.\nout:\n%s", out)
	}
	if !strings.Contains(out, "diverged") {
		t.Fatalf("expected a 'diverged' status.\nout:\n%s", out)
	}
}

// TestSyncDryRunUpToDateReturnsZero guards the benign side of the same
// contract: --dry-run must not turn a harmless behind/up-to-date skip into a
// false-positive failure.
func TestSyncDryRunUpToDateReturnsZero(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 0, 0)
	out, exitCode := runFFSyncStatus(t, binDir, "--db", "app", "--dry-run")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("--dry-run must NEVER push.\nout:\n%s", out)
	}
	if exitCode != 0 {
		t.Fatalf("--dry-run preview of an up-to-date DB should exit zero.\nout:\n%s", out)
	}
}

func TestSyncFetchTimeoutSkipsNeverPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFetchTimeout(t, binDir, "main")
	out := runFFSync(t, binDir, "--db", "app")
	if pushed(readLog(t, logPath)) {
		t.Fatalf("a fetch timeout must NEVER push.\nout:\n%s", out)
	}
	if !strings.Contains(out, "fetch timed out") {
		t.Fatalf("expected a 'fetch timed out' status.\nout:\n%s", out)
	}
}

func TestSyncFirstPushWhenRemoteRefAbsentPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltFirstPush(t, binDir, "main")
	out := runFFSync(t, binDir, "--db", "app")
	if !strings.Contains(readLog(t, logPath), "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("first push (absent remote ref) must push.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
}

func TestSyncForceStillPushesWhenDiverged(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltClassify(t, binDir, 2, 3)
	out := runFFSync(t, binDir, "--db", "app", "--force")
	if !strings.Contains(readLog(t, logPath), "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") {
		t.Fatalf("--force must bypass classification and force-push.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
}

// writeSyncFakeDoltEmptyRemoteFirstPush models a first-ever push to an empty
// remote: DOLT_FETCH errors "no branches found in remote" (the other Dolt 2.1.0
// first-push signal, distinct from "invalid ref spec" for a new branch on a
// populated remote). The push then creates the branch (a fast-forward).
func writeSyncFakeDoltEmptyRemoteFirstPush(t *testing.T, dir, branch string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := fakeDoltHeader(logPath, branch) +
		"  *\"CALL DOLT_FETCH(\"*) printf 'fetch failed: no branches found in remote\\n' >&2 ; exit 1 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

func TestSyncEmptyRemoteFirstPushPushes(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltEmptyRemoteFirstPush(t, binDir, "main")
	out := runFFSync(t, binDir, "--db", "app")
	if !strings.Contains(readLog(t, logPath), "CALL DOLT_PUSH('origin', 'main')") {
		t.Fatalf("first push to an empty remote must push.\nout:\n%s\nlog:\n%s", out, readLog(t, logPath))
	}
}

// --- gc-fqi7kq: deterministic, opt-in-gated remote selection ---
//
// find_remote_sql (and the CLI-mode equivalent) must resolve the SAME remote
// for a given database regardless of what order dolt_remotes rows arrive in;
// must never auto-select a non-file:// remote when a file:// alternative
// exists, or when no local alternative exists at all (skip with a stated
// reason instead); and must honor an explicit GC_DOLT_REMOTE_<DB> override —
// even over a non-local remote. These tests run --force --dry-run so the
// fake dolt only needs to answer the remote-lookup + active_branch queries;
// ff-classification is already covered by the tests above.

// writeSyncFakeDoltMultiRemote installs a fake dolt that answers the
// remote-lookup query with remoteRows ("name,url" pairs) in exactly the
// order given, and active_branch() with "main". Callers only need the
// installed binary (they assert on gc dolt sync's stdout), not the log
// path, so unlike the other fixture builders in this file this one returns
// nothing.
func writeSyncFakeDoltMultiRemote(t *testing.T, dir string, remoteRows []string) {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	reply := "name,url\\n" + strings.Join(remoteRows, "\\n") + "\\n"
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes ORDER BY name"*)
    printf '` + reply + `'
    ;;
  *"SELECT active_branch()"*)
    printf 'active_branch()\nmain\n'
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
}

// TestSyncRemoteSelectionIsOrderIndependent is the RED test from gc-fqi7kq: a
// db whose dolt_remotes returns rows in varying order must resolve the same
// remote every time. Mirrors the real incident's remote set (a git+https
// upstream plus two file:// backups) across three distinct row orderings.
func TestSyncRemoteSelectionIsOrderIndependent(t *testing.T) {
	t.Parallel()
	origin := "origin,git+https://github.com/gastownhall/beads"
	usb := "usb,file:///mnt/usb/beads"
	usbSnap := "usb_snap_20260820,file:///mnt/usb/beads_snap"
	orders := [][]string{
		{origin, usb, usbSnap},
		{usbSnap, usb, origin},
		{usb, origin, usbSnap},
	}
	const want = "-> usb:main (file:///mnt/usb/beads)"
	for i, rows := range orders {
		binDir := t.TempDir()
		writeSyncFakeDoltMultiRemote(t, binDir, rows)
		out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
		if !strings.Contains(out, want) {
			t.Fatalf("order %d: expected remote 'usb' selected regardless of row order.\nrows: %v\nout:\n%s", i, rows, out)
		}
	}
}

// TestSyncMultiRemoteAmbiguousNonLocalSkipsAndNeverPushes covers the case
// where every configured remote is non-local: a default (non-opt-in) sync
// must report the database as skipped, with a stated reason, and never
// select or push to either remote.
func TestSyncMultiRemoteAmbiguousNonLocalSkipsAndNeverPushes(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"mirror,git+https://example.invalid/mirror",
		"origin,git+https://github.com/gastownhall/beads",
	})
	out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "no local remote") {
		t.Fatalf("expected an ambiguous-remote skip with a stated reason.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("ambiguous remotes must never be auto-selected.\nout:\n%s", out)
	}
}

// TestSyncMultiRemotePrefersLocalOverGitHttpsRemote covers the case where a
// git+https remote is configured alongside a file:// alternative: the
// git+https remote must never be the one selected/pushed.
func TestSyncMultiRemotePrefersLocalOverGitHttpsRemote(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
		"usb,file:///mnt/usb/beads",
	})
	out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
	if !strings.Contains(out, "-> usb:main (file:///mnt/usb/beads)") {
		t.Fatalf("expected the local (file://) remote to be preferred over git+https.\nout:\n%s", out)
	}
	if strings.Contains(out, "-> origin:") {
		t.Fatalf("git+https remote must never be auto-selected when a local alternative exists.\nout:\n%s", out)
	}
}

// TestSyncRemoteEnvOverridePinsNonLocalRemote verifies that GC_DOLT_REMOTE_<DB>
// pins remote selection even to a non-local (git+https) remote that the
// default policy would otherwise skip past in favor of a file:// alternative.
func TestSyncRemoteEnvOverridePinsNonLocalRemote(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
		"usb,file:///mnt/usb/beads",
	})

	out, err := runFFSyncEnv(t, binDir, []string{"GC_DOLT_REMOTE_APP=origin"}, "--db", "app", "--force", "--dry-run")
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "-> origin:main (git+https://github.com/gastownhall/beads)") {
		t.Fatalf("GC_DOLT_REMOTE_APP=origin should pin selection to origin even though usb (file://) is available.\nout:\n%s", out)
	}
}

// TestSyncSoleNonLocalRemoteSkipsAndNeverPushes covers the case the original
// fix (ga-fqi7kq) missed: select_remote's sr_count -eq 1 branch returned the
// sole candidate unconditionally, with no locality check at all, so a
// database whose only configured remote is non-local (e.g. a public
// git+https upstream) was auto-selected and pushed to. The locality rule
// that already applies when there are multiple remotes must apply
// identically when there is exactly one.
func TestSyncSoleNonLocalRemoteSkipsAndNeverPushes(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
	})

	out := runFFSync(t, binDir, "--db", "app", "--force", "--dry-run")
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "no local remote") {
		t.Fatalf("a sole non-local remote must be skipped with a stated reason, same as the multi-remote ambiguous case.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("a sole non-local remote must never be auto-selected or pushed to.\nout:\n%s", out)
	}
}

// TestSyncSQLUnknownRemoteOverrideFailsWithStatedReason covers the one path
// where select_remote returns non-zero: GC_DOLT_REMOTE_<DB> names a remote
// that is not configured. The database must fail (not silently fall back to
// the default policy), the stderr must name the offending override, and the
// trailing failure summary must attribute the failure to the refusal rather
// than to a remote *query* failure that did not happen.
func TestSyncSQLUnknownRemoteOverrideFailsWithStatedReason(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
		"usb,file:///mnt/usb/beads",
	})

	out, err := runFFSyncEnv(t, binDir, []string{"GC_DOLT_REMOTE_APP=nope"}, "--db", "app", "--force", "--dry-run")
	if err == nil {
		t.Fatalf("an override naming an unconfigured remote must fail the database.\nout:\n%s", out)
	}
	if !strings.Contains(out, "GC_DOLT_REMOTE override 'nope' does not match any configured remote") {
		t.Fatalf("expected the specific unknown-override error.\nout:\n%s", out)
	}
	if !strings.Contains(out, "remote selection refused") {
		t.Fatalf("summary must attribute the failure to the refusal, not to a remote-query failure.\nout:\n%s", out)
	}
	if strings.Contains(out, "failed to query remotes") {
		t.Fatalf("the remote query succeeded; it must not be blamed.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("a refused override must never fall back to selecting a remote.\nout:\n%s", out)
	}
}

// --- CLI-mode (.dolt/remotes.json) equivalents of the SQL cases above ---
//
// sync_database_cli feeds select_remote from remotes_json_pairs instead of
// dolt_remotes, so the same policy has a second, independently-parsed
// candidate source. These mirror the SQL cases against it: prefer local,
// skip a sole non-local remote, and honor the override. runSync forces CLI
// mode by pointing the script at an unreachable SQL port.

// writeSyncCLIRemotes creates the data/<db>/.dolt/remotes.json fixture that
// sync_database_cli parses, and returns the city path it was created under.
func writeSyncCLIRemotes(t *testing.T, remotesJSON string) string {
	t.Helper()
	cityPath := t.TempDir()
	dbDir := filepath.Join(cityPath, "data", "app")
	if err := os.MkdirAll(filepath.Join(dbDir, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, ".dolt", "remotes.json"), []byte(remotesJSON), 0o644); err != nil {
		t.Fatalf("write remotes: %v", err)
	}
	return cityPath
}

// TestSyncCLIMultiRemotePrefersLocal is the CLI-mode twin of
// TestSyncMultiRemotePrefersLocalOverGitHttpsRemote: with a git+https remote
// listed first and a file:// alternative available, the local one wins.
func TestSyncCLIMultiRemotePrefersLocal(t *testing.T) {
	t.Parallel()
	cityPath := writeSyncCLIRemotes(t, `{"remotes":[{"name":"origin","url":"git+https://github.com/gastownhall/beads"},{"name":"usb","url":"file:///mnt/usb/beads"}]}`)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	out, err := runSync(t, binDir, cityPath, []string{"GC_DOLT_REMOTE_APP="}, "--db", "app", "--dry-run")
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "-> usb:main (file:///mnt/usb/beads)") {
		t.Fatalf("CLI mode should prefer the local (file://) remote over git+https.\nout:\n%s", out)
	}
	if strings.Contains(out, "-> origin:") {
		t.Fatalf("git+https remote must never be auto-selected when a local alternative exists.\nout:\n%s", out)
	}
}

// TestSyncCLISoleNonLocalRemoteSkips is the CLI-mode twin of
// TestSyncSoleNonLocalRemoteSkipsAndNeverPushes: a sole non-local remote is
// not exempt from the locality rule just because there was nothing to
// disambiguate.
func TestSyncCLISoleNonLocalRemoteSkips(t *testing.T) {
	t.Parallel()
	cityPath := writeSyncCLIRemotes(t, `{"remotes":[{"name":"origin","url":"git+https://github.com/gastownhall/beads"}]}`)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	out, err := runSync(t, binDir, cityPath, []string{"GC_DOLT_REMOTE_APP="}, "--db", "app", "--dry-run")
	if err != nil {
		t.Fatalf("a skip is not a failure; sync should exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "no local remote") {
		t.Fatalf("a sole non-local remote must be skipped with a stated reason.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("a sole non-local remote must never be auto-selected or pushed to.\nout:\n%s", out)
	}
}

// TestSyncCLIRemoteOverridePinsNonLocal is the CLI-mode twin of
// TestSyncRemoteEnvOverridePinsNonLocalRemote: an explicit override pins a
// non-local remote the default policy would otherwise pass over.
func TestSyncCLIRemoteOverridePinsNonLocal(t *testing.T) {
	t.Parallel()
	cityPath := writeSyncCLIRemotes(t, `{"remotes":[{"name":"origin","url":"git+https://github.com/gastownhall/beads"},{"name":"usb","url":"file:///mnt/usb/beads"}]}`)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	out, err := runSync(t, binDir, cityPath, []string{"GC_DOLT_REMOTE_APP=origin"}, "--db", "app", "--dry-run")
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "-> origin:main (git+https://github.com/gastownhall/beads)") {
		t.Fatalf("GC_DOLT_REMOTE_APP=origin should pin selection to origin even though usb (file://) is available.\nout:\n%s", out)
	}
}

// --- ga-3o5xrw: a listed GC_DOLT_REMOTE_<DB> pushes each named remote ---
//
// One pinned remote per run starves a database mirrored twice whenever the
// pinned mirror is dead. A comma-separated override names several remotes;
// sync handles each on its own, in the order listed, and one remote's failure
// never suppresses the next. Only named remotes are touched, so the default
// policy's refusal of unpinned network remotes is unchanged.

// TestSyncRemoteEnvOverrideListSelectsEachListedRemote pins the selection half:
// every listed remote is synced in the order given, and an unlisted local
// remote the default policy would have chosen is left alone.
func TestSyncRemoteEnvOverrideListSelectsEachListedRemote(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
		"usb,file:///mnt/usb/beads",
		"mirror,git+https://example.invalid/mirror",
	})

	out, err := runFFSyncEnv(t, binDir, []string{"GC_DOLT_REMOTE_APP=mirror,origin"}, "--db", "app", "--force", "--dry-run")
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}
	mirror := strings.Index(out, "-> mirror:main (git+https://example.invalid/mirror)")
	origin := strings.Index(out, "-> origin:main (git+https://github.com/gastownhall/beads)")
	if mirror < 0 || origin < 0 {
		t.Fatalf("GC_DOLT_REMOTE_APP=mirror,origin should sync both listed remotes.\nout:\n%s", out)
	}
	if mirror > origin {
		t.Fatalf("listed remotes must be synced in the order given (mirror before origin).\nout:\n%s", out)
	}
	if strings.Contains(out, "-> usb:") {
		t.Fatalf("an unlisted remote must not be synced once the override names remotes.\nout:\n%s", out)
	}
}

// TestSyncRemoteEnvOverrideListRefusesUnconfiguredEntry pins that a list is
// validated as a whole before anything is synced: one misspelled name fails
// the database loudly instead of silently syncing the rest.
func TestSyncRemoteEnvOverrideListRefusesUnconfiguredEntry(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	writeSyncFakeDoltMultiRemote(t, binDir, []string{
		"origin,git+https://github.com/gastownhall/beads",
		"usb,file:///mnt/usb/beads",
	})

	out, err := runFFSyncEnv(t, binDir, []string{"GC_DOLT_REMOTE_APP=origin,nope"}, "--db", "app", "--force", "--dry-run")
	if err == nil {
		t.Fatalf("a list naming an unconfigured remote must fail the database.\nout:\n%s", out)
	}
	if !strings.Contains(out, "GC_DOLT_REMOTE override 'nope' does not match any configured remote") {
		t.Fatalf("expected the error to name the unconfigured entry.\nout:\n%s", out)
	}
	if !strings.Contains(out, "remote selection refused") {
		t.Fatalf("summary must attribute the failure to the refusal.\nout:\n%s", out)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("no remote may be synced when the list is invalid.\nout:\n%s", out)
	}
}

// writeSyncFakeDoltDeadAndLiveRemotes installs a fake dolt with two non-local
// remotes: origin's fetch fails the way a mirror with a dangling chunk does,
// and probe is healthy and one commit behind local, so a fast-forward is due.
func writeSyncFakeDoltDeadAndLiveRemotes(t *testing.T, dir string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + logPath + "\"\n" +
		"case \"$*\" in\n" +
		"  *\"SELECT name, url FROM dolt_remotes ORDER BY name\"*)\n" +
		"    printf 'name,url\\norigin,https://example.invalid/dead\\nprobe,https://example.invalid/live\\n' ; exit 0 ;;\n" +
		"  *\"SELECT active_branch()\"*)\n" +
		"    printf 'active_branch()\\nmain\\n' ; exit 0 ;;\n" +
		"  *\"DOLT_FETCH('origin'\"*)\n" +
		"    echo 'fatal: Blob not found: 06ctnedcgrbc44rc8hbd2ird9flmpif3.darc' >&2 ; exit 1 ;;\n" +
		"  *\"DOLT_FETCH('probe'\"*) exit 0 ;;\n" +
		"  *\"dolt_log('remotes/probe/main..main')\"*) printf 'n\\n1\\n' ; exit 0 ;;\n" +
		"  *\"dolt_log('main..remotes/probe/main')\"*) printf 'n\\n0\\n' ; exit 0 ;;\n" +
		"esac\nexit 0\n"
	return installFFFakeDolt(t, dir, body)
}

// TestSyncRemoteEnvOverrideListFailedRemoteDoesNotSuppressTheNext is the
// ga-3o5xrw regression: with origin dead and probe live, a failed fetch on
// origin must not stop the push to probe, and the run must still fail with a
// summary naming the remote that failed.
func TestSyncRemoteEnvOverrideListFailedRemoteDoesNotSuppressTheNext(t *testing.T) {
	binDir := t.TempDir()
	logPath := writeSyncFakeDoltDeadAndLiveRemotes(t, binDir)

	out, err := runFFSyncEnv(t, binDir, []string{"GC_DOLT_REMOTE_APP=origin,probe"}, "--db", "app")
	log := readLog(t, logPath)
	if !strings.Contains(log, "DOLT_FETCH('origin'") {
		t.Fatalf("the dead remote was never attempted.\nlog:\n%s\nout:\n%s", log, out)
	}
	if !strings.Contains(log, "DOLT_PUSH('probe', 'main')") {
		t.Fatalf("the live remote was never pushed: a failed remote suppressed it.\nlog:\n%s\nout:\n%s", log, out)
	}
	if strings.Contains(log, "DOLT_PUSH('origin'") {
		t.Fatalf("the remote whose fetch failed must not be pushed.\nlog:\n%s\nout:\n%s", log, out)
	}
	if err == nil {
		t.Fatalf("a failed remote must still fail the run.\nout:\n%s", out)
	}
	if !strings.Contains(out, "app (remote origin: fetch failed (exit 1))") {
		t.Fatalf("summary must name the failed remote and its reason.\nout:\n%s", out)
	}
}

// TestSyncCLIRemoteOverrideListSyncsEachListedRemote is the CLI-mode twin of
// TestSyncRemoteEnvOverrideListSelectsEachListedRemote.
func TestSyncCLIRemoteOverrideListSyncsEachListedRemote(t *testing.T) {
	t.Parallel()
	cityPath := writeSyncCLIRemotes(t, `{"remotes":[{"name":"origin","url":"git+https://github.com/gastownhall/beads"},{"name":"usb","url":"file:///mnt/usb/beads"}]}`)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	out, err := runSync(t, binDir, cityPath, []string{"GC_DOLT_REMOTE_APP=usb,origin"}, "--db", "app", "--dry-run")
	if err != nil {
		t.Fatalf("gc dolt sync failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"would push main -> usb:main (file:///mnt/usb/beads)",
		"would push main -> origin:main (git+https://github.com/gastownhall/beads)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("CLI mode should sync every listed remote; missing %q.\nout:\n%s", want, out)
		}
	}
}

// TestSyncRejectsInvalidFetchTimeout covers the GC_DOLT_SYNC_FETCH_TIMEOUT_SECS
// validator (the twin of the push-timeout validator): the bound is checked at
// startup before any database is touched, and an empty / non-numeric / all-zero
// value aborts with exit 2 rather than running the fetch unbounded.
func TestSyncRejectsInvalidFetchTimeout(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, syncScript)
	binDir := t.TempDir()
	_ = writeSyncFakeDolt(t, binDir) // never invoked: the validator aborts first
	cityPath := t.TempDir()
	for _, bad := range []string{"abc", "", "0", "00", "-5"} {
		cmd := exec.Command("sh", script, "--db", "app")
		cmd.Env = append(syncFilteredEnv(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"GC_CITY_PATH="+cityPath,
			"GC_PACK_DIR="+root,
			"GC_DOLT_DATA_DIR="+filepath.Join(cityPath, "data"),
			"GC_DOLT_PORT=1",
			"GC_DOLT_USER=root",
			"GC_DOLT_PASSWORD=",
			"GC_DOLT_SYNC_FETCH_TIMEOUT_SECS="+bad,
		)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Errorf("fetch timeout %q: want exit 2, got err=%v\nout: %s", bad, err, out)
		}
		if !strings.Contains(string(out), "invalid GC_DOLT_SYNC_FETCH_TIMEOUT_SECS") {
			t.Errorf("fetch timeout %q: want validation message\nout: %s", bad, out)
		}
	}
}
