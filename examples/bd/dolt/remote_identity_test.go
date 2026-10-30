package dolt_test

// ga-p5bmfx (forward-ported from graft ad04b4e83e, ga-xyuer S2): sync, pull and
// compact must be able to present a remote IDENTITY, because the server-side
// DOLT_* procedures otherwise authenticate to the remote as the local SQL
// session user (root), and a remotesapi hub that knows a different account
// denies them (ga-tj1bgl).
//
// The password is deliberately absent from every assertion below except the one
// that proves it never reaches argv: it travels in the dolt SERVER's process
// environment (DOLT_REMOTE_PASSWORD), never in the call, because argv is
// world-readable via ps.
//
// The unconfigured call shape is pinned by the existing pull, sync and compact
// tests, which assert the bare CALL text and must pass unchanged.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runIdentityScript drives the real command script for database dbName against
// a fake dolt, returning its combined output and the fake's call log.
func runIdentityScript(t *testing.T, script, dbName string, writeFake func(*testing.T, string) string, extraEnv []string, args ...string) (string, string, error) {
	t.Helper()
	root := repoRoot(t)
	port, cleanup := startReachableTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, dbName, ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	binDir := t.TempDir()
	doltLog := writeFake(t, binDir)
	_ = writeSyncFakeBeadsBD(t, cityPath)

	// filteredEnv drops every inherited GC_* and DOLT_* key, so a knob set on the
	// city that runs this test cannot decide it.
	env := append(filteredEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
	)
	out, err := newPackScriptCmd(root, script, append(env, extraEnv...), append([]string{"--db", dbName}, args...)...).CombinedOutput()
	data, readErr := os.ReadFile(doltLog)
	if readErr != nil {
		t.Fatalf("read fake dolt log: %v", readErr)
	}
	return string(out), string(data), err
}

// runIdentityPull pulls database app from a sole file:// remote: carry's pull
// refuses a sole non-local remote before any DOLT_PULL, which would turn these
// assertions red for a reason unrelated to identity.
func runIdentityPull(t *testing.T, extraEnv ...string) (string, string, error) {
	t.Helper()
	return runIdentityScript(t, pullScript, "app", func(t *testing.T, dir string) string {
		return writePullFakeDoltSoleRemote(t, dir, "file:///example.invalid/repo")
	}, extraEnv)
}

func runIdentitySync(t *testing.T, extraEnv []string, args ...string) (string, string, error) {
	t.Helper()
	return runIdentityScript(t, syncScript, "app", writeSyncFakeDolt, extraEnv, args...)
}

func TestPullPassesConfiguredRemoteIdentity(t *testing.T) {
	out, log, err := runIdentityPull(t, "GC_DOLT_REMOTE_USER_APP_ORIGIN=cherub")
	if err != nil {
		t.Fatalf("pull failed: %v\n%s", err, out)
	}
	if !strings.Contains(log, "CALL DOLT_PULL('--user', 'cherub', 'origin', 'main')") {
		t.Fatalf("identity not passed to DOLT_PULL\nlog:\n%s", log)
	}
}

func TestPullIdentityIsScopedToItsRemote(t *testing.T) {
	// Remote names are not unique across databases and there is deliberately no
	// global fallback, so a knob for another pair must not attach here. Assert on
	// the CALL text: every dolt invocation carries "--user root" as a CLI
	// connection flag, which has nothing to do with the remote identity.
	out, log, err := runIdentityPull(t,
		"GC_DOLT_REMOTE_USER_APP_BACKUP=cherub", "GC_DOLT_REMOTE_USER_OTHER_ORIGIN=cherub",
		"GC_DOLT_REMOTE_USER=cherub", "GC_DOLT_REMOTE_USER_ORIGIN=cherub", "GC_DOLT_REMOTE_USER_APP=cherub")
	if err != nil {
		t.Fatalf("pull failed: %v\n%s", err, out)
	}
	if !strings.Contains(log, "CALL DOLT_PULL('origin', 'main')") {
		t.Fatalf("a knob for another pair changed the call\nlog:\n%s", log)
	}
}

func TestPullNeverPutsRemotePasswordInArgv(t *testing.T) {
	const secret = "hub-p4ssw0rd-must-not-leak"
	out, log, err := runIdentityPull(t, "GC_DOLT_REMOTE_USER_APP_ORIGIN=cherub", "DOLT_REMOTE_PASSWORD="+secret)
	if err != nil {
		t.Fatalf("pull failed: %v\n%s", err, out)
	}
	if !strings.Contains(log, "'--user', 'cherub'") {
		t.Fatalf("identity not passed, so this test proved nothing\nlog:\n%s", log)
	}
	if strings.Contains(log, secret) || strings.Contains(out, secret) {
		t.Fatalf("remote password reached argv or output\nlog:\n%s\noutput:\n%s", log, out)
	}
}

func TestPullRefusesImplausibleRemoteUser(t *testing.T) {
	// The newline cases: command substitution strips trailing newlines, so a
	// check that reads the value through $(...) passes "cherub\n" as cherub and
	// a bare newline as no identity, reaching the remote as the session user.
	for name, user := range map[string]string{
		"sql":              "x'); DROP TABLE issues; --",
		"trailing newline": "cherub\n",
		"only a newline":   "\n",
	} {
		t.Run(name, func(t *testing.T) {
			out, log, err := runIdentityPull(t, "GC_DOLT_REMOTE_USER_APP_ORIGIN="+user)
			if err == nil {
				t.Fatalf("pull accepted an implausible remote user:\n%s", out)
			}
			if !strings.Contains(out, "refusing remote user") {
				t.Fatalf("refusal must say why:\n%s", out)
			}
			if strings.Contains(log, "DOLT_PULL") {
				t.Fatalf("pull ran despite a refused identity\nlog:\n%s", log)
			}
		})
	}
}

func TestSyncPassesRemoteIdentityToFetchAndPush(t *testing.T) {
	out, log, err := runIdentitySync(t, []string{"GC_DOLT_REMOTE_USER_APP_ORIGIN=cherub"})
	if err != nil {
		t.Fatalf("sync failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"CALL DOLT_FETCH('--user', 'cherub', 'origin', 'main')",
		"CALL DOLT_PUSH('--user', 'cherub', 'origin', 'main')",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("dolt log missing %q\nlog:\n%s", want, log)
		}
	}
}

func TestSyncForcePassesRemoteIdentity(t *testing.T) {
	out, log, err := runIdentitySync(t, []string{"GC_DOLT_REMOTE_USER_APP_ORIGIN=cherub"}, "--force")
	if err != nil {
		t.Fatalf("sync --force failed: %v\n%s", err, out)
	}
	want := "CALL DOLT_PUSH('--user', 'cherub', '--force', '--set-upstream', 'origin', 'main')"
	if !strings.Contains(log, want) {
		t.Fatalf("dolt log missing %q\nlog:\n%s", want, log)
	}
}

func TestSyncRefusesImplausibleRemoteUserAndReportsIt(t *testing.T) {
	out, log, err := runIdentitySync(t, []string{"GC_DOLT_REMOTE_USER_APP_ORIGIN=x'); DROP TABLE issues; --"})
	if err == nil {
		t.Fatalf("sync accepted an implausible remote user:\n%s", out)
	}
	if strings.Contains(log, "DOLT_FETCH") || strings.Contains(log, "DOLT_PUSH") {
		t.Fatalf("sync reached the remote despite a refused identity\nlog:\n%s", log)
	}
	// The per-remote failure summary reads last_fail_reason, so a refusal that
	// does not set it prints "unknown error" and names nothing an operator can fix.
	if strings.Contains(out, "unknown error") || !strings.Contains(out, "invalid remote identity") {
		t.Fatalf("sync summary must name the identity refusal:\n%s", out)
	}
}

func TestRemoteIdentityKeyFoldsDotsAndHyphens(t *testing.T) {
	// Both halves fold '-' and '.' to '_'; valid_remote_name admits '.', and a
	// missed fold leaves a key that can never be set.
	writeFake := func(t *testing.T, dir string) string {
		t.Helper()
		logPath := filepath.Join(dir, "dolt.log")
		body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"SELECT name, url FROM dolt_remotes"*)
    printf 'name,url\nhub.west-1,file:///example.invalid/repo\n'
    ;;
esac
exit 0
`
		if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake dolt: %v", err)
		}
		return logPath
	}
	out, log, err := runIdentityScript(t, pullScript, "my-db", writeFake,
		[]string{"GC_DOLT_REMOTE_USER_MY_DB_HUB_WEST_1=cherub"})
	if err != nil {
		t.Fatalf("pull failed: %v\n%s", err, out)
	}
	if !strings.Contains(log, "CALL DOLT_PULL('--user', 'cherub', 'hub.west-1', 'main')") {
		t.Fatalf("identity key did not fold\nlog:\n%s", log)
	}
}

func TestCompactPassesRemoteIdentityPerRemote(t *testing.T) {
	// Compact does its own remote round-trip, so without the same identity it
	// stays denied while sync and pull work. Run with a backup remote beside
	// origin: the knob names origin only, so the backup must stay bare.
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "backup_remote_reconcile",
		"GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_REMOTE_USER_BEADS_ORIGIN=cherub")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, want := range []string{
		"CALL DOLT_FETCH('--user', 'cherub', 'origin')",
		"CALL DOLT_PUSH('--user', 'cherub', '--force', '--set-upstream', 'origin', 'main')",
		"CALL DOLT_FETCH('backup')",
		"CALL DOLT_PUSH('--force', '--set-upstream', 'backup', 'main')",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("dolt log missing %q\nlog:\n%s", want, log)
		}
	}
}

func TestCompactRefusedRemoteUserDefersThePush(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_success",
		"GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_REMOTE_USER_BEADS_ORIGIN=x'); DROP TABLE issues; --")
	if err != nil {
		t.Fatalf("a refused identity is a remote failure, not a compaction failure: %v\n%s", err, out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "DOLT_FETCH") || strings.Contains(string(data), "DOLT_PUSH") {
		t.Fatalf("compact reached the remote despite a refused identity\nlog:\n%s", data)
	}
	if !strings.Contains(out, "refusing remote user") {
		t.Fatalf("refusal must say why:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the skipped push must leave a pending-push marker: %v\n%s", err, out)
	}
}
