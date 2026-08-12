// Package dolt_test — regression guard for ga-g3p5rm.
//
// `gc dolt health` used to derive LOCAL backup freshness from the mtime of the
// artifact directory .dolt-backup/<db>, gated only on a manifest file existing.
// A directory's mtime bumps whenever an entry is created inside it, so a
// `dolt backup sync` that wrote chunk files and then died bumped it exactly as a
// completed sync would. The signal measured was "something appeared in this
// directory recently"; the signal reported was "this database has a recent
// backup". Those agree in every case except the one the check exists to catch.
//
// Proven on the hq database 2026-08-12: a sync that exited 124 with stderr
// "context canceled" advanced the directory mtime, and health reported `ok`
// while the newest VALID backup was 6.5 days old. It also self-healed in the
// wrong direction — the more often the backup dog ran and failed, the fresher
// health claimed the backup was.
//
// Freshness now comes from $PACK_STATE_DIR/local-backup-freshness/<db>
// (synced_at_epoch), written only by a sync that exits 0, and readers FAIL
// CLOSED when it is absent.
package dolt_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

type localBackupReport struct {
	Backups struct {
		Local struct {
			State     string `json:"state"`
			Stale     bool   `json:"stale"`
			Databases []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"databases"`
		} `json:"local"`
	} `json:"backups"`
}

// runHealthForLocalBackups drives commands/health/run.sh --json against a
// throwaway city, with the Dolt server stubbed out. Database enumeration falls
// back to scanning .dolt-backup/*/manifest, which is what we want here: this
// test is about the freshness plane, not about SQL discovery.
func runHealthForLocalBackups(t *testing.T, cityPath string) localBackupReport {
	t.Helper()

	root := repoRoot(t)
	fakeBin := t.TempDir()

	// Stub the reachability probes and the dolt client. `dolt` exiting 0 with no
	// output leaves db_info empty, so the script uses the manifest-scan path.
	writeExecutable(t, filepath.Join(fakeBin, "lsof"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeBin, "nc"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(fakeBin, "dolt"), "#!/bin/sh\nexit 0\n")

	cmd := exec.Command("sh", filepath.Join(root, healthScript), "--json")
	cmd.Env = append(filteredEnv(
		"GC_CITY_PATH",
		"GC_PACK_DIR",
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"GC_HEALTH_SKIP_ZOMBIE_SCAN",
		"PATH",
	),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT=1",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("health run.sh --json failed: %v\n%s", err, out)
	}
	var report localBackupReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("health run.sh --json returned invalid JSON: %v\n%s", err, out)
	}
	return report
}

// seedFailedSyncArtifacts reproduces what a KILLED `dolt backup sync` leaves
// behind: an old manifest (never rewritten, because the sync never finished)
// plus freshly written chunk files, which bump the directory's mtime to now.
func seedFailedSyncArtifacts(t *testing.T, cityPath, db string) {
	t.Helper()

	dbDir := filepath.Join(cityPath, ".dolt-backup", db)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dbDir, err)
	}
	manifest := filepath.Join(dbDir, "manifest")
	if err := os.WriteFile(manifest, []byte("5:__DOLT__:stale-root\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	// The manifest is ancient — this is the last actually-good backup.
	old := time.Now().Add(-7 * 24 * time.Hour)
	if err := os.Chtimes(manifest, old, old); err != nil {
		t.Fatalf("Chtimes manifest: %v", err)
	}
	// The orphan chunk a dying sync just wrote. Creating it bumps dbDir's mtime
	// to now, which is precisely the false signal the old code trusted.
	chunk := filepath.Join(dbDir, "orphanchunkfromkilledsync.darc")
	if err := os.WriteFile(chunk, []byte("unreachable chunk data"), 0o644); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
}

func writeLocalSyncStamp(t *testing.T, cityPath, db string, syncedAt time.Time) {
	t.Helper()

	stampDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "local-backup-freshness")
	if err := os.MkdirAll(stampDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", stampDir, err)
	}
	body := "synced_at_epoch=" + strconv.FormatInt(syncedAt.Unix(), 10) + "\nartifact_dir=\n"
	if err := os.WriteFile(filepath.Join(stampDir, db), []byte(body), 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
}

func localStateFor(t *testing.T, report localBackupReport, db string) string {
	t.Helper()

	for _, entry := range report.Backups.Local.Databases {
		if entry.Name == db {
			return entry.State
		}
	}
	t.Fatalf("database %q absent from backups.local.databases: %+v", db, report.Backups.Local)
	return ""
}

// TestHealthLocalBackupRejectsFailedSyncDirectoryMtime is the ga-g3p5rm guard.
// Artifacts are present and the directory mtime is seconds old, but no sync has
// ever recorded success — health must NOT call that ok.
func TestHealthLocalBackupRejectsFailedSyncDirectoryMtime(t *testing.T) {
	cityPath := t.TempDir()
	seedFailedSyncArtifacts(t, cityPath, "hq")

	report := runHealthForLocalBackups(t, cityPath)

	if got := localStateFor(t, report, "hq"); got == "ok" {
		t.Fatalf("hq local backup state = ok, want NOT ok: a killed sync bumped the "+
			"directory mtime while the newest valid backup is 7 days old (ga-g3p5rm). "+
			"full report: %+v", report.Backups.Local)
	}
	if report.Backups.Local.State == "ok" {
		t.Fatalf("backups.local.state = ok, want NOT ok (ga-g3p5rm): %+v", report.Backups.Local)
	}
	if !report.Backups.Local.Stale {
		t.Fatalf("backups.local.stale = false, want true when no sync has proven itself: %+v",
			report.Backups.Local)
	}
}

// TestHealthLocalBackupTrustsSyncSuccessStamp confirms the fix did not simply
// break the signal: a recent success stamp must still read ok.
func TestHealthLocalBackupTrustsSyncSuccessStamp(t *testing.T) {
	cityPath := t.TempDir()
	seedFailedSyncArtifacts(t, cityPath, "hq")
	writeLocalSyncStamp(t, cityPath, "hq", time.Now().Add(-5*time.Minute))

	report := runHealthForLocalBackups(t, cityPath)

	if got := localStateFor(t, report, "hq"); got != "ok" {
		t.Fatalf("hq local backup state = %q, want ok with a 5-minute-old success stamp: %+v",
			got, report.Backups.Local)
	}
}

// TestHealthLocalBackupMarksStaleSyncStampStale proves age is read from the
// stamp, not from the artifact plane: the chunk file is seconds old, so any
// mtime-derived reading would call this fresh.
func TestHealthLocalBackupMarksStaleSyncStampStale(t *testing.T) {
	cityPath := t.TempDir()
	seedFailedSyncArtifacts(t, cityPath, "hq")
	writeLocalSyncStamp(t, cityPath, "hq", time.Now().Add(-72*time.Hour))

	report := runHealthForLocalBackups(t, cityPath)

	if got := localStateFor(t, report, "hq"); got != "stale" {
		t.Fatalf("hq local backup state = %q, want stale with a 72h-old success stamp "+
			"despite a seconds-old chunk file: %+v", got, report.Backups.Local)
	}
}
