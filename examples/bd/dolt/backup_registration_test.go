package dolt_test

// ga-xyuer S6a. Two faults the hq backup rehearsal on carry exposed: the dog's
// success stamp never reached disk, because nothing exports the PACK_STATE_DIR it
// needs, and the doctor accepted a backup only under the name <db>-backup while
// `bd backup init` names its backup `default`, so hq read "backup remote missing"
// and left the doctor's freshness loop.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeBackupArtifactManifest writes dir/manifest two hours in the past.
func writeBackupArtifactManifest(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir artifact: %v", err)
	}
	manifest := filepath.Join(dir, "manifest")
	writeTestFile(t, manifest, "x")
	at := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(manifest, at, at); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func readStamp(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	return string(data)
}

// runBackupOrder leaves PACK_STATE_DIR and GC_PACK_STATE_DIR unset, like a run
// where only the city path is known; no order exports the bare name.
func TestBackupOrderStampsASuccessfulSync(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	writeBackupFakeGC(t, binDir)

	out := mustRunBackupOrder(t, binDir, cityPath, "FAKE_CONFIGURED_DBS=prod")
	if !strings.Contains(out, "synced: 1/1") {
		t.Fatalf("unexpected backup summary:\n%s", out)
	}

	stamp := readStamp(t, filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "local-backup-freshness", "prod"))
	if !strings.Contains(stamp, "synced_at_epoch=") {
		t.Fatalf("stamp has no epoch:\n%s", stamp)
	}
	if want := "artifact_dir=" + filepath.Join(cityPath, ".dolt-backup"); !strings.Contains(stamp, want) {
		t.Fatalf("stamp missing %q:\n%s", want, stamp)
	}
}

// gc exports GC_PACK_STATE_DIR to an order, and a city may point it elsewhere.
func TestBackupOrderStampsUnderTheOrdersPackStateDir(t *testing.T) {
	cityPath := resolvedTempDir(t)
	stateDir := t.TempDir()
	binDir := t.TempDir()
	writeBackupFakeGC(t, binDir)

	mustRunBackupOrder(t, binDir, cityPath, "FAKE_CONFIGURED_DBS=prod", "GC_PACK_STATE_DIR="+stateDir)

	readStamp(t, filepath.Join(stateDir, "local-backup-freshness", "prod"))
}

func TestBackupOrderDoesNotStampAFailedSync(t *testing.T) {
	cityPath := resolvedTempDir(t)
	binDir := t.TempDir()
	writeBackupFakeGC(t, binDir)

	mustRunBackupOrder(t, binDir, cityPath,
		"FAKE_CONFIGURED_DBS=prod", "FAKE_SYNC_FAIL_UNTIL=99", "GC_DOLT_BACKUP_SYNC_ATTEMPTS=1")

	stamp := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "local-backup-freshness", "prod")
	if _, err := os.Stat(stamp); !os.IsNotExist(err) {
		t.Fatalf("a failed sync must leave no stamp (stat err=%v)", err)
	}
}

// runDoctorWithBackupListing runs the doctor for one database whose `dolt backup
// -v` prints listing, with its artifact directory holding a 2 h old manifest.
func runDoctorWithBackupListing(t *testing.T, listing string) string {
	t.Helper()
	cityPath := resolvedTempDir(t)
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(filepath.Join(dataDir, "hq", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir db: %v", err)
	}
	writeBackupArtifactManifest(t, filepath.Join(cityPath, ".dolt-backup", "hq"))

	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
case "$1" in
  backup)
    printf '%s\n' "`+strings.ReplaceAll(listing, "CITY", cityPath)+`"
    exit 0
    ;;
esac
case "$*" in
  *"COUNT(*) FROM information_schema.PROCESSLIST"*)
    printf 'COUNT(*)\n1\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nhq\n'
    ;;
esac
exit 0
`)
	runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, doctorBackupStaleEnv)
	gcLog, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	return string(gcLog)
}

func TestDoctorFindsABackupByItsURLUnderAnyName(t *testing.T) {
	gcLog := runDoctorWithBackupListing(t, "default file://CITY/.dolt-backup/hq {}")

	if strings.Contains(gcLog, "hq backup remote missing") {
		t.Fatalf("a backup at the artifact URL read as missing:\n%s", gcLog)
	}
	// In the freshness loop, not merely unreported: the 2 h old manifest is stale.
	if !strings.Contains(gcLog, "hq backup is") {
		t.Fatalf("hq must be judged for freshness:\n%s", gcLog)
	}
}

func TestDoctorStillReportsABackupPointedElsewhere(t *testing.T) {
	gcLog := runDoctorWithBackupListing(t, "default file:///somewhere/else/hq {}")

	if !strings.Contains(gcLog, "hq backup remote missing") {
		t.Fatalf("a backup at another URL must read as missing:\n%s", gcLog)
	}
}
