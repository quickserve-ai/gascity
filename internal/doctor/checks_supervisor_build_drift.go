package doctor

import "fmt"

// unknownBuildID is the placeholder cmd/gc stamps when neither the ldflags
// nor the embedded VCS info yielded a commit. It is a STRING, not an empty
// value, so a naive comparison reads it as a real build identity and reports
// drift against every supervisor that knows its own commit.
const unknownBuildID = "unknown"

// SupervisorBuildDriftCheck reports whether the supervisor process is serving
// the same build as the gc binary on disk.
//
// WHY THIS IS A SEPARATE CHECK FROM supervisor-http-api: that check asks "is
// the API up" and answers from the HTTP status code alone. Reachability is not
// currency. A supervisor that has been running for days on a build predating
// the fix you just installed answers 200 to every probe, and the operator reads
// a green line. ga-7qkj was filed on exactly that shape — doctor validated dolt
// topology from config while a wedged orphan was live, and a rollup declared the
// split-brain "moot" on the false green.
//
// THE DECISION IS THREE-WAY, DELIBERATELY. DetectBinaryDrift (cmd/gc/drift.go)
// answers a two-way question — drifted or not — and returns FALSE when either
// build identity is missing, documented there as "unknown — cannot compare".
// That is correct for `gc start`, which falls back to an mtime signal. It is
// WRONG for a health check: rendering "cannot compare" as a pass is a vacuous
// OK, the failure StatusSkipped exists to prevent (see its docstring in
// types.go, and the dolt-local-only-remote incident it cites). So this check
// separates "I compared them and they match" from "I could not compare", and
// only the former is ever StatusOK.
//
// The comparison itself is NOT duplicated here. cmd/gc computes it with the
// existing DetectBinaryDrift and passes the verdict in, so there is exactly one
// definition of what drift means. This check owns only the presentation
// decision, which keeps it pure and testable without a live supervisor.
type SupervisorBuildDriftCheck struct {
	supervisorRunning bool
	localBuildID      string
	// fetchServing reads the SERVING build identity, lazily. It is a function
	// rather than a value so the probe costs nothing when the check does not
	// run: this check is not warmup-eligible, and gathering at construction
	// would spend a round-trip on every `gc start` warm-up scan that then
	// filters it out.
	fetchServing func() (string, error)
	// drifted is the comparison itself, injected so there is exactly ONE
	// definition of what drift means. cmd/gc passes DetectBinaryDrift; this
	// check never reimplements it, it only decides whether a comparison was
	// possible at all.
	drifted func(local, serving string) bool
}

// NewSupervisorBuildDriftCheck returns a check comparing the serving build
// against the installed one. All inputs are gathered by the caller — matching
// the convention checks_supervisor_unit_ownership.go states, so the check
// itself performs no I/O.
func NewSupervisorBuildDriftCheck(
	supervisorRunning bool,
	localBuildID string,
	fetchServing func() (string, error),
	drifted func(local, serving string) bool,
) *SupervisorBuildDriftCheck {
	return &SupervisorBuildDriftCheck{
		supervisorRunning: supervisorRunning,
		localBuildID:      localBuildID,
		fetchServing:      fetchServing,
		drifted:           drifted,
	}
}

// WarmupEligible reports that this check stays out of `gc start`'s warm-up
// scan. `gc start` already detects binary drift on its own path — it is the
// caller this check borrows DetectBinaryDrift from — so running it there would
// ask the same question twice and slow the scan to do it.
func (c *SupervisorBuildDriftCheck) WarmupEligible() bool { return false }

// Name returns the check identifier.
func (c *SupervisorBuildDriftCheck) Name() string { return "supervisor-build-drift" }

// CanFix reports that this check does not support automatic remediation.
// Restarting a supervisor is not a repair doctor may perform unasked: it
// interrupts every running session. `gc start` owns that decision.
func (c *SupervisorBuildDriftCheck) CanFix() bool { return false }

// Fix is a no-op; CanFix returns false.
func (c *SupervisorBuildDriftCheck) Fix(_ *CheckContext) error { return nil }

// usableBuildID reports whether an identity can take part in a comparison.
// Empty means the reporter did not supply one; "unknown" means it supplied a
// placeholder, which is the same evidential state wearing a value.
func usableBuildID(id string) bool {
	return id != "" && id != unknownBuildID
}

// Run compares the serving build identity against the installed one.
func (c *SupervisorBuildDriftCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	if !c.supervisorRunning {
		r.Status = StatusSkipped
		r.Message = "supervisor not running — no serving build to compare"
		return r
	}
	// Decided BEFORE the probe: if this side has no identity the answer cannot
	// change, so there is no reason to spend a round-trip discovering that.
	if !usableBuildID(c.localBuildID) {
		r.Status = StatusSkipped
		r.Message = "this gc binary reports no build identity, so it cannot be compared against the serving one"
		return r
	}
	if c.fetchServing == nil || c.drifted == nil {
		r.Status = StatusSkipped
		r.Message = "no supervisor probe wired — serving build not assessed"
		return r
	}

	serving, err := c.fetchServing()
	if err != nil {
		r.Status = StatusSkipped
		r.Message = fmt.Sprintf("cannot read supervisor /health, so the serving build is unknown: %v", err)
		return r
	}
	if !usableBuildID(serving) {
		r.Status = StatusSkipped
		r.Message = "supervisor reported no build identity (predates build_id, or was built without it) — serving build cannot be verified"
		return r
	}

	if c.drifted(c.localBuildID, serving) {
		r.Status = StatusWarning
		r.Message = fmt.Sprintf(
			"supervisor is serving build %s but the gc binary on disk is %s — installing gc does not restart the supervisor; run 'gc start' to pick it up",
			serving, c.localBuildID)
		return r
	}
	r.Status = StatusOK
	r.Message = fmt.Sprintf("supervisor is serving the installed build %s", serving)
	return r
}
