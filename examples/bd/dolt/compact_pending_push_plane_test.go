// Package dolt_test — regression guard for ga-2wvmj9.
//
// mol-dog-compactor flattens a database's history. A flatten deliberately
// rewrites lineage, so the next push to a mirror is CORRECTLY rejected as
// non-fast-forward; the compactor records a deferral marker and retries it on
// later runs, verifying the remote head has not moved before force-updating.
// Past GC_DOLT_COMPACT_PENDING_PUSH_MAX_AGE_SECS it stops auto-retrying and
// prints "manual review required before remote push retry".
//
// It prints that to STDOUT, and an order's stdout is persisted nowhere. Measured
// 2026-09-04: commands/compact/run.sh contains zero dolt_escalate and zero
// dolt_notify calls, and nothing outside the compactor itself reads the marker
// directory — not health, not doctor, not any patrol. So an expired deferral is a
// promise to retry that has quietly become a promise nobody keeps. The live proof
// was sitting on disk: a qcore marker created 2026-07-16 with
// expected_remote_head_verified=0, fifty days old, never mentioned by anything.
//
// These tests pin the plane that makes it visible, and in particular pin that it
// agrees with the compactor about two things it would be easy to disagree on:
// WHICH files are live markers, and WHEN one is too old to retry.
package dolt_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type compactPendingReport struct {
	Compaction struct {
		PendingPush struct {
			State        string `json:"state"`
			Stale        bool   `json:"stale"`
			Count        int    `json:"count"`
			OldestAgeSec int64  `json:"oldest_age_sec"`
			MaxAgeSec    int64  `json:"max_age_sec"`
			Databases    []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"databases"`
		} `json:"pending_push"`
	} `json:"compaction"`
}

func runHealthForPendingPush(t *testing.T, cityPath string, extraEnv ...string) compactPendingReport {
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
		"GC_DOLT_COMPACT_PENDING_PUSH_MAX_AGE_SECS",
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
	var report compactPendingReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("health run.sh --json returned invalid JSON: %v\n%s", err, out)
	}
	return report
}

// writePendingPushMarker writes a deferral marker in the shape the compactor
// writes, named exactly for its database — which is how the compactor finds it
// (`[ -f "$dir/$db" ]`).
func writePendingPushMarker(t *testing.T, cityPath, name string, createdAt time.Time) {
	t.Helper()
	dir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	body := "db=" + name + "\n" +
		"reason=remote push retry deferred: remote has unique commits not in local history\n" +
		"created_at=" + createdAt.UTC().Format("2006-01-02T15:04:05Z") + "\n" +
		"remote=origin\nexpected_remote_head=cr5giidc76bhbvkpqrq1naovkqfk1v0j\n" +
		"expected_remote_head_verified=0\nlocal_branch=main\nremote_branch=main\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

func TestHealthPendingPushIsOkWithNoMarkers(t *testing.T) {
	report := runHealthForPendingPush(t, t.TempDir()).Compaction.PendingPush
	if report.State != "ok" || report.Stale || report.Count != 0 {
		t.Fatalf("state=%q stale=%v count=%d, want ok/false/0", report.State, report.Stale, report.Count)
	}
}

// TestHealthPendingPushReportsAFreshDeferralWithoutAlarming — a deferral inside
// the retry window is not a fault: the compactor will pick it up. It must be
// VISIBLE without being stale, or the plane becomes noise and gets ignored,
// which is the failure mode that made the latched alarms useless.
func TestHealthPendingPushReportsAFreshDeferralWithoutAlarming(t *testing.T) {
	cityPath := t.TempDir()
	writePendingPushMarker(t, cityPath, "hq", time.Now().Add(-2*time.Hour))
	report := runHealthForPendingPush(t, cityPath).Compaction.PendingPush
	if report.State != "deferred" {
		t.Fatalf("state = %q, want deferred for a 2h-old marker", report.State)
	}
	if report.Stale {
		t.Fatalf("a deferral the compactor will still retry must not set stale")
	}
	if report.Count != 1 {
		t.Fatalf("count = %d, want 1", report.Count)
	}
}

// TestHealthPendingPushGoesStalePastTheCompactorsOwnThreshold — the moment the
// compactor stops auto-retrying is exactly the moment a human is needed, so the
// two must use one number. Reading a different threshold here would produce a
// window where nobody retries and nobody says so.
func TestHealthPendingPushGoesStalePastTheCompactorsOwnThreshold(t *testing.T) {
	cityPath := t.TempDir()
	writePendingPushMarker(t, cityPath, "hq", time.Now().Add(-50*24*time.Hour))
	report := runHealthForPendingPush(t, cityPath).Compaction.PendingPush
	if report.State != "stale" {
		t.Fatalf("state = %q, want stale for a 50-day-old marker", report.State)
	}
	if !report.Stale {
		t.Fatalf("a marker past the retry threshold must set stale")
	}
	if report.MaxAgeSec != 172800 {
		t.Fatalf("max_age_sec = %d, want the compactor's 172800 default", report.MaxAgeSec)
	}
}

func TestHealthPendingPushHonoursTheThresholdKnob(t *testing.T) {
	cityPath := t.TempDir()
	writePendingPushMarker(t, cityPath, "hq", time.Now().Add(-2*time.Hour))
	if got := runHealthForPendingPush(t, cityPath).Compaction.PendingPush.State; got != "deferred" {
		t.Fatalf("state = %q under the 48h default, want deferred", got)
	}
	got := runHealthForPendingPush(t, cityPath, "GC_DOLT_COMPACT_PENDING_PUSH_MAX_AGE_SECS=600").Compaction.PendingPush
	if got.State != "stale" || !got.Stale {
		t.Fatalf("state=%q stale=%v with a 600s threshold, want stale/true", got.State, got.Stale)
	}
}

// TestHealthPendingPushIgnoresRetiredMarkers — the directory's retirement
// convention renames a resolved marker to <db>.superseded-<bead>-<date>. The
// compactor's lookup is `[ -f "$dir/$db" ]`, so a renamed file is genuinely
// retired; counting it would alarm on history forever. Two such files sit in the
// live directory today.
func TestHealthPendingPushIgnoresRetiredMarkers(t *testing.T) {
	cityPath := t.TempDir()
	dir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	old := "created_at=2026-07-16T04:31:13Z\n"
	for _, name := range []string{"hq.superseded-ga-u2pf-20260717", "hq.superseded-ga-npnoeo-optionC-20260813"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(old), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	report := runHealthForPendingPush(t, cityPath).Compaction.PendingPush
	if report.Count != 0 || report.State != "ok" {
		t.Fatalf("count=%d state=%q, want 0/ok — a renamed marker is retired", report.Count, report.State)
	}
}

// TestHealthPendingPushFailsClosedOnAnUndatableMarker — a marker we cannot date
// is a marker we cannot decide about, and undecidable is never ok.
func TestHealthPendingPushFailsClosedOnAnUndatableMarker(t *testing.T) {
	cityPath := t.TempDir()
	dir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hq"), []byte("db=hq\nreason=deferred\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	report := runHealthForPendingPush(t, cityPath).Compaction.PendingPush
	if report.State != "unknown" || !report.Stale {
		t.Fatalf("state=%q stale=%v, want unknown/true for a marker with no created_at", report.State, report.Stale)
	}
}

// TestHealthPendingPushWorstStateWins — one healthy deferral must not mask an
// expired one.
func TestHealthPendingPushWorstStateWins(t *testing.T) {
	cityPath := t.TempDir()
	writePendingPushMarker(t, cityPath, "hq", time.Now().Add(-2*time.Hour))
	writePendingPushMarker(t, cityPath, "qcore", time.Now().Add(-50*24*time.Hour))
	report := runHealthForPendingPush(t, cityPath).Compaction.PendingPush
	if report.State != "stale" || !report.Stale {
		t.Fatalf("state=%q stale=%v, want stale/true when any marker is expired", report.State, report.Stale)
	}
	if report.Count != 2 {
		t.Fatalf("count = %d, want 2", report.Count)
	}
}
