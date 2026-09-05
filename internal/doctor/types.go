// Package doctor provides system health diagnostics for a Gas City workspace.
// It defines a Check interface and runner that executes checks with streaming
// output, optional --fix support, and a summary report.
package doctor

import (
	"io"
	"time"
)

// CheckStatus represents the outcome of a health check.
type CheckStatus int

const (
	// StatusOK means the check passed.
	StatusOK CheckStatus = iota
	// StatusWarning means the check found a non-critical issue.
	StatusWarning
	// StatusError means the check found a critical problem.
	StatusError
	// StatusSkipped means the check did NOT assess its subject — it was
	// unconfigured, inapplicable, or its precondition could not be resolved.
	//
	// It exists to separate two states that StatusOK previously merged:
	//
	//	"I looked and it is fine"   -> StatusOK
	//	"I was not asked to look"   -> StatusSkipped
	//
	// The operator read a green line either way, which is worse than a
	// missing check: a missing check is visibly absent, while a vacuous OK
	// actively reassures. dolt-local-only-remote returned OK on an unset
	// config key for at least 22 hours while hq carried precisely the
	// off-box remote that check exists to catch (ga-cc4wzn / ga-51iq0s).
	//
	// Skipped is NOT a failure: it does not gate dispatch, does not affect
	// exit codes, and never triggers --fix. Use IsFailure to ask whether a
	// status represents an actual problem; do not compare against StatusOK.
	//
	// Only use Skipped where the check genuinely did not assess. When
	// "unconfigured" means the subject CANNOT have the fault (no config, so
	// no config can be wrong), StatusOK remains honest and correct.
	StatusSkipped
)

// IsFailure reports whether the status represents a problem the operator
// should act on. StatusOK (assessed, healthy) and StatusSkipped (not
// assessed) are both non-failures.
//
// Prefer this over `status != StatusOK`, which silently treats a skipped
// check as failing — attempting fixes, printing hints, and marking it
// advisory.
func (s CheckStatus) IsFailure() bool {
	return s == StatusWarning || s == StatusError
}

// Assessed reports whether the check actually evaluated its subject. A
// false result means the check produced no evidence either way.
func (s CheckStatus) Assessed() bool { return s != StatusSkipped }

// severityRank orders statuses by how much they demand the operator's
// attention, for "worst result wins" aggregation.
//
// This exists because the CONST ORDER IS NOT A SEVERITY ORDER. StatusSkipped
// is declared last so that StatusOK keeps the zero value (an unset
// CheckResult must default to OK, not to "not assessed") and so existing
// numeric values stay stable. That leaves StatusSkipped numerically GREATER
// than StatusError, so a raw `a > b` comparison would rank a skipped check as
// worse than a failing one and let it mask a genuine error during
// aggregation. Never compare CheckStatus values with < or > directly.
func (s CheckStatus) severityRank() int {
	switch s {
	case StatusSkipped:
		return 0 // not assessed — never outranks a real finding
	case StatusOK:
		return 1
	case StatusWarning:
		return 2
	case StatusError:
		return 3
	}
	return 0
}

// WorseOf returns whichever status demands more attention, ranking a
// not-assessed result below every assessed one.
func WorseOf(a, b CheckStatus) CheckStatus {
	if b.severityRank() > a.severityRank() {
		return b
	}
	return a
}

// CheckSeverity tells consumers (e.g. dispatch gates) whether a failing check
// should be treated as blocking or merely informational. The zero value is
// SeverityBlocking so existing checks remain blocking without modification.
type CheckSeverity int

const (
	// SeverityBlocking means a failing result should gate consumers
	// (dispatch, automation, exit codes). This is the default.
	SeverityBlocking CheckSeverity = iota
	// SeverityAdvisory means a failing result is informational only;
	// consumers may proceed past it without remediation.
	SeverityAdvisory
)

// Check is a single diagnostic check. Implementations are registered with
// a Doctor and executed sequentially during Run.
type Check interface {
	// Name returns a short, unique identifier for this check (e.g. "city-config").
	Name() string
	// Run executes the check and returns a result.
	Run(ctx *CheckContext) *CheckResult
	// CanFix reports whether this check supports automatic remediation.
	CanFix() bool
	// Fix attempts to automatically remediate the issue found by Run.
	// Only called when CanFix returns true and Run returned a non-OK status.
	Fix(ctx *CheckContext) error
	// WarmupEligible reports whether this check should be included in
	// `gc start`'s warm-up scan (in addition to running on demand via
	// `gc doctor`). Default for all in-tree checks is false; opt in by
	// returning true. Pack-declared checks opt in via `warmup = true`
	// on the pack.toml [[doctor]] entry or doctor.toml manifest.
	WarmupEligible() bool
}

// CheckContext carries shared state for all checks during a doctor run.
type CheckContext struct {
	// CityPath is the absolute path to the city root directory.
	CityPath string
	// Verbose enables extra diagnostic output in check results.
	Verbose bool
	// Output is the writer used for doctor output during Doctor.Run.
	// Checks that need to surface fix-time diagnostics should use this
	// writer so captured doctor output includes the diagnostics.
	Output io.Writer
	// ExplainPostgresAuth, when true, opts checks that implement
	// Renderer into emitting their per-scope resolution table after
	// the standard summary line. Today only PostgresAuthCheck honors
	// this flag.
	ExplainPostgresAuth bool
	// CheckTimeout is the per-check budget the runner will wait before
	// abandoning this check (see Doctor.boundedRun); zero means the check
	// runs unbounded. A check that enforces its own internal deadline MUST
	// derive it from this value rather than hardcoding one, because an inner
	// deadline larger than the outer budget can never fire: the runner
	// abandons the check first and reports "timed out ... (outcome unknown)"
	// as an advisory, discarding the specific diagnostic the check would
	// have produced. That inversion silently disabled order-firing-current's
	// own timeout diagnostic when its internal budget was raised from 15s to
	// 4m against a 60s default outer budget (ga-k3ieg3 / ga-ciymkk).
	CheckTimeout time.Duration
}

// Renderer is implemented by checks that produce additional, optional
// output controlled by a flag in CheckContext (e.g., the
// --explain-postgres-auth resolution table). Renderer is opt-in: the
// doctor runner type-asserts each check and skips the call when the
// check does not implement it.
type Renderer interface {
	RenderExtras(ctx *CheckContext, w io.Writer)
}

// CheckResult holds the outcome of a single check execution.
type CheckResult struct {
	// Name identifies which check produced this result.
	Name string
	// Status is the outcome: OK, Warning, or Error.
	Status CheckStatus
	// Severity classifies a failing Status for gate consumers. Zero
	// value (SeverityBlocking) preserves the legacy "every error gates"
	// behavior; checks that opt in to SeverityAdvisory let callers
	// proceed past their failures.
	Severity CheckSeverity
	// Message is a human-readable summary of the result.
	Message string
	// Details holds extra lines shown only in verbose mode.
	Details []string
	// FixHint is a suggestion shown when the check fails and cannot auto-fix.
	FixHint string
	// FixError describes why an attempted automatic remediation failed.
	FixError string
	// FixAttempted is true when automatic remediation ran but did not
	// leave the check passing.
	FixAttempted bool
	// Fixed is true when --fix successfully remediated the issue.
	Fixed bool
	// TimedOut is true when the check exceeded the doctor's per-check
	// timeout and was abandoned. The check's real outcome is unknown:
	// the runner reports StatusError/SeverityAdvisory so the run keeps
	// going without gating automation on an unfinished check.
	TimedOut bool
}
