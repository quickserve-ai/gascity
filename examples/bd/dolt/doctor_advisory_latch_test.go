// Package dolt_test — regression guard for ga-466izs.
//
// mol-dog-doctor's advisory path had no latch and no dedupe. The order fires on
// a 5-minute cooldown and mailed the operator on EVERY pass that any warning was
// non-empty, so a warning that is sticky rather than transient produced 288
// mails/day. Measured 2026-08-13: 43 MEDIUM mails in 4h against a four-week
// baseline of 1-3 per DAY, first on a fail-closed backup warning and then on
// sustained latency.
//
// The mail volume is not the failure being guarded here — alarm fatigue is. An
// advisory arriving 288x/day trains the operator to ignore the channel, so the
// NEXT genuine MEDIUM is invisible (ga-mlvzm3: the human mailbox is already a
// dead-letter box with 187 unread).
//
// The guard has to hold FOUR properties at once, and three of them are ways a
// plausible-looking latch silently does nothing or does harm:
//
//   - suppress a repeat of the SAME incident (the storm), while
//   - still paging when a DIFFERENT warning class appears (an aggregate latch
//     would trade the storm for a blind spot), and
//   - not being defeated by a measurement that DRIFTS between cycles (a
//     signature carrying a raw latency or a backup age changes every pass,
//     matches no marker, and suppresses nothing), and
//   - never latching an alert that was not actually delivered.
package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// doctorFakeDolt is a healthy server: probe succeeds, one connection, no
// databases. Any warning in these tests is therefore driven deliberately.
const doctorFakeDolt = `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"SELECT active_branch()"*)
    printf 'active_branch()\nmain\n'
    exit 0
    ;;
  *"COUNT(*) FROM information_schema.PROCESSLIST"*)
    printf 'COUNT(*)\n1\n'
    exit 0
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\n'
    exit 0
    ;;
esac
exit 0
`

func countAdvisories(t *testing.T, gcLogPath string) int {
	t.Helper()
	raw, err := os.ReadFile(gcLogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read gc log: %v", err)
	}
	return strings.Count(string(raw), "Dolt health advisory")
}

// TestDoctorAdvisoryLatchSuppressesRepeatOfSameIncident is the storm itself:
// the same sticky warning observed on consecutive passes must mail once.
func TestDoctorAdvisoryLatchSuppressesRepeatOfSameIncident(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), doctorFakeDolt)

	// LATENCY_WARN_S=0 makes the latency warning fire on every pass.
	for i := 0; i < 3; i++ {
		runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_LATENCY_WARN_S=0")
	}

	if got := countAdvisories(t, gcLogPath); got != 1 {
		t.Fatalf("sticky warning mailed %d times across 3 passes, want 1 — this is the 288/day storm", got)
	}
}

// TestDoctorAdvisoryLatchIgnoresDriftingBackupAge is the trap that makes a
// naive signature useless. The advisory TEXT changes between passes because the
// backup ages ("13h old" then "20h old"), but it is the same incident and must
// not re-page. A signature built from the human-readable item would differ every
// hour, match no marker, and suppress nothing — while looking correct.
func TestDoctorAdvisoryLatchIgnoresDriftingBackupAge(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	artifactDir := filepath.Join(cityPath, ".dolt-backup")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "prod", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir prod: %v", err)
	}

	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  backup)
    printf 'prod-backup\n'
    exit 0
    ;;
esac
case "$*" in
  *"SELECT active_branch()"*)
    printf 'active_branch()\nmain\n'
    exit 0
    ;;
  *"COUNT(*) FROM information_schema.PROCESSLIST"*)
    printf 'COUNT(*)\n1\n'
    exit 0
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nprod\n'
    exit 0
    ;;
esac
exit 0
`)

	// Pass 1: 13h stale. Pass 2: 20h stale. Both past the 12h default, so both
	// warn — with different prose and the same underlying incident.
	writeDoctorLocalSyncStamp(t, cityPath, "prod", time.Now().Add(-13*time.Hour))
	runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir)
	writeDoctorLocalSyncStamp(t, cityPath, "prod", time.Now().Add(-20*time.Hour))
	runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir)

	raw, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if !strings.Contains(string(raw), "prod backup is 13h old") {
		t.Fatalf("first pass should have reported the stale backup, log:\n%s", raw)
	}
	if got := countAdvisories(t, gcLogPath); got != 1 {
		t.Fatalf("a drifting backup age re-paged %d times, want 1 — the signature is tracking the measurement instead of the incident", got)
	}
}

// TestDoctorAdvisoryLatchPagesWhenNewClassAppears guards the failure mode that
// a too-eager fix introduces: latching the AGGREGATE would let one sticky
// warning swallow a genuinely new one, trading a mail storm for a blind spot.
func TestDoctorAdvisoryLatchPagesWhenNewClassAppears(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)

	// Pass 1: healthy server, latency warning only.
	writeExecutable(t, filepath.Join(binDir, "dolt"), doctorFakeDolt)
	runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_LATENCY_WARN_S=0")
	if got := countAdvisories(t, gcLogPath); got != 1 {
		t.Fatalf("first pass mailed %d advisories, want 1", got)
	}

	// Pass 2: latency STILL warning (latched), plus an orphan database — a new
	// class that must page despite the latched one.
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"SELECT active_branch()"*)
    printf 'active_branch()\nmain\n'
    exit 0
    ;;
  *"COUNT(*) FROM information_schema.PROCESSLIST"*)
    printf 'COUNT(*)\n1\n'
    exit 0
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\ntestdb_leftover\n'
    exit 0
    ;;
esac
exit 0
`)
	runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_LATENCY_WARN_S=0")

	if got := countAdvisories(t, gcLogPath); got != 2 {
		t.Fatalf("a NEW warning class produced %d total advisories, want 2 — a latched class is swallowing new incidents", got)
	}
}

// TestDoctorAdvisoryLatchRearmsAfterConditionClears — a condition that resolves
// and later recurs is a new incident and must page again.
func TestDoctorAdvisoryLatchRearmsAfterConditionClears(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), doctorFakeDolt)

	// Warn, clear (a 60s threshold cannot be exceeded by a fake probe), warn.
	runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_LATENCY_WARN_S=0")
	runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_LATENCY_WARN_S=60")
	runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_LATENCY_WARN_S=0")

	if got := countAdvisories(t, gcLogPath); got != 2 {
		t.Fatalf("recurrence after a healthy pass mailed %d times, want 2 — the latch is not re-arming", got)
	}
}

// TestDoctorAdvisoryLatchHeartbeatResends — a latched incident must not go
// silent forever. It re-sends every GC_DOCTOR_ADVISORY_REPEAT_S.
func TestDoctorAdvisoryLatchHeartbeatResends(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), doctorFakeDolt)

	for i := 0; i < 2; i++ {
		runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir,
			"GC_DOCTOR_LATENCY_WARN_S=0", "GC_DOCTOR_ADVISORY_REPEAT_S=0")
	}

	if got := countAdvisories(t, gcLogPath); got != 2 {
		t.Fatalf("heartbeat produced %d advisories, want 2 — a long-running incident would go silent", got)
	}
}

// TestDoctorAdvisoryLatchNotSetWhenMailFails — latching an alert that was never
// delivered converts a mail outage into a silent one. Undelivered means unlatched.
func TestDoctorAdvisoryLatchNotSetWhenMailFails(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	binDir := t.TempDir()
	attemptLog := filepath.Join(binDir, "attempts.log")
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/usr/bin/env bash
if [ "$1" = "mail" ]; then
  printf 'attempt\n' >> `+attemptLog+`
  echo 'mail dead' >&2
  exit 1
fi
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "dolt"), doctorFakeDolt)

	runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_LATENCY_WARN_S=0")
	runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir, "GC_DOCTOR_LATENCY_WARN_S=0")

	raw, err := os.ReadFile(attemptLog)
	if err != nil {
		t.Fatalf("read attempt log: %v", err)
	}
	if got := strings.Count(string(raw), "attempt"); got != 2 {
		t.Fatalf("mail attempted %d times, want 2 — a failed delivery must not latch", got)
	}
	marker := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "doctor-advisory-latch", "latency")
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("latch marker written despite failed delivery: %s", marker)
	}
}

// TestDoctorUnreachableAlertLatches — the CRITICAL path is the stickiest
// condition the doctor can observe; an hours-long outage must not mail every
// 5 minutes.
func TestDoctorUnreachableAlertLatches(t *testing.T) {
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, "dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	binDir := t.TempDir()
	gcLogPath := writeDogFakeGC(t, binDir)
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/usr/bin/env bash
exit 1
`)

	first := runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir)
	if !strings.Contains(first, "(escalated)") {
		t.Fatalf("first unreachable pass must escalate, output:\n%s", first)
	}
	second := runDogScript(t, "mol-dog-doctor.sh", binDir, cityPath, dataDir)
	if !strings.Contains(second, "(alert latched)") {
		t.Fatalf("second unreachable pass must report the latch, output:\n%s", second)
	}

	raw, err := os.ReadFile(gcLogPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	if got := strings.Count(string(raw), "Dolt server unreachable"); got != 1 {
		t.Fatalf("sustained outage mailed %d times across 2 passes, want 1", got)
	}
}
