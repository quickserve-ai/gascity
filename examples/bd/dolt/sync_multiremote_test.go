package dolt_test

// Multi-remote sync and per-remote mirror freshness (ga-3o5xrw).
//
// THE DEFECT THESE TESTS PIN. `gc dolt sync` resolved a database's remote with
//
//	SELECT name, url FROM dolt_remotes LIMIT 1
//
// — no ORDER BY — and synced only that one row. A database with two configured
// remotes therefore got exactly ONE of them per run, chosen by whatever row
// order the engine happened to return, and the choice varied between runs. On
// qcore (origin = a mirror broken by a half-finished push, probe = the live
// one) the live mirror won 2 of 5 real patrol runs, so an intended 15-minute
// offsite cadence ran at ~37 minutes. Two things made it invisible:
//
//  1. when the arbitrary pick was the BROKEN remote, its fetch failure returned
//     early, so the healthy remote it did not pick got nothing either — a failed
//     READ on a dead mirror suppressed the WRITE to a live one; and
//  2. the freshness stamp was one file per DATABASE, so whichever remote won the
//     flip stamped it, and `gc dolt health` could not distinguish "the live
//     mirror was pushed" from "the dead one was picked and nothing was pushed".
//     It read ok for both.
//
// WHY THESE TESTS AND NOT THE EXISTING ONES. Every pre-existing sync test
// configures exactly ONE remote, and against one remote the broken code and the
// fixed code behave identically — the whole suite passed while the defect was
// live. A test that cannot tell the two apart is not a regression test for this
// bug, it is a test that happens to be green. Each test below was run against
// the pre-fix script and FAILS there; the failure mode is recorded on each.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDoltTwoRemotes builds a fake dolt where `origin` is broken (its fetch
// fails, exactly as a mirror with a dangling chunk does) and `probe` is
// healthy and one commit ahead, so a fast-forward push is due.
//
// The remotes are returned in the order (origin, probe) so the pre-fix
// `LIMIT 1` code deterministically picks the BROKEN one. That ordering is
// deliberate: it makes the pre-fix failure reproducible instead of a coin flip,
// which is the only way this test can be a reliable mutant-killer.
func fakeDoltTwoRemotes(t *testing.T, dir string) string {
	t.Helper()
	logPath := filepath.Join(dir, "dolt.log")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"FROM dolt_remotes"*)
    printf 'name,url\norigin,https://example.invalid/dead\nprobe,https://example.invalid/live\n'
    exit 0 ;;
  *"SELECT active_branch()"*)
    printf 'active_branch()\nmain\n' ; exit 0 ;;
  *"DOLT_FETCH('origin'"*)
    echo "fatal: Blob not found: 06ctnedcgrbc44rc8hbd2ird9flmpif3.darc" >&2
    exit 1 ;;
  *"DOLT_FETCH('probe'"*)
    exit 0 ;;
  *"dolt_log('remotes/probe/main..main')"*)
    printf 'n\n1\n' ; exit 0 ;;
  *"dolt_log('main..remotes/probe/main')"*)
    printf 'n\n0\n' ; exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return logPath
}

// runMultiRemoteSync runs `gc dolt sync --db app` against a one-database city
// and returns the fake's argv log, the command output, and the freshness dir.
func runMultiRemoteSync(t *testing.T, binDir string) (log, out, freshnessDir string) {
	t.Helper()
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	writeSyncFakeBeadsBD(t, cityPath)

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
	raw, _ := cmd.CombinedOutput()
	return readLog(t, filepath.Join(binDir, "dolt.log")), string(raw),
		filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "backup-freshness")
}

// TestSyncPushesEveryConfiguredRemote is the primary mutant-killer: a broken
// remote must not suppress the healthy one.
//
// PRE-FIX FAILURE (verified by reverting find_remote_sql to `LIMIT 1` and
// re-running): the log contains no DOLT_PUSH at all and no app@probe stamp is
// written, because sync resolved `origin`, its fetch failed, and the function
// returned before probe was ever considered. That is the 2-of-5 cadence loss
// reproduced deterministically.
func TestSyncPushesEveryConfiguredRemote(t *testing.T) {
	binDir := t.TempDir()
	fakeDoltTwoRemotes(t, binDir)
	log, out, _ := runMultiRemoteSync(t, binDir)

	if !strings.Contains(log, "DOLT_PUSH('probe'") {
		t.Fatalf("the healthy remote was never pushed — a failed remote suppressed it.\nlog:\n%s\nout:\n%s", log, out)
	}
	// Both remotes must be attempted. Asserting only on probe would still pass
	// if the loop stopped after the first success, which is the same
	// short-circuit shape in the other direction.
	if !strings.Contains(log, "DOLT_FETCH('origin'") {
		t.Fatalf("the broken remote was never attempted.\nlog:\n%s\nout:\n%s", log, out)
	}
	if !strings.Contains(log, "DOLT_FETCH('probe'") {
		t.Fatalf("the healthy remote was never fetched.\nlog:\n%s\nout:\n%s", log, out)
	}
	// The broken remote must still be reported. A fix that pushed the healthy
	// remote by silently swallowing the other one's failure would trade a
	// durability bug for a blind spot.
	if !strings.Contains(out, "remote origin") {
		t.Fatalf("the broken remote's failure was not reported against its name.\nout:\n%s", out)
	}
	// Pin the QUERY, not just the loop. The fake above answers any dolt_remotes
	// query with both rows, so restoring `LIMIT 1` in the SQL text leaves every
	// assertion above green while a real server hands back exactly one row and
	// the defect is fully back. Verified: with `LIMIT 1` restored and this
	// assertion removed, this test passes.
	if strings.Contains(log, "FROM dolt_remotes LIMIT") {
		t.Fatalf("the remote lookup is limited to a single row again — against a real server that reinstates the coin flip.\nlog:\n%s", log)
	}
}

// TestSyncStampsThePushedRemoteNotTheDatabase pins the stamp key. The old
// per-database key could not express "which remote was verified", which is what
// made the health verdict unfalsifiable.
//
// PRE-FIX FAILURE: no stamp of any kind is written (origin was picked and its
// fetch failed). With the pre-fix code and a HEALTHY origin, the stamp lands at
// `app`, not `app@probe`, and this test fails on the missing pair key.
func TestSyncStampsThePushedRemoteNotTheDatabase(t *testing.T) {
	binDir := t.TempDir()
	fakeDoltTwoRemotes(t, binDir)
	_, out, freshnessDir := runMultiRemoteSync(t, binDir)

	stamp := filepath.Join(freshnessDir, "app@probe")
	raw, err := os.ReadFile(stamp)
	if err != nil {
		entries, _ := os.ReadDir(freshnessDir)
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no per-remote stamp for the pushed remote at %s (present: %v): %v\nout:\n%s", stamp, names, err, out)
	}
	if !strings.Contains(string(raw), "remote=probe\n") {
		t.Fatalf("stamp does not name the remote it verified:\n%s", raw)
	}
	// The remote that was NOT verified must NOT be stamped. This is the
	// direction that makes health's verdict mean something: if a failed remote
	// were stamped anyway, per-remote stamps would be per-remote in name only.
	if _, err := os.Stat(filepath.Join(freshnessDir, "app@origin")); err == nil {
		t.Fatalf("the failed remote was stamped as verified")
	}
}

// writeMirrorHealthFake builds the fake binaries `gc dolt health` needs, with a
// database "app" that has the two named remotes configured.
func writeMirrorHealthFake(t *testing.T, binDir string, remotes []string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "gc"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(binDir, "nc"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(binDir, "lsof"), "#!/bin/sh\necho 424242\n")
	writeExecutable(t, filepath.Join(binDir, "date"), `#!/bin/sh
case "$1" in
  +%s%N) echo 2000000000000000000 ;;
  +%s) echo 2000000000 ;;
  *) /bin/date "$@" ;;
esac
`)
	writeExecutable(t, filepath.Join(binDir, "ps"), `#!/bin/sh
if [ "$1" = "-p" ]; then echo " 424242"; exit 0; fi
exit 0
`)
	// Emit the CSV bodies as shell here-doc-free literals so the fake stays a
	// plain `case` dispatch with no quoting subtleties of its own.
	namesCSV := "name\\n"
	for _, r := range remotes {
		namesCSV += r + "\\n"
	}
	body := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\"SELECT 1\"*) exit 0 ;;\n" +
		"  *\"dolt_log\"*) printf 'COUNT(*)\\n1\\n' ;;\n" +
		"  *\"FROM issues\"*) printf 'COUNT(*)\\n0\\n' ;;\n" +
		"  *\"SELECT name FROM dolt_remotes\"*) printf '" + namesCSV + "' ;;\n" +
		fmt.Sprintf("  *\"FROM dolt_remotes\"*) printf 'COUNT(*)\\n%d\\n' ;;\n", len(remotes)) +
		"esac\n" +
		"exit 0\n"
	writeExecutable(t, filepath.Join(binDir, "dolt"), body)
}

// runMirrorHealth runs `gc dolt health --json` over a city whose "app" database
// has the given fresh stamps, and returns the raw JSON.
//
// legacyRemote, when non-empty, ALSO seeds the pre-ga-3o5xrw per-database stamp
// (`app`) naming that remote. That is what the old sync path actually wrote, so
// seeding it is what makes a reverted health check reproduce the historical
// false green rather than merely reading no stamp at all. Without it, reverting
// health fails for the uninteresting reason that the pair-keyed fixture is
// invisible to it — which kills the mutant but proves the wrong thing.
func runMirrorHealth(t *testing.T, remotes []string, freshRemotes []string, parked, legacyRemote string) string {
	t.Helper()
	root := repoRoot(t)
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	if err := os.MkdirAll(filepath.Join(dataDir, "app", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	freshnessDir := filepath.Join(stateDir, "backup-freshness")
	if err := os.MkdirAll(freshnessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const now int64 = 2_000_000_000
	for _, r := range freshRemotes {
		stamp := fmt.Sprintf("pushed_at_epoch=%d\nremote=%s\nrefspec=main:main\n", now-10, r)
		if err := os.WriteFile(filepath.Join(freshnessDir, "app@"+r), []byte(stamp), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if legacyRemote != "" {
		stamp := fmt.Sprintf("pushed_at_epoch=%d\nremote=%s\nrefspec=main:main\n", now-10, legacyRemote)
		if err := os.WriteFile(filepath.Join(freshnessDir, "app"), []byte(stamp), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	binDir := t.TempDir()
	writeMirrorHealthFake(t, binDir, remotes)

	env := append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_PACK_STATE_DIR", "GC_DOLT_DATA_DIR",
		"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_DOLT_MIRROR_PARKED",
		"GC_HEALTH_SKIP_ZOMBIE_SCAN", "PATH"),
		"GC_CITY_PATH="+cityPath, "GC_PACK_DIR="+root, "GC_PACK_STATE_DIR="+stateDir,
		"GC_DOLT_DATA_DIR="+dataDir, "GC_DOLT_HOST=127.0.0.1", "GC_DOLT_PORT=3306",
		"GC_DOLT_USER=root", "GC_DOLT_PASSWORD=", "GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if parked != "" {
		env = append(env, "GC_DOLT_MIRROR_PARKED="+parked)
	}
	cmd := exec.Command("sh", filepath.Join(root, healthScript), "--json")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("health --json: %v\n%s", err, out)
	}
	return string(out)
}

// TestHealthRequiresAStampForEveryConfiguredRemote is the health-side
// mutant-killer, and it is the exact scenario qcore was in all night: two
// remotes configured, one being pushed, one silently receiving nothing.
//
// PRE-FIX FAILURE: the pre-fix check read a single `app` stamp, so one fresh
// remote made the whole database read `"state": "ok"` with `dolt_stale: false`
// — a green durability verdict for a database whose second mirror had not been
// written to. This test fails there on the ok verdict.
func TestHealthRequiresAStampForEveryConfiguredRemote(t *testing.T) {
	// Exactly the state qcore was in: two remotes, `probe` being pushed and
	// stamped, `origin` silently receiving nothing — plus the legacy per-database
	// stamp the old sync path wrote, which is what made this read green.
	out := runMirrorHealth(t, []string{"origin", "probe"}, []string{"probe"}, "", "probe")
	if strings.Contains(out, `"dolt_backup_state": "ok"`) || strings.Contains(out, `"dolt_stale": false`) {
		t.Fatalf("one fresh remote made a two-remote database read healthy:\n%s", out)
	}
	if !strings.Contains(out, `{"name": "app@origin", "state": "unknown"`) {
		t.Fatalf("the unverified remote is not named in the report:\n%s", out)
	}
	if !strings.Contains(out, `"name": "app@probe", "state": "ok"`) {
		t.Fatalf("the verified remote is not reported ok:\n%s", out)
	}
}

// TestHealthGreenWhenEveryRemoteIsVerified is the other direction. Without it,
// a check that simply always reported stale would pass the test above — "fails
// when it should" is only half of a verdict.
func TestHealthGreenWhenEveryRemoteIsVerified(t *testing.T) {
	out := runMirrorHealth(t, []string{"origin", "probe"}, []string{"origin", "probe"}, "", "")
	if !strings.Contains(out, `"dolt_backup_state": "ok"`) || !strings.Contains(out, `"dolt_stale": false`) {
		t.Fatalf("every remote verified but the verdict is not ok:\n%s", out)
	}
}

// TestHealthPrintsParkedMirrorsInsteadOfHidingThem covers the legibility half
// of ga-3o5xrw. qcore's dead mirror was deliberately parked on 2026-08-05, but
// the decision lived only in an env var inside an order file and a note on a
// bead. Three agents in one night each investigated that documented silence as
// a broken alarm and filed it as a monitoring defect before finding the park.
// A parked remote must therefore be excluded from the verdict AND printed with
// its reason, in the same output where the others read fresh or stale.
func TestHealthPrintsParkedMirrorsInsteadOfHidingThem(t *testing.T) {
	out := runMirrorHealth(t, []string{"origin", "probe"}, []string{"probe"}, "app/origin=gated on ga-qo9w", "")
	if !strings.Contains(out, `"name": "app@origin", "state": "parked"`) {
		t.Fatalf("parked remote is not reported as parked:\n%s", out)
	}
	if !strings.Contains(out, "gated on ga-qo9w") {
		t.Fatalf("parked remote is reported without the reason it is parked:\n%s", out)
	}
	// Parking the dead mirror must clear the verdict — otherwise the park does
	// not actually park anything and the alarm stays armed forever.
	if !strings.Contains(out, `"dolt_backup_state": "ok"`) {
		t.Fatalf("parking the unverifiable remote did not clear the verdict:\n%s", out)
	}
}

// TestHealthParkIsPerRemoteNotPerDatabase guards the property that makes the
// park safe. Parking a whole database would also silence its healthy mirrors,
// which is precisely the failure the park exists to keep visible.
func TestHealthParkIsPerRemoteNotPerDatabase(t *testing.T) {
	// "app" alone must not match anything: the key is <db>/<remote>.
	out := runMirrorHealth(t, []string{"origin", "probe"}, []string{"probe"}, "app", "")
	if strings.Contains(out, `"state": "parked"`) {
		t.Fatalf("a database-level park entry silenced a remote:\n%s", out)
	}
	if strings.Contains(out, `"dolt_backup_state": "ok"`) {
		t.Fatalf("a database-level park entry cleared the verdict:\n%s", out)
	}
}
