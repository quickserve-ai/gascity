package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/packman"
)

func verifySourceTestCity(t *testing.T) string {
	t.Helper()
	clearGCEnv(t)
	dir := t.TempDir()
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "sha:abc123"
`)
	return dir
}

func stubOfflineCheck(t *testing.T, report *packman.CheckReport) {
	t.Helper()
	prev := checkInstalledImports
	t.Cleanup(func() { checkInstalledImports = prev })
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		return report, nil
	}
}

// TestDoImportCheckDoesNotProbeSourcesWithoutTheFlag: the probe needs the
// network, so it must never fire on the default path. gc import check is on
// the doctor path and is expected to be cheap and offline.
func TestDoImportCheckDoesNotProbeSourcesWithoutTheFlag(t *testing.T) {
	dir := verifySourceTestCity(t)
	stubOfflineCheck(t, &packman.CheckReport{CheckedSources: 1})

	prev := verifySourceReachability
	t.Cleanup(func() { verifySourceReachability = prev })
	verifySourceReachability = func(_ string, _ map[string]string) (*packman.CheckReport, error) {
		t.Fatal("gc import check probed sources without --verify-source")
		return nil, nil
	}

	var stdout, stderr bytes.Buffer
	if code := doImportCheck(dir, &stdout, &stderr, false); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "Source pin") {
		t.Fatalf("stdout mentions the source phase without the flag:\n%s", stdout.String())
	}
}

// TestDoImportCheckFailsOnAPinItsSourceCannotProduce is the CLI-level
// regression for ga-dyb5na: this is the state that reported "Import state OK"
// while the core bootstrap pack could not be installed anywhere but this box.
func TestDoImportCheckFailsOnAPinItsSourceCannotProduce(t *testing.T) {
	dir := verifySourceTestCity(t)
	stubOfflineCheck(t, &packman.CheckReport{
		CheckedSources: 1,
		CheckedPins:    map[string]string{"https://example.com/tools.git": "abc123"},
	})

	prev := verifySourceReachability
	t.Cleanup(func() { verifySourceReachability = prev })
	var gotPins map[string]string
	verifySourceReachability = func(_ string, pins map[string]string) (*packman.CheckReport, error) {
		gotPins = pins
		return &packman.CheckReport{
			CheckedSources: 1,
			Issues: []packman.CheckIssue{{
				Severity:   packman.CheckSeverityError,
				Code:       packman.CodePinUnreachableAtSource,
				Source:     "https://example.com/tools.git",
				Commit:     "abc123",
				Message:    "no ref at the declared source reaches the pinned commit (12 ref(s) advertised)",
				RepairHint: "point this import at a source that can produce the pin",
			}},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := doImportCheck(dir, &stdout, &stderr, true)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if got := gotPins["https://example.com/tools.git"]; got != "abc123" {
		t.Fatalf("probe received pins %#v, want the pin the offline walk validated", gotPins)
	}
	out := stdout.String()
	for _, want := range []string{
		"Import state OK: 1 remote import(s) checked",
		"[error] " + packman.CodePinUnreachableAtSource,
		"no ref at the declared source reaches the pinned commit",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

// TestDoImportCheckDoesNotFailOnAProbeThatCouldNotReachAVerdict: an offline
// run must report what it could not determine without claiming the imports are
// broken. A check that cries wolf when the network is down gets ignored when
// it is right.
func TestDoImportCheckDoesNotFailOnAProbeThatCouldNotReachAVerdict(t *testing.T) {
	dir := verifySourceTestCity(t)
	stubOfflineCheck(t, &packman.CheckReport{
		CheckedSources: 1,
		CheckedPins:    map[string]string{"https://example.com/tools.git": "abc123"},
	})

	prev := verifySourceReachability
	t.Cleanup(func() { verifySourceReachability = prev })
	verifySourceReachability = func(_ string, _ map[string]string) (*packman.CheckReport, error) {
		return &packman.CheckReport{
			CheckedSources: 1,
			Issues: []packman.CheckIssue{{
				Severity: packman.CheckSeverityWarning,
				Code:     packman.CodeSourceProbeFailed,
				Source:   "https://example.com/tools.git",
				Commit:   "abc123",
				Message:  "could not determine whether the pin is reachable at its source: dial tcp: no route to host",
			}},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	if code := doImportCheck(dir, &stdout, &stderr, true); code != 0 {
		t.Fatalf("code = %d, want 0 for a warning-only probe; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[warning] "+packman.CodeSourceProbeFailed) {
		t.Fatalf("stdout does not report the undetermined probe:\n%s", stdout.String())
	}
}

// TestDoImportCheckProbesSourcesEvenWhenTheOfflinePassFoundIssues: a pin its
// source cannot produce is exactly what leaves the cache looking broken, so
// gating the probe on a clean offline pass would hide the cause behind its own
// symptom -- which is how ga-dyb5na stayed invisible.
func TestDoImportCheckProbesSourcesEvenWhenTheOfflinePassFoundIssues(t *testing.T) {
	dir := verifySourceTestCity(t)
	stubOfflineCheck(t, &packman.CheckReport{
		CheckedSources: 1,
		CheckedPins:    map[string]string{"https://example.com/tools.git": "abc123"},
		Issues: []packman.CheckIssue{{
			Severity: packman.CheckSeverityError,
			Code:     "missing-cache",
			Source:   "https://example.com/tools.git",
			Commit:   "abc123",
			Message:  "locked import is missing from the local repo cache",
		}},
	})

	prev := verifySourceReachability
	t.Cleanup(func() { verifySourceReachability = prev })
	probed := false
	verifySourceReachability = func(_ string, _ map[string]string) (*packman.CheckReport, error) {
		probed = true
		return &packman.CheckReport{
			CheckedSources: 1,
			Issues: []packman.CheckIssue{{
				Severity: packman.CheckSeverityError,
				Code:     packman.CodePinUnreachableAtSource,
				Source:   "https://example.com/tools.git",
				Commit:   "abc123",
				Message:  "no ref at the declared source reaches the pinned commit (12 ref(s) advertised)",
			}},
		}, nil
	}

	var stdout, stderr bytes.Buffer
	if code := doImportCheck(dir, &stdout, &stderr, true); code != 1 {
		t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !probed {
		t.Fatal("source probe was skipped because the offline pass reported issues")
	}
	out := stdout.String()
	if !strings.Contains(out, "missing-cache") || !strings.Contains(out, packman.CodePinUnreachableAtSource) {
		t.Fatalf("stdout should carry both the symptom and the cause:\n%s", out)
	}
}

// TestImportCheckCommandExposesVerifySourceFlag keeps the opt-in surface from
// being renamed or dropped without a test noticing.
func TestImportCheckCommandExposesVerifySourceFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newImportCheckCmd(&stdout, &stderr)
	flag := cmd.Flags().Lookup("verify-source")
	if flag == nil {
		t.Fatal("gc import check has no --verify-source flag")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--verify-source default = %q, want false", flag.DefValue)
	}
}
