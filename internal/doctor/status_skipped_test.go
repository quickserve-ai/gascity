package doctor

import (
	"bytes"
	"strings"
	"testing"
)

// TestStatusSkippedIsNotAFailure pins the core contract: a check that did not
// assess its subject must never be treated as failing (no gating, no --fix, no
// hints) and must never be treated as passing either.
func TestStatusSkippedIsNotAFailure(t *testing.T) {
	cases := []struct {
		status    CheckStatus
		isFailure bool
		assessed  bool
	}{
		{StatusOK, false, true},
		{StatusWarning, true, true},
		{StatusError, true, true},
		{StatusSkipped, false, false},
	}
	for _, tc := range cases {
		if got := tc.status.IsFailure(); got != tc.isFailure {
			t.Errorf("status %v: IsFailure()=%v want %v", tc.status, got, tc.isFailure)
		}
		if got := tc.status.Assessed(); got != tc.assessed {
			t.Errorf("status %v: Assessed()=%v want %v", tc.status, got, tc.assessed)
		}
	}
}

// TestWorseOfNeverLetsSkippedMaskAFinding is the regression guard for the trap
// that adding StatusSkipped created: the const order is NOT severity order
// (StatusSkipped is declared last, so it is numerically greater than
// StatusError). A raw `a > b` comparison would therefore rank a not-assessed
// check as worse than a failing one and swallow the failure during
// aggregation.
func TestWorseOfNeverLetsSkippedMaskAFinding(t *testing.T) {
	cases := []struct {
		a, b, want CheckStatus
	}{
		{StatusSkipped, StatusError, StatusError},
		{StatusError, StatusSkipped, StatusError},
		{StatusSkipped, StatusWarning, StatusWarning},
		{StatusWarning, StatusSkipped, StatusWarning},
		{StatusSkipped, StatusOK, StatusOK},
		{StatusOK, StatusSkipped, StatusOK},
		{StatusSkipped, StatusSkipped, StatusSkipped},
		{StatusOK, StatusError, StatusError},
		{StatusWarning, StatusError, StatusError},
	}
	for _, tc := range cases {
		if got := WorseOf(tc.a, tc.b); got != tc.want {
			t.Errorf("WorseOf(%v,%v)=%v want %v", tc.a, tc.b, got, tc.want)
		}
	}
	// worseStatus is the in-tree caller; it must inherit the same ordering.
	if got := worseStatus(StatusSkipped, StatusError); got != StatusError {
		t.Errorf("worseStatus(Skipped,Error)=%v want StatusError — a skipped "+
			"check must not mask a real error", got)
	}
}

// TestTallyCountsSkippedSeparately proves a not-assessed check is not reported
// as a passing one, which is the whole point of the status.
func TestTallyCountsSkippedSeparately(t *testing.T) {
	r := &Report{}
	r.tally(&CheckResult{Name: "a", Status: StatusOK})
	r.tally(&CheckResult{Name: "b", Status: StatusSkipped})
	r.tally(&CheckResult{Name: "c", Status: StatusSkipped})
	r.tally(&CheckResult{Name: "d", Status: StatusError, Severity: SeverityBlocking})

	if r.Passed != 1 {
		t.Errorf("Passed=%d want 1 (skipped must not count as passed)", r.Passed)
	}
	if r.Skipped != 2 {
		t.Errorf("Skipped=%d want 2", r.Skipped)
	}
	if r.Failed != 1 || r.BlockingFailed != 1 {
		t.Errorf("Failed=%d BlockingFailed=%d want 1/1", r.Failed, r.BlockingFailed)
	}
}

// TestPrintResultSkippedIsVisuallyDistinct covers the acceptance clause that a
// not-assessed check must be distinguishable from a passing one in the output,
// and must not carry failure furniture (advisory tag, fix hints).
func TestPrintResultSkippedIsVisuallyDistinct(t *testing.T) {
	var okBuf, skipBuf bytes.Buffer
	printResult(&okBuf, &CheckResult{Name: "x", Status: StatusOK, Message: "fine"}, false)
	printResult(&skipBuf, &CheckResult{
		Name:     "x",
		Status:   StatusSkipped,
		Message:  "not configured",
		Severity: SeverityAdvisory,
		FixHint:  "do a thing",
	}, false)

	okOut, skipOut := okBuf.String(), skipBuf.String()
	if !strings.Contains(okOut, "✓") {
		t.Errorf("passing check lost its ✓ marker: %q", okOut)
	}
	if strings.Contains(skipOut, "✓") {
		t.Errorf("skipped check rendered with the pass marker ✓ — it reads as a "+
			"pass, which is the exact defect this status exists to fix: %q", skipOut)
	}
	if !strings.Contains(skipOut, "○") {
		t.Errorf("skipped check missing its ○ marker: %q", skipOut)
	}
	if strings.Contains(skipOut, "advisory") {
		t.Errorf("skipped check tagged advisory (failure furniture): %q", skipOut)
	}
	if strings.Contains(skipOut, "hint:") {
		t.Errorf("skipped check printed a fix hint; it did not fail: %q", skipOut)
	}
}

// TestPrintSummaryReportsNotAssessed proves the summary line discloses the gap
// rather than folding it into the passed count.
func TestPrintSummaryReportsNotAssessed(t *testing.T) {
	var buf bytes.Buffer
	PrintSummary(&buf, &Report{Passed: 3, Skipped: 2})
	out := buf.String()
	if !strings.Contains(out, "2 not assessed") {
		t.Errorf("summary hides the not-assessed count: %q", out)
	}
	if !strings.Contains(out, "3 passed") {
		t.Errorf("summary lost the passed count: %q", out)
	}
}
