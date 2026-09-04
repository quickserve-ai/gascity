// Package dolt_test — regression guard for ga-l9smko.
//
// mol-dog-backup is the only recurring durability job a Gas City runs, and its
// off-box leg used to be reported in ONE WORD inside a summary line while the
// RUN recorded success either way:
//
//	OFFSITE_STATUS="skipped"                     # GC_BACKUP_OFFSITE_PATH unset
//	...
//	OFFSITE_STATUS="failed (non-fatal)"          # rsync died
//	SUMMARY="backup — synced: N/N, offsite: $OFFSITE_STATUS"
//	# ... and the script fell off the end, exit 0, every time.
//
// So "nobody ever configured an off-box copy", "the off-box copy failed" and
// "the off-box copy completed" produced byte-identical durable evidence. The
// city's backup posture was green BY CONSTRUCTION, and measurably so: five
// consecutive `gc order history mol-dog-backup` rows read success on
// 2026-09-03/04 while GC_BACKUP_OFFSITE_PATH appeared nowhere in city.toml or
// any pack.
//
// This file pins the three properties that remove that:
//  1. an off-box copy that DID NOT HAPPEN never reads as one that did — not in
//     the summary, not in the exit code where the failure is dischargeable, and
//     not on the freshness plane;
//  2. only an rsync that exits 0 may write the freshness stamp, so no reader
//     can go green without a real copy behind it (the same fail-closed rule
//     ga-g3p5rm established one plane down for local artifacts);
//  3. an operator may declare a WAIVER, and a waiver is visibly not the same
//     thing as a copy.
package dolt_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const offsiteStampRelPath = ".gc/runtime/packs/dolt/offsite-backup-freshness"

// seedBackupCity builds the smallest city mol-dog-backup will run against: one
// database in the data dir and an artifact dir for it to copy from.
func seedBackupCity(t *testing.T) (cityPath, dataDir, artifactDir, binDir string) {
	t.Helper()
	cityPath = t.TempDir()
	dataDir = filepath.Join(cityPath, "dolt-data")
	artifactDir = filepath.Join(cityPath, ".dolt-backup")
	for _, path := range []string{filepath.Join(dataDir, "prod", ".dolt"), artifactDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	binDir = t.TempDir()
	_ = writeDogFakeGC(t, binDir)
	_ = writeBackupFakeDolt(t, binDir, "2.1.0", 0, "prod")
	return cityPath, dataDir, artifactDir, binDir
}

// writeFailingRsync stubs an rsync that dies the way a real one does when the
// off-box target is unreachable: non-zero, with the reason on stderr.
func writeFailingRsync(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "rsync"), `#!/bin/sh
echo "rsync: connection unexpectedly closed" >&2
exit 12
`)
}

func offsiteStampPath(cityPath string) string {
	return filepath.Join(cityPath, filepath.FromSlash(offsiteStampRelPath))
}

// TestBackupNamesAnUnconfiguredOffsiteLegAndWritesNoStamp is the core of
// ga-l9smko: with no off-box target declared, the run must SAY there is no
// off-box copy and must leave the freshness plane empty, so nothing downstream
// can mistake "never configured" for "copied".
func TestBackupNamesAnUnconfiguredOffsiteLegAndWritesNoStamp(t *testing.T) {
	cityPath, dataDir, _, binDir := seedBackupCity(t)
	_ = writeBackupFakeRsync(t, binDir)

	out := runDogScript(t, "mol-dog-backup.sh", binDir, cityPath, dataDir)

	if !strings.Contains(out, "NO OFF-BOX COPY") {
		t.Fatalf("an unconfigured offsite leg must say so in the summary; got:\n%s", out)
	}
	if strings.Contains(out, "offsite: skipped") {
		t.Fatalf("%q is the pre-ga-l9smko wording that read as a benign no-op:\n%s", "offsite: skipped", out)
	}
	if !strings.Contains(out, "last off-box copy: never") {
		t.Fatalf("the run must date the last real off-box copy as never; got:\n%s", out)
	}
	if _, err := os.Stat(offsiteStampPath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("no rsync ran, so no offsite freshness stamp may exist (stat err = %v)", err)
	}
}

// TestBackupUnconfiguredOffsiteFailsTheRunWhenRequired pins the policy knob.
// The default stays 0 deliberately — an order that is red on every cycle for a
// condition its owner cannot discharge is what got qcore/origin PARKED, and the
// park is what made a real outage invisible for three days (ga-8bt85j) — but a
// city that has decided an off-box copy is mandatory must be able to say so and
// get a failing history row.
func TestBackupUnconfiguredOffsiteFailsTheRunWhenRequired(t *testing.T) {
	cityPath, dataDir, _, binDir := seedBackupCity(t)
	_ = writeBackupFakeRsync(t, binDir)

	out, err := runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir,
		"GC_BACKUP_OFFSITE_REQUIRED=1")
	if err == nil {
		t.Fatalf("GC_BACKUP_OFFSITE_REQUIRED=1 with no target and no waiver must fail the run; got exit 0:\n%s", out)
	}
	if !strings.Contains(out, "FAILING THE RUN") {
		t.Fatalf("a failing run must name why on stderr; got:\n%s", out)
	}
}

// TestBackupUnconfiguredOffsiteSucceedsByDefault is the other half of the knob:
// the default must NOT turn the city red, or the fix reintroduces ga-8bt85j.
func TestBackupUnconfiguredOffsiteSucceedsByDefault(t *testing.T) {
	cityPath, dataDir, _, binDir := seedBackupCity(t)
	_ = writeBackupFakeRsync(t, binDir)

	if _, err := runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir); err != nil {
		t.Fatalf("with GC_BACKUP_OFFSITE_REQUIRED unset the run must still exit 0: %v", err)
	}
}

// TestBackupHonoursAnOffsiteWaiverAndStillWritesNoStamp — a declared operator
// risk acceptance is reported as a waiver, never as a copy, and never stamps
// the freshness plane.
func TestBackupHonoursAnOffsiteWaiverAndStillWritesNoStamp(t *testing.T) {
	cityPath, dataDir, _, binDir := seedBackupCity(t)
	_ = writeBackupFakeRsync(t, binDir)

	waiverDir := filepath.Join(cityPath, "config", "dolt")
	if err := os.MkdirAll(waiverDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", waiverDir, err)
	}
	waiver := filepath.Join(waiverDir, "offsite-waived")
	body := "# a comment and a blank line must be skipped\n\nlocal-only accepted 2026-01-01, see ga-example\n"
	if err := os.WriteFile(waiver, []byte(body), 0o644); err != nil {
		t.Fatalf("write waiver: %v", err)
	}

	out, err := runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir,
		"GC_BACKUP_OFFSITE_REQUIRED=1")
	if err != nil {
		t.Fatalf("a declared waiver must satisfy GC_BACKUP_OFFSITE_REQUIRED: %v\n%s", err, out)
	}
	if !strings.Contains(out, "waived: local-only accepted 2026-01-01, see ga-example") {
		t.Fatalf("the waiver reason must reach the summary; got:\n%s", out)
	}
	if _, err := os.Stat(offsiteStampPath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("a waiver records that no copy is expected, not that one exists (stat err = %v)", err)
	}
}

// TestBackupFailsTheRunWhenAConfiguredOffsiteCopyDies is the regression guard
// for the wording that hid this: the old branch labelled a dead rsync
// "failed (non-fatal)" and exited 0. An off-box copy that did not happen is not
// a non-fatal detail — it is the entire purpose of the leg.
func TestBackupFailsTheRunWhenAConfiguredOffsiteCopyDies(t *testing.T) {
	cityPath, dataDir, _, binDir := seedBackupCity(t)
	writeFailingRsync(t, binDir)
	offsiteDir := filepath.Join(cityPath, "offsite")
	if err := os.MkdirAll(offsiteDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", offsiteDir, err)
	}

	out, err := runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir,
		"GC_BACKUP_OFFSITE_PATH="+offsiteDir)
	if err == nil {
		t.Fatalf("a configured offsite leg whose rsync died must fail the run; got exit 0:\n%s", out)
	}
	if strings.Contains(out, "non-fatal") {
		t.Fatalf("the pre-ga-l9smko %q wording must be gone:\n%s", "non-fatal", out)
	}
	if _, err := os.Stat(offsiteStampPath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("a failed rsync must never stamp the freshness plane (stat err = %v)", err)
	}
}

// TestBackupStampsAndDatesARealOffsiteCopy — the positive control. Without it
// the tests above would pass on a script that simply never stamps anything.
func TestBackupStampsAndDatesARealOffsiteCopy(t *testing.T) {
	cityPath, dataDir, _, binDir := seedBackupCity(t)
	_ = writeBackupFakeRsync(t, binDir)
	offsiteDir := filepath.Join(cityPath, "offsite")
	if err := os.MkdirAll(offsiteDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", offsiteDir, err)
	}

	out := runDogScript(t, "mol-dog-backup.sh", binDir, cityPath, dataDir,
		"GC_BACKUP_OFFSITE_PATH="+offsiteDir)
	if !strings.Contains(out, "offsite: ok") {
		t.Fatalf("a successful rsync must report ok; got:\n%s", out)
	}
	stamp, err := os.ReadFile(offsiteStampPath(cityPath))
	if err != nil {
		t.Fatalf("a successful rsync must write the offsite freshness stamp: %v", err)
	}
	if !strings.Contains(string(stamp), "synced_at_epoch=") {
		t.Fatalf("stamp must carry synced_at_epoch; got:\n%s", stamp)
	}
	if !strings.Contains(string(stamp), "offsite_path="+offsiteDir) {
		t.Fatalf("stamp must name the path it copied to; got:\n%s", stamp)
	}

	// A second run reads the stamp back, so the age is answerable rather than
	// assumed — which is the whole point of item 2 on ga-l9smko.
	out = runDogScript(t, "mol-dog-backup.sh", binDir, cityPath, dataDir,
		"GC_BACKUP_OFFSITE_PATH="+offsiteDir)
	if strings.Contains(out, "last off-box copy: never") {
		t.Fatalf("after a real copy the age must not read never; got:\n%s", out)
	}
}

// --- health plane ------------------------------------------------------------

type offsiteHealthReport struct {
	Backups struct {
		Offsite struct {
			State   string `json:"state"`
			Stale   bool   `json:"stale"`
			AgeSec  int64  `json:"age_sec"`
			Note    string `json:"note"`
			Freshns string `json:"freshness"`
		} `json:"offsite"`
	} `json:"backups"`
}

// runHealthForOffsite drives commands/health/run.sh --json against a throwaway
// city with the Dolt server stubbed out, and parses only the offsite plane.
func runHealthForOffsite(t *testing.T, cityPath string, extraEnv ...string) offsiteHealthReport {
	t.Helper()
	root := repoRoot(t)
	fakeBin := t.TempDir()
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
		"GC_BACKUP_OFFSITE_PATH",
		"GC_DOLT_OFFSITE_BACKUP_STALE_SECS",
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
	cmd.Env = append(cmd.Env, extraEnv...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("health run.sh --json failed: %v\n%s", err, out)
	}
	var report offsiteHealthReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("health run.sh --json returned invalid JSON: %v\n%s", err, out)
	}
	return report
}

func writeOffsiteStamp(t *testing.T, cityPath string, syncedAt time.Time) {
	t.Helper()
	path := offsiteStampPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	body := "synced_at_epoch=" + strconv.FormatInt(syncedAt.Unix(), 10) + "\noffsite_path=/somewhere/off/box\nartifact_dir=\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write offsite stamp: %v", err)
	}
}

// TestHealthOffsitePlaneFailsClosedWithNoStamp — before ga-l9smko this report
// had no offsite plane at all, so a city with no off-box copy whatsoever read
// exactly like one whose copy completed an hour ago.
func TestHealthOffsitePlaneFailsClosedWithNoStamp(t *testing.T) {
	report := runHealthForOffsite(t, t.TempDir())
	if report.Backups.Offsite.State != "unconfigured" {
		t.Fatalf("offsite state = %q, want unconfigured", report.Backups.Offsite.State)
	}
	if !report.Backups.Offsite.Stale {
		t.Fatalf("no off-box copy must never read fresh: stale = %v", report.Backups.Offsite.Stale)
	}
}

// TestHealthOffsitePlaneSeparatesConfiguredButUnprovenFromUnconfigured — the
// two have different remedies (fix a failing job vs decide on a target), so
// they must not collapse into one state.
func TestHealthOffsitePlaneSeparatesConfiguredButUnprovenFromUnconfigured(t *testing.T) {
	report := runHealthForOffsite(t, t.TempDir(), "GC_BACKUP_OFFSITE_PATH=/somewhere/off/box")
	if report.Backups.Offsite.State != "unknown" {
		t.Fatalf("offsite state = %q, want unknown for a configured target with no completed copy", report.Backups.Offsite.State)
	}
	if !report.Backups.Offsite.Stale {
		t.Fatalf("a configured-but-unproven leg must not read fresh: stale = %v", report.Backups.Offsite.Stale)
	}
}

// TestHealthOffsitePlaneReadsARealStamp — positive control, and the only state
// allowed to clear `stale`.
func TestHealthOffsitePlaneReadsARealStamp(t *testing.T) {
	cityPath := t.TempDir()
	writeOffsiteStamp(t, cityPath, time.Now().Add(-90*time.Minute))
	report := runHealthForOffsite(t, cityPath)
	if report.Backups.Offsite.State != "ok" {
		t.Fatalf("offsite state = %q, want ok for a 90-minute-old copy", report.Backups.Offsite.State)
	}
	if report.Backups.Offsite.Stale {
		t.Fatalf("a fresh copy must clear stale")
	}
	if report.Backups.Offsite.AgeSec < 5000 || report.Backups.Offsite.AgeSec > 6000 {
		t.Fatalf("age_sec = %d, want ~5400", report.Backups.Offsite.AgeSec)
	}
}

// TestHealthOffsitePlaneGoesStalePastTheThreshold — an old copy is not ok, and
// the threshold is the operator's to set.
func TestHealthOffsitePlaneGoesStalePastTheThreshold(t *testing.T) {
	cityPath := t.TempDir()
	writeOffsiteStamp(t, cityPath, time.Now().Add(-49*time.Hour))
	report := runHealthForOffsite(t, cityPath)
	if report.Backups.Offsite.State != "stale" {
		t.Fatalf("offsite state = %q, want stale for a 49-hour-old copy", report.Backups.Offsite.State)
	}
	if !report.Backups.Offsite.Stale {
		t.Fatalf("a 49-hour-old copy must set stale")
	}
}

// TestHealthOffsitePlaneReportsAWaiverAsNotOk — a waiver is an accepted risk,
// not a second copy, so it must be visible and must not clear `stale`.
func TestHealthOffsitePlaneReportsAWaiverAsNotOk(t *testing.T) {
	cityPath := t.TempDir()
	waiverDir := filepath.Join(cityPath, "config", "dolt")
	if err := os.MkdirAll(waiverDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", waiverDir, err)
	}
	if err := os.WriteFile(filepath.Join(waiverDir, "offsite-waived"),
		[]byte("local-only accepted 2026-01-01, see ga-example\n"), 0o644); err != nil {
		t.Fatalf("write waiver: %v", err)
	}
	report := runHealthForOffsite(t, cityPath)
	if report.Backups.Offsite.State != "waived" {
		t.Fatalf("offsite state = %q, want waived", report.Backups.Offsite.State)
	}
	if !report.Backups.Offsite.Stale {
		t.Fatalf("a waiver accepts the risk, it does not create a copy: stale = %v", report.Backups.Offsite.Stale)
	}
	if !strings.Contains(report.Backups.Offsite.Note, "ga-example") {
		t.Fatalf("the waiver reason must reach the report; note = %q", report.Backups.Offsite.Note)
	}
}
