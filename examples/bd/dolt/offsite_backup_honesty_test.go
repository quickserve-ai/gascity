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

// writeCopyingRsync stubs an rsync that ACTUALLY MOVES BYTES. The shared
// writeBackupFakeRsync only logs its arguments and exits 0, so a test built on it
// proves "the script stamps when rsync exits 0" and can never tell a real copy
// from a lying one — which is the exact class this file exists to close. Every
// positive control here uses this one and then asserts the destination received
// content.
func writeCopyingRsync(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "rsync"), `#!/bin/sh
# args: -a --delete SRC/ DEST/
src=""
dest=""
for a in "$@"; do
  case "$a" in
    -*) continue ;;
  esac
  if [ -z "$src" ]; then src="$a"; else dest="$a"; fi
done
[ -n "$src" ] && [ -n "$dest" ] || exit 2
# Accept a remote-shaped destination (host:/path) and land it locally, so a test
# can exercise the REMOTE branch — the realistic production shape, and the one
# where a local same-volume comparison is meaningless.
case "$dest" in
  /*) : ;;
  *:*) dest="${dest#*:}" ;;
esac
mkdir -p "$dest" || exit 2
(cd "$src" && tar cf - .) | (cd "$dest" && tar xf -) || exit 2
exit 0
`)
}

// writeWaiver declares an operator risk acceptance with the given reason.
func writeWaiver(t *testing.T, cityPath, reason string) {
	t.Helper()
	dir := filepath.Join(cityPath, "config", "dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "offsite-waived"), []byte(reason), 0o644); err != nil {
		t.Fatalf("write waiver: %v", err)
	}
}

// writeDeclaration stands in for what mol-dog-backup publishes every run: what
// off-box target it was told to use. `gc dolt health` runs from a different
// order and cannot read that order's env, so this record is how the fact
// crosses.
func writeDeclaration(t *testing.T, cityPath, declaredPath, outcome string, checkedAt time.Time) {
	t.Helper()
	path := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "offsite-backup-declaration")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	body := "checked_at_epoch=" + strconv.FormatInt(checkedAt.Unix(), 10) +
		"\ndeclared_path=" + declaredPath + "\nlast_outcome=" + outcome + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write declaration: %v", err)
	}
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

	// CRLF on purpose: a comment and a blank line written by a Windows editor
	// must still be skipped, and the reason must not arrive with a raw CR glued
	// on — a CR inside a JSON string makes the health report unparseable.
	writeWaiver(t, cityPath, "# a comment and a blank line must be skipped\r\n\r\nlocal-only accepted 2026-01-01, see ga-example\r\n")

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
	// Remote-shaped so this test fails on the RSYNC, not on the same-volume
	// guard — otherwise it would pass for the wrong reason.
	offsiteTarget := "offsitehost:" + offsiteDir

	out, err := runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir,
		"GC_BACKUP_OFFSITE_PATH="+offsiteTarget)
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
	cityPath, dataDir, artifactDir, binDir := seedBackupCity(t)
	writeCopyingRsync(t, binDir)
	// Something for the copy to actually move, so "ok" means bytes arrived.
	if err := os.WriteFile(filepath.Join(artifactDir, "canary.darc"), []byte("chunk"), 0o644); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	// A second VOLUME cannot be manufactured inside a temp dir, so the positive
	// control uses a REMOTE-shaped target (host:/path) — which is both the
	// realistic production shape and the branch where a local device comparison
	// is meaningless and must be skipped. The same-volume refusal has its own
	// test below.
	offsiteDir := filepath.Join(cityPath, "offsite")
	if err := os.MkdirAll(offsiteDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", offsiteDir, err)
	}
	offsiteTarget := "offsitehost:" + offsiteDir

	out, err := runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir,
		"GC_BACKUP_OFFSITE_PATH="+offsiteTarget)
	if err != nil {
		t.Fatalf("a successful copy must exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "offsite: ok") {
		t.Fatalf("a successful rsync must report ok; got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(offsiteDir, "canary.darc")); err != nil {
		t.Fatalf("ok must mean bytes actually arrived off-box: %v", err)
	}
	stamp, err := os.ReadFile(offsiteStampPath(cityPath))
	if err != nil {
		t.Fatalf("a successful rsync must write the offsite freshness stamp: %v", err)
	}
	if !strings.Contains(string(stamp), "synced_at_epoch=") {
		t.Fatalf("stamp must carry synced_at_epoch; got:\n%s", stamp)
	}
	if !strings.Contains(string(stamp), "offsite_path="+offsiteTarget) {
		t.Fatalf("stamp must name the path it copied to; got:\n%s", stamp)
	}

	// Cross-run persistence: age the stamp on disk so a "never" here can only
	// come from the SECOND run reading what the FIRST one wrote, not from the
	// second run's own write. (Backdating also makes the assertion meaningful —
	// a same-second stamp would read "0s ago" either way.)
	writeOffsiteStamp(t, cityPath, offsiteTarget, time.Now().Add(-2*time.Hour))
	out = runDogScript(t, "mol-dog-backup.sh", binDir, cityPath, dataDir,
		"GC_BACKUP_OFFSITE_PATH="+offsiteTarget)
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

func writeOffsiteStamp(t *testing.T, cityPath, offsitePath string, syncedAt time.Time) {
	t.Helper()
	path := offsiteStampPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	body := "synced_at_epoch=" + strconv.FormatInt(syncedAt.Unix(), 10) +
		"\noffsite_path=" + offsitePath + "\nartifact_dir=\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write offsite stamp: %v", err)
	}
}

// TestHealthOffsitePlaneFailsClosedWithNoStamp — before ga-l9smko this report
// had no offsite plane at all, so a city with no off-box copy whatsoever read
// exactly like one whose copy completed an hour ago.
func TestHealthOffsitePlaneFailsClosedWithNoStamp(t *testing.T) {
	// NOTHING HAS RUN: no stamp and no declaration. This is `unknown`, not
	// `unconfigured` — nobody has reported whether a target is declared, and
	// asserting "unconfigured" here would be the report claiming a fact it never
	// obtained. Both are non-ok and both leave stale set; the distinction is that
	// they have different remedies (start the job vs decide on a target).
	report := runHealthForOffsite(t, t.TempDir())
	if report.Backups.Offsite.State != "unknown" {
		t.Fatalf("offsite state = %q, want unknown when the backup job has never run", report.Backups.Offsite.State)
	}
	if !report.Backups.Offsite.Stale {
		t.Fatalf("no off-box copy must never read fresh: stale = %v", report.Backups.Offsite.Stale)
	}

	// THE JOB RAN AND REPORTED NO TARGET: that is `unconfigured`, and it is a
	// statement backed by an observation rather than by the absence of one.
	cityPath := t.TempDir()
	writeDeclaration(t, cityPath, "", "unconfigured", time.Now())
	report = runHealthForOffsite(t, cityPath)
	if report.Backups.Offsite.State != "unconfigured" {
		t.Fatalf("offsite state = %q, want unconfigured once the job has reported no target", report.Backups.Offsite.State)
	}
	if !report.Backups.Offsite.Stale {
		t.Fatalf("an unconfigured off-box leg must never read fresh")
	}
}

// TestHealthOffsitePlaneSeparatesConfiguredButUnprovenFromUnconfigured — the
// two have different remedies (fix a failing job vs decide on a target), so
// they must not collapse into one state.
func TestHealthOffsitePlaneSeparatesConfiguredButUnprovenFromUnconfigured(t *testing.T) {
	cityPath := t.TempDir()
	// The declaration, not this process's env: `gc dolt health` runs from a
	// different order than mol-dog-backup and inherits none of its env, so a
	// test that set GC_BACKUP_OFFSITE_PATH here would pass while production
	// reported `unconfigured` for a configured city.
	writeDeclaration(t, cityPath, "/somewhere/off/box", "failed", time.Now())
	report := runHealthForOffsite(t, cityPath)
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
	writeDeclaration(t, cityPath, "/somewhere/off/box", "ok", time.Now())
	writeOffsiteStamp(t, cityPath, "/somewhere/off/box", time.Now().Add(-90*time.Minute))
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
	writeDeclaration(t, cityPath, "/somewhere/off/box", "ok", time.Now())
	writeOffsiteStamp(t, cityPath, "/somewhere/off/box", time.Now().Add(-49*time.Hour))
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
	writeWaiver(t, cityPath, "local-only accepted 2026-01-01, see ga-example\n")
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

// --- regression guards for the review round ----------------------------------
//
// Every test below pins a defect an adversarial review found in the FIRST
// version of this change. They are kept as named cases rather than folded into
// the ones above because each one is a distinct way a fail-closed plane fails
// open, and a reader deleting one should have to say which guarantee they are
// dropping.

// TestHealthOffsiteWaiverWithHostileTextStaysValidJSON — the note is
// operator-typed prose interpolated into a JSON document that `gc dolt
// health-check` parses with jq every 30 seconds. An unescaped quote made the
// document unparseable; the consumer's only test is `.server.reachable`, so it
// then reported THE DOLT SERVER AS UNREACHABLE — a false P0 naming the wrong
// subsystem, raised by an operator following this mechanism's own instructions.
func TestHealthOffsiteWaiverWithHostileTextStaysValidJSON(t *testing.T) {
	cityPath := t.TempDir()
	writeWaiver(t, cityPath, "no off-box target yet \u2014 see \"ga-jtjcdy\" and C:\\backup\r\n")
	// runHealthForOffsite json.Unmarshals the report, so an invalid document
	// fails this test at the parse rather than at an assertion.
	report := runHealthForOffsite(t, cityPath)
	if report.Backups.Offsite.State != "waived" {
		t.Fatalf("state = %q, want waived", report.Backups.Offsite.State)
	}
	if strings.ContainsAny(report.Backups.Offsite.Note, "\"\\\r\n") {
		t.Fatalf("note must carry no raw quote, backslash or control char; got %q", report.Backups.Offsite.Note)
	}
	if !strings.Contains(report.Backups.Offsite.Note, "ga-jtjcdy") {
		t.Fatalf("sanitising must not destroy the reason; got %q", report.Backups.Offsite.Note)
	}
}

// TestHealthOffsiteStampForADifferentTargetIsNotOk — a stamp dates a copy to a
// SPECIFIC target. If the declared target has since changed, that copy is not
// the copy the city would now rely on, and reporting it as ok is exactly the
// stale-evidence failure this plane exists to remove.
func TestHealthOffsiteStampForADifferentTargetIsNotOk(t *testing.T) {
	cityPath := t.TempDir()
	writeDeclaration(t, cityPath, "/mnt/new-target", "ok", time.Now())
	writeOffsiteStamp(t, cityPath, "/mnt/usb-that-was-unplugged", time.Now().Add(-10*time.Minute))
	report := runHealthForOffsite(t, cityPath)
	if report.Backups.Offsite.State != "unknown" {
		t.Fatalf("state = %q, want unknown when the stamp names a different target", report.Backups.Offsite.State)
	}
	if !report.Backups.Offsite.Stale {
		t.Fatalf("a copy to a target we no longer use must not clear stale")
	}
}

// TestHealthOffsiteStampWithNoDeclaredTargetIsNotOk — the same failure with the
// target removed entirely: a ten-minute-old stamp must not certify a city that
// has no off-box target at all.
func TestHealthOffsiteStampWithNoDeclaredTargetIsNotOk(t *testing.T) {
	cityPath := t.TempDir()
	writeDeclaration(t, cityPath, "", "unconfigured", time.Now())
	writeOffsiteStamp(t, cityPath, "/mnt/usb-that-was-unplugged", time.Now().Add(-10*time.Minute))
	report := runHealthForOffsite(t, cityPath)
	if report.Backups.Offsite.State != "unknown" || !report.Backups.Offsite.Stale {
		t.Fatalf("state = %q stale = %v, want unknown/true", report.Backups.Offsite.State, report.Backups.Offsite.Stale)
	}
}

// TestHealthOffsiteWaiverOutranksAnEvenFreshStamp — a waiver is a live operator
// decision. Reading the stamp first let an old copy mask it, so a city that had
// declared "we accept having no off-box copy" reported `ok` instead.
func TestHealthOffsiteWaiverOutranksAnEvenFreshStamp(t *testing.T) {
	cityPath := t.TempDir()
	writeWaiver(t, cityPath, "local-only accepted, see ga-example\n")
	writeDeclaration(t, cityPath, "/mnt/old", "ok", time.Now())
	writeOffsiteStamp(t, cityPath, "/mnt/old", time.Now().Add(-1*time.Minute))
	report := runHealthForOffsite(t, cityPath)
	if report.Backups.Offsite.State != "waived" {
		t.Fatalf("state = %q, want waived even with a one-minute-old stamp present", report.Backups.Offsite.State)
	}
	if !report.Backups.Offsite.Stale {
		t.Fatalf("a waiver accepts the risk; it must not clear stale")
	}
}

// TestHealthOffsiteFutureStampIsUnknownNotFresh — clamping a negative age to
// zero made a skewed stamp read permanently fresh. A stamp dated in the future
// is evidence the stamp cannot be trusted, not evidence of a recent copy.
func TestHealthOffsiteFutureStampIsUnknownNotFresh(t *testing.T) {
	cityPath := t.TempDir()
	writeDeclaration(t, cityPath, "/mnt/off", "ok", time.Now())
	writeOffsiteStamp(t, cityPath, "/mnt/off", time.Now().Add(72*time.Hour))
	report := runHealthForOffsite(t, cityPath)
	if report.Backups.Offsite.State != "unknown" {
		t.Fatalf("state = %q, want unknown for a future-dated stamp", report.Backups.Offsite.State)
	}
	if !report.Backups.Offsite.Stale {
		t.Fatalf("a future-dated stamp must not clear stale")
	}
}

// TestHealthOffsiteThresholdKnobIsHonoured — the staleness boundary is the
// operator's to set, and the previous suite asserted that in a comment without
// ever exercising the variable.
func TestHealthOffsiteThresholdKnobIsHonoured(t *testing.T) {
	cityPath := t.TempDir()
	writeDeclaration(t, cityPath, "/mnt/off", "ok", time.Now())
	writeOffsiteStamp(t, cityPath, "/mnt/off", time.Now().Add(-30*time.Minute))

	if got := runHealthForOffsite(t, cityPath).Backups.Offsite.State; got != "ok" {
		t.Fatalf("state = %q under the 24h default, want ok", got)
	}
	got := runHealthForOffsite(t, cityPath, "GC_DOLT_OFFSITE_BACKUP_STALE_SECS=600").Backups.Offsite.State
	if got != "stale" {
		t.Fatalf("state = %q with a 600s threshold, want stale", got)
	}
}

// TestBackupRefusesAnOffsiteTargetOnTheSameVolume — "off-box" is the entire
// claim. `rsync -a --delete X/ Y/` exits 0 whether Y is another machine or a
// sibling directory on the same disk, so without this the plane certified a copy
// that one disk event would take along with the original.
func TestBackupRefusesAnOffsiteTargetOnTheSameVolume(t *testing.T) {
	cityPath, dataDir, _, binDir := seedBackupCity(t)
	writeCopyingRsync(t, binDir)
	sameVolume := filepath.Join(cityPath, "not-really-offsite")
	if err := os.MkdirAll(sameVolume, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", sameVolume, err)
	}

	out, err := runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir,
		"GC_BACKUP_OFFSITE_PATH="+sameVolume)
	if err == nil {
		t.Fatalf("a same-volume target must fail the run; got exit 0:\n%s", out)
	}
	if !strings.Contains(out, "same-volume") {
		t.Fatalf("the refusal must name why; got:\n%s", out)
	}
	if _, err := os.Stat(offsiteStampPath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("a refused target must never stamp the freshness plane (stat err = %v)", err)
	}
}

// TestBackupClearsTheStampWhenACopyFails — the copy runs with --delete, so a run
// killed partway leaves the destination in a state the previous stamp no longer
// describes. Keeping the stamp reported `ok` over a mutilated tree for a whole
// staleness window: the fail-closed reader was the thing that was wrong.
func TestBackupClearsTheStampWhenACopyFails(t *testing.T) {
	cityPath, dataDir, _, binDir := seedBackupCity(t)
	offsiteDir := filepath.Join(cityPath, "offsite")
	if err := os.MkdirAll(offsiteDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", offsiteDir, err)
	}
	offsiteTarget := "offsitehost:" + offsiteDir
	// A stamp from a previous, genuinely successful run.
	writeOffsiteStamp(t, cityPath, offsiteTarget, time.Now().Add(-1*time.Hour))
	writeFailingRsync(t, binDir)

	out, err := runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir,
		"GC_BACKUP_OFFSITE_PATH="+offsiteTarget)
	if err == nil {
		t.Fatalf("a failed copy must fail the run; got exit 0:\n%s", out)
	}
	if _, err := os.Stat(offsiteStampPath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("a failed --delete copy must INVALIDATE the stamp (stat err = %v)", err)
	}
}

// TestBackupPublishesTheDeclarationEveryRun — GC_BACKUP_OFFSITE_PATH is this
// order's env and `gc dolt health` runs from another order, so without a
// published declaration health cannot tell a configured-but-broken city from one
// that never declared a target, and sends the operator after the wrong fix.
func TestBackupPublishesTheDeclarationEveryRun(t *testing.T) {
	cityPath, dataDir, _, binDir := seedBackupCity(t)
	_ = writeBackupFakeRsync(t, binDir)
	declPath := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "offsite-backup-declaration")

	// Unconfigured run: the declaration must still be written, with an empty path.
	if _, err := runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir); err != nil {
		t.Fatalf("unconfigured run must exit 0: %v", err)
	}
	body, err := os.ReadFile(declPath)
	if err != nil {
		t.Fatalf("declaration must be written even when nothing is configured: %v", err)
	}
	if !strings.Contains(string(body), "declared_path=\n") {
		t.Fatalf("declared_path must be empty when no target is configured; got:\n%s", body)
	}
	if !strings.Contains(string(body), "checked_at_epoch=") {
		t.Fatalf("declaration must date itself; got:\n%s", body)
	}

	// Configured-and-failing run: the declaration must name the target, which is
	// the case health could not otherwise distinguish.
	writeFailingRsync(t, binDir)
	target := "offsitehost:/mnt/off"
	if _, err := runDogScriptCommand(t, "mol-dog-backup.sh", binDir, cityPath, dataDir,
		"GC_BACKUP_OFFSITE_PATH="+target); err == nil {
		t.Fatalf("a configured leg whose rsync died must fail the run")
	}
	body, err = os.ReadFile(declPath)
	if err != nil {
		t.Fatalf("read declaration: %v", err)
	}
	if !strings.Contains(string(body), "declared_path="+target) {
		t.Fatalf("declaration must name the declared target; got:\n%s", body)
	}
	if !strings.Contains(string(body), "last_outcome=failed") {
		t.Fatalf("declaration must carry the outcome; got:\n%s", body)
	}
}
